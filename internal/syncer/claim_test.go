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
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/datarobot-labs/datarobot-sync/internal/drapi"
	"github.com/datarobot-labs/datarobot-sync/internal/manifest"
	"github.com/datarobot-labs/datarobot-sync/internal/progress"
	"github.com/datarobot-labs/datarobot-sync/internal/syncer"
)

const claimsPrefix = ".dr-sync/claims/"

// claimNames returns the head claims in a container, sorted.
func claimNames(h *harness) []string {
	var names []string

	for _, name := range h.api.objectNames(h.containerID) {
		if strings.HasPrefix(name, claimsPrefix) {
			names = append(names, name)
		}
	}

	return names
}

// claimGeneration commits a claim for generation with no manifest, so a later commit for that
// generation is rejected.
func claimGeneration(t *testing.T, h *harness, generation int64) {
	t.Helper()

	content := []byte("another writer")

	err := h.store.Commit(context.Background(), nil, []syncer.Object{{
		Name:    manifest.ClaimName(manifest.DefaultMetaPrefix, generation),
		Content: content,
		Size:    int64(len(content)),
	}}, syncer.CommitOptions{})
	require.NoError(t, err)
}

func TestUploadClaimsEachGeneration(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := writeTree(t, map[string]string{"a.txt": "one"})

	h.upload(t, source, func(o *syncer.UploadOptions) { o.NoGC = true })
	assert.Equal(t, []string{claimsPrefix + "0000000001.claim"}, claimNames(h))

	writeInto(t, source, map[string]string{"a.txt": "two"})
	h.upload(t, source, func(o *syncer.UploadOptions) { o.NoGC = true })
	assert.Equal(t, []string{claimsPrefix + "0000000001.claim", claimsPrefix + "0000000002.claim"}, claimNames(h))
}

func TestUploadRefusesAGenerationAnotherWriterClaimed(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := writeTree(t, map[string]string{"a.txt": "one"})
	h.upload(t, source, func(o *syncer.UploadOptions) { o.NoGC = true })

	manifestsBefore := h.api.countObjects(h.containerID, ".dr-sync/manifests/")

	// Revalidation compares heads and ignores claims, so only the commit sees this claim.
	claimGeneration(t, h, 2)
	writeInto(t, source, map[string]string{"a.txt": "two"})

	_, err := syncer.Upload(context.Background(), h.store, progress.Discard(),
		syncer.UploadOptions{Root: source, Level: manifest.DefaultLevel, Lock: true, NoGC: true})

	var conflict *syncer.ConflictError

	require.ErrorAs(t, err, &conflict)
	assert.True(t, syncer.IsConflict(err))
	assert.Contains(t, err.Error(), "claimed")
	assert.Equal(t, manifestsBefore, h.api.countObjects(h.containerID, ".dr-sync/manifests/"),
		"the losing run must not leave a manifest behind")

	// New packs commit in a stage before the manifest, so the loser may leave orphan packs.
	// No manifest references them, so the tree readers see is unchanged.
	destination := t.TempDir()
	h.download(t, destination)
	assert.Equal(t, map[string]string{"a.txt": "one"}, readTree(t, destination))
}

// --overwrite replace is for packs. The manifest stage must still reject a claimed generation.
func TestUploadWithOverwriteReplaceStillRefusesAClaimedGeneration(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := writeTree(t, map[string]string{"a.txt": "one"})
	h.upload(t, source, func(o *syncer.UploadOptions) { o.NoGC = true })

	manifestsBefore := h.api.countObjects(h.containerID, ".dr-sync/manifests/")

	claimGeneration(t, h, 2)
	writeInto(t, source, map[string]string{"a.txt": "two"})

	_, err := syncer.Upload(context.Background(), h.store, progress.Discard(), syncer.UploadOptions{
		Root: source, Level: manifest.DefaultLevel, Lock: true, NoGC: true, Overwrite: drapi.OverwriteReplace,
	})

	require.True(t, syncer.IsConflict(err), "got %v", err)
	assert.Equal(t, manifestsBefore, h.api.countObjects(h.containerID, ".dr-sync/manifests/"))
}

