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
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/datarobot-labs/datarobot-sync/internal/manifest"
	"github.com/datarobot-labs/datarobot-sync/internal/progress"
	"github.com/datarobot-labs/datarobot-sync/internal/syncer"
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

// runCompaction runs one compaction and reports it. The result is returned (as well as any
// error, already classified) so a caller running compaction as part of a larger cycle — watch,
// after a push — can fold the manifest it produced into its own bookkeeping.
func runCompaction(ctx context.Context, session *session, settings compactSettings) (*syncer.CompactResult, error) {
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
		return nil, classify(err)
	}

	if result.Manifest == nil {
		return result, nil
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

	return result, nil
}

func newCompactCommand(global *globalFlags) *cobra.Command {
	settings := compactSettings{lock: true}

	var noLock bool

	cmd := &cobra.Command{
		Use:   "compact <path>",
		Short: "Rewrite sparse packs so their dead weight can be reclaimed",
		Long: "Rewrite packs whose contents are mostly superseded.\n\n" +
			"This runs from the local tree, which is what makes it affordable: on a machine with\n" +
			"a current checkout almost every live member is already on disk, so nothing has to\n" +
			"be downloaded. It refuses rather than silently turning into a large download when\n" +
			"the tree is incomplete.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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

			settings.root = root
			settings.jobs = global.jobs
			settings.lock = !noLock
			settings.dryRun = global.dryRun

			_, err = runCompaction(ctx, session, settings)

			return err
		},
	}

	flags := cmd.Flags()
	flags.Float64Var(&settings.threshold, "compact-threshold", syncer.DefaultCompactThreshold,
		"live fraction below which a pack is rewritten")
	flags.IntVar(&settings.level, "level", manifest.DefaultLevel, "compression level for the fresh packs")
	flags.Var(bytesFlag{&settings.packInputMax}, "pack-input-max", "uncompressed input cap per pack")
	flags.Var(bytesFlag{&settings.bigFileThreshold}, "big-file-threshold",
		"files at least this large get their own pack")
	flags.Var(bytesFlag{&settings.sliceSize}, "slice-size", "cap on one uploaded object")
	flags.IntVar(&settings.uploadJobs, "upload-jobs", 0, "objects uploaded at once")
	flags.StringVar(&settings.tempDir, "tmpdir", "", "where to build packs before uploading them")
	flags.IntVar(&settings.keepGenerations, "keep-generations", 0,
		"manifests to retain (default 1 after a compaction)")
	flags.DurationVar(&settings.gcGrace, "gc-grace", 0, "leave unreferenced objects alone for this long")
	flags.BoolVar(&noLock, "no-lock", false, "do not take an advisory lease")
	flags.DurationVar(&settings.leaseTTL, "lease-ttl", 0, "how long the lease is honoured")
	flags.BoolVar(&settings.force, "force", false, "ignore a foreign lease")

	return cmd
}

func newGCCommand(global *globalFlags) *cobra.Command {
	var (
		keepGenerations int
		gcGrace         time.Duration
	)

	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Delete objects no retained manifest references (moves no data)",
		Long: "Delete manifests beyond the retention count, and objects nothing retained still\n" +
			"references.\n\n" +
			"No data is rewritten, so this is cheap enough to run in CI. It works entirely from\n" +
			"the container and needs no local tree.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			session, err := global.open(cmd, true, "")
			if err != nil {
				return err
			}

			defer session.reporter.Close()

			ctx, cancel := global.withTimeout(cmd.Context())
			defer cancel()

			return runGC(ctx, session, global, keepGenerations, gcGrace)
		},
	}

	flags := cmd.Flags()
	flags.IntVar(&keepGenerations, "keep-generations", 0, "manifests to retain (default 5)")
	flags.DurationVar(&gcGrace, "gc-grace", 0,
		"leave unreferenced objects alone for this long (default 6h)")

	return cmd
}

func runGC(ctx context.Context, session *session, global *globalFlags, keepGenerations int, grace time.Duration) error {
	plan, err := planGC(ctx, session, keepGenerations, grace)
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

	if global.dryRun {
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
			"two manifests exist at generation %d; resolve that before collecting garbage",
			fork.Generation)
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
