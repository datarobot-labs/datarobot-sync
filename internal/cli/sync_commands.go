// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/datarobot/golang-datarobot-filesync/internal/drapi"
	"github.com/datarobot/golang-datarobot-filesync/internal/manifest"
	"github.com/datarobot/golang-datarobot-filesync/internal/packer"
	"github.com/datarobot/golang-datarobot-filesync/internal/progress"
	"github.com/datarobot/golang-datarobot-filesync/internal/syncer"
)

// classify turns a syncer failure into the right exit code, so a caller in CI can react
// without parsing text.
func classify(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled):
		return Fail(CodeInterrupted, err)
	case syncer.IsConflict(err):
		return Fail(CodeConflict, err)
	case syncer.IsStaleManifest(err), packer.IsIntegrityError(err):
		return Fail(CodeIntegrity, err)
	case errors.Is(err, syncer.ErrNoManifest):
		return Fail(CodeUsage, err)
	case errors.Is(err, manifest.ErrUnsupportedVersion):
		return Fail(CodeIntegrity, err)
	case drapi.IsAuth(err):
		return Fail(CodeAPI, err)
	case drapi.StatusOf(err) != 0:
		return Fail(CodeAPI, err)
	default:
		return Fail(CodeInternal, err)
	}
}

// uploadFlags are the push-only options.
type uploadFlags struct {
	common commonFlags

	delete           bool
	followSymlinks   bool
	level            int
	packSize         int64
	packInputMax     int64
	bigFileThreshold int64
	sliceSize        int64
	compressJobs     int
	uploadJobs       int
	tempDir          string
	maxStageFiles    int
	maxStageBytes    int64
	overwrite        string
	keepGenerations  int
	gcGrace          time.Duration
	noGC             bool
	compact          bool
	compactThreshold float64
	noLock           bool
	leaseTTL         time.Duration
	force            bool
}

func uploadCommand() *Command {
	return &Command{
		Name:  "upload",
		Usage: "<path>",
		Short: "Push a directory into a container, transferring only what changed",
		Run:   runUpload,
	}
}

