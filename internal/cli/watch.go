// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/datarobot/golang-datarobot-filesync/internal/drapi"
	"github.com/datarobot/golang-datarobot-filesync/internal/manifest"
	"github.com/datarobot/golang-datarobot-filesync/internal/syncer"
	"github.com/datarobot/golang-datarobot-filesync/internal/watcher"
)

// DefaultWatchGCInterval is how often watch sweeps tier-1 garbage, independent of any push.
//
// Skipping GC on every push, unlike upload, is deliberate: tier-1 GC is the expensive tail of a
// commit — a full listing plus up to KeepGenerations manifest downloads — and a daemon pushing
// every few seconds would pay that far more often than it needs to.
const DefaultWatchGCInterval = 10 * time.Minute

// watchFlags are watch's own options, plus the upload options it shares with `upload`.
type watchFlags struct {
	upload uploadFlags

	debounce     time.Duration
	minInterval  time.Duration
	pollInterval time.Duration
	pollOnly     bool
	gcInterval   time.Duration
	writerTTL    time.Duration
}

func newWatchCommand(global *globalFlags) *cobra.Command {
	opts := &watchFlags{}

	cmd := &cobra.Command{
		Use:   "watch <path>",
		Short: "Keep a container mirroring a directory as it changes",
		Long: "Watch a directory and push it continuously.\n\n" +
			"A filesystem event is only a hint that something changed: every cycle re-walks and\n" +
			"re-hashes the tree exactly as `upload` does, so a missed or dropped event costs\n" +
			"latency, never correctness. Unlike `upload`, --delete defaults on here: watch mirrors\n" +
			"the local tree rather than only adding to it.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWatch(cmd, args, global, opts)
		},
	}

	opts.upload.register(cmd, true)

	flags := cmd.Flags()
	flags.DurationVar(&opts.debounce, "debounce", watcher.DefaultDebounce,
		"quiet period after the last change before pushing")
	flags.DurationVar(&opts.minInterval, "min-interval", watcher.DefaultMinInterval,
		"floor between the start of two pushes")
	flags.DurationVar(&opts.pollInterval, "poll-interval", watcher.DefaultPollInterval,
		"safety-net rescan on a timer, independent of filesystem events; 0 disables it")
	flags.BoolVar(&opts.pollOnly, "poll-only", false,
		"skip the OS filesystem watcher and rely on --poll-interval alone")
	flags.DurationVar(&opts.gcInterval, "gc-interval", DefaultWatchGCInterval,
		"how often unreferenced objects are collected; 0 disables it")
	flags.DurationVar(&opts.writerTTL, "writer-ttl", syncer.DefaultWriterTTL,
		"how long this process's entry in the writer registry is honoured")

	return cmd
}

func runWatch(cmd *cobra.Command, args []string, global *globalFlags, opts *watchFlags) error {
	root, err := syncRoot(args)
	if err != nil {
		return err
	}

	session, err := global.open(cmd, true, root)
	if err != nil {
		return err
	}

	defer session.reporter.Close()

	overwrite, err := parseOverwrite(opts.upload.overwrite)
	if err != nil {
		return err
	}

	clientID, err := manifest.NewClientID()
	if err != nil {
		return Fail(CodeInternal, err)
	}

	ctx := cmd.Context()

	registration, err := registerWatchWriter(ctx, global, session, clientID, root, opts.writerTTL)
	if err != nil {
		return classify(err)
	}

	w, err := watcher.New(watcher.Options{
		Root:         root,
		MetaPrefix:   global.metaPrefix,
		IgnoreFile:   global.ignoreFile,
		Matcher:      session.matcher,
		Debounce:     opts.debounce,
		MinInterval:  opts.minInterval,
		PollInterval: opts.pollInterval,
		PollOnly:     opts.pollOnly,
		Warn:         session.reporter.Warn,
	})
	if err != nil {
		if deregErr := registration.Deregister(ctx); deregErr != nil {
			session.reporter.Warn(deregErr.Error())
		}

		return Fail(CodeUsage, err)
	}

	defer w.Close()

	cycle := &watchCycle{
		session:   session,
		global:    global,
		opts:      opts,
		root:      root,
		clientID:  clientID,
		overwrite: overwrite,
		watcher:   w,
		warned:    map[string]bool{},
	}

	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error { return w.Run(groupCtx) })
	group.Go(func() error { return cycle.loop(groupCtx, w.Triggers(), registration) })

	runErr := group.Wait()

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	cycle.gcSweep(shutdownCtx)

	if deregErr := registration.Deregister(shutdownCtx); deregErr != nil {
		session.reporter.Warn(deregErr.Error())
	}

	if runErr != nil {
		return classify(runErr)
	}

	return nil
}

