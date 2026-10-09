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
)

// CreateContainer creates an empty Files container and returns its id and first version.
func (c *Client) CreateContainer(ctx context.Context) (Container, error) {
	body, length, err := jsonBody(struct{}{})
	if err != nil {
		return Container{}, err
	}

	var out Container

	err = c.do(ctx, request{
		method:        http.MethodPost,
		path:          "files/",
		body:          body,
		contentLength: length,
		contentType:   "application/json",
	}, &out)
	if err != nil {
		return Container{}, err
	}

	if out.ID == "" {
		return Container{}, errors.New("POST files/: response carried no catalogId")
	}

	return out, nil
}

// GetContainer reads a container's catalog metadata.
func (c *Client) GetContainer(ctx context.Context, containerID string) (ContainerDetails, error) {
	var out ContainerDetails

	err := c.do(ctx, request{
		method: http.MethodGet,
		path:   fmt.Sprintf("catalogItems/%s/", url.PathEscape(containerID)),
	}, &out)

	return out, err
}

// GetExtendedDetails reads the container's current version id.
func (c *Client) GetExtendedDetails(ctx context.Context, containerID string) (ExtendedDetails, error) {
	var out ExtendedDetails

	err := c.do(ctx, request{
		method: http.MethodGet,
		path:   fmt.Sprintf("catalogItems/%s/extendedDetails/", url.PathEscape(containerID)),
	}, &out)

	return out, err
}

// ContainerUpdate is the mutable catalog metadata dr-sync sets.
//
// Tags are a pointer because omitting the field on the wire leaves them alone while sending
// an empty list clears them — a distinction the API makes and callers keep tripping over.
type ContainerUpdate struct {
	Name        string    `json:"name,omitempty"`
	Description string    `json:"description,omitempty"`
	Tags        *[]string `json:"tags,omitempty"`
}

// UpdateContainer patches a container's name, description and tags.
func (c *Client) UpdateContainer(ctx context.Context, containerID string, update ContainerUpdate) error {
	body, length, err := jsonBody(update)
	if err != nil {
		return err
	}

	return c.do(ctx, request{
		method:        http.MethodPatch,
		path:          fmt.Sprintf("catalogItems/%s/", url.PathEscape(containerID)),
		body:          body,
		contentLength: length,
		contentType:   "application/json",
	}, nil)
}

// DeleteContainer soft-deletes a container. Subsequent reads answer 410 Gone.
func (c *Client) DeleteContainer(ctx context.Context, containerID string) error {
	return c.do(ctx, request{
		method: http.MethodDelete,
		path:   fmt.Sprintf("files/%s/", url.PathEscape(containerID)),
	}, nil)
}
