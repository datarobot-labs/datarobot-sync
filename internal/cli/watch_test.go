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