// registerWatchWriter announces this process's presence on the container, unless --dry-run
// promised nothing would be written: a registration would otherwise outlive the run in the
// writer registry even though the run itself changed nothing.
func registerWatchWriter(ctx context.Context, global *globalFlags, session *session, clientID, root string, ttl time.Duration) (*syncer.WriterRegistration, error) {
	if global.dryRun {
		return nil, nil
	}

	return syncer.RegisterWriter(ctx, session.store, clientID, "watch", root, ttl)
}

// watchCycle holds what one cycle needs across the life of the daemon: the session, the static
// options, and the head/generation this process last saw so it can tell whether the container
// moved out from under it.
type watchCycle struct {
	session   *session
	global    *globalFlags
	opts      *watchFlags
	root      string
	clientID  string
	overwrite drapi.OverwriteStrategy
	watcher   *watcher.Watcher

	lastHeadID     string
	lastGeneration int64
	lastManifestID string
	lastHint       time.Time
	warned         map[string]bool
}

// loop drives cycles from triggers, refreshes this process's writer registration, and sweeps
// garbage on a timer, all independent of one another.
func (c *watchCycle) loop(ctx context.Context, triggers <-chan watcher.Trigger, registration *syncer.WriterRegistration) error {
	refresh := time.NewTicker(writerRefreshInterval(c.opts.writerTTL))
	defer refresh.Stop()

	sweep := newOptionalTicker(c.opts.gcInterval)
	if sweep != nil {
		defer sweep.Stop()
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-refresh.C:
			c.refreshWriter(ctx, registration)

		case <-tickerChan(sweep):
			c.gcSweep(ctx)

		case trigger, ok := <-triggers:
			if stop, err := c.onTrigger(ctx, trigger, ok); stop {
				return err
			}
		}
	}
}

// onTrigger runs one cycle for a trigger. stop reports whether the daemon should shut down:
// true with a nil err means the trigger stream closed cleanly, true with a non-nil err means
// the cycle failed in a way that is not worth retrying.
func (c *watchCycle) onTrigger(ctx context.Context, trigger watcher.Trigger, ok bool) (stop bool, err error) {
	if !ok {
		return true, nil
	}

	if err := c.run(ctx, trigger); err != nil {
		return true, err
	}

	return false, nil
}

func (c *watchCycle) refreshWriter(ctx context.Context, registration *syncer.WriterRegistration) {
	if registration == nil {
		return
	}

	if err := registration.Refresh(ctx, c.lastGeneration, c.lastManifestID); err != nil {
		c.session.reporter.Warn("refreshing the writer registration: " + err.Error())
	}
}

// writerRefreshInterval is a third of the TTL, so a registration is renewed well before it
// could lapse even if one refresh is delayed. Floored at one second: a --writer-ttl under
// three seconds would otherwise floor-divide to zero, and time.NewTicker(0) panics.
func writerRefreshInterval(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		ttl = syncer.DefaultWriterTTL
	}

	if interval := ttl / 3; interval >= time.Second {
		return interval
	}

	return time.Second
}

// newOptionalTicker returns nil for a non-positive interval, so the feature it drives can be
// disabled by passing 0 rather than needing a separate bool.
func newOptionalTicker(d time.Duration) *time.Ticker {
	if d <= 0 {
		return nil
	}

	return time.NewTicker(d)
}

func tickerChan(t *time.Ticker) <-chan time.Time {
	if t == nil {
		return nil
	}

	return t.C
}

