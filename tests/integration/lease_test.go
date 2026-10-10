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
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/datarobot-labs/datarobot-sync/internal/syncer"
)

// The renewal replaces one lease object with the next on the real service, which the in-memory
// fake only approximates: the real delete and listing decide whether exactly one lease is left.
func TestLiveLeaseRenewalReplacesTheLease(t *testing.T) {
	l := newLive(t)
	ctx := context.Background()

	state, err := l.store.ReadState(ctx)
	require.NoError(t, err)

	// A 3 s TTL renews every second.
	holder, err := syncer.AcquireLease(ctx, l.store, state, "live-renewal-test", 3*time.Second, false)
	require.NoError(t, err)

	stop := holder.KeepAlive(ctx, func(err error) { t.Errorf("renewal failed: %v", err) })

	require.Eventually(t, func() bool {
		current, err := l.store.ReadState(ctx)

		return err == nil && len(current.Leases) == 1 && current.Leases[0].Sequence >= 2
	}, 60*time.Second, time.Second, "the lease was never replaced by a renewal")

	stop()

	require.NoError(t, holder.Release(ctx))

	released, err := l.store.ReadState(ctx)
	require.NoError(t, err)
	assert.Empty(t, released.Leases, "the lease must be gone after Release")
}
