// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

package syncer

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/datarobot/golang-datarobot-filesync/internal/drapi"
	"github.com/datarobot/golang-datarobot-filesync/internal/manifest"
	"github.com/datarobot/golang-datarobot-filesync/internal/packer"
	"github.com/datarobot/golang-datarobot-filesync/internal/progress"
	"github.com/datarobot/golang-datarobot-filesync/internal/scan"
)

// CompactOptions configures a compaction.
type CompactOptions struct {
	Root string

	Check     scan.CheckMode
	Matcher   *scan.Matcher
	Threshold float64

	Level            int
	PackInputMax     int64
	BigFileThreshold int64
	SliceSize        int64

	Jobs         int
	CompressJobs int
	UploadJobs   int

	TempDir       string
	MaxStageFiles int
	MaxStageBytes int64

	KeepGenerations int
	GCGrace         time.Duration

	Lock     bool
	LeaseTTL time.Duration
	Force    bool

	DryRun bool
}

// CompactResult reports what a compaction reclaimed.
type CompactResult struct {
	Manifest *manifest.Manifest
	// Rewritten is the packs that were replaced.
	Rewritten []string
	// Repacked is how many blobs were moved into fresh packs.
	Repacked int
	// BytesUploaded is the compressed volume of the new packs.
	BytesUploaded int64
	// Reclaimed estimates the container space the deleted packs occupied.
	Reclaimed int64
	Deleted   int
	DryRun    bool
}

// Compact rewrites sparse packs so their dead weight can be deleted.
//
// It runs from the local tree rather than from the container, which is the whole reason it is
// affordable: on a machine with a current checkout, almost every live member of a sparse pack
// is already on disk, so compaction usually transfers nothing at all in the download
// direction.
func Compact(ctx context.Context, store *Store, reporter *progress.Reporter, opts CompactOptions) (*CompactResult, error) {
	threshold := opts.Threshold
	if threshold <= 0 {
		threshold = DefaultCompactThreshold
	}

	state, err := store.ReadState(ctx)
	if err != nil {
		return nil, err
	}

	head, headRef, err := store.Head(ctx, state)
	if err != nil {
		return nil, err
	}

	index := manifest.NewIndex(head)
	report := index.Garbage(threshold)

	reporter.Phase("remote", fmt.Sprintf("%s  %.0f%% dead (%s)",
		head.Describe(), report.DeadRatio()*100, progress.HumanBytes(report.DeadCompressedBytes)))

	rewrite := packsToRewrite(report)
	if len(rewrite) == 0 {
		reporter.Phase("done", "nothing worth compacting")

		return &CompactResult{Manifest: head, DryRun: opts.DryRun}, nil
	}

	blobs, err := planRepack(ctx, store, reporter, head, index, rewrite, opts)
	if err != nil {
		return nil, err
	}

	reporter.Phase("plan", fmt.Sprintf("rewrite %d packs, %s blobs, %s (all from the local tree)",
		len(rewrite), progress.Count(len(blobs)), progress.HumanBytes(totalSize(blobs))))

	result := &CompactResult{
		Rewritten: rewrite,
		Repacked:  len(blobs),
		Reclaimed: reclaimableBytes(report),
		DryRun:    opts.DryRun,
	}

	if opts.DryRun {
		reporter.Phase("done", "dry run: nothing was written")

		return result, nil
	}

	if err := writeCompaction(ctx, store, reporter, head, headRef, blobs, rewrite, opts, result); err != nil {
		return nil, err
	}

	return result, nil
}

// writeCompaction is the half of a compaction that changes the container.
func writeCompaction(ctx context.Context, store *Store, reporter *progress.Reporter, head *manifest.Manifest,
	headRef manifest.Ref, blobs []packer.Blob, rewrite []string, opts CompactOptions, result *CompactResult,
) error {
	next, uploaded, err := rewritePacks(ctx, store, reporter, head, headRef, blobs, rewrite, opts)
	if err != nil {
		return err
	}

	result.Manifest = next
	result.BytesUploaded = uploaded

	reporter.Phase("commit", next.ID+" committed")

	deleted, err := collectAfterCompaction(ctx, store, reporter, next, opts)
	if err != nil {
		reporter.Warn("garbage collection did not complete: " + err.Error())
	}

	result.Deleted = deleted

	return nil
}

// planRepack walks the local tree and works out which content has to move.
func planRepack(ctx context.Context, store *Store, reporter *progress.Reporter, head *manifest.Manifest,
	index *manifest.Index, rewrite []string, opts CompactOptions,
) ([]packer.Blob, error) {
	local, err := scan.Walk(ctx, scan.Options{
		Root:       opts.Root,
		MetaPrefix: store.MetaPrefix(),
		Matcher:    opts.Matcher,
		Baseline:   index,
		Check:      opts.Check,
		Jobs:       opts.Jobs,
		Warn:       reporter.Warn,
	})
	if err != nil {
		return nil, err
	}

	blobs, missing := liveBlobsToRepack(head, index, rewrite, local)
	if len(missing) > 0 {
		return nil, &IncompleteLocalTreeError{Paths: missing, Packs: rewrite}
	}

	return blobs, nil
}

