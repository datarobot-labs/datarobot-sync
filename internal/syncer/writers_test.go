// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

package syncer_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/datarobot/golang-datarobot-filesync/internal/manifest"
	"github.com/datarobot/golang-datarobot-filesync/internal/progress"
	"github.com/datarobot/golang-datarobot-filesync/internal/syncer"
)

// A writer registration must round-trip through the same object-naming scheme a lease uses,
// including a client id that itself contains dashes.
func TestWriterRegistrationRoundTrips(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	registration, err := syncer.RegisterWriter(context.Background(), h.store,
		"host-a-workstation", "watch", "/srv/data", time.Minute)
	require.NoError(t, err)

	state, err := h.store.ReadState(context.Background())
	require.NoError(t, err)
	require.Len(t, state.Writers, 1)

	writer := state.Writers[0]
	assert.Equal(t, "host-a-workstation", writer.ClientID)
	assert.Equal(t, "watch", writer.Mode)
	assert.Equal(t, "/srv/data", writer.Root)
	assert.Equal(t, 1, writer.Sequence)

	require.NoError(t, registration.Deregister(context.Background()))

	state, err = h.store.ReadState(context.Background())
	require.NoError(t, err)
	assert.Empty(t, state.Writers)
}

// Refresh must write the next sequence and remove the previous one, never leaving two live
// entries for the same process.
func TestWriterRefreshReplacesThePreviousEntry(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	registration, err := syncer.RegisterWriter(context.Background(), h.store, "abc", "watch", "/root", time.Minute)
	require.NoError(t, err)

	require.NoError(t, registration.Refresh(context.Background(), 42, "0000000042-deadbeef"))

	state, err := h.store.ReadState(context.Background())
	require.NoError(t, err)
	require.Len(t, state.Writers, 1, "refresh must leave exactly one live entry")

	writer := state.Writers[0]
	assert.Equal(t, 2, writer.Sequence)
	assert.EqualValues(t, 42, writer.LastGeneration)
	assert.Equal(t, "0000000042-deadbeef", writer.LastManifestID)
}

// A refresh heartbeat updates RefreshedAt but must never reset StartedAt: peers use the gap
// between the two to tell how long this process has been running versus how current it is.
func TestWriterRefreshPreservesStartedAt(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	registration, err := syncer.RegisterWriter(context.Background(), h.store, "abc", "watch", "/root", time.Minute)
	require.NoError(t, err)

	state, err := h.store.ReadState(context.Background())
	require.NoError(t, err)
	require.Len(t, state.Writers, 1)
	startedAt := state.Writers[0].StartedAt

	require.NoError(t, registration.Refresh(context.Background(), 1, "0000000001-deadbeef"))

	state, err = h.store.ReadState(context.Background())
	require.NoError(t, err)
	require.Len(t, state.Writers, 1)

	assert.Equal(t, startedAt, state.Writers[0].StartedAt,
		"a refresh must not reset when this process started")
	assert.NotEqual(t, startedAt, state.Writers[0].RefreshedAt,
		"a refresh must still update when this process last touched the registration")
}

// ForeignWriters must exclude the caller's own registration and anything expired, mirroring
// ForeignLeases exactly.
func TestForeignWritersExcludesSelfAndExpired(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	_, err := syncer.RegisterWriter(context.Background(), h.store, "self", "watch", "/root", time.Minute)
	require.NoError(t, err)

	_, err = syncer.RegisterWriter(context.Background(), h.store, "peer", "watch", "/root", time.Nanosecond)
	require.NoError(t, err)

	time.Sleep(2 * time.Millisecond)

	_, err = syncer.RegisterWriter(context.Background(), h.store, "live-peer", "upload", "/root", time.Minute)
	require.NoError(t, err)

	state, err := h.store.ReadState(context.Background())
	require.NoError(t, err)

	foreign := syncer.ForeignWriters(state.Writers, "self", time.Now())

	names := make([]string, 0, len(foreign))
	for _, writer := range foreign {
		names = append(names, writer.ClientID)
	}

	assert.Equal(t, []string{"live-peer"}, names)
}

// A stale writer registration sits outside the pack/manifest reference graph, so nothing but
// an explicit sweep will ever remove it.
func TestStaleWriterRegistrationsAreCollected(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	_, err := syncer.RegisterWriter(context.Background(), h.store, "abandoned", "watch", "/root", time.Nanosecond)
	require.NoError(t, err)

	time.Sleep(2 * time.Millisecond)

	state, err := h.store.ReadState(context.Background())
	require.NoError(t, err)
	require.Len(t, state.Writers, 1)

	plan, err := syncer.PlanCollect(context.Background(), h.store, state, syncer.CollectOptions{
		KeepGenerations: 5,
		Now:             time.Now(),
	})
	require.NoError(t, err)
	assert.Len(t, plan.Writers, 1)

	deleted, err := syncer.Collect(context.Background(), h.store, state, syncer.CollectOptions{
		KeepGenerations: 5,
		Now:             time.Now(),
	})
	require.NoError(t, err)
	assert.Equal(t, 1, deleted)

	state, err = h.store.ReadState(context.Background())
	require.NoError(t, err)
	assert.Empty(t, state.Writers)
}

// A live writer registration must never be swept, even when a collection pass runs.
func TestLiveWriterRegistrationsSurviveCollection(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	_, err := syncer.RegisterWriter(context.Background(), h.store, "resident", "watch", "/root", time.Hour)
	require.NoError(t, err)

	state, err := h.store.ReadState(context.Background())
	require.NoError(t, err)

	deleted, err := syncer.Collect(context.Background(), h.store, state, syncer.CollectOptions{
		KeepGenerations: 5,
		Now:             time.Now(),
	})
	require.NoError(t, err)
	assert.Zero(t, deleted)

	state, err = h.store.ReadState(context.Background())
	require.NoError(t, err)
	require.Len(t, state.Writers, 1)
}

// A resident writer's identity must survive across many uploads: Upload must not mint a fresh
// random lease client id on every call when the caller supplies one, or a crashed cycle would
// see its own orphaned lease as foreign and self-block for the lease TTL.
func TestStableClientIDDoesNotSelfBlockASubsequentUpload(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := writeTree(t, fixture)

	withClientID := func(id string) func(*syncer.UploadOptions) {
		return func(o *syncer.UploadOptions) { o.ClientID = id }
	}

	h.upload(t, source, withClientID("resident-daemon"))

	writeInto(t, source, map[string]string{"src/main.go": "package main\n\nfunc main() { println(2) }\n"})

	result, err := syncer.Upload(context.Background(), h.store, progress.Discard(), syncer.UploadOptions{
		Root:     source,
		Level:    manifest.DefaultLevel,
		Lock:     true,
		ClientID: "resident-daemon",
	})
	require.NoError(t, err, "the same client id must be allowed to take the lease again")
	require.NotNil(t, result.Manifest)
}
