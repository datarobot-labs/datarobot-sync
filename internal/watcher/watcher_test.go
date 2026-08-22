// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

package watcher

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/datarobot/golang-datarobot-filesync/internal/scan"
)

// testTimeout is generous relative to the debounce window every test below configures, so real
// OS event latency never makes the test flaky.
const testTimeout = 5 * time.Second

// startWatcher builds a Watcher with a short debounce, starts Run in the background, drains
// its initial trigger, and arranges for everything to stop at test end.
func startWatcher(t *testing.T, opts Options) (*Watcher, <-chan Trigger) {
	t.Helper()

	opts.Debounce = 20 * time.Millisecond
	opts.MinInterval = 20 * time.Millisecond
	opts.Warn = func(msg string) { t.Logf("watcher warning: %s", msg) }

	w, err := New(opts)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan struct{})

	go func() {
		defer close(done)

		if err := w.Run(ctx); err != nil {
			t.Logf("watcher stopped: %v", err)
		}
	}()

	t.Cleanup(func() {
		cancel()
		<-done
		require.NoError(t, w.Close())
	})

	triggers := w.Triggers()
	initial := waitTrigger(t, triggers)
	require.Equal(t, ReasonInitial, initial.Reason)

	return w, triggers
}

func waitTrigger(t *testing.T, triggers <-chan Trigger) Trigger {
	t.Helper()

	select {
	case trigger := <-triggers:
		return trigger
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for a trigger")

		return Trigger{}
	}
}

func assertNoTrigger(t *testing.T, triggers <-chan Trigger, within time.Duration) {
	t.Helper()

	select {
	case trigger := <-triggers:
		t.Fatalf("unexpected trigger: %+v", trigger)
	case <-time.After(within):
	}
}

func TestWatcherPicksUpANewNestedDirectory(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	_, triggers := startWatcher(t, Options{Root: root})

	nested := filepath.Join(root, "a", "b")
	require.NoError(t, os.MkdirAll(nested, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(nested, "leaf.txt"), []byte("hi"), 0o600))

	trigger := waitTrigger(t, triggers)
	assert.Equal(t, ReasonChange, trigger.Reason)
	assert.Positive(t, trigger.Events)
}

func TestWatcherNeverWatchesAnExcludedDirectory(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "vendor", "pkg"), 0o755))

	matcher, err := scan.NewMatcher([]string{"/vendor/"})
	require.NoError(t, err)

	w, triggers := startWatcher(t, Options{Root: root, Matcher: matcher})

	for _, watched := range w.Watched() {
		assert.NotContains(t, watched, "vendor", "an excluded directory must never be watched")
	}

	// A change inside the excluded directory must not produce a trigger: it was never
	// watched, so the OS never reports it.
	require.NoError(t, os.WriteFile(filepath.Join(root, "vendor", "pkg", "new.go"), []byte("x"), 0o600))
	assertNoTrigger(t, triggers, 300*time.Millisecond)
}

func TestWatcherAtomicRenameSaveProducesATrigger(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	dest := filepath.Join(root, "config.yaml")
	require.NoError(t, os.WriteFile(dest, []byte("v1"), 0o600))

	_, triggers := startWatcher(t, Options{Root: root})

	// The way an editor actually saves: write a sibling temp file, then rename it over the
	// destination. The watcher must not be watching config.yaml itself for this to work — it
	// watches the containing directory, exactly as fsnotify's own docs recommend.
	tmp := filepath.Join(root, "config.yaml.tmp")
	require.NoError(t, os.WriteFile(tmp, []byte("v2"), 0o600))
	require.NoError(t, os.Rename(tmp, dest))

	trigger := waitTrigger(t, triggers)
	assert.Equal(t, ReasonChange, trigger.Reason)
}

func TestWatcherIgnoreFileEditProducesReasonIgnoreFile(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	ignorePath := filepath.Join(root, ".drsyncignore")
	require.NoError(t, os.WriteFile(ignorePath, []byte("*.log\n"), 0o600))

	_, triggers := startWatcher(t, Options{Root: root, IgnoreFile: ".drsyncignore"})

	require.NoError(t, os.WriteFile(ignorePath, []byte("*.log\n*.tmp\n"), 0o600))

	trigger := waitTrigger(t, triggers)
	assert.Equal(t, ReasonIgnoreFile, trigger.Reason)
}