// run executes one cycle: read state, decide whether the remote is dirty, push, and record
// what landed.
func (c *watchCycle) run(ctx context.Context, trigger watcher.Trigger) error {
	cycleCtx, cancel := c.global.withTimeout(ctx)
	defer cancel()

	session := c.session

	if trigger.Reason == watcher.ReasonIgnoreFile {
		c.rebuildMatcher()
	}

	session.reporter.Phase("watch", fmt.Sprintf("%s (%d events)", trigger.Reason, trigger.Events))

	state, err := session.store.ReadState(cycleCtx)
	if err != nil {
		return c.handleCycleError(classify(err))
	}

	dirty := c.dirty(state)
	c.warnForeignWriters(state)

	deleteThisCycle := c.opts.upload.delete
	if deleteThisCycle && dirty && !c.opts.upload.force {
		deleteThisCycle = false

		session.reporter.Warn("the container changed since this process last saw it; " +
			"holding back --delete this cycle so it does not race another writer's paths " +
			"(pass --force to override)")
	}

	result, err := syncer.Upload(cycleCtx, session.store, session.reporter, syncer.UploadOptions{
		Root:             c.root,
		Check:            session.check,
		Matcher:          session.matcher,
		Delete:           deleteThisCycle,
		FollowSymlinks:   c.opts.upload.followSymlinks,
		Level:            c.opts.upload.level,
		PackInputMax:     c.opts.upload.packInputMax,
		BigFileThreshold: c.opts.upload.bigFileThreshold,
		SliceSize:        c.opts.upload.sliceSize,
		Jobs:             c.global.jobs,
		CompressJobs:     c.opts.upload.compressJobs,
		UploadJobs:       c.opts.upload.uploadJobs,
		TempDir:          c.opts.upload.tempDir,
		MaxStageFiles:    c.opts.upload.maxStageFiles,
		MaxStageBytes:    c.opts.upload.maxStageBytes,
		Overwrite:        c.overwrite,
		KeepGenerations:  c.opts.upload.keepGenerations,
		GCGrace:          c.opts.upload.gcGrace,
		NoGC:             true,
		CompactThreshold: c.opts.upload.compactThreshold,
		Lock:             !c.opts.upload.noLock,
		LeaseTTL:         c.opts.upload.leaseTTL,
		Force:            c.opts.upload.force,
		ClientID:         c.clientID,
		DryRun:           c.global.dryRun,
	})
	if err != nil {
		return c.handleCycleError(classify(err))
	}

	reportUpload(session.reporter, result)

	session.reporter.Event("watch", map[string]any{
		"reason": string(trigger.Reason),
		"events": trigger.Events,
		"dirty":  dirty,
	})

	// A no-op cycle commits nothing, but the head it saw is still this process's baseline:
	// without falling back to it here, an already-in-sync tree never sets lastHeadID, so
	// dirty() can never fire and the --delete holdback never arms.
	head, haveHead := manifest.SelectHead(state.Manifests)

	switch {
	case result.Manifest != nil:
		c.lastHeadID = result.Manifest.ID
		c.lastGeneration = result.Manifest.Generation
		c.lastManifestID = result.Manifest.ID

	case haveHead:
		c.lastHeadID = head.ID
		c.lastGeneration = head.Generation
		c.lastManifestID = head.ID
	}

	c.reportHint(result.CompactHint)

	if err := c.maybeCompact(cycleCtx, result); err != nil {
		return c.handleCycleError(err)
	}

	return nil
}

// maybeCompact rewrites sparse packs after a cycle when --compact was requested, exactly as
// `upload --compact` does after its one push. A manifest it commits becomes this process's new
// baseline, or the next cycle would see its own compaction as a foreign write and needlessly
// hold back --delete.
func (c *watchCycle) maybeCompact(ctx context.Context, uploaded *syncer.UploadResult) error {
	if !c.opts.upload.compact || uploaded.DryRun {
		return nil
	}

	result, err := runCompaction(ctx, c.session, compactSettings{
		root:             c.root,
		threshold:        c.opts.upload.compactThreshold,
		level:            c.opts.upload.level,
		packInputMax:     c.opts.upload.packInputMax,
		bigFileThreshold: c.opts.upload.bigFileThreshold,
		sliceSize:        c.opts.upload.sliceSize,
		jobs:             c.global.jobs,
		uploadJobs:       c.opts.upload.uploadJobs,
		tempDir:          c.opts.upload.tempDir,
		maxStageFiles:    c.opts.upload.maxStageFiles,
		maxStageBytes:    c.opts.upload.maxStageBytes,
		gcGrace:          c.opts.upload.gcGrace,
		lock:             !c.opts.upload.noLock,
		leaseTTL:         c.opts.upload.leaseTTL,
		force:            c.opts.upload.force,
	})
	if err != nil {
		return err
	}

	if result.Manifest != nil {
		c.lastHeadID = result.Manifest.ID
		c.lastGeneration = result.Manifest.Generation
		c.lastManifestID = result.Manifest.ID
	}

	return nil
}

