// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/datarobot/golang-datarobot-filesync/internal/progress"
	"github.com/datarobot/golang-datarobot-filesync/internal/syncer"
	"github.com/datarobot/golang-datarobot-filesync/internal/watcher"
)

// newTestWatchCycle builds a watchCycle with just enough wired up to exercise the
// session-independent pieces of a cycle: no client or store, since neither
// handleCycleError nor rebuildMatcher touches them.
func newTestWatchCycle(t *testing.T, root string) *watchCycle {
	t.Helper()

	w, err := watcher.New(watcher.Options{Root: root, PollOnly: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, w.Close()) })

	return &watchCycle{
		session: &session{reporter: progress.Discard()},
		global:  &globalFlags{ignoreFile: ".drsyncignore"},
		opts:    &watchFlags{},
		root:    root,
		watcher: w,
		warned:  map[string]bool{},
	}
}

func TestHandleCycleErrorRetriesACycleTimeout(t *testing.T) {
	t.Parallel()

	cycle := newTestWatchCycle(t, t.TempDir())

	err := classify(context.DeadlineExceeded)
	require.Error(t, err)

	assert.NoError(t, cycle.handleCycleError(err),
		"--timeout bounds one cycle, not the daemon, so a deadline must not stop it")
}

func TestHandleCycleErrorRetriesConflictAndAPIFailures(t *testing.T) {
	t.Parallel()

	cycle := newTestWatchCycle(t, t.TempDir())

	assert.NoError(t, cycle.handleCycleError(Fail(CodeConflict, errors.New("generation moved"))))
	assert.NoError(t, cycle.handleCycleError(Fail(CodeAPI, errors.New("503"))))
}

func TestHandleCycleErrorStopsOnAnythingElse(t *testing.T) {
	t.Parallel()

	cycle := newTestWatchCycle(t, t.TempDir())

	assert.Error(t, cycle.handleCycleError(Fail(CodeUsage, errors.New("bad flag"))))
}

func TestRebuildMatcherPicksUpAnIgnoreFileEdit(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cycle := newTestWatchCycle(t, root)

	assert.Nil(t, cycle.session.matcher, "no ignore file yet, so the matcher starts nil")

	require.NoError(t, os.WriteFile(filepath.Join(root, ".drsyncignore"), []byte("*.log\n"), 0o600))

	cycle.rebuildMatcher()

	require.NotNil(t, cycle.session.matcher, "an ignore-file edit must rebuild the matcher without a restart")
	assert.True(t, cycle.session.matcher.Match("debug.log", false))
	assert.False(t, cycle.session.matcher.Match("main.go", false))
}

func TestRegisterWatchWriterSkipsUnderDryRun(t *testing.T) {
	t.Parallel()

	// A nil session.store would panic if registerWatchWriter tried to use it: reaching the
	// end of this call without a panic already proves --dry-run took the skip path rather
	// than announcing a presence the run promised not to write.
	registration, err := registerWatchWriter(t.Context(), &globalFlags{dryRun: true}, &session{}, "client", "/tmp/root", 0)

	require.NoError(t, err)
	assert.Nil(t, registration, "--dry-run must not write a writer-registry entry")
}

func TestRefreshWriterToleratesNoRegistration(t *testing.T) {
	t.Parallel()

	cycle := newTestWatchCycle(t, t.TempDir())

	assert.NotPanics(t, func() { cycle.refreshWriter(t.Context(), nil) },
		"--dry-run leaves registration nil; refreshing it must be a no-op, not a crash")
}

func TestGCSweepSkipsUnderDryRun(t *testing.T) {
	t.Parallel()

	cycle := newTestWatchCycle(t, t.TempDir())
	cycle.global.dryRun = true

	// A nil session.store would panic if gcSweep tried to read state: reaching the end of
	// this call without a panic already proves the dry-run guard fired before any use of it.
	assert.NotPanics(t, func() { cycle.gcSweep(t.Context()) },
		"--dry-run must never delete objects, so gcSweep must return before touching the store")
}

func TestMaybeCompactSkipsWithoutTheFlagOrUnderDryRun(t *testing.T) {
	t.Parallel()

	cycle := newTestWatchCycle(t, t.TempDir())

	// Neither call reaches runCompaction, which would need a real store: reaching the end of
	// this call without a panic already proves both guards short-circuit correctly.
	assert.NoError(t, cycle.maybeCompact(t.Context(), &syncer.UploadResult{}),
		"--compact was not requested, so nothing should run")

	cycle.opts.upload.compact = true

	assert.NoError(t, cycle.maybeCompact(t.Context(), &syncer.UploadResult{DryRun: true}),
		"--dry-run must skip compaction even when --compact was requested")
}
