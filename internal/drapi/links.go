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
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SignedLinks returns presigned URLs for the named files.
//
// The API fails the whole request if any single name is missing, so callers should keep
// batches small (MaxLinkBatch) and, on a "does not exist" failure, re-request singly to
// learn which name is the problem. That failure is also the normal way a stale manifest
// surfaces: a pack it references has been compacted away.
func (c *Client) SignedLinks(ctx context.Context, containerID string, names []string, duration time.Duration) ([]Link, error) {
	if len(names) == 0 {
		return nil, nil
	}

	seconds := int(duration.Seconds())
	if seconds <= 0 || seconds > MaxLinkDuration {
		seconds = MaxLinkDuration
	}

	body, length, err := jsonBody(map[string]any{"fileNames": names, "duration": seconds})
	if err != nil {
		return nil, err
	}

	var out linksResponse

	// Stamped before the call, so the recorded lifetime is conservative rather than
	// optimistic: the URL is already ticking while the request is in flight.
	issuedAt := time.Now()

	err = c.do(ctx, request{
		method:        http.MethodPost,
		path:          fmt.Sprintf("files/%s/links/", url.PathEscape(containerID)),
		body:          body,
		contentLength: length,
		contentType:   "application/json",
	}, &out)
	if err != nil {
		return nil, err
	}

	for i := range out.Links {
		out.Links[i].IssuedAt = issuedAt
	}

	return out.Links, nil
}

// LinkLifetime is how long a presigned URL from SignedLinks stays valid.
const LinkLifetime = MaxLinkDuration * time.Second

// LinkRefreshMargin is how much validity a link must have left before a transfer starts.
// A pack can take minutes, so starting one on a nearly-expired URL invites a mid-stream 403.
const LinkRefreshMargin = 5 * time.Minute

// Expiring reports whether the link is too close to expiry to start a new transfer with.
func (l Link) Expiring() bool {
	if l.IssuedAt.IsZero() {
		return true
	}

	return time.Since(l.IssuedAt) > LinkLifetime-LinkRefreshMargin
}

// OpenBlob fetches a presigned URL, optionally from a byte offset, and returns the body.
// The caller owns closing it.
//
// The request deliberately carries no Authorization header: S3 answers 400 when a presigned
// signature and an auth header both appear.
func (c *Client) OpenBlob(ctx context.Context, signedURL string, offset int64) (io.ReadCloser, int64, error) {
	target, err := stripDefaultPort(signedURL)
	if err != nil {
		return nil, 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("building blob request: %w", err)
	}

	req.Header.Set("User-Agent", c.userAgent)

	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}

	resp, err := c.blob.Do(req)
	if err != nil {
		return nil, 0, blobDoError(blobPath, resp, err)
	}

	if err := checkBlobStatus(resp, offset); err != nil {
		resp.Body.Close()

		return nil, 0, err
	}

	return resp.Body, resp.ContentLength, nil
}

// blobDoError wraps a failed round trip of the blob client. Do returns the last response next
// to the error when the redirect policy refuses a hop. The server answered then, so the result
// is not a transport error: it must neither count as an unreachable store nor be retried.
func blobDoError(path string, resp *http.Response, err error) error {
	if resp != nil {
		return fmt.Errorf("%s %s: %w", http.MethodGet, path, err)
	}

	return &transportError{Method: http.MethodGet, Path: path, Err: err}
}

// blobPath marks a presigned fetch in a transport error. IsBlobUnreachable matches it, so
// OpenFile failures never count as an unreachable store.
const blobPath = "presigned blob"

// IsBlobUnreachable reports whether err is a presigned fetch that got no HTTP response, which is
// how a network that blocks the object store looks. It returns false once ctx is done.
func IsBlobUnreachable(ctx context.Context, err error) bool {
	// Ask the context, not the error: a dial timeout matches context.DeadlineExceeded, so the
	// error cannot tell an outage from the caller's own deadline.
	if ctx.Err() != nil {
		return false
	}

	var transportErr *transportError

	return errors.As(err, &transportErr) && transportErr.Path == blobPath
}

