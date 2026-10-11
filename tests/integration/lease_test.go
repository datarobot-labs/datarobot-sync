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

//go:build integration

package integration

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/datarobot-labs/datarobot-sync/internal/drapi"
	"github.com/datarobot-labs/datarobot-sync/internal/syncer"
)

// The renewal replaces one lease object with the next on the real service, which the in-memory
// fake only approximates: the real delete and listing decide whether exactly one lease is left.
func TestLiveLeaseRenewalReplacesTheLease(t *testing.T) {
	l := newLive(t)
	ctx := context.Background()

	state, err := l.store.ReadState(ctx)
	require.NoError(t, err)

	// A 30 s TTL renews every 10 s, and a renewal gets 10 s. A renewal makes four requests, and
	// on the real service that takes longer than the 1 s a 3 s TTL would allow.
	holder, err := syncer.AcquireLease(ctx, l.store, state, "live-renewal-test", 30*time.Second, false)
	require.NoError(t, err)

	stop := holder.KeepAlive(ctx, func(err error) { t.Errorf("renewal failed: %v", err) })

	require.Eventually(t, func() bool {
		current, err := l.store.ReadState(ctx)

		return err == nil && len(current.Leases) == 1 && current.Leases[0].Sequence >= 2
	}, 120*time.Second, 2*time.Second, "the lease was never replaced by a renewal")

	stop()

	require.NoError(t, holder.Release(ctx))

	released, err := l.store.ReadState(ctx)
	require.NoError(t, err)
	assert.Empty(t, released.Leases, "the lease must be gone after Release")
}

// A renewal that times out cannot tell whether the heartbeat landed, so the holder deletes that
// name later whether or not it exists. The real service must accept a delete of a missing name.
func TestLiveDeletingAMissingLeaseSucceeds(t *testing.T) {
	l := newLive(t)

	deleted, err := l.store.Delete(context.Background(), []string{".dr-sync/locks/never-written-1.lock"})

	require.NoError(t, err)
	assert.Zero(t, deleted)
}

// The lease cleanup treats a 409 on a heartbeat as someone else's object and keeps it, and any
// other failure as possibly its own. That only holds if replaying a commit that already landed
// does not come back as a 409.
func TestLiveReplayedCommitIsNotAConflict(t *testing.T) {
	l := newLive(t)
	ctx := context.Background()

	stage, err := l.client.CreateStage(ctx, l.containerID)
	require.NoError(t, err)

	content := []byte("replay")

	require.NoError(t, l.client.UploadToStage(ctx, l.containerID, stage.StageID, drapi.FormFile{
		Filename: ".dr-sync/probe/replay.txt",
		Size:     int64(len(content)),
		Open:     func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(content)), nil },
	}))

	_, err = l.client.CommitStage(ctx, l.containerID, stage.StageID, drapi.OverwriteError)
	require.NoError(t, err)

	_, err = l.client.CommitStage(ctx, l.containerID, stage.StageID, drapi.OverwriteError)

	require.Error(t, err, "a stage cannot be applied twice")
	assert.False(t, drapi.IsConflict(err), "a replayed commit must not look like a name collision: %v", err)
}
