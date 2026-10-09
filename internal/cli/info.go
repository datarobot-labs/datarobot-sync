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
	"io/fs"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/datarobot/golang-datarobot-filesync/internal/config"
	"github.com/datarobot/golang-datarobot-filesync/internal/manifest"
	"github.com/datarobot/golang-datarobot-filesync/internal/progress"
	"github.com/datarobot/golang-datarobot-filesync/internal/syncer"
)

func newInitCommand(global *globalFlags) *cobra.Command {
	var (
		name        string
		description string
		tags        []string
	)

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create a container and print its id",
		Long: "Create a Files container and seed it with an empty generation-zero manifest.\n\n" +
			"The id is printed on stdout so it can be captured; the export line to reuse it goes\n" +
			"to stderr beside it.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// The container does not exist yet, so --catalog-id is not required here.
			session, err := global.open(cmd, false, "")
			if err != nil {
				return err
			}

			defer session.reporter.Close()

			ctx, cancel := global.withTimeout(cmd.Context())
			defer cancel()

			result, err := syncer.Init(ctx, session.client, session.reporter, syncer.InitOptions{
				Name:        name,
				Description: description,
				Tags:        tags,
				MetaPrefix:  global.metaPrefix,
			})
			if err != nil {
				return classify(err)
			}

			session.reporter.Phase("done", "container ready")
			session.reporter.Event("init", map[string]any{
				"catalogId": result.ContainerID,
				"manifest":  result.Manifest.ID,
			})

			fmt.Fprintln(cmd.OutOrStdout(), result.ContainerID)
			fmt.Fprintf(cmd.ErrOrStderr(), "\nexport %s=%s\n", config.EnvCatalogID, result.ContainerID)
			fmt.Fprintf(cmd.ErrOrStderr(), "%s upload <path>\n", programName())

			return nil
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&name, "name", "", "catalog name for the new container")
	flags.StringVar(&description, "description", "", "catalog description")
	flags.StringArrayVar(&tags, "tag", nil, "catalog tag (repeatable; dashes are not allowed)")

	return cmd
}

func newLSCommand(global *globalFlags) *cobra.Command {
	var (
		prefix     string
		long       bool
		generation int64
		manifestID string
	)

	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List the files a manifest records, fetching no content",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			session, err := global.open(cmd, true, "")
			if err != nil {
				return err
			}

			defer session.reporter.Close()

			ctx, cancel := global.withTimeout(cmd.Context())
			defer cancel()

			loaded, err := loadManifest(ctx, session, generation, manifestID)
			if err != nil {
				return classify(err)
			}

			writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 8, 2, ' ', 0)

			for _, entry := range loaded.Entries {
				if prefix != "" && !underPrefix(entry.Path, prefix) {
					continue
				}

				if !long {
					fmt.Fprintln(writer, renderPath(entry))

					continue
				}

				fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n",
					fs.FileMode(entry.Mode).Perm(),
					progress.HumanBytes(entry.Size),
					shortDigest(entry.SHA256),
					renderPath(entry))
			}

			return writer.Flush()
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&prefix, "prefix", "", "only list paths under this subtree")
	flags.BoolVar(&long, "long", false, "show mode, size and digest")
	flags.Int64Var(&generation, "generation", 0, "list a specific retained generation")
	flags.StringVar(&manifestID, "manifest", "", "list a specific manifest id")

	return cmd
}

// renderPath annotates non-files so a listing is not ambiguous.
func renderPath(entry manifest.Entry) string {
	switch entry.Type {
	case manifest.TypeDir:
		return entry.Path + "/"
	case manifest.TypeSymlink:
		return entry.Path + " -> " + entry.Target
	case manifest.TypeFile:
		return entry.Path
	default:
		return entry.Path
	}
}

func shortDigest(digest string) string {
	if len(digest) < 12 {
		return strings.Repeat("-", 12)
	}

	return digest[:12]
}

func underPrefix(path, prefix string) bool {
	trimmed := strings.TrimSuffix(prefix, "/")

	return path == trimmed || strings.HasPrefix(path, trimmed+"/")
}

func loadManifest(ctx context.Context, session *session, generation int64, manifestID string) (*manifest.Manifest, error) {
	state, err := session.store.ReadState(ctx)
	if err != nil {
		return nil, err
	}

	if manifestID != "" {
		for _, ref := range state.Manifests {
			if ref.ID == manifestID {
				return session.store.LoadManifest(ctx, ref)
			}
		}

		return nil, fmt.Errorf("manifest %s is not retained in this container", manifestID)
	}

	if generation > 0 {
		return loadGeneration(ctx, session, state, generation)
	}

	loaded, _, err := session.store.Head(ctx, state)

	return loaded, err
}