// rebuildMatcher re-reads the ignore file and --exclude-from files after a ReasonIgnoreFile
// trigger, so an edit to them takes effect on the very next scan and the very next filesystem
// event, instead of only after the daemon restarts.
func (c *watchCycle) rebuildMatcher() {
	matcher, err := c.global.buildMatcher(c.root)
	if err != nil {
		c.session.reporter.Warn("rebuilding the ignore matcher: " + err.Error())

		return
	}

	c.session.matcher = matcher
	c.watcher.SetMatcher(matcher)
}

// handleCycleError decides whether a failed cycle should stop the daemon.
//
// A conflict is the documented "someone else committed meanwhile" race, an API failure is
// presumed transient, and a cycle timeout is documented to bound one cycle, not the daemon
// (SPEC.md) — all three heal by simply trying again on the next trigger, and the retry is
// nearly free because packs are already uploaded and content-addressed. Everything else — a
// bad flag, a corrupt container, a shutdown request, or a bug — is not something looping again
// can fix, so it stops the daemon rather than retrying blindly.
func (c *watchCycle) handleCycleError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		c.session.reporter.Warn(err.Error())

		return nil
	}

	switch CodeFor(err) {
	case CodeConflict, CodeAPI:
		c.session.reporter.Warn(err.Error())

		return nil

	default:
		return err
	}
}

// dirty reports whether the container moved since this process last saw it: another writer
// committed a generation, or a fork was left unresolved.
func (c *watchCycle) dirty(state *syncer.State) bool {
	if head, ok := manifest.SelectHead(state.Manifests); ok && c.lastHeadID != "" && head.ID != c.lastHeadID {
		return true
	}

	_, forked := manifest.DetectFork(state.Manifests)

	return forked
}

// warnForeignWriters announces a peer the first time it is seen, and stays quiet about it
// after that so a long-running two-writer session does not fill the log.
func (c *watchCycle) warnForeignWriters(state *syncer.State) {
	for _, writer := range syncer.ForeignWriters(state.Writers, c.clientID, time.Now()) {
		if c.warned[writer.ClientID] {
			continue
		}

		c.warned[writer.ClientID] = true

		c.session.reporter.Warn("another writer is active on this container: " + writer.Describe())
	}
}

// reportHint surfaces a compaction hint at most once per garbage-sweep interval, so the same
// advice does not repeat every cycle.
func (c *watchCycle) reportHint(hint string) {
	if hint == "" {
		return
	}

	interval := c.opts.gcInterval
	if interval <= 0 {
		interval = DefaultWatchGCInterval
	}

	if time.Since(c.lastHint) < interval {
		return
	}

	c.lastHint = time.Now()

	c.session.reporter.Hint(hint)
}

// gcSweep runs one tier-1 garbage collection pass, independent of any push. It refuses on a
// fork exactly as `upload` and `gc` do: the losing manifest may be the only reference to packs
// that would otherwise look unreferenced.
func (c *watchCycle) gcSweep(ctx context.Context) {
	if c.global.dryRun {
		return
	}

	session := c.session

	state, err := session.store.ReadState(ctx)
	if err != nil {
		session.reporter.Warn("skipping the garbage sweep: " + err.Error())

		return
	}

	if fork, forked := manifest.DetectFork(state.Manifests); forked {
		session.reporter.Warn(fmt.Sprintf(
			"two manifests exist at generation %d; skipping the garbage sweep until that is resolved",
			fork.Generation))

		return
	}

	deleted, err := syncer.Collect(ctx, session.store, state, syncer.CollectOptions{
		KeepGenerations: c.opts.upload.keepGenerations,
		Grace:           c.opts.upload.gcGrace,
	})
	if err != nil {
		session.reporter.Warn("the garbage sweep did not complete: " + err.Error())

		return
	}

	if deleted > 0 {
		session.reporter.Phase("gc", fmt.Sprintf("%d unreferenced objects removed", deleted))
	}
}
