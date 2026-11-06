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
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/datarobot-labs/datarobot-sync/internal/manifest"
	"github.com/datarobot-labs/datarobot-sync/internal/progress"
	"github.com/datarobot-labs/datarobot-sync/internal/syncer"
)

// runIn runs command against root with the defaults of `dr-sync run`: deletes in both directions,
// lease on.
func (h *harness) runIn(root string, command func(ctx context.Context) error) (*syncer.RunResult, error) {
	return syncer.Run(context.Background(), h.store, progress.Discard(), syncer.RunOptions{
		Download: syncer.DownloadOptions{Root: root, Delete: true},
		Upload:   syncer.UploadOptions{Root: root, Level: manifest.DefaultLevel, Delete: true},
		Lock:     true,
		Command:  command,
	})
}

// pulled returns what a fresh pull of the container holds, and the directory it went to.
func (h *harness) pulled(t *testing.T) (map[string]string, string) {
	t.Helper()

	destination := t.TempDir()
	h.download(t, destination)

	return readTree(t, destination), destination
}

func TestRunPushesWhatTheCommandChanged(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.upload(t, writeTree(t, map[string]string{"a.txt": "one"}))

	work := t.TempDir()

	result, err := h.runIn(work, func(context.Context) error {
		writeInto(t, work, map[string]string{"a.txt": "two", "b.txt": "new"})

		return nil
	})

	require.NoError(t, err)
	require.NoError(t, result.CommandErr)
	require.NotNil(t, result.Uploaded)

	tree, _ := h.pulled(t)
	assert.Equal(t, map[string]string{"a.txt": "two", "b.txt": "new"}, tree)
	assert.Zero(t, h.api.countObjects(h.containerID, locksPrefix), "the lease must be released")
}

