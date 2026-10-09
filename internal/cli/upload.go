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

package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

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
	case errors.Is(err, syncer.ErrNoManifest), errors.Is(err, syncer.ErrInvalidTag):
		return Fail(CodeUsage, err)
	case errors.Is(err, manifest.ErrUnsupportedVersion):
		return Fail(CodeIntegrity, err)
	case drapi.StatusOf(err) != 0:
		return Fail(CodeAPI, err)
	default:
		return Fail(CodeInternal, err)
	}
}

// uploadFlags are the push-only options.
type uploadFlags struct {
	delete           bool
	followSymlinks   bool
	level            int
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

// register adds the shared upload flags. deleteDefault lets a caller flip what --delete
// defaults to: off for a one-shot upload, which stays additive unless asked otherwise, on for
// watch, which is expected to mirror the local tree.
func (u *uploadFlags) register(cmd *cobra.Command, deleteDefault bool) {
	flags := cmd.Flags()

	flags.BoolVar(&u.delete, "delete", deleteDefault, "drop paths from the manifest that are no longer local")
	flags.BoolVar(&u.followSymlinks, "follow-symlinks", false,
		"store what links point at instead of the links")
	flags.IntVar(&u.level, "level", manifest.DefaultLevel,
		"compression level 0-9; 0 stores without compressing")
	flags.Var(bytesFlag{&u.packInputMax}, "pack-input-max", "uncompressed input cap per pack")
	flags.Var(bytesFlag{&u.bigFileThreshold}, "big-file-threshold",
		"files at least this large get their own pack")
	flags.Var(bytesFlag{&u.sliceSize}, "slice-size", "cap on one uploaded object")
	flags.IntVar(&u.compressJobs, "compress-jobs", 0, "packs compressed at once (default: CPUs, capped at 4)")
	flags.IntVar(&u.uploadJobs, "upload-jobs", 0, "objects uploaded at once (default 4)")
	flags.StringVar(&u.tempDir, "tmpdir", "", "where to build packs before uploading them")
	flags.IntVar(&u.maxStageFiles, "max-stage-files", 0, "objects per commit (default 500)")
	flags.Var(bytesFlag{&u.maxStageBytes}, "max-stage-bytes", "bytes per commit (default 4GiB)")
	flags.StringVar(&u.overwrite, "overwrite", string(drapi.OverwriteError),
		"what to do if a name already exists: error or replace")
	flags.IntVar(&u.keepGenerations, "keep-generations", 0, "manifests to retain (default 5)")
	flags.DurationVar(&u.gcGrace, "gc-grace", 0,
		"leave unreferenced objects alone for this long (default 6h)")
	flags.BoolVar(&u.noGC, "no-gc", false, "do not delete unreferenced objects after committing")
	flags.BoolVar(&u.compact, "compact", false, "rewrite sparse packs after pushing")
	flags.Float64Var(&u.compactThreshold, "compact-threshold", syncer.DefaultCompactThreshold,
		"live fraction below which a pack is worth rewriting")
	flags.BoolVar(&u.noLock, "no-lock", false, "do not take an advisory lease")
	flags.DurationVar(&u.leaseTTL, "lease-ttl", 0, "how long the lease is honoured (default 30m)")
	flags.BoolVar(&u.force, "force", false, "ignore a foreign lease and a moved head")
}

func newUploadCommand(global *globalFlags) *cobra.Command {
	opts := &uploadFlags{}

	cmd := &cobra.Command{
		Use:   "upload <path>",
		Short: "Push a directory into a container, transferring only what changed",
		Long: "Push a directory into a container.\n\n" +
			"Only content the container does not already hold is transferred, so a rename or a\n" +
			"revert costs nothing. Without --delete the push is additive: paths that have gone\n" +
			"from the local tree stay in the manifest.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUpload(cmd, args, global, opts)
		},
	}

	opts.register(cmd, false)

	return cmd
}

func runUpload(cmd *cobra.Command, args []string, global *globalFlags, opts *uploadFlags) error {
	root, err := syncRoot(args)
	if err != nil {
		return err
	}

	session, err := global.open(cmd, true, root)
	if err != nil {
		return err
	}

	defer session.reporter.Close()

	ctx, cancel := global.withTimeout(cmd.Context())
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
		Jobs:             global.jobs,
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
		DryRun:           global.dryRun,
	})
	if err != nil {
		return classify(err)
	}

	reportUpload(session.reporter, result)

	if !opts.compact || result.DryRun {
		return nil
	}

	_, err = runCompaction(ctx, session, compactSettings{
		root:             root,
		threshold:        opts.compactThreshold,
		level:            opts.level,
		packInputMax:     opts.packInputMax,
		bigFileThreshold: opts.bigFileThreshold,
		sliceSize:        opts.sliceSize,
		jobs:             global.jobs,
		uploadJobs:       opts.uploadJobs,
		tempDir:          opts.tempDir,
		maxStageFiles:    opts.maxStageFiles,
		maxStageBytes:    opts.maxStageBytes,
		gcGrace:          opts.gcGrace,
		lock:             !opts.noLock,
		leaseTTL:         opts.leaseTTL,
		force:            opts.force,
	})

	return err
}

// parseOverwrite validates the strategy, rejecting the two the API offers but dr-sync must
// never use.
func parseOverwrite(value string) (drapi.OverwriteStrategy, error) {
	switch drapi.OverwriteStrategy(value) {
	case drapi.OverwriteError, drapi.OverwriteReplace:
		return drapi.OverwriteStrategy(value), nil

	case drapi.OverwriteRename, drapi.OverwriteSkip:
		// Both would leave a pack under a name no manifest references: rename stores it as
		// "p_x (1).tar.xz", skip silently keeps whatever was already there. Either way the
		// manifest points at content that is not where it says.
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

func newStatusCommand(global *globalFlags) *cobra.Command {
	opts := &uploadFlags{}

	cmd := &cobra.Command{
		Use:   "status <path>",
		Short: "Show what an upload would do, without writing anything",
		Long: "Plan an upload and print it, writing nothing.\n\n" +
			"This is `upload --dry-run` sharing the same code path, on purpose: a status that\n" +
			"could disagree with the push it describes would be worse than no status at all.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			global.dryRun = true

			return runUpload(cmd, args, global, opts)
		},
	}

	opts.register(cmd, false)

	return cmd
}
