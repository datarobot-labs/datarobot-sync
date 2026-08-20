// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

package syncer_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/datarobot/golang-datarobot-filesync/internal/manifest"
	"github.com/datarobot/golang-datarobot-filesync/internal/progress"
	"github.com/datarobot/golang-datarobot-filesync/internal/syncer"
)

// Retention is the real garbage dial: an old manifest pins the packs it references, so nothing
// can be reclaimed until it is pruned.
func TestRetentionPinsPacks(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := writeTree(t, map[string]string{"a.txt": strings.Repeat("a", 5000)})

	// Several pushes, each superseding the last file's content.
	for i := range 4 {
		writeInto(t, source, map[string]string{
			"a.txt": strings.Repeat("a", 5000) + string(rune('a'+i)),
		})

		h.upload(t, source, func(o *syncer.UploadOptions) {
			o.NoGC = true
			o.Delete = true
		})
	}

	packsBefore := h.api.countObjects(h.containerID, ".dr-sync/packs/")
	require.GreaterOrEqual(t, packsBefore, 4)

	// Every pack is young, so the grace window protects them all even though most are dead.
	state, err := h.store.ReadState(context.Background())
	require.NoError(t, err)

	deleted, err := syncer.Collect(context.Background(), h.store, state, syncer.CollectOptions{
		KeepGenerations: 1,
		Grace:           time.Hour,
		Now:             time.Now(),
	})
	require.NoError(t, err)
	assert.Zero(t, deleted,
		"the grace window protects both packs a concurrent push might still commit and manifests "+
			"a concurrent reader might have just selected")

	// Backdate everything and collect for real.
	for _, name := range h.api.objectNames(h.containerID) {
		h.api.setObjectAge(h.containerID, name, 24*time.Hour)
	}

	state, err = h.store.ReadState(context.Background())
	require.NoError(t, err)

	deleted, err = syncer.Collect(context.Background(), h.store, state, syncer.CollectOptions{
		KeepGenerations: 1,
		Grace:           time.Hour,
		Now:             time.Now(),
	})
	require.NoError(t, err)
	assert.Positive(t, deleted)

	// What survived must still be a complete, readable tree.
	destination := t.TempDir()
	h.download(t, destination)
	assert.Equal(t, readTree(t, source), readTree(t, destination))
}

// A manifest may only be deleted once a newer one exists, and a pack only after the last
// manifest referencing it is gone. This checks the ordering the plan produces.
func TestCollectPlanDeletesManifestsBeforePacks(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := writeTree(t, map[string]string{"a.txt": "one"})

	h.upload(t, source, func(o *syncer.UploadOptions) { o.NoGC = true })

	writeInto(t, source, map[string]string{"a.txt": "two"})
	h.upload(t, source, func(o *syncer.UploadOptions) { o.NoGC = true; o.Delete = true })

	for _, name := range h.api.objectNames(h.containerID) {
		h.api.setObjectAge(h.containerID, name, 24*time.Hour)
	}

	state, err := h.store.ReadState(context.Background())
	require.NoError(t, err)

	plan, err := syncer.PlanCollect(context.Background(), h.store, state, syncer.CollectOptions{
		KeepGenerations: 1,
		Grace:           time.Hour,
		Now:             time.Now(),
	})
	require.NoError(t, err)

	require.NotEmpty(t, plan.Manifests)
	require.NotEmpty(t, plan.Packs)

	names := plan.Names()

	lastManifest := -1
	firstPack := len(names)

	for i, name := range names {
		if strings.Contains(name, "/manifests/") {
			lastManifest = i
		}

		if strings.Contains(name, "/packs/") && i < firstPack {
			firstPack = i
		}
	}

	assert.Less(t, lastManifest, firstPack,
		"every manifest must be deleted before any pack, so an interrupted pass never leaves a "+
			"manifest pointing at objects that are gone")
}

// Garbage collection is suppressed entirely when two manifests share a generation: the losing
// one may be the only thing keeping some packs alive.
func TestForkSuppressesCollection(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := writeTree(t, map[string]string{"a.txt": "one"})
	h.upload(t, source, func(o *syncer.UploadOptions) { o.NoGC = true })

	state, err := h.store.ReadState(context.Background())
	require.NoError(t, err)

	head, _, err := h.store.Head(context.Background(), state)
	require.NoError(t, err)

	// Fabricate a sibling manifest at the same generation, which is what two concurrent
	// writers produce.
	sibling := *head

	clientID, err := manifest.NewClientID()
	require.NoError(t, err)

	sibling.Generation = head.Generation
	require.NoError(t, sibling.Stamp(clientID, time.Now()))

	encoded, err := manifest.Encode(&sibling)
	require.NoError(t, err)

	require.NoError(t, h.store.Commit(context.Background(), nil, []syncer.Object{{
		Name:    sibling.ObjectName(),
		Content: encoded,
		Size:    int64(len(encoded)),
	}}, syncer.CommitOptions{}))

	state, err = h.store.ReadState(context.Background())
	require.NoError(t, err)

	fork, forked := manifest.DetectFork(state.Manifests)
	require.True(t, forked)
	assert.Equal(t, head.Generation, fork.Generation)

	// An upload notices and declines to collect rather than risking someone else's packs.
	result, err := syncer.Upload(context.Background(), h.store, progress.Discard(), syncer.UploadOptions{
		Root:  source,
		Level: manifest.DefaultLevel,
	})
	require.NoError(t, err)
	assert.Zero(t, result.Deleted)
}