// Compaction races an upload for the same generation, so it needs the claim as much as upload does.
func TestCompactRefusesAGenerationAnotherWriterClaimed(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := writeTree(t, map[string]string{
		"keep.txt":  strings.Repeat("keep this content ", 300),
		"churn.txt": strings.Repeat("original ", 300),
	})

	h.upload(t, source, func(o *syncer.UploadOptions) { o.NoGC = true })

	for i := range 3 {
		writeInto(t, source, map[string]string{"churn.txt": strings.Repeat("revision ", 300) + string(rune('a'+i))})
		h.upload(t, source, func(o *syncer.UploadOptions) { o.NoGC = true; o.Delete = true })
	}

	// The head is generation 4, so compaction commits generation 5.
	claimGeneration(t, h, 5)

	manifestsBefore := h.api.countObjects(h.containerID, ".dr-sync/manifests/")

	_, err := syncer.Compact(context.Background(), h.store, progress.Discard(), syncer.CompactOptions{
		Root: source, Threshold: 0.9, KeepGenerations: 5, Level: manifest.DefaultLevel,
	})

	require.True(t, syncer.IsConflict(err), "got %v", err)
	assert.Equal(t, manifestsBefore, h.api.countObjects(h.containerID, ".dr-sync/manifests/"))
}

// The server can apply a commit and lose the answer. The client retries a stage that no longer
// exists and fails, but the head is the run's own manifest, so the push landed.
func TestUploadSucceedsWhenTheCommitLandedAndTheAnswerWasLost(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := writeTree(t, map[string]string{"a.txt": "one"})
	h.upload(t, source, func(o *syncer.UploadOptions) { o.NoGC = true })

	writeInto(t, source, map[string]string{"a.txt": "two"})

	var lost atomic.Bool

	h.api.loseResponse = func(names []string) bool {
		carriesManifest := slices.ContainsFunc(names, func(name string) bool {
			return strings.HasPrefix(name, ".dr-sync/manifests/")
		})

		return carriesManifest && lost.CompareAndSwap(false, true)
	}

	result := h.upload(t, source, func(o *syncer.UploadOptions) { o.NoGC = true })

	assert.True(t, lost.Load(), "the manifest commit must have lost its answer")
	assert.EqualValues(t, 2, result.Manifest.Generation)

	destination := t.TempDir()
	h.download(t, destination)
	assert.Equal(t, map[string]string{"a.txt": "two"}, readTree(t, destination))
}

func TestUploadSucceedsOnAContainerWrittenWithoutClaims(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := writeTree(t, map[string]string{"a.txt": "one"})
	h.upload(t, source, func(o *syncer.UploadOptions) { o.NoGC = true })

	// A container last written by a dr-sync that predates claims has none.
	_, err := h.store.Delete(context.Background(), claimNames(h))
	require.NoError(t, err)
	require.Empty(t, claimNames(h))

	writeInto(t, source, map[string]string{"a.txt": "two"})
	h.upload(t, source, func(o *syncer.UploadOptions) { o.NoGC = true })

	assert.Equal(t, []string{claimsPrefix + "0000000002.claim"}, claimNames(h))
}

func TestCollectPrunesOnlyClaimsBelowTheRetainedGenerations(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := writeTree(t, map[string]string{"a.txt": "0"})

	for i := range 4 {
		writeInto(t, source, map[string]string{"a.txt": strings.Repeat("x", i+1)})
		h.upload(t, source, func(o *syncer.UploadOptions) { o.NoGC = true; o.Delete = true })
	}

	require.Len(t, claimNames(h), 4)

	for _, name := range h.api.objectNames(h.containerID) {
		h.api.setObjectAge(h.containerID, name, 24*time.Hour)
	}

	state, err := h.store.ReadState(context.Background())
	require.NoError(t, err)

	plan, err := syncer.PlanCollect(context.Background(), h.store, state, syncer.CollectOptions{
		KeepGenerations: 2,
		Grace:           time.Hour,
		Now:             time.Now(),
	})
	require.NoError(t, err)

	assert.Equal(t, []string{claimsPrefix + "0000000001.claim", claimsPrefix + "0000000002.claim"}, plan.Claims,
		"claims under the oldest retained manifest can never be contended again")

	_, err = syncer.Collect(context.Background(), h.store, state, syncer.CollectOptions{
		KeepGenerations: 2,
		Grace:           time.Hour,
		Now:             time.Now(),
	})
	require.NoError(t, err)
	assert.Equal(t, []string{claimsPrefix + "0000000003.claim", claimsPrefix + "0000000004.claim"}, claimNames(h),
		"the head's claim and every retained generation's claim stay")
}

