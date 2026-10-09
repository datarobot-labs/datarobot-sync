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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Async polling schedule. The API's own SDK sleeps a flat 5s, which is a poor fit here: the
// operations dr-sync triggers usually finish in well under a second, so it starts tight and
// backs off.
const (
	asyncInitialDelay = 250 * time.Millisecond
	asyncMaxDelay     = 2 * time.Second
	asyncBackoff      = 1.6
	asyncDeadline     = 10 * time.Minute
)

// AsyncTimeoutError is a status URL that never reached a terminal state. It carries the URL
// because the safe response is to report it and reconcile on the next run, never to re-issue
// the operation.
type AsyncTimeoutError struct {
	StatusURL string
	Waited    time.Duration
}

func (e *AsyncTimeoutError) Error() string {
	return fmt.Sprintf("operation did not complete within %s; status: %s", e.Waited.Round(time.Second), e.StatusURL)
}

// asyncStatus is the body of a 200 response from a status URL.
type asyncStatus struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

// PollAsync follows a Location status URL until the operation finishes.
//
// The four documented outcomes are all handled explicitly, and anything unrecognized is a
// hard error rather than another loop iteration — a poller that treats an unknown status as
// "keep waiting" hangs until the deadline and reports the wrong cause.
func (c *Client) PollAsync(ctx context.Context, statusURL string) error {
	started := time.Now()
	delay := asyncInitialDelay

	for {
		done, next, err := c.pollOnce(ctx, statusURL)
		if err != nil {
			return err
		}

		if done {
			return nil
		}

		if next != "" {
			statusURL = next

			continue
		}

		if waited := time.Since(started); waited > asyncDeadline {
			return &AsyncTimeoutError{StatusURL: statusURL, Waited: waited}
		}

		timer := time.NewTimer(delay)

		select {
		case <-ctx.Done():
			timer.Stop()

			return ctx.Err()
		case <-timer.C:
		}

		if delay = time.Duration(float64(delay) * asyncBackoff); delay > asyncMaxDelay {
			delay = asyncMaxDelay
		}
	}
}

// pollOnce performs one status check. It reports whether the operation finished, and a
// replacement URL when the server redirected the poll elsewhere.
func (c *Client) pollOnce(ctx context.Context, statusURL string) (done bool, next string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, statusURL, nil)
	if err != nil {
		return false, "", fmt.Errorf("building status request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")

	// Redirects are the signal here, not a transport detail, so they must not be followed.
	resp, err := c.http.Transport.RoundTrip(req)
	if err != nil {
		return false, "", &transportError{Method: http.MethodGet, Path: "async status", Err: err}
	}

	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusSeeOther:
		// 303: finished. Location names the resulting resource.
		return true, "", nil

	case http.StatusTemporaryRedirect:
		location := resp.Header.Get("Location")
		if location == "" {
			return false, "", errors.New("async status returned 307 with no Location")
		}

		return false, location, nil

	case http.StatusOK:
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		if readErr != nil {
			return false, "", fmt.Errorf("reading async status: %w", readErr)
		}

		return interpretAsyncStatus(body)

	default:
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		if readErr != nil {
			body = nil
		}

		return false, "", parseAPIError(http.MethodGet, "async status", resp, body)
	}
}

// interpretAsyncStatus maps a 200 status body to a decision.
func interpretAsyncStatus(body []byte) (done bool, next string, err error) {
	var status asyncStatus
	if unmarshalErr := json.Unmarshal(body, &status); unmarshalErr != nil {
		return false, "", fmt.Errorf("decoding async status %q: %w", truncate(string(body), 200), unmarshalErr)
	}

	normalized := strings.ToLower(strings.TrimSpace(status.Status))

	switch {
	case normalized == "completed":
		return true, "", nil

	case strings.HasPrefix(normalized, "error"), strings.HasPrefix(normalized, "abort"):
		message := status.Message
		if message == "" {
			message = normalized
		}

		return false, "", fmt.Errorf("operation failed: %s", message)

	case normalized == "initialized" || normalized == "running" || normalized == "inprogress" || normalized == "uploading":
		return false, "", nil

	default:
		return false, "", fmt.Errorf("unrecognized async status %q; refusing to keep polling", status.Status)
	}
}

// decodeJSON reads a JSON body into out, tolerating an empty response.
func decodeJSON(r io.Reader, out any) error {
	body, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}

	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}

	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decoding response %q: %w", truncate(string(body), 200), err)
	}

	return nil
}

// newBytesReader exists so stage.go can build a replayable reader without importing bytes.
func newBytesReader(b []byte) io.Reader {
	return bytes.NewReader(b)
}
