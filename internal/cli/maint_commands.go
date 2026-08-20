// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

package cli

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/datarobot/golang-datarobot-filesync/internal/manifest"
	"github.com/datarobot/golang-datarobot-filesync/internal/progress"
	"github.com/datarobot/golang-datarobot-filesync/internal/syncer"
)

// compactSettings carries the knobs a compaction needs, whether it was asked for directly or
// tacked onto an upload.
type compactSettings struct {
	root             string
	threshold        float64
	level            int
	packInputMax     int64
	bigFileThreshold int64
	sliceSize        int64
	jobs             int
	uploadJobs       int
	tempDir          string
	maxStageFiles    int
	maxStageBytes    int64
	keepGenerations  int
	gcGrace          time.Duration
	lock             bool
	leaseTTL         time.Duration
	force            bool
	dryRun           bool
}

func runCompaction(ctx context.Context, session *session, settings compactSettings) error {
	result, err := syncer.Compact(ctx, session.store, session.reporter, syncer.CompactOptions{
		Root:             settings.root,
		Check:            session.check,
		Matcher:          session.matcher,
		Threshold:        settings.threshold,
		Level:            settings.level,
		PackInputMax:     settings.packInputMax,
		BigFileThreshold: settings.bigFileThreshold,
		SliceSize:        settings.sliceSize,
		Jobs:             settings.jobs,
		UploadJobs:       settings.uploadJobs,
		TempDir:          settings.tempDir,
		MaxStageFiles:    settings.maxStageFiles,
		MaxStageBytes:    settings.maxStageBytes,
		KeepGenerations:  settings.keepGenerations,
		GCGrace:          settings.gcGrace,
		Lock:             settings.lock,
		LeaseTTL:         settings.leaseTTL,
		Force:            settings.force,
		DryRun:           settings.dryRun,
	})
	if err != nil {
		return classify(err)
	}

	if result.Manifest == nil {
		return nil
	}

	session.reporter.Phase("done", fmt.Sprintf("%s  %d packs rewritten  ~%s reclaimed",
		result.Manifest.ID, len(result.Rewritten), progress.HumanBytes(result.Reclaimed)))

	session.reporter.Event("compact", map[string]any{
		"manifest":       result.Manifest.ID,
		"packsRewritten": len(result.Rewritten),
		"blobsRepacked":  result.Repacked,
		"bytesUploaded":  result.BytesUploaded,
		"objectsDeleted": result.Deleted,
	})

	return nil
}

func compactCommand() *Command {
	return &Command{
		Name:  "compact",
		Usage: "<path>",
		Short: "Rewrite sparse packs so their dead weight can be reclaimed",
		Run:   runCompact,
	}
}

func runCompact(ctx context.Context, env *Env, args []string) error {
	var (
		common          commonFlags
		threshold       float64
		level           int
		packInputMax    int64
		bigFile         int64
		sliceSize       int64
		uploadJobs      int
		tempDir         string
		keepGenerations int
		gcGrace         time.Duration
		noLock          bool
		leaseTTL        time.Duration
		force           bool
	)

	set := flag.NewFlagSet("compact", flag.ContinueOnError)
	set.SetOutput(env.Stderr)
	common.register(set)

	set.Float64Var(&threshold, "compact-threshold", syncer.DefaultCompactThreshold,
		"live fraction below which a pack is rewritten")
	set.IntVar(&level, "level", manifest.DefaultLevel, "compression level for the fresh packs")
	set.Var(bytesValue{&packInputMax}, "pack-input-max", "uncompressed input cap per pack")
	set.Var(bytesValue{&bigFile}, "big-file-threshold", "files at least this large get their own pack")
	set.Var(bytesValue{&sliceSize}, "slice-size", "cap on one uploaded object")
	set.IntVar(&uploadJobs, "upload-jobs", 0, "objects uploaded at once")
	set.StringVar(&tempDir, "tmpdir", "", "where to build packs before uploading them")
	set.IntVar(&keepGenerations, "keep-generations", 0, "manifests to retain (default 1 after a compaction)")
	set.DurationVar(&gcGrace, "gc-grace", 0, "leave unreferenced objects alone for this long")
	set.BoolVar(&noLock, "no-lock", false, "do not take an advisory lease")
	set.DurationVar(&leaseTTL, "lease-ttl", 0, "how long the lease is honoured")
	set.BoolVar(&force, "force", false, "ignore a foreign lease")

	if err := set.Parse(args); err != nil {
		return err
	}

	root, err := resolveRoot(set.Args())
	if err != nil {
		return err
	}

	session, err := common.open(env, set, true, root)
	if err != nil {
		return err
	}

	defer session.reporter.Close()

	ctx, cancel := common.withTimeout(ctx)
	defer cancel()

	return runCompaction(ctx, session, compactSettings{
		root:             root,
		threshold:        threshold,
		level:            level,
		packInputMax:     packInputMax,
		bigFileThreshold: bigFile,
		sliceSize:        sliceSize,
		jobs:             common.jobs,
		uploadJobs:       uploadJobs,
		tempDir:          tempDir,
		keepGenerations:  keepGenerations,
		gcGrace:          gcGrace,
		lock:             !noLock,
		leaseTTL:         leaseTTL,
		force:            force,
		dryRun:           common.dryRun,
	})
}

