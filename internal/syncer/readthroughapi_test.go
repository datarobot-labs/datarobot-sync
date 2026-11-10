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

package syncer_test

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/datarobot-labs/datarobot-sync/internal/manifest"
	"github.com/datarobot-labs/datarobot-sync/internal/progress"
	"github.com/datarobot-labs/datarobot-sync/internal/syncer"
)

// notices collects what the store announces, from whichever goroutine announces it.
type notices struct {
	mu       sync.Mutex
	messages []string
}

func (n *notices) add(message string) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.messages = append(n.messages, message)
}

func (n *notices) all() []string {
	n.mu.Lock()
	defer n.mu.Unlock()

	return append([]string(nil), n.messages...)
}

// seeded returns a harness whose container already holds one push of a small tree.
func seeded(t *testing.T) (*harness, string) {
	t.Helper()

	h := newHarness(t)
	source := writeTree(t, map[string]string{
		"a.txt":     "alpha",
		"dir/b.txt": strings.Repeat("beta", 500),
	})
	h.upload(t, source)

	return h, source
}

func TestDownloadFallsBackToTheAPIWhenTheObjectStoreIsUnreachable(t *testing.T) {
	t.Parallel()

	h, source := seeded(t)
	h.api.blockObjectStore()

	heard := &notices{}
	h.store.NotifyFallback(heard.add)

	destination := t.TempDir()
	h.download(t, destination)

	assert.Equal(t, readTree(t, source), readTree(t, destination))
	assert.Positive(t, h.api.fileReads.Load(), "the bytes must have come through the API")

	messages := heard.all()
	require.Len(t, messages, 1, "the switch is announced once, not once per object")
	assert.Contains(t, messages[0], "not reachable")
}

// The manifest comes through a reachable store, so the switch to API reads has to happen on the
// pack slices themselves.
func TestDownloadFallsBackForPackSlicesWhenOnlyPacksAreUnreachable(t *testing.T) {
	t.Parallel()

	h, source := seeded(t)
	h.api.blockObjectStoreFor(".dr-sync/packs/")

	heard := &notices{}
	h.store.NotifyFallback(heard.add)

	destination := t.TempDir()
	h.download(t, destination)

	assert.Equal(t, readTree(t, source), readTree(t, destination))
	assert.Positive(t, h.api.blobFetches.Load(), "the manifest must have come from the object store")
	assert.Positive(t, h.api.fileReads.Load(), "the pack slices must have come through the API")
	assert.Len(t, heard.all(), 1)
}

func TestDownloadDoesNotFallBackForPackSlicesWhenTheStoreAnswersAnError(t *testing.T) {
	t.Parallel()

	h, _ := seeded(t)
	h.api.failObjectStoreFor(http.StatusForbidden, ".dr-sync/packs/")

	result, err := syncer.Download(context.Background(), h.store, progress.Discard(),
		syncer.DownloadOptions{Root: t.TempDir()})

	require.NoError(t, err)
	assert.True(t, result.Partial(), "the pack that answered 403 must be reported as not pulled")
	assert.Zero(t, h.api.fileReads.Load(), "a 403 on a pack slice must not be retried through the API")
}

func TestDownloadWithAPIReadsSkipsTheObjectStore(t *testing.T) {
	t.Parallel()

	h, source := seeded(t)

	heard := &notices{}
	h.store.NotifyFallback(heard.add)
	h.store.UseAPIReads()

	fetchesBefore := h.api.blobFetches.Load()
	destination := t.TempDir()
	h.download(t, destination)

	assert.Equal(t, readTree(t, source), readTree(t, destination))
	assert.Positive(t, h.api.fileReads.Load())
	assert.Equal(t, fetchesBefore, h.api.blobFetches.Load(), "an explicit choice must not try the store first")
	assert.Empty(t, heard.all(), "nothing fell back, so nothing is announced")
}

func TestDownloadWithAPIReadsRequestsNoLinks(t *testing.T) {
	t.Parallel()

	h, source := seeded(t)
	h.store.UseAPIReads()

	linksBefore := h.api.linkRequests.Load()
	destination := t.TempDir()
	h.download(t, destination)

	assert.Equal(t, readTree(t, source), readTree(t, destination))
	assert.Equal(t, linksBefore, h.api.linkRequests.Load(), "API reads use no presigned links")
}

// The first link answers 403 as an expired one does, and the link issued in its place points at
// a store that does not answer.
func TestDownloadFallsBackWhenARefreshedLinkIsUnreachable(t *testing.T) {
	t.Parallel()

	h, source := seeded(t)
	h.api.failObjectStoreFor(http.StatusForbidden, ".dr-sync/packs/")

	var packLinkIssues atomic.Int64

	h.api.onLinks = func(names []string) {
		for _, name := range names {
			if strings.HasPrefix(name, ".dr-sync/packs/") {
				if packLinkIssues.Add(1) == 2 {
					h.api.blockObjectStoreFor(".dr-sync/packs/")
				}

				return
			}
		}
	}

	destination := t.TempDir()
	h.download(t, destination)

	assert.Equal(t, readTree(t, source), readTree(t, destination))
	assert.Positive(t, h.api.fileReads.Load(), "the pack slices must have come through the API")
}

func TestDownloadDoesNotFallBackWhenTheObjectStoreAnswersAnError(t *testing.T) {
	t.Parallel()

	h, _ := seeded(t)
	h.api.failObjectStore(http.StatusForbidden)

	heard := &notices{}
	h.store.NotifyFallback(heard.add)

	_, err := syncer.Download(context.Background(), h.store, progress.Discard(),
		syncer.DownloadOptions{Root: t.TempDir()})

	require.Error(t, err)
	assert.Zero(t, h.api.fileReads.Load(), "a 403 from the store must not be retried through the API")
	assert.Empty(t, heard.all())
}

func TestReadObjectViaAPIMatchesAPresignedRead(t *testing.T) {
	t.Parallel()

	h, _ := seeded(t)

	state, err := h.store.ReadState(context.Background())
	require.NoError(t, err)

	head, ok := manifest.SelectHead(state.Manifests)
	require.True(t, ok)

	presigned, err := h.store.ReadObject(context.Background(), head.ObjectName)
	require.NoError(t, err)

	h.store.UseAPIReads()

	viaAPI, err := h.store.ReadObject(context.Background(), head.ObjectName)
	require.NoError(t, err)

	assert.Equal(t, presigned, viaAPI)
}

func TestReadObjectViaAPIReportsAVanishedObjectAsStale(t *testing.T) {
	t.Parallel()

	h, _ := seeded(t)
	h.store.UseAPIReads()

	_, err := h.store.ReadObject(context.Background(), ".dr-sync/packs/p_gone.tar.xz")

	require.Error(t, err)
	assert.True(t, syncer.IsStaleManifest(err))
}
