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
	"io/fs"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/datarobot/golang-datarobot-filesync/internal/config"
	"github.com/datarobot/golang-datarobot-filesync/internal/manifest"
	"github.com/datarobot/golang-datarobot-filesync/internal/progress"
	"github.com/datarobot/golang-datarobot-filesync/internal/syncer"
)

func initCommand() *Command {
	return &Command{
		Name:  "init",
		Short: "Create a container and print its id",
		Run:   runInit,
	}
}

func runInit(ctx context.Context, env *Env, args []string) error {
	var (
		common      commonFlags
		name        string
		description string
		tags        repeatable
	)

	set := flag.NewFlagSet("init", flag.ContinueOnError)
	set.SetOutput(env.Stderr)
	common.register(set)

	set.StringVar(&name, "name", "", "catalog name for the new container")
	set.StringVar(&description, "description", "", "catalog description")
	set.Var(&tags, "tag", "catalog tag (repeatable)")

	if err := set.Parse(args); err != nil {
		return err
	}

	if set.NArg() > 0 {
		return Failf(CodeUsage, "init takes no path argument")
	}

	// A container does not exist yet, so --catalog-id is not required here.
	session, err := common.open(env, set, false, "")
	if err != nil {
		return err
	}

	defer session.reporter.Close()

	ctx, cancel := common.withTimeout(ctx)
	defer cancel()

	result, err := syncer.Init(ctx, session.client, session.reporter, syncer.InitOptions{
		Name:        name,
		Description: description,
		Tags:        tags,
		MetaPrefix:  common.metaPrefix,
	})
	if err != nil {
		return classify(err)
	}

	session.reporter.Phase("done", "container ready")
	session.reporter.Event("init", map[string]any{
		"catalogId": result.ContainerID,
		"manifest":  result.Manifest.ID,
	})

	// The id goes to stdout so it can be captured, with the export line as a comment beside
	// it: the next command needs it, and re-deriving it is a lookup nobody should have to do.
	fmt.Fprintln(env.Stdout, result.ContainerID)
	fmt.Fprintf(env.Stderr, "\nexport %s=%s\n", config.EnvCatalogID, result.ContainerID)
	fmt.Fprintf(env.Stderr, "%s upload <path> --catalog-id %s\n", env.Program, result.ContainerID)

	return nil
}

func statusCommand() *Command {
	return &Command{
		Name:  "status",
		Usage: "<path>",
		Short: "Show what an upload would do, without writing anything",
		Run:   runStatus,
	}
}

// runStatus is an upload that stops after planning. Sharing the code path is the point: a
// status that could disagree with the push it describes would be worse than no status.
func runStatus(ctx context.Context, env *Env, args []string) error {
	return runUpload(ctx, env, append([]string{"--dry-run"}, args...))
}

func lsCommand() *Command {
	return &Command{
		Name:  "ls",
		Short: "List the files a manifest records, fetching no content",
		Run:   runLS,
	}
}

func runLS(ctx context.Context, env *Env, args []string) error {
	var (
		common     commonFlags
		prefix     string
		long       bool
		generation int64
		manifestID string
	)

	set := flag.NewFlagSet("ls", flag.ContinueOnError)
	set.SetOutput(env.Stderr)
	common.register(set)

	set.StringVar(&prefix, "prefix", "", "only list paths under this subtree")
	set.BoolVar(&long, "long", false, "show mode, size and digest")
	set.Int64Var(&generation, "generation", 0, "list a specific retained generation")
	set.StringVar(&manifestID, "manifest", "", "list a specific manifest id")

	if err := set.Parse(args); err != nil {
		return err
	}

	session, err := common.open(env, set, true, "")
	if err != nil {
		return err
	}

	defer session.reporter.Close()

	ctx, cancel := common.withTimeout(ctx)
	defer cancel()

	loaded, err := loadManifest(ctx, session, generation, manifestID)
	if err != nil {
		return classify(err)
	}

	writer := tabwriter.NewWriter(env.Stdout, 0, 8, 2, ' ', 0)

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

	loaded, _, err := session.store.Head(ctx, state)

	return loaded, err
}

func inspectCommand() *Command {
	return &Command{
		Name:  "inspect",
		Short: "Report generations, pack liveness, dead bytes and leases",
		Run:   runInspect,
	}
}

func runInspect(ctx context.Context, env *Env, args []string) error {
	var (
		common    commonFlags
		showPacks bool
		threshold float64
	)

	set := flag.NewFlagSet("inspect", flag.ContinueOnError)
	set.SetOutput(env.Stderr)
	common.register(set)

	set.BoolVar(&showPacks, "packs", false, "list every pack with its live fraction")
	set.Float64Var(&threshold, "compact-threshold", syncer.DefaultCompactThreshold,
		"live fraction below which a pack counts as sparse")

	if err := set.Parse(args); err != nil {
		return err
	}

	session, err := common.open(env, set, true, "")
	if err != nil {
		return err
	}

	defer session.reporter.Close()

	ctx, cancel := common.withTimeout(ctx)
	defer cancel()

	inspection, err := syncer.Inspect(ctx, session.store, threshold)
	if err != nil {
		return classify(err)
	}

	printInspection(env, inspection, showPacks)

	if inspection.Fork != nil {
		return Failf(CodeConflict,
			"two manifests exist at generation %d; the higher id is the head and the other's paths are not visible",
			inspection.Fork.Generation)
	}

	return nil
}

func printInspection(env *Env, inspection *syncer.Inspection, showPacks bool) {
	out := env.Stdout

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
		fmt.Fprintf(out, "\nactive leases:\n")

		for _, lease := range active {
			fmt.Fprintf(out, "  %s\n", lease.Describe())
		}
	}

	if !showPacks {
		return
	}

	fmt.Fprintf(out, "\npacks:\n")

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
