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
	"errors"
	"fmt"
	"path"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/datarobot-labs/datarobot-sync/internal/drapi"
	"github.com/datarobot-labs/datarobot-sync/internal/manifest"
	"github.com/datarobot-labs/datarobot-sync/internal/progress"
	"github.com/datarobot-labs/datarobot-sync/internal/syncer"
)

const claimRaceRounds = 20

// runTogether starts every function behind one barrier and returns when all have finished.
func runTogether(count int, run func(i int)) {
	var ready, done sync.WaitGroup

	start := make(chan struct{})

	ready.Add(count)
	done.Add(count)

	for i := range count {
		go func() {
			defer done.Done()

			ready.Done()
			<-start

			run(i)
		}()
	}

	ready.Wait()
	close(start)
	done.Wait()
}

// stagedClaim is a stage holding the contested claim and one object that only this writer stages.
type stagedClaim struct {
	stageID string
	own     string
}

func stageClaim(t *testing.T, l *live, claimName string, writer int) stagedClaim {
	t.Helper()

	ctx := context.Background()

	stage, err := l.client.CreateStage(ctx, l.containerID)
	require.NoError(t, err)

	own := fmt.Sprintf(".dr-sync/probe/%s-writer-%d.txt", path.Base(claimName), writer)

	require.NoError(t, l.client.UploadBytesToStage(ctx, l.containerID, stage.StageID, claimName,
		fmt.Appendf(nil, "writer %d", writer)))
	require.NoError(t, l.client.UploadBytesToStage(ctx, l.containerID, stage.StageID, own, []byte("own")))

	return stagedClaim{stageID: stage.StageID, own: own}
}

// The head claim only works if the real fromStage checks names and applies a stage atomically.
// If it checks first and writes later, two simultaneous commits can both succeed, which is the
// fork the claim exists to prevent.
func TestLiveSameClaimNameAdmitsExactlyOneCommit(t *testing.T) {
	l := newLive(t)
	ctx := context.Background()

	applied := make([]string, 0, claimRaceRounds)
	rejected := make([]string, 0, claimRaceRounds)

	for round := range claimRaceRounds {
		claim := manifest.ClaimName(manifest.DefaultMetaPrefix, int64(1000+round))
		commits := []stagedClaim{stageClaim(t, l, claim, 0), stageClaim(t, l, claim, 1)}
		errs := make([]error, len(commits))

		runTogether(len(commits), func(i int) {
			_, errs[i] = l.client.CommitStage(ctx, l.containerID, commits[i].stageID, drapi.OverwriteError)
		})

		var winner, loser int

		succeeded := 0

		for i, err := range errs {
			if err == nil {
				winner = i
				succeeded++

				continue
			}

			loser = i

			assert.True(t, drapi.IsConflict(err), "round %d: the losing commit must be a 409: %v", round, err)
		}

		require.Equal(t, 1, succeeded, "round %d: exactly one commit may apply, got %v", round, errs)

		applied = append(applied, commits[winner].own)
		rejected = append(rejected, commits[loser].own)
	}

	entries, err := l.client.ListFiles(ctx, l.containerID, drapi.ListOptions{Prefix: ".dr-sync/probe/"})
	require.NoError(t, err)

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name)
	}

	for _, name := range applied {
		assert.Contains(t, names, name, "the winning stage must be applied whole")
	}

	for _, name := range rejected {
		assert.NotContains(t, names, name, "a rejected commit must apply none of its stage")
	}
}

// Two uploads from the same head race for one generation. Whoever loses must fail with a conflict,
// and the container must never hold two manifests for a generation.
func TestLiveConcurrentUploadsDoNotFork(t *testing.T) {
	l := newLive(t)
	ctx := context.Background()

	const writers = 2

	for round := range 3 {
		sources := make([]string, writers)
		for i := range sources {
			sources[i] = t.TempDir()
			writeTree(t, sources[i], map[string]string{
				fmt.Sprintf("writer-%d.txt", i): fmt.Sprintf("round %d writer %d", round, i),
			})
		}

		results := make([]*syncer.UploadResult, writers)
		errs := make([]error, writers)

		runTogether(writers, func(i int) {
			results[i], errs[i] = syncer.Upload(ctx, l.store, progress.Discard(), syncer.UploadOptions{
				Root: sources[i], Level: manifest.DefaultLevel, NoGC: true,
			})
		})

		generations := map[int64]int{}
		succeeded := 0

		for i, err := range errs {
			if err != nil {
				require.True(t, syncer.IsConflict(err), "round %d writer %d: %v", round, i, err)

				continue
			}

			require.NotNil(t, results[i].Manifest)

			generations[results[i].Manifest.Generation]++
			succeeded++
		}

		require.NotEmpty(t, generations, "round %d: at least one writer must win", round)

		for generation, wins := range generations {
			assert.Equal(t, 1, wins, "round %d: generation %d was won more than once", round, generation)
		}

		t.Logf("round %d: %d of %d uploads succeeded", round, succeeded, writers)

		state, err := l.store.ReadState(ctx)
		require.NoError(t, err)

		fork, forked := manifest.DetectFork(state.Manifests)
		require.False(t, forked, "round %d: two manifests for generation %d", round, fork.Generation)
	}
}

