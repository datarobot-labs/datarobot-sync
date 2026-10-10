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
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/datarobot-labs/datarobot-sync/internal/manifest"
	"github.com/datarobot-labs/datarobot-sync/internal/progress"
	"github.com/datarobot-labs/datarobot-sync/internal/syncer"
)

const (
	locksPrefix = ".dr-sync/locks/"

	// renewalTTL makes the lease renew every 200 ms, so a test sees several renewals quickly.
	// A renewal is bounded by the TTL, so it must stay well above a loopback request under -race.
	renewalTTL = 600 * time.Millisecond

	// renewalDeadline bounds every wait for a renewal, so a regression fails the test instead of
	// hanging it.
	renewalDeadline = 5 * time.Second
)

// lockNames lists the lease objects currently in the container.
func (h *harness) lockNames() []string {
	var locks []string

	for _, name := range h.api.objectNames(h.containerID) {
		if strings.HasPrefix(name, locksPrefix) {
			locks = append(locks, name)
		}
	}

	return locks
}

// holdsManifest reports whether a staged commit carries a manifest.
func holdsManifest(names []string) bool {
	for _, name := range names {
		if strings.HasPrefix(name, ".dr-sync/manifests/") {
			return true
		}
	}

	return false
}

// waitFor polls until condition holds or renewalDeadline passes.
func waitFor(condition func() bool) bool {
	deadline := time.Now().Add(renewalDeadline)

	for time.Now().Before(deadline) {
		if condition() {
			return true
		}

		time.Sleep(10 * time.Millisecond)
	}

	return false
}

// An upload that outlives a third of the TTL must keep its lease: otherwise a second client
// sees an expired lease and starts writing into a container that is still being pushed to.
func TestUploadRenewsItsLeaseWhileTheManifestCommitIsPending(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := writeTree(t, fixture)

	var renewed atomic.Pointer[[]string]

	h.api.beforeCommit = func(_ string, names []string) error {
		if !holdsManifest(names) {
			return nil
		}

		// Hold the manifest back until a renewal has replaced the first lease.
		replaced := waitFor(func() bool {
			locks := h.lockNames()

			return len(locks) == 1 && !strings.HasSuffix(locks[0], "-1.lock")
		})
		if !replaced {
			return errors.New("the lease was never renewed")
		}

		locks := h.lockNames()
		renewed.Store(&locks)

		return nil
	}

	h.upload(t, source, func(opts *syncer.UploadOptions) { opts.LeaseTTL = renewalTTL })

	require.NotNil(t, renewed.Load(), "the manifest commit never saw a renewed lease")
	assert.Len(t, *renewed.Load(), 1, "a renewal must replace the previous lease, not add to it")
	assert.Empty(t, h.lockNames(), "the lease must be released when the upload ends")
}

// A failed renewal must not fail the upload: the lease only lapses after the whole TTL, and
// losing a finished push over a guard that is advisory anyway would be the wrong trade.
func TestFailedLeaseRenewalWarnsAndTheUploadStillCompletes(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := writeTree(t, fixture)
	reporter := progress.Discard()

	h.api.beforeCommit = func(_ string, names []string) error {
		for _, name := range names {
			if strings.HasPrefix(name, locksPrefix) && !strings.HasSuffix(name, "-1.lock") {
				return errors.New("simulated renewal failure")
			}
		}

		if holdsManifest(names) && !waitFor(func() bool { return reporter.Warnings() > 0 }) {
			return errors.New("the failed renewal was never reported")
		}

		return nil
	}

	_, err := syncer.Upload(context.Background(), h.store, reporter, syncer.UploadOptions{
		Root:     source,
		Level:    manifest.DefaultLevel,
		Lock:     true,
		LeaseTTL: renewalTTL,
	})
	require.NoError(t, err)

	assert.Positive(t, reporter.Warnings())
	assert.Empty(t, h.lockNames(), "the original lease must still be released")
}

