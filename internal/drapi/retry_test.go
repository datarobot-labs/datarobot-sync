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
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dropConnection closes the connection without answering: to the client, a reset after the
// request was sent.
func dropConnection(t *testing.T, w http.ResponseWriter) {
	t.Helper()

	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		t.Errorf("hijacking: %v", err)

		return
	}

	if err := conn.Close(); err != nil {
		t.Errorf("closing: %v", err)
	}
}

// A timed-out attempt wraps context.DeadlineExceeded just as a caller's expired context does.
// Only the caller's context may stop the retries.
func TestTimedOutAttemptIsRetried(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			<-r.Context().Done()

			return
		}

		respondOK(t, w, `{"id":"abc"}`)
	}))
	defer srv.Close()

	client, err := New(Config{Endpoint: srv.URL + "/api/v2", Token: "t", RequestTimeout: 100 * time.Millisecond})
	require.NoError(t, err)

	_, err = client.GetContainer(context.Background(), "abc")
	require.NoError(t, err)
	assert.Equal(t, int32(2), calls.Load())
}

// A create that may have landed must not be sent again: the replay makes a second container.
func TestCreateIsNotReplayedOnceItMayHaveLanded(t *testing.T) {
	t.Parallel()

	failures := map[string]func(*testing.T, http.ResponseWriter){
		"502":   func(t *testing.T, w http.ResponseWriter) { respond(t, w, http.StatusBadGateway, `{}`) },
		"504":   func(t *testing.T, w http.ResponseWriter) { respond(t, w, http.StatusGatewayTimeout, `{}`) },
		"reset": dropConnection,
	}

	for name, fail := range failures {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var calls atomic.Int32

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				fail(t, w)
			}))
			defer srv.Close()

			_, err := newTestClient(t, srv).CreateContainer(context.Background())
			require.Error(t, err)
			assert.Equal(t, int32(1), calls.Load())
		})
	}
}

// A 503 says the server did not act, so even a create may go again.
func TestCreateIsRetriedWhenTheServerSaysItDidNotAct(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			respond(t, w, http.StatusServiceUnavailable, `{}`)

			return
		}

		respondOK(t, w, `{"catalogId":"abc"}`)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv).CreateContainer(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int32(2), calls.Load())
}

// Every other request survives a reset: reads, stage uploads, and a commit, whose replay after
// landing answers a 422 that the caller's re-read turns into success.
func TestReplayableRequestsRetryAReset(t *testing.T) {
	t.Parallel()

	calls := map[string]func(context.Context, *Client) error{
		"GetContainer": func(ctx context.Context, c *Client) error {
			_, err := c.GetContainer(ctx, "abc")

			return err
		},
		"CreateStage": func(ctx context.Context, c *Client) error {
			_, err := c.CreateStage(ctx, "abc")

			return err
		},
		"UploadToStage": func(ctx context.Context, c *Client) error {
			return c.UploadBytesToStage(ctx, "abc", "s", "m.json", []byte("{}"))
		},
		"CommitStage": func(ctx context.Context, c *Client) error {
			_, err := c.CommitStage(ctx, "abc", "s", OverwriteError)

			return err
		},
		"OpenFile": func(ctx context.Context, c *Client) error {
			body, _, err := c.OpenFile(ctx, "abc", "p", 0)
			if err != nil {
				return err
			}

			return body.Close()
		},
		"SignedLinks": func(ctx context.Context, c *Client) error {
			_, err := c.SignedLinks(ctx, "abc", []string{"p"}, time.Minute)

			return err
		},
	}

	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var count atomic.Int32

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if count.Add(1) == 1 {
					dropConnection(t, w)

					return
				}

				respondOK(t, w, `{"id":"abc","stageId":"s","catalogId":"abc","links":[]}`)
			}))
			defer srv.Close()

			require.NoError(t, call(context.Background(), newTestClient(t, srv)))
			assert.Equal(t, int32(2), count.Load())
		})
	}
}

// A refused dial never reached the server, so even a create may go again.
func TestNeverSentCoversARefusedDial(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	addr := listener.Addr().String()
	require.NoError(t, listener.Close())

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://"+addr+"/", nil)
	require.NoError(t, err)

	_, err = http.DefaultClient.Do(req) //nolint:bodyclose // the dial fails, so there is no body
	require.Error(t, err)
	assert.True(t, neverSent(err))
	assert.False(t, neverSent(fmt.Errorf("wrapped: %w", context.DeadlineExceeded)))
}

func TestMissingFileNameIsOnlyAMissingFile(t *testing.T) {
	t.Parallel()

	missing := "Filename .dr-sync/packs/p_x.tar.xz does not exist."

	assert.True(t, IsMissingFileName(&APIError{Status: http.StatusUnprocessableEntity, Message: missing}))
	assert.True(t, IsMissingFileName(&APIError{Status: http.StatusNotFound, Message: missing}))
	assert.False(t, IsMissingFileName(&APIError{Status: http.StatusInternalServerError, Message: missing}))
	assert.False(t, IsMissingFileName(&APIError{Status: http.StatusNotFound, Message: "Catalog item abc does not exist."}))
}

func TestTruncateKeepsWholeRunes(t *testing.T) {
	t.Parallel()

	got := truncate("aé", 2)
	assert.Equal(t, "a…", got)
	assert.True(t, utf8.ValidString(got))
}

// The single-item read uses `name` and `type`, not the listing's `catalogName` and
// `catalogType`. Mapping the listing's names here decodes both as "".
func TestGetContainerReadsTheSingleItemFieldNames(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		respondOK(t, w, `{"id":"abc","name":"repo","type":"files"}`)
	}))
	defer srv.Close()

	details, err := newTestClient(t, srv).GetContainer(context.Background(), "abc")
	require.NoError(t, err)

	assert.Equal(t, "repo", details.Name)
	assert.Equal(t, "files", details.Type)
}
