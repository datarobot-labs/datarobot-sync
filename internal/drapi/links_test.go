// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

package drapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSignedLinksClampsDurationAndStampsIssuedAt(t *testing.T) {
	t.Parallel()

	var body map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decodeRequest(t, r, &body)

		respondOK(t, w, `{"links":[{"fileName":"a","url":"https://s3.example/a?sig=1"}]}`)
	}))
	defer srv.Close()

	before := time.Now()

	links, err := newTestClient(t, srv).SignedLinks(context.Background(), "abc",
		[]string{"a"}, 10*time.Hour)
	require.NoError(t, err)

	assert.EqualValues(t, MaxLinkDuration, body["duration"], "duration must be clamped to the API maximum")
	require.Len(t, links, 1)
	assert.False(t, links[0].IssuedAt.Before(before.Add(-time.Second)),
		"IssuedAt must be stamped so a caller can refresh before expiry")
}

func TestSignedLinksSkipsEmptyRequest(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("no request should be made for an empty name list")
	}))
	defer srv.Close()

	links, err := newTestClient(t, srv).SignedLinks(context.Background(), "abc", nil, time.Minute)
	require.NoError(t, err)
	assert.Empty(t, links)
}

// A missing name fails the whole batch. Recognizing that specific message is how a stale
// manifest — one referencing a pack that has since been compacted away — is detected.
func TestMissingFileNameIsRecognizable(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		respond(t, w, http.StatusUnprocessableEntity,
			`{"message":"Filename .dr-sync/packs/p_x.tar.xz does not exist."}`)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv).SignedLinks(context.Background(), "abc",
		[]string{".dr-sync/packs/p_x.tar.xz"}, time.Minute)
	require.Error(t, err)

	assert.True(t, IsMissingFileName(err))
	assert.False(t, IsMissingFileName(assert.AnError))
}

func TestLinkExpiringTracksLifetime(t *testing.T) {
	t.Parallel()

	assert.True(t, Link{}.Expiring(), "a link with no issue time cannot be trusted")
	assert.False(t, Link{IssuedAt: time.Now()}.Expiring())
	assert.True(t, Link{IssuedAt: time.Now().Add(-LinkLifetime + time.Minute)}.Expiring(),
		"a link inside the refresh margin must be renewed before a transfer starts")
}

// Sending an Authorization header to a presigned URL returns 400 from S3, so the blob client
// must never carry credentials.
func TestOpenBlobSendsNoCredentials(t *testing.T) {
	t.Parallel()

	var sawAuth bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization") != ""

		respondRaw(t, w, http.StatusOK, []byte("blob bytes"))
	}))
	defer srv.Close()

	body, _, err := newTestClient(t, srv).OpenBlob(context.Background(), srv.URL+"/blob?sig=1", 0)
	require.NoError(t, err)

	defer body.Close()

	content, err := io.ReadAll(body)
	require.NoError(t, err)

	assert.False(t, sawAuth, "the presigned request must not carry an Authorization header")
	assert.Equal(t, "blob bytes", string(content))
}

// A same-host redirect keeps headers in Go's client, so the redirect policy has to strip
// them explicitly rather than rely on the cross-host rule.
func TestOpenBlobStripsCredentialsAcrossRedirects(t *testing.T) {
	t.Parallel()

	var authOnFinal string

	mux := http.NewServeMux()
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/final", http.StatusFound)
	})
	mux.HandleFunc("/final", func(w http.ResponseWriter, r *http.Request) {
		authOnFinal = r.Header.Get("Authorization")

		respondRaw(t, w, http.StatusOK, []byte("ok"))
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := newTestClient(t, srv)

	// Simulate a caller that has somehow attached credentials, then confirm the policy
	// removes them on the hop.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/redirect", nil)
	require.NoError(t, err)

	req.Header.Set("Authorization", "Bearer leaked")

	resp, err := client.blob.Do(req)
	require.NoError(t, err)

	defer resp.Body.Close()

	assert.Empty(t, authOnFinal, "credentials must be dropped when following a redirect")
}

func TestOpenBlobRangeRequest(t *testing.T) {
	t.Parallel()

	payload := []byte("0123456789abcdef")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "" {
			respondRaw(t, w, http.StatusOK, payload)

			return
		}

		w.Header().Set("Content-Range", "bytes 8-15/16")
		respondRaw(t, w, http.StatusPartialContent, payload[8:])
	}))
	defer srv.Close()

	body, _, err := newTestClient(t, srv).OpenBlob(context.Background(), srv.URL+"/b?sig=1", 8)
	require.NoError(t, err)

	defer body.Close()

	rest, err := io.ReadAll(body)
	require.NoError(t, err)
	assert.Equal(t, "89abcdef", string(rest))
}

// A store that ignores Range would silently hand back the whole object, which breaks a
// resume by duplicating the prefix. Better to say so than to corrupt the output.
func TestOpenBlobRejectsIgnoredRange(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		respondRaw(t, w, http.StatusOK, []byte("whole object"))
	}))
	defer srv.Close()

	_, _, err := newTestClient(t, srv).OpenBlob(context.Background(), srv.URL+"/b?sig=1", 4)
	require.ErrorContains(t, err, "does not support resume")
}

func TestOpenBlobClassifiesExpiry(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		respondRaw(t, w, http.StatusForbidden,
			[]byte(`<?xml version="1.0"?><Error><Code>AccessDenied</Code></Error>`))
	}))
	defer srv.Close()

	_, _, err := newTestClient(t, srv).OpenBlob(context.Background(), srv.URL+"/b?sig=1", 0)
	require.Error(t, err)

	assert.True(t, IsBlobExpired(err), "a 403 on a presigned URL is a refreshable expiry")
	require.ErrorContains(t, err, "AccessDenied")
}
