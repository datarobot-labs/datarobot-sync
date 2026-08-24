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
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"

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
	// watchedMu guards watched: every mutation happens on the single goroutine Run starts, but
	// Watched is documented as safe to call from another goroutine while Run is active, so its
	// read needs the same lock.
	watchedMu sync.Mutex
	watched   map[string]bool
	matcher   atomic.Pointer[scan.Matcher]

	// havePushed and lastPushStart record the last trigger this Watcher actually delivered,
	// of any Reason — Initial and Poll included. Both run() and loop() are the single
	// goroutine started by Run, so these need no synchronization of their own.
	havePushed    bool
	lastPushStart time.Time

	// rewatch signals loop's goroutine to re-walk the tree after SetMatcher swaps the
	// matcher: a directory excluded at startup was never added to the OS watcher, and
	// un-excluding it later does not retroactively add one. Buffered so SetMatcher never
	// blocks and back-to-back calls before loop drains it coalesce into a single re-walk.
	rewatch chan struct{}

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
		metaDir:  scan.MetaDirName(opts.MetaPrefix),
		watched:  map[string]bool{},
		rewatch:  make(chan struct{}, 1),
		triggers: make(chan Trigger),
		warn:     opts.Warn,
	}
	w.matcher.Store(opts.Matcher)

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

// SetMatcher swaps the matcher used to decide exclusions and re-walks the tree, so a directory
// excluded at startup — and so never added to the OS watcher — starts being watched once an
// edit to the ignore file un-excludes it. Safe to call concurrently with Run, so a caller can
// rebuild it after a ReasonIgnoreFile trigger without restarting the watcher.
func (w *Watcher) SetMatcher(m *scan.Matcher) {
	w.matcher.Store(m)

	select {
	case w.rewatch <- struct{}{}:
	default:
		// Already one pending re-walk loop has not gotten to yet; a second request before
		// that runs would just re-walk against the same, now-current matcher again.
	}
}

// Watched reports the directories currently under an OS watch, for diagnostics and tests. It
// is empty in poll-only mode. Safe to call concurrently with Run.
func (w *Watcher) Watched() []string {
	w.watchedMu.Lock()
	defer w.watchedMu.Unlock()

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

	state := &loopState{poll: newOptionalTicker(w.pollInterval())}
	defer func() {
		if state.poll != nil {
			state.poll.Stop()
		}
	}()

	state.events, state.errs = w.eventChannels()

	for {
		stop, err := w.tick(ctx, debounce, timer, state)
		if stop {
			return err
		}
	}
}

// loopState is the set of loop-local values one tick can replace: the OS watcher's channels go
// nil once closed, and the poll ticker can start mid-run once resource exhaustion needs it.
type loopState struct {
	events <-chan fsnotify.Event
	errs   <-chan error
	poll   *time.Ticker
}

// tick waits for and dispatches exactly one of the events loop selects on. stop reports
// whether loop should return, with err as its result.
func (w *Watcher) tick(ctx context.Context, debounce *debouncer, timer *time.Timer, state *loopState) (bool, error) {
	select {
	case <-ctx.Done():
		return true, ctx.Err()

	case event, ok := <-state.events:
		state.events = w.onEvent(event, ok, state.events, debounce, timer)
		state.poll = w.ensurePoll(state.poll)

	case err, ok := <-state.errs:
		state.errs = w.onWatchError(err, ok, state.errs)
		state.poll = w.ensurePoll(state.poll)

	case <-w.rewatch:
		w.onRewatch()

	case <-tickerChan(state.poll):
		if err := w.firePoll(ctx, debounce, timer); err != nil {
			return true, err
		}

	case <-timer.C:
		if err := w.fireDebounced(ctx, debounce, timer); err != nil {
			return true, err
		}
	}

	return false, nil
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
	resetTimer(timer, debounce.Deadline(w.lastPushStart, w.havePushed))

	return events
}

// onWatchError reports an OS watcher error and returns the channel to keep selecting on, nil
// once it has closed.
//
// A resource-exhaustion error can arrive here asynchronously — kqueue auto-watching a newly
// created file inside an already-watched directory, for one — not only synchronously from
// addWatch's own fsw.Add call, so it degrades to poll-only exactly the same way that does.
func (w *Watcher) onWatchError(err error, ok bool, errs <-chan error) <-chan error {
	if !ok {
		return nil
	}

	if isResourceExhausted(err) {
		w.warnResourceExhausted(err)
		w.pollOnly = true

		return errs
	}

	w.warnf("watch error: %v", err)

	return errs
}