// Without locking there is no lease to renew, so no lock object may appear during the upload.
func TestUploadWithoutLockNeverWritesALease(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := writeTree(t, fixture)

	var sawLock atomic.Bool

	h.api.beforeCommit = func(_ string, names []string) error {
		if len(h.lockNames()) > 0 {
			sawLock.Store(true)
		}

		if holdsManifest(names) {
			// Longer than a renewal interval would be for any TTL a test would pick.
			time.Sleep(2 * renewalTTL)

			if len(h.lockNames()) > 0 {
				sawLock.Store(true)
			}
		}

		return nil
	}

	h.upload(t, source, func(opts *syncer.UploadOptions) {
		opts.Lock = false
		opts.LeaseTTL = renewalTTL
	})

	assert.False(t, sawLock.Load())
	assert.Empty(t, h.lockNames())
}

// KeepAlive must stop writing once its stop function returns, or a renewal could recreate a
// lease right after Release removed it and block the next run for the whole TTL.
func TestKeepAliveStopsRenewingAfterStop(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	ctx := context.Background()

	state, err := h.store.ReadState(ctx)
	require.NoError(t, err)

	holder, err := syncer.AcquireLease(ctx, h.store, state, "keepalive-client", renewalTTL, false)
	require.NoError(t, err)

	stop := holder.KeepAlive(ctx, func(err error) { t.Errorf("unexpected renewal error: %v", err) })

	require.True(t, waitFor(func() bool {
		locks := h.lockNames()

		return len(locks) == 1 && !strings.HasSuffix(locks[0], "-1.lock")
	}), "the lease was never renewed")

	stop()

	settled := h.lockNames()

	time.Sleep(3 * renewalTTL)

	assert.Equal(t, settled, h.lockNames(), "a renewal ran after stop returned")
	require.NoError(t, holder.Release(ctx))
	assert.Empty(t, h.lockNames())
}

// Stopping while a renewal is in flight must wait for it to land. Cancelling it instead can
// write the new lease on the server while the holder still tracks the old one, so Release
// deletes the old lease and leaves the new one to block the next run for the whole TTL.
func TestKeepAliveStopWaitsForARenewalInFlight(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	ctx := context.Background()

	state, err := h.store.ReadState(ctx)
	require.NoError(t, err)

	holder, err := syncer.AcquireLease(ctx, h.store, state, "inflight-client", renewalTTL, false)
	require.NoError(t, err)

	blocked := make(chan struct{})
	release := make(chan struct{})

	var announced atomic.Bool

	// Unblock the handler however the test ends: the fake server waits for it on cleanup.
	var releaseOnce sync.Once

	releaseHandler := func() { releaseOnce.Do(func() { close(release) }) }

	t.Cleanup(releaseHandler)

	h.api.beforeCommit = func(_ string, names []string) error {
		for _, name := range names {
			if strings.HasSuffix(name, "-2.lock") {
				if announced.CompareAndSwap(false, true) {
					close(blocked)
				}

				<-release
			}
		}

		return nil
	}

	stop := holder.KeepAlive(ctx, func(err error) { t.Errorf("unexpected renewal error: %v", err) })

	select {
	case <-blocked:
	case <-time.After(renewalDeadline):
		t.Fatal("the renewal never started")
	}

	stopped := make(chan struct{})

	go func() {
		stop()
		close(stopped)
	}()

	select {
	case <-stopped:
		t.Fatal("stop returned while a renewal was still in flight")
	case <-time.After(renewalTTL / 4):
	}

	releaseHandler()

	select {
	case <-stopped:
	case <-time.After(renewalDeadline):
		t.Fatal("stop never returned after the renewal finished")
	}

	require.NoError(t, holder.Release(ctx))
	assert.Empty(t, h.lockNames(), "Release must remove the lease the interrupted renewal wrote")
}