// reclaimableBytes estimates the container space the replaced packs occupy.
func reclaimableBytes(report manifest.GarbageReport) int64 {
	var total int64

	for _, stats := range report.Sparse {
		total += stats.Pack.CompressedSize
	}

	for _, stats := range report.Dead {
		total += stats.Pack.CompressedSize
	}

	return total
}

// packsToRewrite lists the pack ids whose live members should be moved into fresh packs.
func packsToRewrite(report manifest.GarbageReport) []string {
	ids := make([]string, 0, len(report.Sparse))
	for _, stats := range report.Sparse {
		ids = append(ids, stats.Pack.ID)
	}

	sort.Strings(ids)

	return ids
}

// liveBlobsToRepack finds the content that has to move, and the paths whose content is not
// available locally to move it from.
func liveBlobsToRepack(head *manifest.Manifest, index *manifest.Index, rewrite []string, local *scan.Result) ([]packer.Blob, []string) {
	rewriting := make(map[string]bool, len(rewrite))
	for _, id := range rewrite {
		rewriting[id] = true
	}

	var (
		blobs   []packer.Blob
		missing []string
	)

	seen := map[string]bool{}

	for _, entry := range head.Entries {
		if entry.Type != manifest.TypeFile || entry.Size == 0 || seen[entry.SHA256] {
			continue
		}

		pack, _, ok := index.PackForBlob(entry.SHA256)
		if !ok || !rewriting[pack.ID] {
			continue
		}

		seen[entry.SHA256] = true

		source, available := local.SourcePath[entry.SHA256]
		if !available {
			missing = append(missing, entry.Path)

			continue
		}

		blobs = append(blobs, packer.Blob{SHA256: entry.SHA256, Size: entry.Size, SourcePath: source})
	}

	sort.Strings(missing)

	return blobs, missing
}

// IncompleteLocalTreeError means compaction would have to download content it cannot get
// locally.
//
// Refusing is deliberate: a compaction that streams live members back out of the packs it is
// replacing is a much more expensive operation, and doing it silently would surprise someone
// who ran `compact` expecting it to be cheap.
type IncompleteLocalTreeError struct {
	Paths []string
	Packs []string
}

func (e *IncompleteLocalTreeError) Error() string {
	shown := e.Paths
	if len(shown) > 5 {
		shown = shown[:5]
	}

	suffix := ""
	if len(e.Paths) > len(shown) {
		suffix = fmt.Sprintf(" and %d more", len(e.Paths)-len(shown))
	}

	return fmt.Sprintf("compaction needs %d files that are not in the local tree (%s%s); "+
		"run it from a complete checkout, or download first",
		len(e.Paths), strings.Join(shown, ", "), suffix)
}

func totalSize(blobs []packer.Blob) int64 {
	var total int64
	for _, blob := range blobs {
		total += blob.Size
	}

	return total
}

// rewritePacks builds fresh packs for the given blobs and commits a manifest that references
// only the packs being kept plus the new ones.
func rewritePacks(ctx context.Context, store *Store, reporter *progress.Reporter, head *manifest.Manifest,
	headRef manifest.Ref, blobs []packer.Blob, rewrite []string, opts CompactOptions,
) (*manifest.Manifest, int64, error) {
	base := opts.TempDir
	if base == "" {
		base = os.TempDir()
	}

	scratch, err := os.MkdirTemp(base, "dr-sync-compact-*")
	if err != nil {
		return nil, 0, fmt.Errorf("creating scratch directory under %s: %w", base, err)
	}

	defer os.RemoveAll(scratch)

	level := opts.Level
	if level < 0 {
		level = manifest.DefaultLevel
	}

	planned := packer.Plan(blobs, packer.PlanOptions{
		PackInputMax:     opts.PackInputMax,
		BigFileThreshold: opts.BigFileThreshold,
		Level:            level,
	})

	fresh := make([]manifest.Pack, 0, len(planned))
	objects := make([]Object, 0, len(planned))

	var uploaded int64

	for _, plannedPack := range planned {
		built, err := packer.Build(ctx, plannedPack.Blobs, packer.BuildOptions{
			ScratchDir: scratch,
			Codec:      plannedPack.Codec,
			DictCap:    manifest.DictCapForLevel(level),
			SliceSize:  opts.SliceSize,
		})
		if err != nil {
			return nil, 0, err
		}

		record, slices := describeBuiltPack(store.MetaPrefix(), built)
		fresh = append(fresh, record)
		objects = append(objects, slices...)
		uploaded += built.CompressedSize
	}

	next, err := rebuildManifest(head, headRef, rewrite, fresh, level)
	if err != nil {
		return nil, 0, err
	}

	encoded, err := manifest.Encode(next)
	if err != nil {
		return nil, 0, err
	}

	reporter.Phase("pack", fmt.Sprintf("%d fresh packs, %s", len(fresh), progress.HumanBytes(uploaded)))

	final := []Object{{Name: next.ObjectName(), Content: encoded, Size: int64(len(encoded))}}

	err = store.Commit(ctx, objects, final, CommitOptions{
		Parallel:      opts.UploadJobs,
		MaxStageFiles: opts.MaxStageFiles,
		MaxStageBytes: opts.MaxStageBytes,
		Overwrite:     drapi.OverwriteError,
	})
	if err != nil {
		return nil, 0, err
	}

	return next, uploaded, nil
}