func runUpload(ctx context.Context, env *Env, args []string) error {
	var opts uploadFlags

	set := flag.NewFlagSet("upload", flag.ContinueOnError)
	set.SetOutput(env.Stderr)
	opts.common.register(set)

	set.BoolVar(&opts.delete, "delete", false, "drop paths from the manifest that are no longer local")
	set.BoolVar(&opts.followSymlinks, "follow-symlinks", false, "store what links point at instead of the links")
	set.IntVar(&opts.level, "level", manifest.DefaultLevel, "compression level 0-9; 0 stores without compressing")
	set.Var(bytesValue{&opts.packSize}, "pack-size", "compressed size a pack aims for")
	set.Var(bytesValue{&opts.packInputMax}, "pack-input-max", "uncompressed input cap per pack")
	set.Var(bytesValue{&opts.bigFileThreshold}, "big-file-threshold", "files at least this large get their own pack")
	set.Var(bytesValue{&opts.sliceSize}, "slice-size", "cap on one uploaded object")
	set.IntVar(&opts.compressJobs, "compress-jobs", 0, "packs compressed at once (default: CPUs, capped at 4)")
	set.IntVar(&opts.uploadJobs, "upload-jobs", 0, "objects uploaded at once (default 4)")
	set.StringVar(&opts.tempDir, "tmpdir", "", "where to build packs before uploading them")
	set.IntVar(&opts.maxStageFiles, "max-stage-files", 0, "objects per commit (default 500)")
	set.Var(bytesValue{&opts.maxStageBytes}, "max-stage-bytes", "bytes per commit (default 4GiB)")
	set.StringVar(&opts.overwrite, "overwrite", string(drapi.OverwriteError),
		"what to do if a name already exists: error or replace")
	set.IntVar(&opts.keepGenerations, "keep-generations", 0, "manifests to retain (default 5)")
	set.DurationVar(&opts.gcGrace, "gc-grace", 0, "leave unreferenced objects alone for this long (default 6h)")
	set.BoolVar(&opts.noGC, "no-gc", false, "do not delete unreferenced objects after committing")
	set.BoolVar(&opts.compact, "compact", false, "rewrite sparse packs after pushing")
	set.Float64Var(&opts.compactThreshold, "compact-threshold", syncer.DefaultCompactThreshold,
		"live fraction below which a pack is worth rewriting")
	set.BoolVar(&opts.noLock, "no-lock", false, "do not take an advisory lease")
	set.DurationVar(&opts.leaseTTL, "lease-ttl", 0, "how long the lease is honoured (default 30m)")
	set.BoolVar(&opts.force, "force", false, "ignore a foreign lease and a moved head")

	if err := set.Parse(args); err != nil {
		return err
	}

	root, err := resolveRoot(set.Args())
	if err != nil {
		return err
	}

	session, err := opts.common.open(env, set, true, root)
	if err != nil {
		return err
	}

	defer session.reporter.Close()

	ctx, cancel := opts.common.withTimeout(ctx)
	defer cancel()

	overwrite, err := parseOverwrite(opts.overwrite)
	if err != nil {
		return err
	}

	result, err := syncer.Upload(ctx, session.store, session.reporter, syncer.UploadOptions{
		Root:             root,
		Check:            session.check,
		Matcher:          session.matcher,
		Delete:           opts.delete,
		FollowSymlinks:   opts.followSymlinks,
		Level:            opts.level,
		PackInputMax:     opts.packInputMax,
		BigFileThreshold: opts.bigFileThreshold,
		SliceSize:        opts.sliceSize,
		Jobs:             opts.common.jobs,
		CompressJobs:     opts.compressJobs,
		UploadJobs:       opts.uploadJobs,
		TempDir:          opts.tempDir,
		MaxStageFiles:    opts.maxStageFiles,
		MaxStageBytes:    opts.maxStageBytes,
		Overwrite:        overwrite,
		KeepGenerations:  opts.keepGenerations,
		GCGrace:          opts.gcGrace,
		NoGC:             opts.noGC,
		CompactThreshold: opts.compactThreshold,
		Lock:             !opts.noLock,
		LeaseTTL:         opts.leaseTTL,
		Force:            opts.force,
		DryRun:           opts.common.dryRun,
	})
	if err != nil {
		return classify(err)
	}

	reportUpload(session.reporter, result)

	if !opts.compact || result.DryRun {
		return nil
	}

	return runCompaction(ctx, session, compactSettings{
		root:             root,
		threshold:        opts.compactThreshold,
		level:            opts.level,
		packInputMax:     opts.packInputMax,
		bigFileThreshold: opts.bigFileThreshold,
		sliceSize:        opts.sliceSize,
		jobs:             opts.common.jobs,
		uploadJobs:       opts.uploadJobs,
		tempDir:          opts.tempDir,
		maxStageFiles:    opts.maxStageFiles,
		maxStageBytes:    opts.maxStageBytes,
		gcGrace:          opts.gcGrace,
		lock:             !opts.noLock,
		leaseTTL:         opts.leaseTTL,
		force:            opts.force,
	})
}

func parseOverwrite(value string) (drapi.OverwriteStrategy, error) {
	switch drapi.OverwriteStrategy(value) {
	case drapi.OverwriteError, drapi.OverwriteReplace:
		return drapi.OverwriteStrategy(value), nil

	case drapi.OverwriteRename, drapi.OverwriteSkip:
		// The API offers these, but both would leave a pack under a name no manifest
		// references: rename stores it as "p_x (1).tar.xz", skip silently keeps whatever was
		// already there. Either way the manifest points at content that is not where it says.
		return "", Failf(CodeUsage,
			"--overwrite=%s would store a pack under a name the manifest cannot find; want error or replace",
			value)

	default:
		return "", Failf(CodeUsage, "unknown --overwrite %q: want error or replace", value)
	}
}

func reportUpload(reporter *progress.Reporter, result *syncer.UploadResult) {
	if result.Manifest == nil {
		return
	}

	summary := fmt.Sprintf("%s  %s uploaded", result.Manifest.ID, progress.HumanBytes(result.BytesUploaded))

	if result.BytesCompressed > 0 && result.BytesUploaded > 0 {
		summary += fmt.Sprintf(" (%.1fx from %s)",
			float64(result.BytesCompressed)/float64(result.BytesUploaded),
			progress.HumanBytes(result.BytesCompressed))
	}

	if result.PacksSkipped > 0 {
		summary += fmt.Sprintf(", %d packs already present", result.PacksSkipped)
	}

	reporter.Phase("done", summary)

	reporter.Event("upload", map[string]any{
		"manifest":      result.Manifest.ID,
		"generation":    result.Manifest.Generation,
		"packsBuilt":    result.PacksBuilt,
		"packsSkipped":  result.PacksSkipped,
		"bytesUploaded": result.BytesUploaded,
		"filesTotal":    result.Manifest.Stats.FileCount,
		"objectsPruned": result.Deleted,
	})

	if result.CompactHint != "" {
		reporter.Hint(result.CompactHint)
	}
}