func TestCollectKeepsEveryClaimWhenNothingIsPruned(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := writeTree(t, map[string]string{"a.txt": "one"})
	h.upload(t, source, func(o *syncer.UploadOptions) { o.NoGC = true })

	state, err := h.store.ReadState(context.Background())
	require.NoError(t, err)

	plan, err := syncer.PlanCollect(context.Background(), h.store, state, syncer.CollectOptions{
		KeepGenerations: 5,
		Grace:           time.Hour,
		Now:             time.Now(),
	})
	require.NoError(t, err)

	assert.Empty(t, plan.Claims)
}

func TestResolveCommitError(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := writeTree(t, map[string]string{"a.txt": "one"})
	h.upload(t, source, func(o *syncer.UploadOptions) { o.NoGC = true })

	state, err := h.store.ReadState(context.Background())
	require.NoError(t, err)

	head, ok := manifest.SelectHead(state.Manifests)
	require.True(t, ok)

	rejected := &drapi.APIError{Method: http.MethodPost, Path: "files/x/fromStage/", Status: http.StatusConflict}
	rival := &manifest.Manifest{ID: "0000000002-deadbeef", Generation: 2}

	t.Run("a non-conflict error passes through", func(t *testing.T) {
		t.Parallel()

		boom := errors.New("boom")

		assert.Same(t, boom, syncer.ResolveCommitError(context.Background(), h.store, boom, head.ID, rival))
	})

	t.Run("a 409 with the head unmoved and no claim is not a lost update", func(t *testing.T) {
		t.Parallel()

		// This is what a pack name that a concurrent writer uploaded first looks like.
		err := syncer.ResolveCommitError(context.Background(), h.store, rejected, head.ID, rival)

		assert.Same(t, rejected, err)
		assert.False(t, syncer.IsConflict(err))
	})

	t.Run("a 409 after the head moved is a conflict", func(t *testing.T) {
		t.Parallel()

		err := syncer.ResolveCommitError(context.Background(), h.store, rejected, "0000000000-00000000",
			&manifest.Manifest{ID: "0000000001-deadbeef", Generation: 1})

		var conflict *syncer.ConflictError

		require.ErrorAs(t, err, &conflict)
		assert.Equal(t, head.ID, conflict.Actual)
	})

	t.Run("a head that is the next manifest means the commit landed", func(t *testing.T) {
		t.Parallel()

		landed := &manifest.Manifest{ID: head.ID, Generation: head.Generation}
		replayed := &drapi.APIError{Method: http.MethodPost, Path: "files/x/fromStage/", Status: http.StatusUnprocessableEntity}

		for _, commitErr := range []error{rejected, replayed, errors.New("boom")} {
			assert.NoError(t, syncer.ResolveCommitError(context.Background(), h.store, commitErr, "0000000000-00000000", landed),
				"%v", commitErr)
		}
	})

	t.Run("a container with no manifests passes the error through", func(t *testing.T) {
		t.Parallel()

		empty := newHarness(t)

		var manifests []string

		for _, name := range empty.api.objectNames(empty.containerID) {
			if strings.HasPrefix(name, ".dr-sync/manifests/") {
				manifests = append(manifests, name)
			}
		}

		_, err := empty.store.Delete(context.Background(), manifests)
		require.NoError(t, err)

		assert.Same(t, rejected, syncer.ResolveCommitError(context.Background(), empty.store, rejected, head.ID, rival))
	})
}

func TestResolveCommitErrorSeesAnExistingClaim(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := writeTree(t, map[string]string{"a.txt": "one"})
	h.upload(t, source, func(o *syncer.UploadOptions) { o.NoGC = true })

	state, err := h.store.ReadState(context.Background())
	require.NoError(t, err)

	head, _ := manifest.SelectHead(state.Manifests)

	claimGeneration(t, h, 2)

	rejected := &drapi.APIError{Method: http.MethodPost, Path: "files/x/fromStage/", Status: http.StatusConflict}
	err = syncer.ResolveCommitError(context.Background(), h.store, rejected, head.ID,
		&manifest.Manifest{ID: "0000000002-deadbeef", Generation: 2})

	var conflict *syncer.ConflictError

	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, conflict.Expected, conflict.Actual,
		"the head has not moved, but the generation is taken")
}
