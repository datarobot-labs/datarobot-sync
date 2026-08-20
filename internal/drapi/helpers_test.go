// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

package drapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// respond writes a JSON test response. A failure is recorded rather than fatal: a
// require inside a handler would abort the server's goroutine instead of the test.
func respond(t *testing.T, w http.ResponseWriter, status int, body string) {
	t.Helper()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if _, err := io.WriteString(w, body); err != nil {
		t.Errorf("writing test response: %v", err)
	}
}

// respondOK writes a 200 JSON response.
func respondOK(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()

	respond(t, w, http.StatusOK, body)
}

// respondRaw writes a non-JSON body, for the presigned-blob cases.
func respondRaw(t *testing.T, w http.ResponseWriter, status int, body []byte) {
	t.Helper()

	w.WriteHeader(status)

	if _, err := w.Write(body); err != nil {
		t.Errorf("writing test response: %v", err)
	}
}

// decodeRequest reads and decodes a request body inside a handler.
func decodeRequest(t *testing.T, r *http.Request, out any) {
	t.Helper()

	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Errorf("reading request body: %v", err)

		return
	}

	if len(bytes.TrimSpace(raw)) == 0 {
		return
	}

	if err := json.Unmarshal(raw, out); err != nil {
		t.Errorf("decoding request body %q: %v", raw, err)
	}
}