// If deleting the previous heartbeat fails, the old object stays visible to other clients until
// it expires. The holder must remember it and drop it on the next renewal.
func TestRenewDropsAPreviousLeaseThatFailedToDelete(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	ctx := context.Background()

	state, err := h.store.ReadState(ctx)
	require.NoError(t, err)

	holder, err := syncer.AcquireLease(ctx, h.store, state, "delete-retry-client", time.Hour, false)
	require.NoError(t, err)

	var failDelete atomic.Bool

	failDelete.Store(true)

	h.api.beforeDelete = func([]string) error {
		if failDelete.Load() {
			return errors.New("simulated delete failure")
		}

		return nil
	}

	require.Error(t, holder.Renew(ctx), "a failed delete must be reported")
	assert.Len(t, h.lockNames(), 2, "the new heartbeat is written and the old one is still there")

	failDelete.Store(false)

	require.NoError(t, holder.Renew(ctx))

	locks := h.lockNames()
	require.Len(t, locks, 1, "the next renewal must drop both older heartbeats")
	assert.True(t, strings.HasSuffix(locks[0], "-3.lock"))
}

// Release is the last chance to drop a heartbeat whose delete failed during a renewal.
func TestReleaseDropsAPreviousLeaseThatFailedToDelete(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	ctx := context.Background()

	state, err := h.store.ReadState(ctx)
	require.NoError(t, err)

	holder, err := syncer.AcquireLease(ctx, h.store, state, "release-retry-client", time.Hour, false)
	require.NoError(t, err)

	var failDelete atomic.Bool

	failDelete.Store(true)

	h.api.beforeDelete = func([]string) error {
		if failDelete.Load() {
			return errors.New("simulated delete failure")
		}

		return nil
	}

	require.Error(t, holder.Renew(ctx))

	failDelete.Store(false)

	require.NoError(t, holder.Release(ctx))
	assert.Empty(t, h.lockNames(), "Release must remove the heartbeat whose delete failed")
}

// A renewal that hangs must give up before the lease it extends expires, so the next tick can
// try again while the lease is still valid. It starts a third of the TTL in, so it has two
// thirds left; a timeout of the whole TTL would run a third past the expiry.
func TestKeepAliveGivesUpOnAHungRenewalBeforeTheLeaseExpires(t *testing.T) {
	t.Parallel()

	const ttl = 3 * time.Second

	h := newHarness(t)
	ctx := context.Background()

	state, err := h.store.ReadState(ctx)
	require.NoError(t, err)

	acquired := time.Now()

	holder, err := syncer.AcquireLease(ctx, h.store, state, "hung-client", ttl, false)
	require.NoError(t, err)

	release := make(chan struct{})

	var releaseOnce sync.Once

	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	h.api.beforeCommit = func(_ string, names []string) error {
		for _, name := range names {
			if strings.HasSuffix(name, "-2.lock") {
				<-release
			}
		}

		return nil
	}

	failures := make(chan error, 1)

	stop := holder.KeepAlive(ctx, func(err error) {
		select {
		case failures <- err:
		default:
		}
	})

	select {
	case err := <-failures:
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Less(t, time.Since(acquired), ttl+ttl/6, "the renewal ran past the expiry of the lease")
	case <-time.After(2 * ttl):
		t.Fatal("a hung renewal was not abandoned")
	}

	releaseOnce.Do(func() { close(release) })
	stop()
}

// A TTL too short to split into renewals would make the ticker panic, so there is nothing to
// renew: the lease is already expired, and KeepAlive must leave it alone.
func TestKeepAliveOnATTLTooShortToRenewDoesNothing(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	ctx := context.Background()

	state, err := h.store.ReadState(ctx)
	require.NoError(t, err)

	holder, err := syncer.AcquireLease(ctx, h.store, state, "short-client", 2*time.Nanosecond, false)
	require.NoError(t, err)

	stop := holder.KeepAlive(ctx, func(err error) { t.Errorf("unexpected renewal error: %v", err) })

	time.Sleep(renewalTTL / 2)
	stop()

	locks := h.lockNames()
	require.Len(t, locks, 1)
	assert.True(t, strings.HasSuffix(locks[0], "-1.lock"), "no renewal may run for a TTL this short")
}

// A nil holder is what an upload without locking passes around, so KeepAlive must accept it.
func TestKeepAliveOnANilHolderDoesNothing(t *testing.T) {
	t.Parallel()

	var holder *syncer.LeaseHolder

	stop := holder.KeepAlive(context.Background(), func(error) { t.Error("a nil holder cannot fail to renew") })

	assert.NotPanics(t, stop)
}