func gcCommand() *Command {
	return &Command{
		Name:  "gc",
		Short: "Delete objects no retained manifest references (moves no data)",
		Run:   runGC,
	}
}

func runGC(ctx context.Context, env *Env, args []string) error {
	var (
		common          commonFlags
		keepGenerations int
		gcGrace         time.Duration
	)

	set := flag.NewFlagSet("gc", flag.ContinueOnError)
	set.SetOutput(env.Stderr)
	common.register(set)

	set.IntVar(&keepGenerations, "keep-generations", 0, "manifests to retain (default 5)")
	set.DurationVar(&gcGrace, "gc-grace", 0, "leave unreferenced objects alone for this long (default 6h)")

	if err := set.Parse(args); err != nil {
		return err
	}

	if set.NArg() > 0 {
		return Failf(CodeUsage, "gc takes no path argument; it works entirely from the container")
	}

	session, err := common.open(env, set, true, "")
	if err != nil {
		return err
	}

	defer session.reporter.Close()

	ctx, cancel := common.withTimeout(ctx)
	defer cancel()

	plan, err := planGC(ctx, session, keepGenerations, gcGrace)
	if err != nil {
		return err
	}

	session.reporter.Phase("plan", fmt.Sprintf("%d manifests, %d packs (%s), %d stale leases",
		len(plan.Manifests), len(plan.Packs), progress.HumanBytes(plan.Bytes), len(plan.Leases)))

	for _, name := range plan.Names() {
		session.reporter.Note("delete " + name)
	}

	if plan.Empty() {
		session.reporter.Phase("done", "nothing to collect")

		return nil
	}

	if common.dryRun {
		session.reporter.Phase("done", "dry run: nothing was deleted")

		return nil
	}

	deleted, err := session.store.Delete(ctx, plan.Names())
	if err != nil {
		return classify(err)
	}

	session.reporter.Phase("done", fmt.Sprintf("%d objects deleted, ~%s reclaimed",
		deleted, progress.HumanBytes(plan.Bytes)))

	session.reporter.Event("gc", map[string]any{
		"objectsDeleted": deleted,
		"bytesReclaimed": plan.Bytes,
	})

	return nil
}

// planGC reads the container and decides what can go, refusing outright on a fork.
func planGC(ctx context.Context, session *session, keepGenerations int, grace time.Duration) (*syncer.CollectPlan, error) {
	state, err := session.store.ReadState(ctx)
	if err != nil {
		return nil, classify(err)
	}

	if fork, forked := manifest.DetectFork(state.Manifests); forked {
		// The losing manifest may be the only thing keeping some packs alive.
		return nil, Failf(CodeConflict,
			"two manifests exist at generation %d; resolve that before collecting garbage", fork.Generation)
	}

	plan, err := syncer.PlanCollect(ctx, session.store, state, syncer.CollectOptions{
		KeepGenerations: keepGenerations,
		Grace:           grace,
		Now:             time.Now(),
	})
	if err != nil {
		return nil, classify(err)
	}

	return plan, nil
}
