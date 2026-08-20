// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/datarobot/golang-datarobot-filesync/internal/progress"
	"github.com/datarobot/golang-datarobot-filesync/internal/syncer"
)

type downloadFlags struct {
	prefixes             []string
	delete               bool
	deleteExcluded       bool
	noLocalReuse         bool
	preserveSpecialModes bool
	allowUnsafeLinks     bool
	parallel             int
	generation           int64
	manifestID           string
	tempDir              string
}

func newDownloadCommand(global *globalFlags) *cobra.Command {
	opts := &downloadFlags{}

	cmd := &cobra.Command{
		Use:   "download <path>",
		Short: "Pull a container into a directory, fetching only what is missing",
		Long: "Pull a container into a directory.\n\n" +
			"Files whose content is already on disk — even under a different path — are produced\n" +
			"locally rather than fetched. Local files the manifest does not mention are left\n" +
			"alone unless --delete.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDownload(cmd, args, global, opts)
		},
	}

	flags := cmd.Flags()
	flags.StringArrayVar(&opts.prefixes, "prefix", nil, "restrict the pull to this subtree (repeatable)")
	flags.BoolVar(&opts.delete, "delete", false, "remove local paths the manifest does not mention")
	flags.BoolVar(&opts.deleteExcluded, "delete-excluded", false,
		"also remove directories that only hold excluded files")
	flags.BoolVar(&opts.noLocalReuse, "no-local-reuse", false,
		"always fetch, even when identical content is already on disk elsewhere")
	flags.BoolVar(&opts.preserveSpecialModes, "preserve-special-modes", false,
		"restore setuid, setgid and sticky bits")
	flags.BoolVar(&opts.allowUnsafeLinks, "allow-unsafe-links", false,
		"create symlinks whose targets leave the sync root")
	flags.IntVar(&opts.parallel, "parallel", 0, "packs fetched at once (default 4)")
	flags.Int64Var(&opts.generation, "generation", 0,
		"pull a specific retained generation instead of the head")
	flags.StringVar(&opts.manifestID, "manifest", "", "pull a specific manifest id instead of the head")
	flags.StringVar(&opts.tempDir, "tmpdir", "",
		"scratch directory; must share a filesystem with the sync root")

	return cmd
}

func runDownload(cmd *cobra.Command, args []string, global *globalFlags, opts *downloadFlags) error {
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

	result, err := syncer.Download(ctx, session.store, session.reporter, syncer.DownloadOptions{
		Root:                 root,
		Check:                session.check,
		Matcher:              session.matcher,
		Prefixes:             opts.prefixes,
		Generation:           opts.generation,
		ManifestID:           opts.manifestID,
		Delete:               opts.delete,
		DeleteExcluded:       opts.deleteExcluded,
		NoLocalReuse:         opts.noLocalReuse,
		PreserveSpecialModes: opts.preserveSpecialModes,
		AllowUnsafeLinks:     opts.allowUnsafeLinks,
		Jobs:                 global.jobs,
		Parallel:             opts.parallel,
		Retries:              global.retries,
		TempDir:              opts.tempDir,
		DryRun:               global.dryRun,
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

	return Failf(CodePartial,
		"%d of %d files could not be written; the rest are complete, so re-run to finish",
		len(result.Failed), len(result.Failed)+result.Written)
}