// describeBuiltPack turns a built pack into its manifest record and upload objects.
func describeBuiltPack(metaPrefix string, built *packer.Built) (manifest.Pack, []Object) {
	record := manifest.Pack{
		ID:               built.ID,
		Codec:            built.Codec,
		SHA256:           built.SHA256,
		CompressedSize:   built.CompressedSize,
		UncompressedSize: built.UncompressedSize,
		Members:          built.Members,
		CreatedAt:        time.Now().UTC().Format(time.RFC3339Nano),
	}

	objects := make([]Object, 0, len(built.Slices))

	for i, slice := range built.Slices {
		name := manifest.SliceName(metaPrefix, built.ID, i, len(built.Slices))

		record.Slices = append(record.Slices, manifest.Slice{
			FileName: name,
			Size:     slice.Size,
			SHA256:   slice.SHA256,
		})

		objects = append(objects, Object{Name: name, LocalPath: slice.LocalPath, Size: slice.Size})
	}

	return record, objects
}

// rebuildManifest produces the next manifest: the same tree, but with the rewritten packs
// replaced by the fresh ones.
func rebuildManifest(head *manifest.Manifest, headRef manifest.Ref, rewrite []string, fresh []manifest.Pack, level int) (*manifest.Manifest, error) {
	rewriting := make(map[string]bool, len(rewrite))
	for _, id := range rewrite {
		rewriting[id] = true
	}

	kept := make([]manifest.Pack, 0, len(head.Packs))

	for _, pack := range head.Packs {
		if !rewriting[pack.ID] {
			kept = append(kept, pack)
		}
	}

	plan := &UploadPlan{Entries: head.Entries}

	next := BuildManifest(head, plan, append(kept, fresh...), manifest.Compression{
		Codec:   manifest.CodecXZ,
		Level:   level,
		DictCap: manifest.DictCapForLevel(level),
	})

	next.ObservedCatalogVersionID = head.ObservedCatalogVersionID
	next.ParentManifestID = headRef.ID

	clientID, err := manifest.NewClientID()
	if err != nil {
		return nil, err
	}

	if err := next.Stamp(clientID, time.Now()); err != nil {
		return nil, err
	}

	return next, nil
}

// collectAfterCompaction removes what the new manifest no longer needs.
//
// The ordering is the invariant that makes this safe: the new manifest is already committed and
// verified to be the maximum before anything is deleted, so a reader can never select a
// manifest whose packs have just been removed.
func collectAfterCompaction(ctx context.Context, store *Store, reporter *progress.Reporter, next *manifest.Manifest, opts CompactOptions) (int, error) {
	state, err := store.ReadState(ctx)
	if err != nil {
		return 0, err
	}

	head, ok := manifest.SelectHead(state.Manifests)
	if !ok || head.ID != next.ID {
		// Someone else committed while we were working. Their manifest may reference packs
		// we were about to delete, so nothing is deleted.
		return 0, fmt.Errorf("the head advanced to %s during compaction, so nothing was deleted", head.ID)
	}

	if fork, forked := manifest.DetectFork(state.Manifests); forked {
		return 0, fmt.Errorf("two manifests exist at generation %d, so nothing was deleted", fork.Generation)
	}

	keep := opts.KeepGenerations
	if keep <= 0 {
		// A compaction exists to reclaim space, so retaining several older manifests — each
		// pinning the packs it references — would undo the point of it.
		keep = 1
	}

	deleted, err := Collect(ctx, store, state, CollectOptions{
		KeepGenerations: keep,
		Grace:           opts.GCGrace,
		Now:             time.Now(),
	})
	if err != nil {
		return deleted, err
	}

	if deleted > 0 {
		reporter.Phase("gc", fmt.Sprintf("%d objects removed", deleted))
	}

	return deleted, nil
}