// onRewatch re-walks the tree after SetMatcher signals that it changed, so a directory
// excluded at startup — and so never added to the OS watcher — gets watched once an
// un-excluding edit reaches here. A no-op in poll-only mode, where addWatch would only walk
// the whole tree to no-op on every directory.
func (w *Watcher) onRewatch() {
	if w.pollOnly {
		return
	}

	if err := w.watchTree(w.opts.Root); err != nil {
		w.warnf("rewatching after the matcher changed: %v", err)
	}
}

// firePoll sends the periodic safety-net trigger and, on success, re-arms the debounce timer:
// sendTrigger can move lastPushStart, which moves the floor a pending debounced batch is
// waiting on, so a stale, earlier deadline must not fire (or leave the timer permanently
// disarmed when it does).
func (w *Watcher) firePoll(ctx context.Context, debounce *debouncer, timer *time.Timer) error {
	if !w.sendTrigger(ctx, Trigger{Reason: ReasonPoll}) {
		return ctx.Err()
	}

	resetTimer(timer, debounce.Deadline(w.lastPushStart, w.havePushed))

	return nil
}

// fireDebounced sends a coalesced batch once it is ready. A non-nil return means the caller
// should stop: ctx was done while sending.
//
// The timer having fired does not mean Ready still holds: something else — a poll delivered in
// between arming and firing, say — can have moved the floor out from under this exact deadline.
// Re-arming against the recomputed one on that path is what keeps the batch from getting stuck
// forever with a stopped timer and nothing left to wake it but a fresh filesystem event.
func (w *Watcher) fireDebounced(ctx context.Context, debounce *debouncer, timer *time.Timer) error {
	now := time.Now()
	if !debounce.Ready(now, w.lastPushStart, w.havePushed) {
		resetTimer(timer, debounce.Deadline(w.lastPushStart, w.havePushed))

		return nil
	}

	if w.sendTrigger(ctx, debounce.Fire()) {
		return nil
	}

	return ctx.Err()
}

// pollInterval is the safety-net ticker period loop should use.
//
// A resource-exhaustion fallback (New's fsnotify.NewWatcher failing, or addWatch losing a watch
// descriptor mid-run) sets pollOnly without the caller ever having asked for --poll-only, and
// the warning it prints promises "falling back to periodic polling" regardless of what
// --poll-interval was set to. Honoring that promise even when --poll-interval was left at its
// 0 "trust the OS watcher" default is the whole point of this override; an explicit
// --poll-only combined with --poll-interval 0 is a deliberate fully-inert request instead, and
// is left alone.
func (w *Watcher) pollInterval() time.Duration {
	if w.opts.PollInterval > 0 {
		return w.opts.PollInterval
	}

	if w.pollOnly && !w.opts.PollOnly {
		return DefaultPollInterval
	}

	return 0
}

// ensurePoll starts the safety-net ticker the moment resource exhaustion switches the watcher
// into poll-only mid-run, for the case loop started with no ticker at all because
// --poll-interval was 0 and nothing had failed yet.
func (w *Watcher) ensurePoll(poll *time.Ticker) *time.Ticker {
	if poll != nil {
		return poll
	}

	return newOptionalTicker(w.pollInterval())
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

// sendTrigger delivers trigger and, once it actually lands with the consumer, records this as
// the last push for --min-interval purposes — for every Reason, not only a debounced one, and
// at delivery time rather than whenever the caller decided to fire, since an unbuffered send
// can sit blocked behind a long-running previous cycle well past that decision.
func (w *Watcher) sendTrigger(ctx context.Context, trigger Trigger) bool {
	select {
	case w.triggers <- trigger:
		w.havePushed = true
		w.lastPushStart = time.Now()

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
		w.unmarkWatched(event.Name)
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
	if w.pollOnly || w.isWatched(path) {
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

	w.markWatched(path)

	return nil
}

// isWatched and markWatched, along with the delete in handleEvent, are the only places watched
// is touched — all guarded by watchedMu so Watched can read it from another goroutine.
func (w *Watcher) isWatched(path string) bool {
	w.watchedMu.Lock()
	defer w.watchedMu.Unlock()

	return w.watched[path]
}

func (w *Watcher) markWatched(path string) {
	w.watchedMu.Lock()
	defer w.watchedMu.Unlock()

	w.watched[path] = true
}

func (w *Watcher) unmarkWatched(path string) {
	w.watchedMu.Lock()
	defer w.watchedMu.Unlock()

	delete(w.watched, path)
}

// excluded applies the same rules a scan does: dr-sync's own state and the download scratch
// directory are excluded at the root only, everything else defers to the matcher.
func (w *Watcher) excluded(rel string, isDir bool) bool {
	for _, reserved := range [...]string{w.metaDir, scan.TempDirName} {
		if rel == reserved || strings.HasPrefix(rel, reserved+"/") {
			return true
		}
	}

	matcher := w.matcher.Load()

	return matcher != nil && matcher.Match(rel, isDir)
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