// OpenFile streams one object through the API instead of a presigned URL, optionally from a
// byte offset, and returns the body. The caller closes it. Unlike a presigned fetch, the request
// carries the Authorization header, so the client strips it on every redirect. A failure to
// open is retried like any other API request. A failure while reading the body is not.
func (c *Client) OpenFile(ctx context.Context, containerID, name string, offset int64) (io.ReadCloser, int64, error) {
	path := fmt.Sprintf("files/%s/file/", url.PathEscape(containerID))

	target, err := c.resolve(path, url.Values{"fileName": {name}})
	if err != nil {
		return nil, 0, err
	}

	attempt := 0

	for {
		body, length, retryAfter, err := c.openFileOnce(ctx, target, path, offset)
		if err == nil {
			return body, length, nil
		}

		if attempt >= c.maxRetries || !isRetryable(err) {
			return nil, 0, err
		}

		if waitErr := waitBeforeRetry(ctx, attempt, retryAfter); waitErr != nil {
			return nil, 0, waitErr
		}

		attempt++
	}
}

// openFileOnce performs one round trip of OpenFile. The returned duration is the server's
// Retry-After, when it supplied one.
func (c *Client) openFileOnce(ctx context.Context, target, path string, offset int64) (io.ReadCloser, int64, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("building file request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", c.userAgent)

	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}

	resp, err := c.blob.Do(req)
	if err != nil {
		return nil, 0, 0, blobDoError(path, resp, err)
	}

	if resp.StatusCode >= http.StatusBadRequest {
		defer resp.Body.Close()

		// The error body carries the message IsMissingFileName matches.
		errBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		if readErr != nil {
			errBody = nil
		}

		return nil, 0, retryAfterOf(resp), parseAPIError(http.MethodGet, path, resp, errBody)
	}

	if err := checkBlobStatus(resp, offset); err != nil {
		resp.Body.Close()

		return nil, 0, 0, err
	}

	return resp.Body, resp.ContentLength, 0, nil
}

// checkBlobStatus validates an object response against the requested offset. The caller closes
// the body; this only inspects it.
func checkBlobStatus(resp *http.Response, offset int64) error {
	if resp.StatusCode == http.StatusOK && offset == 0 {
		return nil
	}

	if resp.StatusCode == http.StatusPartialContent && offset > 0 {
		return nil
	}

	if resp.StatusCode == http.StatusOK && offset > 0 {
		// The store ignored the Range header. Discarding the prefix would be correct but
		// wasteful, so say so rather than silently re-downloading from zero.
		return &BlobError{
			Status: resp.StatusCode,
			Err:    errors.New("range request was answered in full; this store does not support resume"),
		}
	}

	snippet, readErr := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if readErr != nil {
		snippet = nil
	}

	return &BlobError{
		Status: resp.StatusCode,
		Err:    fmt.Errorf("fetching presigned blob: HTTP %d: %s", resp.StatusCode, truncate(string(snippet), 300)),
	}
}

// stripDefaultPort removes an explicit :443 or :80 from a URL's host.
//
// This is load-bearing, not cosmetic. DataRobot hands back presigned URLs of the form
// https://s3.us-east-1.amazonaws.com:443/..., and SigV4 signs the canonical host, which omits
// the scheme's default port. Go sends the Host header exactly as the URL spells it — port and
// all — so the signed value and the sent value disagree and S3 answers
// SignatureDoesNotMatch. curl strips the port itself, which is why the URL works there and
// nowhere else.
func stripDefaultPort(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parsing presigned URL: %w", err)
	}

	port := parsed.Port()
	if port == "" {
		return raw, nil
	}

	if (parsed.Scheme == "https" && port != "443") || (parsed.Scheme == "http" && port != "80") {
		return raw, nil
	}

	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return raw, nil
	}

	host := parsed.Hostname()

	// Hostname() drops the brackets an IPv6 literal needs.
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}

	parsed.Host = host

	return parsed.String(), nil
}

// BlobError is a failure fetching a presigned URL. Its body is S3's XML rather than a
// DataRobot envelope, so it is kept separate from APIError.
type BlobError struct {
	Status int
	Err    error
}

func (e *BlobError) Error() string {
	return e.Err.Error()
}

func (e *BlobError) Unwrap() error {
	return e.Err
}

// Expired reports whether the failure looks like a presigned URL that has run out. That is
// worth distinguishing because the fix — request a fresh link and resume — is automatic,
// whereas a genuine 403 is not.
func (e *BlobError) Expired() bool {
	return e.Status == http.StatusForbidden
}

// IsBlobExpired reports whether err is a presigned URL that has expired.
func IsBlobExpired(err error) bool {
	var blobErr *BlobError

	return errors.As(err, &blobErr) && blobErr.Expired()
}
