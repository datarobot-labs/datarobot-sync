// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

// Package watcher turns filesystem activity into a coalesced stream of triggers for a resident
// sync loop.
//
// A trigger is only ever a hint. dr-sync keeps no persistent local state (see SPEC.md), so the
// re-scan that follows a trigger re-derives everything from disk and from the remote manifest
// regardless of what the trigger claims changed. A dropped inotify event, a kqueue watch limit,
// or a network filesystem that never raises one at all cost latency here, never correctness —
// which is also why a periodic poll exists as a backstop alongside the OS watcher.
package watcher

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/datarobot/golang-datarobot-filesync/internal/manifest"
	"github.com/datarobot/golang-datarobot-filesync/internal/scan"
)

// Default timing. Chosen so a burst of saves from an editor or a build coalesces into one push,
// while a long watch session still bounds how many generations it can mint.
const (
	DefaultDebounce     = 2 * time.Second
	DefaultMinInterval  = 15 * time.Second
	DefaultPollInterval = 60 * time.Second
)

// Reason explains why a Trigger fired.
type Reason string

const (
	// ReasonInitial fires once, before anything is watched, so a tree that changed while the
	// daemon was down still reconciles at startup.
	ReasonInitial Reason = "initial"
	// ReasonChange fires after a debounced batch of filesystem events.
	ReasonChange Reason = "change"
	// ReasonPoll fires on the periodic safety-net timer, independent of any event.
	ReasonPoll Reason = "poll"
	// ReasonIgnoreFile fires when the ignore file itself changed, so the caller knows to
	// rebuild its matcher before the next scan.
	ReasonIgnoreFile Reason = "ignore-file"
)

// Trigger is one signal that a cycle should run.
type Trigger struct {
	Reason Reason
	// Events is how many raw filesystem events were coalesced into this trigger. Zero for
	// ReasonInitial and ReasonPoll, which are not event-driven.
	Events int
	// Sample is a few of the relative paths that changed, for --verbose output.
	Sample []string
}

// Options configures a Watcher.
type Options struct {
	Root       string
	MetaPrefix string
	// IgnoreFile is the root-relative path a change to which should be reported as
	// ReasonIgnoreFile rather than ReasonChange. Empty disables the special case.
	IgnoreFile string
	// Matcher decides exclusions, exactly as it does for a scan. May be nil.
	Matcher *scan.Matcher

	Debounce     time.Duration
	MinInterval  time.Duration
	PollInterval time.Duration
	// PollOnly skips the OS filesystem watcher entirely, for network filesystems where it
	// never fires, or to avoid touching watch-descriptor limits at all.
	PollOnly bool

	// Warn receives non-fatal problems: a watch-limit fallback, an error from the OS watcher.
	Warn func(string)
}

func (o *Options) applyDefaults() {
	if o.Debounce <= 0 {
		o.Debounce = DefaultDebounce
	}

	if o.MinInterval <= 0 {
		o.MinInterval = DefaultMinInterval
	}

	// PollInterval is deliberately not defaulted here: zero means "disabled", and the CLI is
	// what carries the real default of DefaultPollInterval into an unset flag.
}

// Watcher watches a directory tree and emits coalesced Triggers.
type Watcher struct {
	opts    Options
	metaDir string

	fsw      *fsnotify.Watcher
	pollOnly bool
	watched  map[string]bool

	triggers chan Trigger
	warn     func(string)
}

// New builds a Watcher. It never fails on a watch-descriptor limit: that degrades to polling
// with a warning instead, since a resident daemon should keep running rather than die because
// the OS ran out of inotify or kqueue slots.
func New(opts Options) (*Watcher, error) {
	opts.applyDefaults()

	w := &Watcher{
		opts:     opts,
		metaDir:  metaDirName(opts.MetaPrefix),
		watched:  map[string]bool{},
		triggers: make(chan Trigger),
		warn:     opts.Warn,
	}

	if opts.PollOnly {
		w.pollOnly = true

		return w, nil
	}

	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		if isResourceExhausted(err) {
			w.warnResourceExhausted(err)
			w.pollOnly = true

			return w, nil
		}

		return nil, fmt.Errorf("starting the filesystem watcher: %w", err)
	}

	w.fsw = fsw

	return w, nil
}

// Triggers is the stream of coalesced signals to run a cycle. The channel is unbuffered, so
// Run blocks between triggers until the caller has consumed the previous one — a daemon that
// processes triggers one at a time never has two cycles running together.
func (w *Watcher) Triggers() <-chan Trigger {
	return w.triggers
}

