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
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenFileStreamsThroughTheAPIWithCredentials(t *testing.T) {
	t.Parallel()

	var (
		gotPath string
		gotName string
		gotAuth string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotName = r.URL.Query().Get("fileName")
		gotAuth = r.Header.Get("Authorization")

		respondRaw(t, w, http.StatusOK, []byte("pack bytes"))
	}))
	defer srv.Close()

	body, length, err := newTestClient(t, srv).OpenFile(context.Background(), "abc", ".dr-sync/packs/p_x.tar.xz", 0)
	require.NoError(t, err)

	defer body.Close()

	content, err := io.ReadAll(body)
	require.NoError(t, err)

	assert.Equal(t, "/api/v2/files/abc/file/", gotPath)
	assert.Equal(t, ".dr-sync/packs/p_x.tar.xz", gotName, "the name must survive query encoding")
	assert.Equal(t, "Bearer test-token", gotAuth, "a read through the API is authenticated")
	assert.Equal(t, "pack bytes", string(content))
	assert.EqualValues(t, len("pack bytes"), length)
}

func TestOpenFileRangeRequest(t *testing.T) {
	t.Parallel()

	var gotRange string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRange = r.Header.Get("Range")

		respondRaw(t, w, http.StatusPartialContent, []byte("tail"))
	}))
	defer srv.Close()

	body, _, err := newTestClient(t, srv).OpenFile(context.Background(), "abc", "x", 6)
	require.NoError(t, err)

	defer body.Close()

	assert.Equal(t, "bytes=6-", gotRange)
}

func TestOpenFileRejectsIgnoredRange(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		respondRaw(t, w, http.StatusOK, []byte("whole file"))
	}))
	defer srv.Close()

	_, _, err := newTestClient(t, srv).OpenFile(context.Background(), "abc", "x", 6)
	require.Error(t, err, "a full answer to a ranged request must not be read as the tail")
}

func TestOpenFileReportsAMissingName(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		respond(t, w, http.StatusNotFound, `{"message":"Filename x does not exist."}`)
	}))
	defer srv.Close()

	_, _, err := newTestClient(t, srv).OpenFile(context.Background(), "abc", "x", 0)
	require.Error(t, err)

	assert.True(t, IsMissingFileName(err))
	assert.False(t, IsBlobUnreachable(context.Background(), err), "an answer from the API is not an unreachable store")
}

func TestOpenFileSurfacesAuthFailure(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		respond(t, w, http.StatusUnauthorized, `{"message":"bad token"}`)
	}))
	defer srv.Close()

	_, _, err := newTestClient(t, srv).OpenFile(context.Background(), "abc", "x", 0)
	require.Error(t, err)

	assert.True(t, IsAuth(err))
}

func TestOpenFileDropsCredentialsOnRedirect(t *testing.T) {
	t.Parallel()

	var authOnFinal string

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/files/abc/file/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/store/object", http.StatusSeeOther)
	})
	mux.HandleFunc("/store/object", func(w http.ResponseWriter, r *http.Request) {
		authOnFinal = r.Header.Get("Authorization")

		respondRaw(t, w, http.StatusOK, []byte("from the store"))
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	body, _, err := newTestClient(t, srv).OpenFile(context.Background(), "abc", "x", 0)
	require.NoError(t, err)

	defer body.Close()

	assert.Empty(t, authOnFinal, "credentials must not follow a redirect to the object store")
}

func TestIsBlobUnreachable(t *testing.T) {
	t.Parallel()

	t.Run("a refused connection is unreachable", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.NotFoundHandler())
		url := srv.URL + "/blob?sig=1"
		client := newTestClient(t, srv)

		srv.Close()

		_, _, err := client.OpenBlob(context.Background(), url, 0)
		require.Error(t, err)

		assert.True(t, IsBlobUnreachable(context.Background(), err))
	})

	t.Run("a store that answers 403 is reachable", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			respondRaw(t, w, http.StatusForbidden, []byte("expired"))
		}))
		defer srv.Close()

		_, _, err := newTestClient(t, srv).OpenBlob(context.Background(), srv.URL+"/blob?sig=1", 0)
		require.Error(t, err)

		assert.False(t, IsBlobUnreachable(context.Background(), err))
	})

	t.Run("a canceled run is not an unreachable store", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.NotFoundHandler())
		defer srv.Close()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, _, err := newTestClient(t, srv).OpenBlob(ctx, srv.URL+"/blob?sig=1", 0)
		require.Error(t, err)

		assert.False(t, IsBlobUnreachable(ctx, err))
	})

	t.Run("a dial that times out is unreachable", func(t *testing.T) {
		t.Parallel()

		expired, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
		defer cancel()

		<-expired.Done()

		_, dialErr := (&net.Dialer{}).DialContext(expired, "tcp", "127.0.0.1:9")
		require.ErrorIs(t, dialErr, context.DeadlineExceeded)

		wrapped := &transportError{Method: http.MethodGet, Path: blobPath, Err: dialErr}

		assert.True(t, IsBlobUnreachable(context.Background(), wrapped))
		assert.False(t, IsBlobUnreachable(expired, wrapped),
			"when the caller's own deadline passed, the store is not to blame")
	})

	t.Run("no error is not unreachable", func(t *testing.T) {
		t.Parallel()

		assert.False(t, IsBlobUnreachable(context.Background(), nil))
	})
}