// A stale lease is cleaned up rather than lingering forever.
func TestStaleLeasesAreCollected(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	state, err := h.store.ReadState(context.Background())
	require.NoError(t, err)

	_, err = syncer.AcquireLease(context.Background(), h.store, state, "abandoned", time.Nanosecond, false)
	require.NoError(t, err)

	time.Sleep(2 * time.Millisecond)

	state, err = h.store.ReadState(context.Background())
	require.NoError(t, err)
	require.Len(t, state.Leases, 1)

	plan, err := syncer.PlanCollect(context.Background(), h.store, state, syncer.CollectOptions{
		KeepGenerations: 5,
		Now:             time.Now(),
	})
	require.NoError(t, err)
	assert.Len(t, plan.Leases, 1)
}

// Compaction runs from the local tree, so on a complete checkout it uploads fresh packs and
// deletes the sparse ones without downloading anything.
func TestCompactRewritesSparsePacks(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := writeTree(t, map[string]string{
		"keep.txt":  strings.Repeat("keep this content ", 300),
		"churn.txt": strings.Repeat("original ", 300),
	})

	h.upload(t, source, func(o *syncer.UploadOptions) { o.NoGC = true })

	// Rewrite the churning file several times so its pack accumulates dead members.
	for i := range 3 {
		writeInto(t, source, map[string]string{
			"churn.txt": strings.Repeat("revision ", 300) + string(rune('a'+i)),
		})

		h.upload(t, source, func(o *syncer.UploadOptions) { o.NoGC = true; o.Delete = true })
	}

	for _, name := range h.api.objectNames(h.containerID) {
		h.api.setObjectAge(h.containerID, name, 24*time.Hour)
	}

	packsBefore := h.api.countObjects(h.containerID, ".dr-sync/packs/")

	result, err := syncer.Compact(context.Background(), h.store, progress.Discard(), syncer.CompactOptions{
		Root:            source,
		Threshold:       0.9,
		KeepGenerations: 1,
		GCGrace:         time.Hour,
		Level:           manifest.DefaultLevel,
	})
	require.NoError(t, err)
	require.NotNil(t, result.Manifest)

	assert.NotEmpty(t, result.Rewritten)
	assert.Less(t, h.api.countObjects(h.containerID, ".dr-sync/packs/"), packsBefore,
		"compaction should leave fewer packs than it found")

	destination := t.TempDir()
	h.download(t, destination)
	assert.Equal(t, readTree(t, source), readTree(t, destination))
}

// Compaction refuses rather than silently turning into a large download.
func TestCompactRefusesAnIncompleteLocalTree(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := writeTree(t, map[string]string{
		"a.txt": strings.Repeat("a", 2000),
		"b.txt": strings.Repeat("b", 2000),
	})

	h.upload(t, source, func(o *syncer.UploadOptions) { o.NoGC = true })

	writeInto(t, source, map[string]string{"a.txt": strings.Repeat("A", 2000)})
	h.upload(t, source, func(o *syncer.UploadOptions) { o.NoGC = true })

	// Remove a file the manifest still records, so its content is live but not local.
	require.NoError(t, os.Remove(filepath.Join(source, "b.txt")))

	_, err := syncer.Compact(context.Background(), h.store, progress.Discard(), syncer.CompactOptions{
		Root:      source,
		Threshold: 0.99,
		Level:     manifest.DefaultLevel,
	})
	require.ErrorContains(t, err, "not in the local tree")
	assert.ErrorContains(t, err, "b.txt")
}

// A pack the manifest references but the container no longer holds must be reported clearly
// rather than surfacing as a mid-transfer surprise.
func TestDownloadDetectsACompactedRemote(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := writeTree(t, map[string]string{"a.txt": strings.Repeat("a", 4000)})
	h.upload(t, source, func(o *syncer.UploadOptions) { o.NoGC = true })

	var packName string

	for _, name := range h.api.objectNames(h.containerID) {
		if strings.HasPrefix(name, ".dr-sync/packs/") {
			packName = name
		}
	}

	require.NotEmpty(t, packName)

	_, err := h.store.Delete(context.Background(), []string{packName})
	require.NoError(t, err)

	_, err = syncer.Download(context.Background(), h.store, progress.Discard(), syncer.DownloadOptions{
		Root: t.TempDir(),
	})
	require.Error(t, err)
	assert.True(t, syncer.IsStaleManifest(err))
	assert.ErrorContains(t, err, "re-run")
}

// An older generation can still be pulled while it is retained, which is what makes a bad push
// recoverable.
func TestDownloadAnOlderGeneration(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := writeTree(t, map[string]string{"a.txt": "first"})

	first := h.upload(t, source, func(o *syncer.UploadOptions) { o.NoGC = true })
	require.NotNil(t, first.Manifest)

	writeInto(t, source, map[string]string{"a.txt": "second"})
	h.upload(t, source, func(o *syncer.UploadOptions) { o.NoGC = true })

	destination := t.TempDir()

	h.download(t, destination, func(o *syncer.DownloadOptions) {
		o.Generation = first.Manifest.Generation
	})

	assert.Equal(t, "first", readTree(t, destination)["a.txt"])
}