// Watched reports the directories currently under an OS watch, for diagnostics and tests. It
// is empty in poll-only mode.
func (w *Watcher) Watched() []string {
	names := make([]string, 0, len(w.watched))
	for name := range w.watched {
		names = append(names, name)
	}

	return names
}

// Close releases the OS watcher, if one was opened.
func (w *Watcher) Close() error {
	if w.fsw == nil {
		return nil
	}

	return w.fsw.Close()
}

// Run watches the tree and sends Triggers until ctx is done, which is also how it returns.
func (w *Watcher) Run(ctx context.Context) error {
	if !w.pollOnly {
		if err := w.watchTree(w.opts.Root); err != nil {
			return err
		}
	}

	if !w.sendTrigger(ctx, Trigger{Reason: ReasonInitial}) {
		return ctx.Err()
	}

	return w.loop(ctx)
}

func (w *Watcher) loop(ctx context.Context) error {
	debounce := newDebouncer(w.opts.Debounce, w.opts.MinInterval)

	timer := newStoppedTimer()
	defer timer.Stop()

	poll := newOptionalTicker(w.opts.PollInterval)
	if poll != nil {
		defer poll.Stop()
	}

	events, errs := w.eventChannels()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case event, ok := <-events:
			events = w.onEvent(event, ok, events, debounce, timer)

		case err, ok := <-errs:
			errs = w.onWatchError(err, ok, errs)

		case <-tickerChan(poll):
			if err := w.firePoll(ctx); err != nil {
				return err
			}

		case <-timer.C:
			if err := w.fireDebounced(ctx, debounce); err != nil {
				return err
			}
		}
	}
}

// eventChannels returns the OS watcher's channels, or nils in poll-only mode so the
// corresponding select cases simply never fire.
func (w *Watcher) eventChannels() (<-chan fsnotify.Event, <-chan error) {
	if w.fsw == nil {
		return nil, nil
	}

	return w.fsw.Events, w.fsw.Errors
}

// onEvent folds one event into the debouncer and returns the channel to keep selecting on: nil
// once the OS watcher's Events channel has closed, unchanged otherwise.
func (w *Watcher) onEvent(event fsnotify.Event, ok bool, events <-chan fsnotify.Event, debounce *debouncer, timer *time.Timer) <-chan fsnotify.Event {
	if !ok {
		return nil
	}

	w.handleEvent(event, debounce)
	resetTimer(timer, debounce.Deadline())

	return events
}

// onWatchError reports an OS watcher error and returns the channel to keep selecting on, nil
// once it has closed.
func (w *Watcher) onWatchError(err error, ok bool, errs <-chan error) <-chan error {
	if !ok {
		return nil
	}

	w.warnf("watch error: %v", err)

	return errs
}

// firePoll sends the periodic safety-net trigger. A non-nil return means the caller should
// stop: ctx was done while sending.
func (w *Watcher) firePoll(ctx context.Context) error {
	if w.sendTrigger(ctx, Trigger{Reason: ReasonPoll}) {
		return nil
	}

	return ctx.Err()
}

// fireDebounced sends a coalesced batch once it is ready. A non-nil return means the caller
// should stop: ctx was done while sending.
func (w *Watcher) fireDebounced(ctx context.Context, debounce *debouncer) error {
	now := time.Now()
	if !debounce.Ready(now) {
		return nil
	}

	if w.sendTrigger(ctx, debounce.Fire(now)) {
		return nil
	}

	return ctx.Err()
}

// tickerChan returns t.C, or a nil channel (which blocks forever in a select) when t is nil.
func tickerChan(t *time.Ticker) <-chan time.Time {
	if t == nil {
		return nil
	}

	return t.C
}

// newOptionalTicker returns nil for a non-positive interval, so the feature it drives can be
// disabled by passing 0 rather than needing a separate bool.
func newOptionalTicker(d time.Duration) *time.Ticker {
	if d <= 0 {
		return nil
	}

	return time.NewTicker(d)
}

// newStoppedTimer returns a Timer armed against nothing until resetTimer is called on it.
func newStoppedTimer() *time.Timer {
	t := time.NewTimer(time.Hour)
	if !t.Stop() {
		<-t.C
	}

	return t
}

// resetTimer arms t for deadline, draining a stale fire first. A zero deadline leaves it
// stopped.
func resetTimer(t *time.Timer, deadline time.Time) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}

	if deadline.IsZero() {
		return
	}

	t.Reset(max(time.Until(deadline), 0))
}

func (w *Watcher) sendTrigger(ctx context.Context, trigger Trigger) bool {
	select {
	case w.triggers <- trigger:
		return true
	case <-ctx.Done():
		return false
	}
}

