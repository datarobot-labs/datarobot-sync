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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestClient points a client at srv with retries fast enough not to slow the suite.
func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()

	client, err := New(Config{
		Endpoint:   srv.URL + "/api/v2",
		Token:      "test-token",
		MaxRetries: 3,
	})
	require.NoError(t, err)

	return client
}

func TestNewRejectsIncompleteConfig(t *testing.T) {
	t.Parallel()

	_, err := New(Config{Token: "t"})
	require.ErrorContains(t, err, "endpoint")

	_, err = New(Config{Endpoint: "https://app.datarobot.com/api/v2"})
	require.ErrorContains(t, err, "token")

	_, err = New(Config{Endpoint: "ftp://example.com", Token: "t"})
	require.ErrorContains(t, err, "http(s)")
}

// The endpoint already contains /api/v2, so path joining must preserve it. Losing that
// prefix is the classic way a DataRobot client 404s on everything.
func TestPathsResolveUnderApiV2(t *testing.T) {
	t.Parallel()

	var got atomic.Pointer[string]

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uri := r.URL.RequestURI()
		got.Store(&uri)
		respondOK(t, w, `{"count":0,"data":[],"next":null}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv)

	_, err := client.ListFiles(context.Background(), "abc123", ListOptions{})
	require.NoError(t, err)

	require.NotNil(t, got.Load())
	assert.Contains(t, *got.Load(), "/api/v2/files/abc123/allFiles/")
}

func TestResolveRejectsAbsolutePaths(t *testing.T) {
	t.Parallel()

	client, err := New(Config{Endpoint: "https://app.datarobot.com/api/v2", Token: "t"})
	require.NoError(t, err)

	_, err = client.resolve("/files/", nil)
	require.ErrorContains(t, err, "must be relative")
}

func TestEveryRequestCarriesAuthAndUserAgent(t *testing.T) {
	t.Parallel()

	var authHeader, userAgent string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		userAgent = r.Header.Get("User-Agent")

		respondOK(t, w, `{}`)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv).GetContainer(context.Background(), "abc")
	require.NoError(t, err)

	assert.Equal(t, "Bearer test-token", authHeader)
	assert.True(t, strings.HasPrefix(userAgent, "dr-sync/"), "user agent was %q", userAgent)
}

func TestRetriesRateLimitThenSucceeds(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			respond(t, w, http.StatusTooManyRequests, `{"message":"slow down"}`)

			return
		}

		respondOK(t, w, `{"id":"abc","catalogType":"files"}`)
	}))
	defer srv.Close()

	details, err := newTestClient(t, srv).GetContainer(context.Background(), "abc")
	require.NoError(t, err)
	assert.Equal(t, "files", details.Type)
	assert.Equal(t, int32(2), calls.Load())
}

func TestDoesNotRetryClientErrors(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("X-DataRobot-Request-ID", "req-42")
		respond(t, w, http.StatusBadRequest,
			`{"message":{"limit":"value is greater than 1000"},"errorName":"ValidationError"}`)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv).GetContainer(context.Background(), "abc")
	require.Error(t, err)

	assert.Equal(t, int32(1), calls.Load(), "a 400 must not be retried")
	assert.Equal(t, http.StatusBadRequest, StatusOf(err))
	// A field-error object must be flattened rather than dumped as raw JSON.
	require.ErrorContains(t, err, "limit: value is greater than 1000")
	require.ErrorContains(t, err, "ValidationError")
	require.ErrorContains(t, err, "req-42")
}

// A soft-deleted container answers 410, which every caller should read as "gone".
func TestNotFoundCoversGone(t *testing.T) {
	t.Parallel()

	for _, status := range []int{http.StatusNotFound, http.StatusGone} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			respond(t, w, status, `{"message":"Not Found"}`)
		}))

		_, err := newTestClient(t, srv).GetContainer(context.Background(), "abc")
		assert.True(t, IsNotFound(err), "status %d should read as not found", status)

		srv.Close()
	}
}

func TestContextCancellationIsNotRetried(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(50 * time.Millisecond)
		respondOK(t, w, `{}`)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := newTestClient(t, srv).GetContainer(ctx, "abc")
	require.ErrorIs(t, err, context.Canceled)
}
