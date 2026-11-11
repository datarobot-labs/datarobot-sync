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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/datarobot-labs/datarobot-sync/internal/manifest"
	"github.com/datarobot-labs/datarobot-sync/internal/progress"
	"github.com/datarobot-labs/datarobot-sync/internal/syncer"
)

var errCommandFailed = errors.New("the command failed")

func runOptions(root string, command func(ctx context.Context) error) syncer.RunOptions {
	return syncer.RunOptions{
		Download: syncer.DownloadOptions{Root: root},
		Upload:   syncer.UploadOptions{Root: root, Level: manifest.DefaultLevel},
		Lock:     true,
		Command:  command,
	}
}

// seed pushes files to the container as the first head.
func seed(t *testing.T, l *live, files map[string]string) {
	t.Helper()

	source := t.TempDir()
	writeTree(t, source, files)

	_, err := syncer.Upload(context.Background(), l.store, progress.Discard(), syncer.UploadOptions{
		Root: source, Level: manifest.DefaultLevel, Lock: true,
	})
	require.NoError(t, err)
}

// pulled downloads the container head into a fresh directory and returns its files.
func pulled(t *testing.T, l *live) map[string]string {
	t.Helper()

	destination := t.TempDir()

	_, err := syncer.Download(context.Background(), l.store, progress.Discard(),
		syncer.DownloadOptions{Root: destination})
	require.NoError(t, err)

	return readTree(t, destination)
}

func TestLiveRunPullsRunsAndPushes(t *testing.T) {
	l := newLive(t)
	seed(t, l, map[string]string{"state/a.txt": "one", "state/b.txt": "two"})

	dir := t.TempDir()

	var seen map[string]string

	result, err := syncer.Run(context.Background(), l.store, progress.Discard(),
		runOptions(dir, func(context.Context) error {
			seen = readTree(t, dir)

			writeTree(t, dir, map[string]string{"state/a.txt": "one, edited", "state/c.txt": "three"})

			return nil
		}))
	require.NoError(t, err)
	require.NoError(t, result.CommandErr)

	assert.Equal(t, map[string]string{"state/a.txt": "one", "state/b.txt": "two"}, seen,
		"the command must start from the pulled head")
	require.NotNil(t, result.Uploaded)

	assert.Equal(t, map[string]string{
		"state/a.txt": "one, edited",
		"state/b.txt": "two",
		"state/c.txt": "three",
	}, pulled(t, l), "the command's changes must reach the container")

	// A held lease would refuse this second run for the next 30 minutes.
	_, err = syncer.Run(context.Background(), l.store, progress.Discard(),
		runOptions(t.TempDir(), func(context.Context) error { return nil }))
	require.NoError(t, err, "the first run must release its lease")
}

func TestLiveRunPushesAfterAFailedCommand(t *testing.T) {
	l := newLive(t)
	seed(t, l, map[string]string{"state/a.txt": "one"})

	dir := t.TempDir()

	result, err := syncer.Run(context.Background(), l.store, progress.Discard(),
		runOptions(dir, func(context.Context) error {
			writeTree(t, dir, map[string]string{"state/partial.txt": "half done"})

			return errCommandFailed
		}))
	require.NoError(t, err, "a failed command is reported in the result, not as a run error")
	require.ErrorIs(t, result.CommandErr, errCommandFailed)

	assert.Equal(t, "half done", pulled(t, l)["state/partial.txt"],
		"the state a failed command left behind is what the next run needs")
}

func TestLiveRunRefusesWhileAnotherRunHoldsTheLease(t *testing.T) {
	l := newLive(t)
	seed(t, l, map[string]string{"state/a.txt": "one"})

	innerStarted := false

	_, err := syncer.Run(context.Background(), l.store, progress.Discard(),
		runOptions(t.TempDir(), func(ctx context.Context) error {
			_, innerErr := syncer.Run(ctx, l.store, progress.Discard(),
				runOptions(t.TempDir(), func(context.Context) error {
					innerStarted = true

					return nil
				}))

			var held *syncer.LeaseHeldError

			assert.ErrorAs(t, innerErr, &held, "a second run must be refused while the first holds the lease")

			return nil
		}))
	require.NoError(t, err)
	assert.False(t, innerStarted, "a refused run must not start its command")
}

func TestLiveRunRefusesToPushOverAHeadThatMoved(t *testing.T) {
	l := newLive(t)
	seed(t, l, map[string]string{"state/a.txt": "one"})

	dir := t.TempDir()
	rival := t.TempDir()

	result, err := syncer.Run(context.Background(), l.store, progress.Discard(),
		runOptions(dir, func(ctx context.Context) error {
			// A writer that skips the lease pushes while the run works.
			writeTree(t, rival, map[string]string{"state/a.txt": "one", "state/rival.txt": "theirs"})

			_, pushErr := syncer.Upload(ctx, l.store, progress.Discard(), syncer.UploadOptions{
				Root: rival, Level: manifest.DefaultLevel,
			})
			require.NoError(t, pushErr)

			writeTree(t, dir, map[string]string{"state/mine.txt": "ours"})

			return nil
		}))

	var stale *syncer.StaleBaseError

	require.ErrorAs(t, err, &stale)
	require.NotNil(t, result)

	assert.Equal(t, "theirs", pulled(t, l)["state/rival.txt"])
	assert.NotContains(t, pulled(t, l), "state/mine.txt", "the refused push must not land")

	kept, readErr := os.ReadFile(filepath.Join(dir, "state", "mine.txt"))
	require.NoError(t, readErr)
	assert.Equal(t, "ours", string(kept), "the command's changes must stay in the directory")
}

func TestLiveRunKeepsTheLeaseAliveWhileTheCommandRuns(t *testing.T) {
	l := newLive(t)
	seed(t, l, map[string]string{"state/a.txt": "one"})

	const ttl = 6 * time.Second

	options := runOptions(t.TempDir(), func(ctx context.Context) error {
		// Past the original expiry. Only a renewed lease still blocks a second run.
		time.Sleep(ttl + 2*time.Second)

		_, innerErr := syncer.Run(ctx, l.store, progress.Discard(),
			runOptions(t.TempDir(), func(context.Context) error { return nil }))

		var held *syncer.LeaseHeldError

		assert.ErrorAs(t, innerErr, &held, "the lease must outlive its first TTL while the command runs")

		return nil
	})
	options.LeaseTTL = ttl

	_, err := syncer.Run(context.Background(), l.store, progress.Discard(), options)
	require.NoError(t, err)
}