func loadGeneration(ctx context.Context, session *session, state *syncer.State, generation int64) (*manifest.Manifest, error) {
	var matches []manifest.Ref

	for _, ref := range state.Manifests {
		if ref.Generation == generation {
			matches = append(matches, ref)
		}
	}

	if len(matches) == 0 {
		return nil, fmt.Errorf("generation %d is not retained in this container", generation)
	}

	head, _ := manifest.SelectHead(matches)

	return session.store.LoadManifest(ctx, head)
}

func newInspectCommand(global *globalFlags) *cobra.Command {
	var (
		showPacks bool
		threshold float64
	)

	cmd := &cobra.Command{
		Use:   "inspect",
		Short: "Report generations, pack liveness, dead bytes and leases",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			session, err := global.open(cmd, true, "")
			if err != nil {
				return err
			}

			defer session.reporter.Close()

			ctx, cancel := global.withTimeout(cmd.Context())
			defer cancel()

			inspection, err := syncer.Inspect(ctx, session.store, threshold)
			if err != nil {
				return classify(err)
			}

			printInspection(cmd, inspection, showPacks)

			if inspection.Fork == nil {
				return nil
			}

			return Failf(CodeConflict,
				"two manifests exist at generation %d; the higher id is the head and the other's "+
					"paths are not visible", inspection.Fork.Generation)
		},
	}

	flags := cmd.Flags()
	flags.BoolVar(&showPacks, "packs", false, "list every pack with its live fraction")
	flags.Float64Var(&threshold, "compact-threshold", syncer.DefaultCompactThreshold,
		"live fraction below which a pack counts as sparse")

	return cmd
}

func printInspection(cmd *cobra.Command, inspection *syncer.Inspection, showPacks bool) {
	out := cmd.OutOrStdout()

	fmt.Fprintf(out, "container   %s\n", inspection.ContainerID)
	fmt.Fprintf(out, "head        %s (generation %d)\n", inspection.HeadRef.ID, inspection.Head.Generation)
	fmt.Fprintf(out, "written by  %s\n", inspection.Head.Generator)
	fmt.Fprintf(out, "files       %s in %s\n",
		progress.Count(inspection.Head.Stats.FileCount),
		progress.HumanBytes(inspection.Head.Stats.TotalSize))
	fmt.Fprintf(out, "packs       %d, %s stored\n",
		inspection.Head.Stats.PackCount,
		progress.HumanBytes(inspection.Head.Stats.PackCompressedBytes))
	fmt.Fprintf(out, "dead        %.0f%% of pack bytes (~%s reclaimable)\n",
		inspection.Garbage.DeadRatio()*100,
		progress.HumanBytes(inspection.Garbage.DeadCompressedBytes))
	fmt.Fprintf(out, "sparse      %d packs below the threshold, %d fully dead\n",
		len(inspection.Garbage.Sparse), len(inspection.Garbage.Dead))

	if len(inspection.Orphans) > 0 {
		fmt.Fprintf(out, "orphans     %d objects the head does not reference (%s)\n",
			len(inspection.Orphans), progress.HumanBytes(inspection.UnreferencedBytes))
	}

	fmt.Fprintf(out, "\ngenerations retained: %d\n", len(inspection.Generations))

	for _, ref := range inspection.Generations {
		fmt.Fprintf(out, "  %s\n", ref.ID)
	}

	if active := inspection.ActiveLeases(time.Now()); len(active) > 0 {
		fmt.Fprintln(out, "\nactive leases:")

		for _, lease := range active {
			fmt.Fprintf(out, "  %s\n", lease.Describe())
		}
	}

	if !showPacks {
		return
	}

	fmt.Fprintln(out, "\npacks:")

	writer := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)

	for _, stats := range inspection.Liveness {
		fmt.Fprintf(writer, "  %s\t%s\t%d/%d members\t%.0f%% live\t%s\n",
			stats.Pack.ID, stats.Pack.Codec,
			stats.LiveCount, len(stats.Pack.Members),
			stats.Ratio*100,
			progress.HumanBytes(stats.Pack.CompressedSize))
	}

	_ = writer.Flush()
}