func TestRunStartsTheCommandFromTheHeadAndKeepsWhatItReplaced(t *testing.T) {
	t.Parallel()

	h, bob := bobEdits(t)
	writeInto(t, bob, map[string]string{"scratch.txt": "only on bob's disk"})

	var seen map[string]string

	_, err := h.runIn(bob, func(context.Context) error {
		seen = readTree(t, bob)

		return nil
	})
	require.NoError(t, err)

	assert.Equal(t, map[string]string{"a.txt": "v2 from alice", "keep.txt": "same everywhere"}, seen,
		"the command must see the head, without bob's edit or his stray file")

	kept, err := os.ReadFile(filepath.Join(onlyBackupDir(t, bob), "a.txt"))
	require.NoError(t, err)
	assert.Equal(t, "bob's local edit, longer than v1", string(kept))

	stray, err := os.ReadFile(filepath.Join(onlyBackupDir(t, bob), "scratch.txt"))
	require.NoError(t, err)
	assert.Equal(t, "only on bob's disk", string(stray))

	tree, fresh := h.pulled(t)
	assert.Equal(t, seen, tree, "the push must not bring the backed-up files back")

	_, statErr := os.Stat(filepath.Join(fresh, ".dr-sync-backup"))
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

// onlyBackupDir returns the one timestamped directory a run left under .dr-sync-backup.
func onlyBackupDir(t *testing.T, root string) string {
	t.Helper()

	entries, err := os.ReadDir(filepath.Join(root, ".dr-sync-backup"))
	require.NoError(t, err)
	require.Len(t, entries, 1)

	return filepath.Join(root, ".dr-sync-backup", entries[0].Name())
}

func TestRunDropsAPathTheCommandDeleted(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.upload(t, writeTree(t, map[string]string{"a.txt": "one", "gone.txt": "bye"}))

	work := t.TempDir()

	_, err := h.runIn(work, func(context.Context) error {
		return os.Remove(filepath.Join(work, "gone.txt"))
	})
	require.NoError(t, err)

	tree, _ := h.pulled(t)
	assert.Equal(t, map[string]string{"a.txt": "one"}, tree)
}

func TestRunPushesEvenWhenTheCommandFails(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.upload(t, writeTree(t, map[string]string{"a.txt": "one"}))

	work := t.TempDir()
	failure := errors.New("exit status 255")

	result, err := h.runIn(work, func(context.Context) error {
		writeInto(t, work, map[string]string{"state.json": "partial"})

		return failure
	})

	require.NoError(t, err, "a failing command is not a failing sync")
	require.ErrorIs(t, result.CommandErr, failure)

	tree, _ := h.pulled(t)
	assert.Equal(t, "partial", tree["state.json"])
}

func TestRunOnAFreshlyInitializedContainerPushesOverTheSeed(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	work := t.TempDir()

	result, err := h.runIn(work, func(context.Context) error {
		writeInto(t, work, map[string]string{"first.txt": "hello"})

		return nil
	})

	require.NoError(t, err)
	require.NotNil(t, result.Downloaded, "the seed manifest is the base")

	tree, _ := h.pulled(t)
	assert.Equal(t, map[string]string{"first.txt": "hello"}, tree)
}

func TestRunOnAContainerWithoutManifestsMakesTheFirstPush(t *testing.T) {
	t.Parallel()

	api := newFakeAPI(t)
	client := api.client(t)

	container, err := client.CreateContainer(context.Background())
	require.NoError(t, err)

	h := &harness{
		api:         api,
		store:       syncer.NewStore(client, container.ID, manifest.DefaultMetaPrefix),
		containerID: container.ID,
	}
	work := t.TempDir()

	result, err := h.runIn(work, func(context.Context) error {
		writeInto(t, work, map[string]string{"first.txt": "hello"})

		return nil
	})

	require.NoError(t, err)
	assert.Nil(t, result.Downloaded, "there was nothing to pull")
	require.NotNil(t, result.Uploaded)

	tree, _ := h.pulled(t)
	assert.Equal(t, map[string]string{"first.txt": "hello"}, tree)
}

func TestRunRefusesWhileAnotherWriterHoldsTheLease(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.upload(t, writeTree(t, map[string]string{"a.txt": "one"}))

	state, err := h.store.ReadState(context.Background())
	require.NoError(t, err)

	_, err = syncer.AcquireLease(context.Background(), h.store, state, "someone-else", time.Hour, false)
	require.NoError(t, err)

	started := false
	work := t.TempDir()

	_, err = h.runIn(work, func(context.Context) error {
		started = true

		return nil
	})

	var held *syncer.LeaseHeldError

	require.ErrorAs(t, err, &held)
	assert.True(t, syncer.IsConflict(err))
	assert.False(t, started, "the command must not start without the lease")

	entries, readErr := os.ReadDir(work)
	require.NoError(t, readErr)
	assert.Empty(t, entries, "nothing may be pulled before the lease is held")
}

func TestRunRefusesToPushOverAHeadThatMovedAndKeepsTheChanges(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.upload(t, writeTree(t, map[string]string{"a.txt": "base"}))

	work := t.TempDir()

	_, err := h.runIn(work, func(ctx context.Context) error {
		teammate := t.TempDir()

		if _, err := syncer.Download(ctx, h.store, progress.Discard(), syncer.DownloadOptions{Root: teammate}); err != nil {
			return err
		}

		writeInto(t, teammate, map[string]string{"a.txt": "a teammate's edit"})

		// Force gets this push past the run's lease.
		_, err := syncer.Upload(ctx, h.store, progress.Discard(),
			syncer.UploadOptions{Root: teammate, Level: manifest.DefaultLevel, Lock: true, Force: true})
		if err != nil {
			return err
		}

		writeInto(t, work, map[string]string{"mine.txt": "the command's output"})

		return nil
	})

	var stale *syncer.StaleBaseError

	require.ErrorAs(t, err, &stale)
	assert.True(t, syncer.IsConflict(err))
	assert.Contains(t, err.Error(), work, "the message must say where the unpushed changes are")

	content, readErr := os.ReadFile(filepath.Join(work, "mine.txt"))
	require.NoError(t, readErr)
	assert.Equal(t, "the command's output", string(content), "the changes must stay on disk")

	tree, _ := h.pulled(t)
	assert.Equal(t, map[string]string{"a.txt": "a teammate's edit"}, tree, "the head must stay the teammate's")
	assert.Zero(t, h.api.countObjects(h.containerID, locksPrefix), "the lease must be released")
}

func TestRunPushesAfterTheContextIsCanceled(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.upload(t, writeTree(t, map[string]string{"a.txt": "one"}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	work := t.TempDir()

	_, err := syncer.Run(ctx, h.store, progress.Discard(), syncer.RunOptions{
		Download: syncer.DownloadOptions{Root: work, Delete: true},
		Upload:   syncer.UploadOptions{Root: work, Level: manifest.DefaultLevel, Delete: true},
		Lock:     true,
		Command: func(ctx context.Context) error {
			writeInto(t, work, map[string]string{"state.json": "written before the interrupt"})
			cancel()

			return ctx.Err()
		},
	})
	require.NoError(t, err)

	tree, _ := h.pulled(t)
	assert.Equal(t, "written before the interrupt", tree["state.json"])
	assert.Zero(t, h.api.countObjects(h.containerID, locksPrefix))
}

func TestRunDoesNotStartTheCommandWhenThePullFails(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.upload(t, writeTree(t, map[string]string{"a.txt": "one"}))
	h.api.failObjectStore(http.StatusForbidden)

	started := false
	manifestsBefore := h.api.countObjects(h.containerID, ".dr-sync/manifests/")

	_, err := h.runIn(t.TempDir(), func(context.Context) error {
		started = true

		return nil
	})

	require.Error(t, err)
	assert.False(t, started)
	assert.Equal(t, manifestsBefore, h.api.countObjects(h.containerID, ".dr-sync/manifests/"), "nothing may be pushed")
	assert.Zero(t, h.api.countObjects(h.containerID, locksPrefix), "the lease must be released")
}

func TestIncompleteDownloadErrorNamesTheFilesAndStaysShort(t *testing.T) {
	t.Parallel()

	err := &syncer.IncompleteDownloadError{Paths: []string{"a", "b", "c", "d", "e", "f", "g"}}

	assert.Contains(t, err.Error(), "7 paths were not pulled")
	assert.Contains(t, err.Error(), "a, b, c, d, e")
	assert.NotContains(t, err.Error(), "f, g", "the list is capped so one bad pull cannot flood the terminal")
}

// The mirror push would drop an entry the pull skipped, so the run must stop before the command.
func TestRunDoesNotStartTheCommandWhenThePullSkipsAnEntry(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}

	h := newHarness(t)
	source := writeTree(t, map[string]string{"a.txt": "one"})
	require.NoError(t, os.Symlink("/etc/hosts", filepath.Join(source, "abs-link")))
	h.upload(t, source)

	manifestsBefore := h.api.countObjects(h.containerID, ".dr-sync/manifests/")
	started := false

	_, err := h.runIn(t.TempDir(), func(context.Context) error {
		started = true

		return nil
	})

	require.True(t, syncer.IsIncompleteDownload(err), "got %v", err)
	assert.Contains(t, err.Error(), "abs-link (symlink target is absolute)")
	assert.False(t, started)
	assert.Equal(t, manifestsBefore, h.api.countObjects(h.containerID, ".dr-sync/manifests/"))
	assert.Zero(t, h.api.countObjects(h.containerID, locksPrefix), "the lease must be released")
}

// A pack that answers 403 leaves its files out of the pull without failing it.
func TestRunDoesNotStartTheCommandWhenAPackCannotBePulled(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.upload(t, writeTree(t, map[string]string{"a.txt": "one", "b.txt": "two"}))
	h.api.failObjectStoreFor(http.StatusForbidden, ".dr-sync/packs/")

	started := false

	_, err := h.runIn(t.TempDir(), func(context.Context) error {
		started = true

		return nil
	})

	require.True(t, syncer.IsIncompleteDownload(err), "got %v", err)
	assert.False(t, started)
}

// With no manifest, the first push has no head to compare. It must still refuse once a writer
// that ignored the lease has pushed one, or it would drop that writer's files.
func TestRunRefusesToPushOverAFirstPushItDidNotSee(t *testing.T) {
	t.Parallel()

	api := newFakeAPI(t)
	client := api.client(t)

	container, err := client.CreateContainer(context.Background())
	require.NoError(t, err)

	h := &harness{
		api:         api,
		store:       syncer.NewStore(client, container.ID, manifest.DefaultMetaPrefix),
		containerID: container.ID,
	}
	work := t.TempDir()

	_, err = h.runIn(work, func(ctx context.Context) error {
		theirs := writeTree(t, map[string]string{"theirs.txt": "a teammate's first push"})

		if _, err := syncer.Upload(ctx, h.store, progress.Discard(),
			syncer.UploadOptions{Root: theirs, Level: manifest.DefaultLevel, Lock: true, Force: true}); err != nil {
			return err
		}

		writeInto(t, work, map[string]string{"mine.txt": "the command's output"})

		return nil
	})

	var stale *syncer.StaleBaseError

	require.ErrorAs(t, err, &stale)
	assert.Empty(t, stale.Expected)
	assert.Contains(t, err.Error(), "expects an empty container")

	tree, _ := h.pulled(t)
	assert.Equal(t, map[string]string{"theirs.txt": "a teammate's first push"}, tree)
}

func TestUploadRefusesAnExpectedEmptyContainerThatHoldsAManifest(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := writeTree(t, map[string]string{"a.txt": "one"})

	_, err := syncer.Upload(context.Background(), h.store, progress.Discard(),
		syncer.UploadOptions{Root: source, Level: manifest.DefaultLevel, Lock: true, ExpectEmpty: true})

	var stale *syncer.StaleBaseError

	require.ErrorAs(t, err, &stale)
	assert.True(t, syncer.IsConflict(err))
}

// cancelOnApply cancels a context when the pull reports that it is applying its plan, which is
// the last point where the pull still depends on the context.
type cancelOnApply struct{ cancel context.CancelFunc }

func (w cancelOnApply) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "apply") {
		w.cancel()
	}

	return len(p), nil
}

