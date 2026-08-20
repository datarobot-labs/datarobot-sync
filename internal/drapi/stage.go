// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

package drapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
)

// CreateStage opens a staging area for a batch of uploads.
func (c *Client) CreateStage(ctx context.Context, containerID string) (Stage, error) {
	var out Stage

	err := c.do(ctx, request{
		method:      http.MethodPost,
		path:        fmt.Sprintf("files/%s/stages/", url.PathEscape(containerID)),
		contentType: "application/json",
	}, &out)
	if err != nil {
		return Stage{}, err
	}

	if out.StageID == "" {
		return Stage{}, errors.New("POST files/{id}/stages/: response carried no stageId")
	}

	return out, nil
}

// UploadToStage streams one object into a stage under the given name, which may contain
// "/" to place it in a directory.
//
// Uploading the same name twice into one stage is accepted and deduplicated by the API, so
// a retry after an ambiguous failure is safe.
func (c *Client) UploadToStage(ctx context.Context, containerID, stageID string, file FormFile) error {
	if file.FieldName == "" {
		file.FieldName = "file"
	}

	body, length, contentType, err := buildMultipart(nil, file)
	if err != nil {
		return err
	}

	return c.do(ctx, request{
		method:        http.MethodPost,
		path:          fmt.Sprintf("files/%s/stages/%s/upload/", url.PathEscape(containerID), url.PathEscape(stageID)),
		body:          body,
		contentLength: length,
		contentType:   contentType,
		timeout:       uploadTimeout,
	}, nil)
}

// UploadFileToStage streams a local file into a stage under the given remote name.
func (c *Client) UploadFileToStage(ctx context.Context, containerID, stageID, remoteName, localPath string) error {
	info, err := os.Stat(localPath)
	if err != nil {
		return fmt.Errorf("staging %s: %w", remoteName, err)
	}

	return c.UploadToStage(ctx, containerID, stageID, FormFile{
		Filename: remoteName,
		Size:     info.Size(),
		Open: func() (io.ReadCloser, error) {
			return os.Open(localPath)
		},
	})
}

// UploadBytesToStage streams an in-memory object into a stage. Intended for manifests, which
// are small; file content always goes through UploadFileToStage so nothing large is buffered.
func (c *Client) UploadBytesToStage(ctx context.Context, containerID, stageID, remoteName string, content []byte) error {
	return c.UploadToStage(ctx, containerID, stageID, FormFile{
		Filename: remoteName,
		Size:     int64(len(content)),
		Open: func() (io.ReadCloser, error) {
			return io.NopCloser(newBytesReader(content)), nil
		},
	})
}

// CommitStage applies a stage to the container.
//
// This is synchronous — 200 with the new version id, no Location header, in well under a
// second even for a 200-file stage — and atomic: OverwriteError rejects the entire commit if
// any staged name already exists and applies none of it. A Location, should one ever appear,
// is handled by the caller through PollAsync.
func (c *Client) CommitStage(ctx context.Context, containerID, stageID string, overwrite OverwriteStrategy) (CommitResult, error) {
	if overwrite == "" {
		overwrite = OverwriteError
	}

	body, length, err := jsonBody(map[string]string{"stageId": stageID, "overwrite": string(overwrite)})
	if err != nil {
		return CommitResult{}, err
	}

	req := request{
		method:        http.MethodPost,
		path:          fmt.Sprintf("files/%s/fromStage/", url.PathEscape(containerID)),
		body:          body,
		contentLength: length,
		contentType:   "application/json",
		timeout:       uploadTimeout,
	}

	resp, err := c.doRaw(ctx, req)
	if err != nil {
		return CommitResult{}, err
	}

	defer resp.Body.Close()

	var out CommitResult

	if err := decodeJSON(resp.Body, &out); err != nil {
		return CommitResult{}, fmt.Errorf("POST files/%s/fromStage/: %w", containerID, err)
	}

	// The synchronous path is what the API actually does today. The asynchronous one is kept
	// because a large enough stage could plausibly switch to it, and silently treating a
	// pending commit as finished would corrupt the packs-before-manifest ordering.
	if location := resp.Header.Get("Location"); location != "" {
		if err := c.PollAsync(ctx, location); err != nil {
			return CommitResult{}, err
		}
	}

	if out.ContainerID == "" {
		out.ContainerID = containerID
	}

	return out, nil
}
