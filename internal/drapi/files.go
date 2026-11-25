// Copyright 2026 DataRobot, Inc. and its affiliates.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package drapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ListOptions filters an allFiles listing.
type ListOptions struct {
	// Prefix restricts results to paths under a directory. It must end in "/" — the API
	// rejects anything else — so a missing slash is added rather than passed through.
	Prefix string
	// FileType filters by extension, e.g. "xz".
	FileType string
	// PageSize is the per-request page. Zero uses MaxListLimit; larger is clamped, since
	// the API answers 422 above 1000.
	PageSize int
}

// listQuery builds the query string for the opening allFiles request.
func listQuery(opts ListOptions) url.Values {
	pageSize := opts.PageSize
	if pageSize <= 0 || pageSize > MaxListLimit {
		pageSize = MaxListLimit
	}

	query := url.Values{}
	query.Set("limit", strconv.Itoa(pageSize))
	query.Set("offset", "0")
	// Lowercase, unlike the capitalized Python-style booleans the multipart form fields
	// want. The API is inconsistent here and both spellings are load-bearing.
	query.Set("recursive", "true")

	if opts.Prefix != "" {
		query.Set("prefix", ensureTrailingSlash(opts.Prefix))
	}

	if opts.FileType != "" {
		query.Set("fileType", opts.FileType)
	}

	return query
}

// ListFiles returns every object in the container matching opts, following pagination.
//
// The default page size is 100 and the cap is 1000, so following `next` is not optional:
// a container with 1001 objects silently truncates otherwise.
func (c *Client) ListFiles(ctx context.Context, containerID string, opts ListOptions) ([]FileEntry, error) {
	var entries []FileEntry

	err := c.EachFile(ctx, containerID, opts, func(entry FileEntry) error {
		entries = append(entries, entry)

		return nil
	})

	return entries, err
}

// EachFile calls visit for every object in the container matching opts. A visit error stops
// the walk and is returned unchanged.
func (c *Client) EachFile(ctx context.Context, containerID string, opts ListOptions, visit func(FileEntry) error) error {
	query := listQuery(opts)

	req := request{
		method: http.MethodGet,
		path:   fmt.Sprintf("files/%s/allFiles/", url.PathEscape(containerID)),
		query:  query,
	}

	page, err := c.listFirstPage(ctx, req, query)
	if err != nil {
		return err
	}

	for {
		for _, entry := range page.Data {
			if err := visit(entry); err != nil {
				return err
			}
		}

		if page.Next == "" {
			return nil
		}

		next, err := nextQuery(page.Next)
		if err != nil {
			return err
		}

		if next.Encode() == req.query.Encode() {
			return fmt.Errorf("GET %s: the next link repeats the page just read: %s", req.path, page.Next)
		}

		req.query = next
		// Reset rather than decode over the previous page: encoding/json treats a JSON
		// `null` as a no-op on a non-pointer field, so a last page's "next": null would
		// otherwise leave the prior page's non-empty Next in place and loop forever.
		page = listResponse{}

		if err := c.do(ctx, req, &page); err != nil {
			return err
		}
	}
}

// listFirstPage issues the opening allFiles request, falling back once to omitting `recursive`
// when the server predates it (see recursiveParamUnsupported). Only the first request can hit
// this: every later page comes from the server's own `next` link, which a server that never
// heard of `recursive` does not generate one of.
func (c *Client) listFirstPage(ctx context.Context, req request, query url.Values) (listResponse, error) {
	var page listResponse

	err := c.do(ctx, req, &page)
	if err == nil || !recursiveParamUnsupported(err) {
		return page, err
	}

	query.Del("recursive")
	req.query = query

	err = c.do(ctx, req, &page)

	return page, err
}

// recursiveParamUnsupported reports whether err is a DataRobot instance predating the release
// that added the `recursive` query param to allFiles/, rejecting it as an unrecognized key.
// Before that param existed the endpoint's only behavior was already what recursive=true asks
// for today — a flat listing, no folder synthesis — so omitting it reproduces the same result.
func recursiveParamUnsupported(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
		return false
	}

	return strings.Contains(apiErr.Message, "recursive") && strings.Contains(apiErr.Message, "not allowed key")
}

// DeletePaths removes files or directories from a container. Directory paths end in "/" and
// delete recursively.
//
// This is a DELETE with a JSON request body, which is unusual but is what the API wants.
func (c *Client) DeletePaths(ctx context.Context, containerID string, paths []string) (DeleteResult, error) {
	if len(paths) == 0 {
		return DeleteResult{}, nil
	}

	body, length, err := jsonBody(map[string][]string{"paths": paths})
	if err != nil {
		return DeleteResult{}, err
	}

	var out DeleteResult

	err = c.do(ctx, request{
		method:        http.MethodDelete,
		path:          fmt.Sprintf("files/%s/allFiles/", url.PathEscape(containerID)),
		body:          body,
		contentLength: length,
		contentType:   "application/json",
	}, &out)

	return out, err
}

// nextQuery takes only the query of a `next` link: the path is the one already requested, and
// the origin is the endpoint's. The query already carries limit/offset/recursive (the API
// spells recursive "True" in it), so it is used as given. The rest of the link is the server's
// own view of itself: behind a proxy it can be http://, an internal host, or miss the path
// prefix the endpoint has, and the request carries the token.
func nextQuery(next string) (url.Values, error) {
	u, err := url.Parse(next)
	if err != nil {
		return nil, fmt.Errorf("parsing next link: %w", err)
	}

	return u.Query(), nil
}

func ensureTrailingSlash(prefix string) string {
	if strings.HasSuffix(prefix, "/") {
		return prefix
	}

	return prefix + "/"
}