// stageTimings is how long one stage took, by phase.
type stageTimings struct {
	create, upload, commit time.Duration
}

// timeStage runs the stage in the same order as Store.Commit and times each phase.
func timeStage(t *testing.T, l *live, objects map[string][]byte) stageTimings {
	t.Helper()

	ctx := context.Background()

	var timings stageTimings

	began := time.Now()

	stage, err := l.client.CreateStage(ctx, l.containerID)
	require.NoError(t, err)

	timings.create = time.Since(began)

	began = time.Now()
	names := make([]string, 0, len(objects))

	for name := range objects {
		names = append(names, name)
	}

	errs := make([]error, len(names))

	runTogether(len(names), func(i int) {
		errs[i] = l.client.UploadBytesToStage(ctx, l.containerID, stage.StageID, names[i], objects[names[i]])
	})

	require.NoError(t, errors.Join(errs...))

	timings.upload = time.Since(began)

	began = time.Now()
	_, err = l.client.CommitStage(ctx, l.containerID, stage.StageID, drapi.OverwriteError)
	require.NoError(t, err)

	timings.commit = time.Since(began)

	return timings
}

func median(values []time.Duration) time.Duration {
	sorted := slices.Clone(values)
	slices.Sort(sorted)

	return sorted[len(sorted)/2]
}

// This measures and asserts nothing, because a wall-clock bound would flake. It logs the
// median time of the final stage with and without the claim, so a server that queues uploads
// within a stage shows up as a gap in the upload phase. Run with -v to read it.
func TestLiveMeasureClaimCommitLatency(t *testing.T) {
	l := newLive(t)

	const rounds = 15

	manifestBody := make([]byte, 4<<10)
	claimBody := []byte("0000000002-deadbeef")

	var withoutClaim, withClaim []stageTimings

	for round := range rounds {
		plain := func() stageTimings {
			return timeStage(t, l, map[string][]byte{
				fmt.Sprintf(".dr-sync/probe/timing-%02d-plain/manifest", round): manifestBody,
			})
		}
		claimed := func() stageTimings {
			return timeStage(t, l, map[string][]byte{
				fmt.Sprintf(".dr-sync/probe/timing-%02d-claimed/manifest", round): manifestBody,
				fmt.Sprintf(".dr-sync/probe/timing-%02d-claimed/claim", round):    claimBody,
			})
		}

		// Alternate the order so a warming connection does not favor one side.
		if round%2 == 0 {
			withoutClaim = append(withoutClaim, plain())
			withClaim = append(withClaim, claimed())
		} else {
			withClaim = append(withClaim, claimed())
			withoutClaim = append(withoutClaim, plain())
		}
	}

	phase := func(samples []stageTimings, pick func(stageTimings) time.Duration) time.Duration {
		values := make([]time.Duration, len(samples))
		for i, sample := range samples {
			values[i] = pick(sample)
		}

		return median(values)
	}

	for _, p := range []struct {
		name string
		pick func(stageTimings) time.Duration
	}{
		{"CreateStage", func(s stageTimings) time.Duration { return s.create }},
		{"upload", func(s stageTimings) time.Duration { return s.upload }},
		{"CommitStage", func(s stageTimings) time.Duration { return s.commit }},
		{"total", func(s stageTimings) time.Duration { return s.create + s.upload + s.commit }},
	} {
		before, after := phase(withoutClaim, p.pick), phase(withClaim, p.pick)

		t.Logf("%-11s one object %7v   two objects %7v   difference %7v (median of %d)",
			p.name, before.Round(time.Millisecond), after.Round(time.Millisecond),
			(after - before).Round(time.Millisecond), rounds)
	}
}
