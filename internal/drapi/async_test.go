// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

package drapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPollAsyncCompletesOnStatusBody(t *testing.T) {
	t.Parallel()

	var polls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if polls.Add(1) < 3 {
			respondOK(t, w, `{"status":"RUNNING"}`)

			return
		}

		respondOK(t, w, `{"status":"COMPLETED"}`)
	}))
	defer srv.Close()

	require.NoError(t, newTestClient(t, srv).PollAsync(context.Background(), srv.URL+"/status/"))
	assert.EqualValues(t, 3, polls.Load())
}

// A 303 is the terminal signal, so the poller must not follow it as a redirect.
func TestPollAsyncTreatsSeeOtherAsDone(t *testing.T) {
	t.Parallel()

	var polls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		polls.Add(1)
		w.Header().Set("Location", "/api/v2/catalogItems/abc/")
		w.WriteHeader(http.StatusSeeOther)
	}))
	defer srv.Close()

	require.NoError(t, newTestClient(t, srv).PollAsync(context.Background(), srv.URL+"/status/"))
	assert.EqualValues(t, 1, polls.Load(), "303 is terminal, not a hop to follow with more polls")
}

func TestPollAsyncFollowsTemporaryRedirect(t *testing.T) {
	t.Parallel()

	var srv *httptest.Server

	mux := http.NewServeMux()
	mux.HandleFunc("/first/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", srv.URL+"/second/")
		w.WriteHeader(http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/second/", func(w http.ResponseWriter, _ *http.Request) {
		respondOK(t, w, `{"status":"COMPLETED"}`)
	})

	srv = httptest.NewServer(mux)
	defer srv.Close()

	require.NoError(t, newTestClient(t, srv).PollAsync(context.Background(), srv.URL+"/first/"))
}

func TestPollAsyncSurfacesFailureMessage(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		respondOK(t, w, `{"status":"ERROR","message":"Duplicate file name(s): a.txt"}`)
	}))
	defer srv.Close()

	err := newTestClient(t, srv).PollAsync(context.Background(), srv.URL+"/status/")
	require.ErrorContains(t, err, "Duplicate file name(s): a.txt")
}

// Treating an unknown status as "keep waiting" would hang until the deadline and then report
// a timeout instead of the real problem.
func TestPollAsyncRefusesUnknownStatus(t *testing.T) {
	t.Parallel()

	var polls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		polls.Add(1)
		respondOK(t, w, `{"status":"WAT"}`)
	}))
	defer srv.Close()

	err := newTestClient(t, srv).PollAsync(context.Background(), srv.URL+"/status/")
	require.ErrorContains(t, err, `unrecognized async status "WAT"`)
	assert.EqualValues(t, 1, polls.Load(), "an unknown status must stop the loop immediately")
}

func TestInterpretAsyncStatus(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		body    string
		done    bool
		wantErr string
	}{
		{name: "completed", body: `{"status":"COMPLETED"}`, done: true},
		{name: "mixed case", body: `{"status":"Completed"}`, done: true},
		{name: "running", body: `{"status":"RUNNING"}`},
		{name: "initialized", body: `{"status":"INITIALIZED"}`},
		{name: "uploading", body: `{"status":"uploading"}`},
		{name: "error prefix", body: `{"status":"ERRORED","message":"bad"}`, wantErr: "bad"},
		{name: "abort prefix", body: `{"status":"ABORTED"}`, wantErr: "aborted"},
		{name: "unknown", body: `{"status":"weird"}`, wantErr: "unrecognized"},
		{name: "not json", body: `<html>`, wantErr: "decoding async status"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			done, next, err := interpretAsyncStatus([]byte(tc.body))

			assert.Empty(t, next)

			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.done, done)
		})
	}
}
