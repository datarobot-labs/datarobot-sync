// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

package drapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The default page is 100 and the cap is 1000, so a listing that ignores `next` silently
// truncates. This asserts every page is walked and the absolute `next` URL is used verbatim.
func TestListFilesFollowsPagination(t *testing.T) {
	t.Parallel()

	var srv *httptest.Server

	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("offset") == "2" {
			respondOK(t, w, `{"count":1,"totalCount":3,"next":null,"data":[
				{"fileName":"c.txt","fileType":"txt","fileSize":3,"fileChecksum":"cc"}]}`)

			return
		}

		// Deliberately spells recursive as "True", the way the live API writes its own
		// `next` links, to prove the URL is not being rebuilt.
		next := srv.URL + "/api/v2/files/abc/allFiles/?offset=2&limit=2&recursive=True"
		respondOK(t, w, fmt.Sprintf(`{"count":2,"totalCount":3,"next":%q,"data":[
			{"fileName":"a.txt","fileType":"txt","fileSize":1,"fileChecksum":"aa"},
			{"fileName":"b/nested.txt","fileType":"txt","fileSize":2,"fileChecksum":"bb"}]}`, next))
	}))
	defer srv.Close()

	entries, err := newTestClient(t, srv).ListFiles(context.Background(), "abc", ListOptions{PageSize: 2})
	require.NoError(t, err)

	require.Len(t, entries, 3)
	assert.Equal(t, []string{"a.txt", "b/nested.txt", "c.txt"},
		[]string{entries[0].Name, entries[1].Name, entries[2].Name})
	assert.Equal(t, "cc", entries[2].Checksum)
}

// The API rejects a prefix that does not end in "/", so the client supplies it.
func TestListFilesNormalizesPrefixAndClampsPageSize(t *testing.T) {
	t.Parallel()

	var query map[string]string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = map[string]string{}
		for key := range r.URL.Query() {
			query[key] = r.URL.Query().Get(key)
		}

		respondOK(t, w, `{"count":0,"data":[],"next":null}`)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv).ListFiles(context.Background(), "abc", ListOptions{
		Prefix:   ".dr-sync/packs",
		PageSize: 99999,
	})
	require.NoError(t, err)

	assert.Equal(t, ".dr-sync/packs/", query["prefix"])
	assert.Equal(t, "1000", query["limit"], "page size must be clamped to the API cap")
	assert.Equal(t, "true", query["recursive"], "the query string wants a lowercase boolean")
}

func TestEachFileStopsOnVisitError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		respondOK(t, w, `{"count":2,"next":null,"data":[{"fileName":"a"},{"fileName":"b"}]}`)
	}))
	defer srv.Close()

	sentinel := errors.New("stop here")
	seen := 0

	err := newTestClient(t, srv).EachFile(context.Background(), "abc", ListOptions{}, func(FileEntry) error {
		seen++

		return sentinel
	})

	require.ErrorIs(t, err, sentinel)
	assert.Equal(t, 1, seen)
}

// DELETE with a JSON request body is unusual enough that a client can easily drop it.
func TestDeletePathsSendsJSONBodyOnDelete(t *testing.T) {
	t.Parallel()

	var (
		method string
		body   map[string][]string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		decodeRequest(t, r, &body)

		respondOK(t, w, `{"catalogId":"abc","numFiles":1,
			"results":[{"path":".dr-sync/packs/p_x.tar.xz","numFilesDeleted":1}]}`)
	}))
	defer srv.Close()

	result, err := newTestClient(t, srv).DeletePaths(context.Background(), "abc",
		[]string{".dr-sync/packs/p_x.tar.xz"})
	require.NoError(t, err)

	assert.Equal(t, http.MethodDelete, method)
	assert.Equal(t, []string{".dr-sync/packs/p_x.tar.xz"}, body["paths"])
	require.Len(t, result.Results, 1)
	assert.Equal(t, 1, result.Results[0].NumFilesDeleted)
}

func TestDeletePathsSkipsEmptyRequest(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("no request should be made for an empty path list")
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv).DeletePaths(context.Background(), "abc", nil)
	assert.NoError(t, err)
}

// Omitting `tags` leaves them alone; sending an empty list clears them. Both must be
// expressible, which is why the field is a pointer.
func TestUpdateContainerDistinguishesAbsentAndEmptyTags(t *testing.T) {
	t.Parallel()

	var body map[string]json.RawMessage

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decodeRequest(t, r, &body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := newTestClient(t, srv)

	require.NoError(t, client.UpdateContainer(context.Background(), "abc",
		ContainerUpdate{Name: "only a rename"}))
	assert.NotContains(t, body, "tags", "a rename must not touch tags")

	empty := []string{}
	require.NoError(t, client.UpdateContainer(context.Background(), "abc",
		ContainerUpdate{Tags: &empty}))
	assert.JSONEq(t, `[]`, string(body["tags"]), "an empty list must be sent, to clear them")
}
