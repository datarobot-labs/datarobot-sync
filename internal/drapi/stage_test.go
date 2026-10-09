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
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type capturedUpload struct {
	contentLength int64
	fieldName     string
	// disposition is the raw Content-Disposition header. Go's Part.FileName() runs
	// filepath.Base() over the value as reader-side hardening, so it cannot observe the
	// directory component we deliberately send — and which the live API does preserve.
	disposition string
	content     []byte
	chunked     bool
}

// uploadCapturingServer parses a multipart upload the way the API would.
func uploadCapturingServer(t *testing.T, captured *capturedUpload) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Errorf("parsing Content-Type: %v", err)

			return
		}

		assert.Equal(t, "multipart/form-data", mediaType)

		captured.contentLength = r.ContentLength
		captured.chunked = r.ContentLength < 0

		reader := multipart.NewReader(r.Body, params["boundary"])

		for {
			part, err := reader.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}

			if err != nil {
				t.Errorf("reading multipart part: %v", err)

				return
			}

			body, err := io.ReadAll(part)
			if err != nil {
				t.Errorf("reading part body: %v", err)

				return
			}

			captured.fieldName = part.FormName()
			captured.disposition = part.Header.Get("Content-Disposition")
			captured.content = body
		}

		respond(t, w, http.StatusCreated, `{"catalogId":"abc","stageId":"stage-1"}`)
	}))
}

// A pack name carries its directory in the multipart filename, and that must survive
// verbatim — a mangled name is a pack no manifest can find.
func TestUploadPreservesNestedDottedFilename(t *testing.T) {
	t.Parallel()

	var captured capturedUpload

	srv := uploadCapturingServer(t, &captured)
	defer srv.Close()

	payload := []byte("pack bytes")

	err := newTestClient(t, srv).UploadBytesToStage(context.Background(), "abc", "stage-1",
		".dr-sync/packs/p_9f2a1c8d3e4f5061.tar.xz", payload)
	require.NoError(t, err)

	assert.Equal(t, "file", captured.fieldName)
	assert.Contains(t, captured.disposition, `filename=".dr-sync/packs/p_9f2a1c8d3e4f5061.tar.xz"`)
	assert.Equal(t, payload, captured.content)
}

// An unknown length means chunked transfer encoding, which a gateway may refuse on a large
// upload. The body streams, so the length has to be computed rather than measured.
func TestUploadSetsExactContentLengthWithoutBuffering(t *testing.T) {
	t.Parallel()

	var captured capturedUpload

	srv := uploadCapturingServer(t, &captured)
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "pack.tar.xz")
	payload := bytes.Repeat([]byte("x"), 1<<20)
	require.NoError(t, os.WriteFile(path, payload, 0o600))

	err := newTestClient(t, srv).UploadFileToStage(context.Background(), "abc", "stage-1",
		".dr-sync/packs/p_big.tar.xz", path)
	require.NoError(t, err)

	assert.False(t, captured.chunked, "the request must declare its length")
	assert.Greater(t, captured.contentLength, int64(len(payload)),
		"length covers the envelope as well as the payload")
	assert.Equal(t, payload, captured.content)
}

// Uploading the same name twice into one stage is deduplicated by the API, which is what
// makes replaying an upload after an ambiguous failure safe.
func TestUploadRetriesReplayTheBody(t *testing.T) {
	t.Parallel()

	var (
		attempts atomic.Int32
		bodies   []string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Errorf("parsing Content-Type: %v", err)

			return
		}

		part, err := multipart.NewReader(r.Body, params["boundary"]).NextPart()
		if err != nil {
			t.Errorf("reading multipart part: %v", err)

			return
		}

		body, err := io.ReadAll(part)
		if err != nil {
			t.Errorf("reading part body: %v", err)

			return
		}

		bodies = append(bodies, string(body))

		if attempts.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusServiceUnavailable)

			return
		}

		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	err := newTestClient(t, srv).UploadBytesToStage(context.Background(), "abc", "stage-1",
		".dr-sync/packs/p_x.tar.xz", []byte("replay me"))
	require.NoError(t, err)

	assert.Equal(t, int32(2), attempts.Load())
	assert.Equal(t, []string{"replay me", "replay me"}, bodies,
		"the retry must resend the full body, not a drained reader")
}

func TestCreateStageRejectsMissingStageID(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		respondOK(t, w, `{"catalogId":"abc"}`)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv).CreateStage(context.Background(), "abc")
	require.ErrorContains(t, err, "no stageId")
}

// The commit is synchronous: 200 with the new version and no Location header.
func TestCommitStageIsSynchronous(t *testing.T) {
	t.Parallel()

	var body map[string]string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decodeRequest(t, r, &body)

		respondOK(t, w, `{"catalogId":"abc","catalogVersionId":"v2","numFiles":204}`)
	}))
	defer srv.Close()

	result, err := newTestClient(t, srv).CommitStage(context.Background(), "abc", "stage-1", "")
	require.NoError(t, err)

	assert.Equal(t, "stage-1", body["stageId"])
	assert.Equal(t, "error", body["overwrite"], "the default must be the strict strategy")
	assert.Equal(t, "v2", result.VersionID)
	assert.Equal(t, 204, result.NumFiles)
}