// handleEvent folds one OS event into the debouncer: it decides whether the change is in
// scope, whether it is the ignore file, and whether a newly created directory needs its own
// watch before the next event inside it can be seen.
func (w *Watcher) handleEvent(event fsnotify.Event, debounce *debouncer) {
	now := time.Now()

	if w.isIgnoreFile(event.Name) {
		debounce.Event(now, w.relPath(event.Name), true)

		return
	}

	rel := w.relPath(event.Name)
	if rel == "" {
		return
	}

	if event.Op.Has(fsnotify.Remove) || event.Op.Has(fsnotify.Rename) {
		delete(w.watched, event.Name)
	}

	if event.Op.Has(fsnotify.Create) {
		w.watchNewPath(event.Name)
	}

	isDir := false
	if info, err := os.Lstat(event.Name); err == nil {
		isDir = info.IsDir()
	}

	normalized := scan.Normalize(filepath.ToSlash(rel))
	if w.excluded(normalized, isDir) {
		return
	}

	debounce.Event(now, normalized, false)
}

// watchNewPath adds a watch for a path fsnotify just reported as created, if it turned out to
// be a directory dr-sync would otherwise sync.
func (w *Watcher) watchNewPath(path string) {
	if w.pollOnly {
		return
	}

	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() {
		return
	}

	if err := w.watchTree(path); err != nil {
		w.warnf("watching %s: %v", path, err)
	}
}

// watchTree adds a watch for dir and every non-excluded subdirectory beneath it.
func (w *Watcher) watchTree(dir string) error {
	return filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			w.warnf("walking %s: %v", path, err)

			return nil
		}

		if !entry.IsDir() {
			return nil
		}

		rel, relErr := filepath.Rel(w.opts.Root, path)
		if relErr != nil {
			return nil
		}

		if rel != "." {
			normalized := scan.Normalize(filepath.ToSlash(rel))

			if w.excluded(normalized, true) {
				return filepath.SkipDir
			}
		}

		return w.addWatch(path)
	})
}

// addWatch adds one directory watch, degrading to poll-only for the rest of the run if the OS
// has run out of watch descriptors rather than failing the whole daemon.
func (w *Watcher) addWatch(path string) error {
	if w.pollOnly || w.watched[path] {
		return nil
	}

	if err := w.fsw.Add(path); err != nil {
		if isResourceExhausted(err) {
			w.warnResourceExhausted(err)
			w.pollOnly = true

			return nil
		}

		return fmt.Errorf("watching %s: %w", path, err)
	}

	w.watched[path] = true

	return nil
}

// excluded applies the same rules a scan does: dr-sync's own state and the download scratch
// directory are excluded at the root only, everything else defers to the matcher.
func (w *Watcher) excluded(rel string, isDir bool) bool {
	for _, reserved := range [...]string{w.metaDir, scan.TempDirName} {
		if rel == reserved || strings.HasPrefix(rel, reserved+"/") {
			return true
		}
	}

	return w.opts.Matcher != nil && w.opts.Matcher.Match(rel, isDir)
}

func (w *Watcher) isIgnoreFile(absPath string) bool {
	if w.opts.IgnoreFile == "" {
		return false
	}

	return filepath.Clean(absPath) == filepath.Clean(filepath.Join(w.opts.Root, w.opts.IgnoreFile))
}

func (w *Watcher) relPath(absPath string) string {
	rel, err := filepath.Rel(w.opts.Root, absPath)
	if err != nil {
		return ""
	}

	return rel
}

func (w *Watcher) warnf(format string, args ...any) {
	if w.warn == nil {
		return
	}

	w.warn(fmt.Sprintf(format, args...))
}

// isResourceExhausted reports whether err is the OS refusing one more watch: inotify surfaces
// this as ENOSPC (fs.inotify.max_user_watches or max_user_instances), kqueue as EMFILE (the
// open-file limit, since kqueue needs one descriptor per watched path).
func isResourceExhausted(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EMFILE)
}

func (w *Watcher) warnResourceExhausted(err error) {
	w.warnf("ran out of filesystem watches (%v); on Linux raise fs.inotify.max_user_watches "+
		"(sysctl fs.inotify.max_user_watches=124983), on macOS/BSD raise the open-file limit "+
		"(ulimit -n); falling back to periodic polling. Pass --poll-only to skip straight to "+
		"polling next time.", err)
}

// metaDirName returns the reserved directory name without its trailing slash, mirroring how a
// scan decides what its own state directory is called.
func metaDirName(metaPrefix string) string {
	trimmed := strings.TrimSuffix(metaPrefix, "/")
	if trimmed == "" {
		return strings.TrimSuffix(manifest.DefaultMetaPrefix, "/")
	}

	return trimmed
}