func TestWatcherSetMatcherRebuildsExclusions(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	w, triggers := startWatcher(t, Options{Root: root})

	// Before SetMatcher, a new "build" directory is in scope: creating it produces a trigger.
	require.NoError(t, os.Mkdir(filepath.Join(root, "build"), 0o755))

	trigger := waitTrigger(t, triggers)
	assert.Equal(t, ReasonChange, trigger.Reason)

	matcher, err := scan.NewMatcher([]string{"/dist/"})
	require.NoError(t, err)

	w.SetMatcher(matcher)

	// After SetMatcher, creating a newly-excluded directory must neither produce a trigger nor
	// be added to the OS watch, without restarting the watcher.
	require.NoError(t, os.Mkdir(filepath.Join(root, "dist"), 0o755))
	assertNoTrigger(t, triggers, 300*time.Millisecond)

	for _, watched := range w.Watched() {
		assert.NotContains(t, watched, "dist", "an excluded directory must never be watched")
	}
}

func TestWatcherMinIntervalCountsTheInitialTrigger(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	w, err := New(Options{
		Root:        root,
		Debounce:    5 * time.Millisecond,
		MinInterval: 300 * time.Millisecond,
		Warn:        func(msg string) { t.Logf("watcher warning: %s", msg) },
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan struct{})

	go func() {
		defer close(done)

		if err := w.Run(ctx); err != nil {
			t.Logf("watcher stopped: %v", err)
		}
	}()

	t.Cleanup(func() {
		cancel()
		<-done
		require.NoError(t, w.Close())
	})

	triggers := w.Triggers()

	start := time.Now()
	initial := waitTrigger(t, triggers)
	require.Equal(t, ReasonInitial, initial.Reason)

	require.NoError(t, os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o600))

	trigger := waitTrigger(t, triggers)
	assert.Equal(t, ReasonChange, trigger.Reason)
	assert.GreaterOrEqual(t, time.Since(start), 300*time.Millisecond,
		"the initial trigger must count toward --min-interval, exactly like a debounced one")
}

func TestWatcherPollOnlySkipsTheOSWatcherEntirely(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	w, err := New(Options{Root: root, PollOnly: true, PollInterval: 30 * time.Millisecond})
	require.NoError(t, err)

	defer func() { require.NoError(t, w.Close()) }()

	assert.Empty(t, w.Watched(), "poll-only mode must add no OS watches")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan struct{})

	go func() {
		defer close(done)

		if err := w.Run(ctx); err != nil {
			t.Logf("watcher stopped: %v", err)
		}
	}()

	t.Cleanup(func() { <-done })

	initial := waitTrigger(t, w.Triggers())
	require.Equal(t, ReasonInitial, initial.Reason)

	poll := waitTrigger(t, w.Triggers())
	assert.Equal(t, ReasonPoll, poll.Reason)
}

func TestPollIntervalFallsBackAfterResourceExhaustion(t *testing.T) {
	t.Parallel()

	w := &Watcher{opts: Options{PollInterval: 0}, pollOnly: true}

	assert.Equal(t, DefaultPollInterval, w.pollInterval(),
		"an unrequested fallback to poll-only must still poll, even with --poll-interval left at 0")
}

func TestPollIntervalRespectsAnExplicitPollOnlyWithNoInterval(t *testing.T) {
	t.Parallel()

	w := &Watcher{opts: Options{PollInterval: 0, PollOnly: true}, pollOnly: true}

	assert.Zero(t, w.pollInterval(),
		"--poll-only together with --poll-interval 0 is a deliberate fully-inert request")
}

func TestPollIntervalKeepsTheConfiguredValue(t *testing.T) {
	t.Parallel()

	w := &Watcher{opts: Options{PollInterval: 5 * time.Second}}

	assert.Equal(t, 5*time.Second, w.pollInterval())
}

func TestEnsurePollStartsATickerOnceExhaustionFlipsPollOnly(t *testing.T) {
	t.Parallel()

	w := &Watcher{opts: Options{PollInterval: 0}}

	assert.Nil(t, w.ensurePoll(nil), "no fallback is needed while the OS watcher is still working")

	w.pollOnly = true

	poll := w.ensurePoll(nil)
	require.NotNil(t, poll, "resource exhaustion mid-run must start a ticker even if loop began with none")

	defer poll.Stop()

	assert.Same(t, poll, w.ensurePoll(poll), "an existing ticker must never be replaced")
}