func TestRunDoesNotStartTheCommandAfterAnInterruptDuringThePull(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.upload(t, writeTree(t, map[string]string{"a.txt": "one"}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	reporter := progress.New(progress.Options{Mode: progress.ModeText, Human: cancelOnApply{cancel}})

	started := false
	work := t.TempDir()

	_, err := syncer.Run(ctx, h.store, reporter, syncer.RunOptions{
		Download: syncer.DownloadOptions{Root: work, Delete: true},
		Upload:   syncer.UploadOptions{Root: work, Level: manifest.DefaultLevel, Delete: true},
		Lock:     true,
		Command: func(context.Context) error {
			started = true

			return nil
		},
	})

	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, started, "an interrupt before the command starts must not start it")
	assert.Zero(t, h.api.countObjects(h.containerID, locksPrefix), "the lease must be released")
}

// lockedBuffer collects what the reporter writes, from whichever goroutine writes it.
type lockedBuffer struct {
	mu   sync.Mutex
	text strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.text.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.text.String()
}

// Moving a tree aside is how unpushed state from an earlier failed run gets hidden, so the run
// has to say so.
func TestRunWarnsWhenThePullMovedLocalFilesAside(t *testing.T) {
	t.Parallel()

	h, bob := bobEdits(t)

	var output lockedBuffer

	_, err := syncer.Run(context.Background(), h.store,
		progress.New(progress.Options{Mode: progress.ModeText, Human: &output}),
		syncer.RunOptions{
			Download: syncer.DownloadOptions{Root: bob, Delete: true},
			Upload:   syncer.UploadOptions{Root: bob, Level: manifest.DefaultLevel, Delete: true},
			Lock:     true,
			Command:  func(context.Context) error { return nil },
		})
	require.NoError(t, err)

	assert.Contains(t, output.String(), "differed from the head and were moved to")
}
