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

	"github.com/datarobot/golang-datarobot-filesync/internal/progress"
	"github.com/datarobot/golang-datarobot-filesync/internal/syncer"
)

func downloadCommand() *Command {
	return &Command{
		Name:  "download",
		Usage: "<path>",
		Short: "Pull a container into a directory, fetching only what is missing",
		Run:   runDownload,
	}
}

func runDownload(ctx context.Context, env *Env, args []string) error {
	var (
		common               commonFlags
		prefixes             repeatable
		remove               bool
		removeExcluded       bool
		noLocalReuse         bool
		preserveSpecialModes bool
		allowUnsafeLinks     bool
		parallel             int
		generation           int64
		manifestID           string
		tempDir              string
	)

	set := flag.NewFlagSet("download", flag.ContinueOnError)
	set.SetOutput(env.Stderr)
	common.register(set)

	set.Var(&prefixes, "prefix", "restrict the pull to this subtree (repeatable)")
	set.BoolVar(&remove, "delete", false, "remove local paths the manifest does not mention")
	set.BoolVar(&removeExcluded, "delete-excluded", false, "also remove directories that only hold excluded files")
	set.BoolVar(&noLocalReuse, "no-local-reuse", false,
		"always fetch, even when identical content is already on disk elsewhere")
	set.BoolVar(&preserveSpecialModes, "preserve-special-modes", false,
		"restore setuid, setgid and sticky bits")
	set.BoolVar(&allowUnsafeLinks, "allow-unsafe-links", false,
		"create symlinks whose targets leave the sync root")
	set.IntVar(&parallel, "parallel", 0, "packs fetched at once (default 4)")
	set.Int64Var(&generation, "generation", 0, "pull a specific retained generation instead of the head")
	set.StringVar(&manifestID, "manifest", "", "pull a specific manifest id instead of the head")
	set.StringVar(&tempDir, "tmpdir", "", "scratch directory; must share a filesystem with the sync root")

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

	result, err := syncer.Download(ctx, session.store, session.reporter, syncer.DownloadOptions{
		Root:                 root,
		Check:                session.check,
		Matcher:              session.matcher,
		Prefixes:             prefixes,
		Generation:           generation,
		ManifestID:           manifestID,
		Delete:               remove,
		DeleteExcluded:       removeExcluded,
		NoLocalReuse:         noLocalReuse,
		PreserveSpecialModes: preserveSpecialModes,
		AllowUnsafeLinks:     allowUnsafeLinks,
		Jobs:                 common.jobs,
		Parallel:             parallel,
		Retries:              common.retries,
		TempDir:              tempDir,
		DryRun:               common.dryRun,
	})
	if err != nil {
		return classify(err)
	}

	return reportDownload(session.reporter, result)
}

func reportDownload(reporter *progress.Reporter, result *syncer.DownloadResult) error {
	if result.DryRun {
		return nil
	}

	summary := fmt.Sprintf("%s  %s written  %s transferred",
		result.Manifest.ID,
		progress.Count(result.Written),
		progress.HumanBytes(result.BytesTransferred))

	if result.Removed > 0 {
		summary += fmt.Sprintf("  %s removed", progress.Count(result.Removed))
	}

	reporter.Phase("done", summary)

	reporter.Event("download", map[string]any{
		"manifest":         result.Manifest.ID,
		"generation":       result.Manifest.Generation,
		"filesWritten":     result.Written,
		"bytesTransferred": result.BytesTransferred,
		"filesFailed":      len(result.Failed),
		"pathsRemoved":     result.Removed,
	})

	for _, skip := range result.Plan.Skipped {
		reporter.Warn(fmt.Sprintf("skipped %s: %s", skip.Path, skip.Reason))
	}

	if !result.Partial() {
		return nil
	}

	// What landed is verified and complete; what did not is untouched. Re-running picks up
	// exactly the remainder, because local truth is re-derived every time.
	for _, path := range result.Failed {
		reporter.Warn("not written: " + path)
	}

	return Failf(CodePartial, "%d of %d files could not be written; the rest are complete, so re-run to finish",
		len(result.Failed), len(result.Failed)+result.Written)
}