// overwrite=error rejects the whole commit and applies nothing, which is why an upload must
// skip packs that are already present rather than re-send them.
func TestCommitStageSurfacesDuplicateNameConflict(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		respond(t, w, http.StatusConflict,
			`{"message":"Duplicate file name(s): .dr-sync/packs/p_x.tar.xz"}`)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv).CommitStage(context.Background(), "abc", "stage-1", OverwriteError)
	require.Error(t, err)

	assert.True(t, IsConflict(err))
	require.ErrorContains(t, err, ".dr-sync/packs/p_x.tar.xz")
}

// If a commit ever does answer asynchronously, treating it as finished would break the
// packs-before-manifest ordering, so the Location must be followed to a terminal state.
func TestCommitStageFollowsAsyncLocationWhenPresent(t *testing.T) {
	t.Parallel()

	var polls atomic.Int32

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/status/1/", func(w http.ResponseWriter, _ *http.Request) {
		if polls.Add(1) < 2 {
			respondOK(t, w, `{"status":"RUNNING"}`)

			return
		}

		respondOK(t, w, `{"status":"COMPLETED"}`)
	})

	var srv *httptest.Server

	mux.HandleFunc("/api/v2/files/abc/fromStage/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", srv.URL+"/api/v2/status/1/")
		respond(t, w, http.StatusAccepted, `{"catalogId":"abc","catalogVersionId":"v2"}`)
	})

	srv = httptest.NewServer(mux)
	defer srv.Close()

	result, err := newTestClient(t, srv).CommitStage(context.Background(), "abc", "stage-1", OverwriteError)
	require.NoError(t, err)

	assert.Equal(t, "v2", result.VersionID)
	assert.GreaterOrEqual(t, polls.Load(), int32(2), "polling must continue past a non-terminal status")
}

func TestMultipartLengthMatchesWhatIsSent(t *testing.T) {
	t.Parallel()

	payload := []byte("0123456789")

	body, length, contentType, err := buildMultipart(
		[]FormField{{Name: "overwrite", Value: "error"}},
		FormFile{
			FieldName: "file",
			Filename:  `weird"name\path.tar.xz`,
			Size:      int64(len(payload)),
			Open: func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(payload)), nil
			},
		},
	)
	require.NoError(t, err)
	assert.Contains(t, contentType, "boundary=dr-sync-")

	reader, err := body()
	require.NoError(t, err)

	defer reader.Close()

	rendered, err := io.ReadAll(reader)
	require.NoError(t, err)

	assert.Equal(t, length, int64(len(rendered)),
		"the declared Content-Length must equal the bytes actually produced")

	// The quote and backslash must be escaped on the wire per RFC 7578, so the header stays
	// parseable rather than terminating the filename early.
	assert.Contains(t, string(rendered), `filename="weird\"name\\path.tar.xz"`)

	_, params, err := mime.ParseMediaType(contentType)
	require.NoError(t, err)

	parts := multipart.NewReader(bytes.NewReader(rendered), params["boundary"])

	first, err := parts.NextPart()
	require.NoError(t, err)
	assert.Equal(t, "overwrite", first.FormName())

	value, err := io.ReadAll(first)
	require.NoError(t, err)
	assert.Equal(t, "error", string(value))

	second, err := parts.NextPart()
	require.NoError(t, err)
	assert.Equal(t, "file", second.FormName())
}

func TestEscapeFormValueBlocksHeaderInjection(t *testing.T) {
	t.Parallel()

	// Newlines are dropped outright: a crafted name must not be able to close the header
	// and inject one of its own.
	escaped := escapeFormValue("pack.tar.xz\r\nContent-Type: text/html")
	assert.NotContains(t, escaped, "\r")
	assert.NotContains(t, escaped, "\n")

	assert.Equal(t, `a\\b`, escapeFormValue(`a\b`))
	assert.Equal(t, `a\"b`, escapeFormValue(`a"b`))
	assert.Equal(t, ".dr-sync/packs/p_x.tar.xz", escapeFormValue(".dr-sync/packs/p_x.tar.xz"),
		"slashes are legal and must survive untouched")
}

func TestUploadBytesLengthIsStable(t *testing.T) {
	t.Parallel()

	var captured capturedUpload

	srv := uploadCapturingServer(t, &captured)
	defer srv.Close()

	require.NoError(t, newTestClient(t, srv).UploadBytesToStage(
		context.Background(), "abc", "stage-1", "m.json.xz", []byte("{}")))

	assert.Equal(t, "{}", string(captured.content))
	assert.Positive(t, captured.contentLength)
}
