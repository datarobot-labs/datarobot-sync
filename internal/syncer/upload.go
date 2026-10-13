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

package syncer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/datarobot-labs/datarobot-sync/internal/drapi"
	"github.com/datarobot-labs/datarobot-sync/internal/manifest"
	"github.com/datarobot-labs/datarobot-sync/internal/packer"
	"github.com/datarobot-labs/datarobot-sync/internal/progress"
	"github.com/datarobot-labs/datarobot-sync/internal/scan"
)

// UploadOptions configures a push.
type UploadOptions struct {
	Root string

	Check          scan.CheckMode
	Matcher        *scan.Matcher
	Delete         bool
	FollowSymlinks bool

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
	Overwrite     drapi.OverwriteStrategy

	KeepGenerations  int
	GCGrace          time.Duration
	NoGC             bool
	CompactThreshold float64

	Lock     bool
	LeaseTTL time.Duration
	Force    bool
	// ClientID, when set, names the lease this run takes rather than minting a fresh random
	// one. A resident caller that runs many uploads under one identity needs this: with a new
	// random id every call, a lease left behind by a run that died mid-commit looks foreign to
	// the very process that orphaned it, and the next run blocks for the full lease TTL.
	ClientID string

	DryRun bool
}

// UploadResult summarizes what a push did.
type UploadResult struct {
	Plan       *UploadPlan
	Manifest   *manifest.Manifest
	PacksBuilt int
	// PacksSkipped counts packs already present in the container from an earlier attempt.
	PacksSkipped    int
	BytesUploaded   int64
	BytesCompressed int64
	Deleted         int
	Scan            *scan.Result
	DryRun          bool
	// CompactHint is set when enough dead weight has accumulated to be worth reclaiming.
	CompactHint string
}

// Upload makes the container match the local tree.
func Upload(ctx context.Context, store *Store, reporter *progress.Reporter, opts UploadOptions) (*UploadResult, error) {
	run := &uploadRun{store: store, reporter: reporter, opts: opts}

	return run.execute(ctx)
}

type uploadRun struct {
	store    *Store
	reporter *progress.Reporter
	opts     UploadOptions

	state     *State
	parent    *manifest.Manifest
	parentRef manifest.Ref
	baseline  *manifest.Index
	local     *scan.Result
	version   string
	scratch   string
}

func (r *uploadRun) execute(ctx context.Context) (*UploadResult, error) {
	if err := r.guardMetaCollision(); err != nil {
		return nil, err
	}

	if err := r.readRemoteAndScan(ctx); err != nil {
		return nil, err
	}

	local := r.local
	plan := Classify(local, r.baseline, ClassifyOptions{Delete: r.opts.Delete})

	r.reportPlan(plan)

	result := &UploadResult{Plan: plan, Scan: local, DryRun: r.opts.DryRun}

	if plan.Empty() {
		r.reporter.Phase("done", "already up to date at "+r.parent.Describe())

		return result, nil
	}

	if r.opts.DryRun {
		r.reporter.Phase("done", "dry run: nothing was written")

		return result, nil
	}

	if err := r.write(ctx, plan, result); err != nil {
		return nil, err
	}

	return result, nil
}

// write is the half of a push that changes the container: build, claim, commit, collect.
func (r *uploadRun) write(ctx context.Context, plan *UploadPlan, result *UploadResult) error {
	if err := r.prepareScratch(); err != nil {
		return err
	}

	defer os.RemoveAll(r.scratch)

	built, present, err := r.buildPacks(ctx, plan)
	if err != nil {
		return err
	}

	defer func() {
		for _, pack := range built {
			pack.Remove()
		}
	}()

	result.PacksBuilt = len(built) - len(present)
	result.PacksSkipped = len(present)

	lease, err := r.acquireLease(ctx)
	if err != nil {
		return err
	}

	defer r.releaseLease(ctx, lease)

	// Deferred after releaseLease, so it runs first: renewal has to stop before the release.
	stopRenewing := lease.KeepAlive(ctx, func(err error) {
		r.reporter.Warn(fmt.Sprintf("renewing the lease: %v", err))
	})
	defer stopRenewing()

	next, err := r.commit(ctx, plan, built, present)
	if err != nil {
		return err
	}

	result.Manifest = next

	for _, pack := range built {
		if present[pack.ID] {
			continue
		}

		result.BytesUploaded += pack.CompressedSize
		result.BytesCompressed += pack.UncompressedSize
	}

	r.reporter.Phase("commit", next.ID+" committed")

	result.Deleted, result.CompactHint = r.collect(ctx, next)

	return nil
}

// guardMetaCollision refuses to run when the reserved directory name is already taken by
// something of the user's own.
//
// Silently declining to sync a real directory is a data-loss bug that only surfaces months
// later, so this is a hard failure with the escape hatch named in the message.
func (r *uploadRun) guardMetaCollision() error {
	local := filepath.Join(r.opts.Root, filepath.FromSlash(r.store.MetaPrefix()))

	info, err := os.Lstat(local)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		return nil
	}

	kind := "file"
	if info.IsDir() {
		kind = "directory"
	}

	return fmt.Errorf("%s exists as a local %s, which dr-sync reserves for its own state; "+
		"move it aside or choose another name with --meta-prefix", local, kind)
}

// readRemoteAndScan reads the container's state and walks the local tree at the same time.
//
// They are independent, and both take real time — one is round-trips, the other is I/O — so
// overlapping them is most of the reason a no-op run feels instant.
func (r *uploadRun) readRemoteAndScan(ctx context.Context) error {
	group, groupCtx := errgroup.WithContext(ctx)

	group.Go(func() error {
		state, err := r.store.ReadState(groupCtx)
		if err != nil {
			return err
		}

		r.state = state

		parent, ref, err := r.store.Head(groupCtx, state)
		if errors.Is(err, ErrNoManifest) {
			// A container with no manifest is not an error for an upload: it is the first
			// push, and generation zero is the right parent.
			parent = manifest.New(r.store.MetaPrefix())
			parent.Generation = -1
			ref = manifest.Ref{}
			err = nil
		}

		if err != nil {
			return err
		}

		r.parent = parent
		r.parentRef = ref
		r.baseline = manifest.NewIndex(parent)

		version, err := r.store.CatalogVersion(groupCtx)
		if err != nil {
			return err
		}

		r.version = version

		return nil
	})

	if err := group.Wait(); err != nil {
		return err
	}

	r.reporter.Phase("remote", r.describeRemote())

	scanned, err := scan.Walk(ctx, scan.Options{
		Root:           r.opts.Root,
		MetaPrefix:     r.store.MetaPrefix(),
		Matcher:        r.opts.Matcher,
		Baseline:       r.baseline,
		Check:          r.opts.Check,
		Jobs:           r.opts.Jobs,
		FollowSymlinks: r.opts.FollowSymlinks,
		Warn:           r.reporter.Warn,
	})
	if err != nil {
		return err
	}

	r.local = scanned

	r.reporter.Phase("scan", fmt.Sprintf("%s entries  hashed %s (%s)  reused %s",
		progress.Count(len(scanned.Entries)),
		progress.Count(scanned.Hashed),
		progress.HumanBytes(scanned.HashedSize),
		progress.Count(scanned.Reused)))

	return nil
}

func (r *uploadRun) describeRemote() string {
	if r.parent.Generation < 0 {
		return "empty container; this will be generation 0"
	}

	report := manifest.NewIndex(r.parent).Garbage(r.compactThreshold())

	return fmt.Sprintf("%s  %.0f%% dead", r.parent.Describe(), report.DeadRatio()*100)
}

func (r *uploadRun) reportPlan(plan *UploadPlan) {
	r.reporter.Phase("plan", fmt.Sprintf("new %s  changed %s  unchanged %s  remote-only %s%s",
		progress.Count(plan.New), progress.Count(plan.Changed),
		progress.Count(plan.Unchanged), progress.Count(plan.RemoteOnly),
		droppedSuffix(plan)))

	if plan.ReusedBlobs > 0 {
		r.reporter.Phase("plan", fmt.Sprintf("reuse %s stored blobs (%s, nothing to transfer)",
			progress.Count(plan.ReusedBlobs), progress.HumanBytes(plan.ReusedBytes)))
	}

	if len(plan.NeedUpload) > 0 {
		r.reporter.Phase("plan", fmt.Sprintf("upload %s blobs, %s",
			progress.Count(len(plan.NeedUpload)), progress.HumanBytes(plan.NeedBytes)))
	}
}

func droppedSuffix(plan *UploadPlan) string {
	if plan.Dropped == 0 {
		if plan.RemoteOnly > 0 {
			return " (kept)"
		}

		return ""
	}

	return fmt.Sprintf(" (%s dropped)", progress.Count(plan.Dropped))
}

// prepareScratch creates the working directory for built packs.
//
// This is scratch, not state: it holds compressed packs between building and uploading so a
// pack never has to be buffered in memory, and nothing about correctness depends on it
// surviving the run.
func (r *uploadRun) prepareScratch() error {
	base := r.opts.TempDir
	if base == "" {
		base = os.TempDir()
	}

	dir, err := os.MkdirTemp(base, "dr-sync-*")
	if err != nil {
		return fmt.Errorf("creating scratch directory under %s: %w", base, err)
	}

	r.scratch = dir

	return nil
}

// buildPacks compresses the pending content, skipping anything already in the container.
func (r *uploadRun) buildPacks(ctx context.Context, plan *UploadPlan) ([]*packer.Built, map[string]bool, error) {
	planned := packer.Plan(plan.NeedUpload, packer.PlanOptions{
		PackInputMax:     r.opts.PackInputMax,
		BigFileThreshold: r.opts.BigFileThreshold,
		Level:            r.level(),
	})

	if len(planned) == 0 {
		return nil, nil, nil
	}

	built := make([]*packer.Built, len(planned))

	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(r.compressJobs())

	var completed atomic.Int64

	for i, pack := range planned {
		group.Go(func() error {
			result, err := packer.Build(groupCtx, pack.Blobs, packer.BuildOptions{
				ScratchDir: r.scratch,
				Codec:      pack.Codec,
				DictCap:    manifest.DictCapForLevel(r.level()),
				SliceSize:  r.opts.SliceSize,
			})
			if err != nil {
				return err
			}

			built[i] = result

			r.reporter.Update("pack", fmt.Sprintf("[%s] %d/%d packs",
				progress.Bar(completed.Add(1), int64(len(planned)), 24), completed.Load(), len(planned)))

			return nil
		})
	}

	if err := group.Wait(); err != nil {
		for _, pack := range built {
			if pack != nil {
				pack.Remove()
			}
		}

		return nil, nil, err
	}

	present := r.alreadyPresent(built)

	var compressed, raw int64

	for _, pack := range built {
		compressed += pack.CompressedSize
		raw += pack.UncompressedSize
	}

	r.reporter.Phase("pack", fmt.Sprintf("%d packs  %s in  %s out%s",
		len(built), progress.HumanBytes(raw), progress.HumanBytes(compressed), ratio(raw, compressed)))

	return built, present, nil
}

// alreadyPresent reports which packs the container already holds from an earlier attempt.
//
// Not uploading those is not an optimization. The commit rejects any name that already exists
// and applies none of the stage, so a resumed run that re-sent a pack it had already landed
// would fail outright. Content addressing is what makes the check possible from the listing
// alone: same name and same server-computed digest means same bytes.
//
// They are still referenced by the new manifest — skipping the upload must not mean forgetting
// the pack exists, or every file inside it would vanish from the tree.
func (r *uploadRun) alreadyPresent(built []*packer.Built) map[string]bool {
	present := map[string]bool{}

	for _, pack := range built {
		if !r.alreadyUploaded(pack) {
			continue
		}

		present[pack.ID] = true

		r.reporter.Note(pack.ID + " is already in the container from an earlier attempt")
	}

	return present
}

func (r *uploadRun) alreadyUploaded(pack *packer.Built) bool {
	for i := range pack.Slices {
		name := manifest.SliceName(r.store.MetaPrefix(), pack.ID, i, len(pack.Slices))

		remote, present := r.state.Objects[name]
		if !present {
			return false
		}

		if remote.Size != pack.Slices[i].Size {
			return false
		}

		// A name that exists with a different digest is a corrupted earlier upload, not
		// something to quietly reuse.
		if remote.Checksum != "" && remote.Checksum != pack.Slices[i].SHA256 {
			r.reporter.Warn(fmt.Sprintf("%s exists in the container with a different checksum; "+
				"it will not be reused", name))

			return false
		}
	}

	return true
}

// acquireLease claims the container unless locking is disabled.
func (r *uploadRun) acquireLease(ctx context.Context) (*LeaseHolder, error) {
	if !r.opts.Lock {
		return nil, nil
	}

	clientID := r.opts.ClientID
	if clientID == "" {
		generated, err := manifest.NewClientID()
		if err != nil {
			return nil, err
		}

		clientID = generated
	}

	holder, err := AcquireLease(ctx, r.store, r.state, clientID, r.opts.LeaseTTL, r.opts.Force)
	if err != nil {
		return nil, err
	}

	return holder, nil
}

func (r *uploadRun) releaseLease(ctx context.Context, lease *LeaseHolder) {
	if lease == nil {
		return
	}

	// Release with a context that survives an interrupt, or the delete is canceled with the
	// upload and the lease stays until it expires.
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), leaseReleaseTimeout)
	defer cancel()

	// A release failure is a warning, not a failure: the lease expires on its own, and
	// failing a completed sync over a cleanup call would be the wrong trade.
	if err := lease.Release(releaseCtx); err != nil {
		r.reporter.Warn(err.Error())
	}
}

// commit uploads the packs and the new manifest.
func (r *uploadRun) commit(ctx context.Context, plan *UploadPlan, built []*packer.Built, present map[string]bool) (*manifest.Manifest, error) {
	packs := make([]manifest.Pack, 0, len(built))
	objects := make([]Object, 0, len(built))
	pending := map[string]bool{}

	for _, pack := range built {
		record, slices := r.describePack(pack)
		packs = append(packs, record)

		if present[pack.ID] {
			// Already in the container: referenced by the manifest, but not re-sent.
			continue
		}

		for _, object := range slices {
			pending[object.Name] = true
		}

		objects = append(objects, slices...)
	}

	next := BuildManifest(r.parent, plan, packs, manifest.Compression{
		Codec:   manifest.CodecXZ,
		Level:   r.level(),
		DictCap: manifest.DictCapForLevel(r.level()),
	})

	clientID, err := manifest.NewClientID()
	if err != nil {
		return nil, err
	}

	next.ObservedCatalogVersionID = r.version

	if err := next.Stamp(clientID, time.Now()); err != nil {
		return nil, err
	}

	encoded, err := manifest.Encode(next)
	if err != nil {
		return nil, err
	}

	if err := r.revalidate(ctx, next, pending); err != nil {
		return nil, err
	}

	final := []Object{{
		Name:    next.ObjectName(),
		Content: encoded,
		Size:    int64(len(encoded)),
	}}

	uploadStarted := time.Now()

	err = r.store.Commit(ctx, objects, final, CommitOptions{
		Parallel:      r.uploadJobs(),
		MaxStageFiles: r.opts.MaxStageFiles,
		MaxStageBytes: r.opts.MaxStageBytes,
		Overwrite:     r.opts.Overwrite,
		Progress: func(uploaded int, bytes int64) {
			r.reporter.Update("upload", fmt.Sprintf("[%s] %d/%d objects  %s  %s",
				progress.Bar(int64(uploaded), int64(len(objects)+1), 24),
				uploaded, len(objects)+1,
				progress.HumanBytes(bytes),
				progress.Rate(bytes, time.Since(uploadStarted))))
		},
	})
	if err != nil {
		return nil, err
	}

	return next, nil
}

// describePack turns a built pack into its manifest record and the objects to upload.
func (r *uploadRun) describePack(pack *packer.Built) (manifest.Pack, []Object) {
	record := manifest.Pack{
		ID:               pack.ID,
		Codec:            pack.Codec,
		SHA256:           pack.SHA256,
		CompressedSize:   pack.CompressedSize,
		UncompressedSize: pack.UncompressedSize,
		Members:          pack.Members,
		CreatedAt:        time.Now().UTC().Format(time.RFC3339Nano),
	}

	objects := make([]Object, 0, len(pack.Slices))

	for i, slice := range pack.Slices {
		name := manifest.SliceName(r.store.MetaPrefix(), pack.ID, i, len(pack.Slices))

		record.Slices = append(record.Slices, manifest.Slice{
			FileName: name,
			Size:     slice.Size,
			SHA256:   slice.SHA256,
		})

		objects = append(objects, Object{Name: name, LocalPath: slice.LocalPath, Size: slice.Size})
	}

	return record, objects
}

// revalidate re-checks that nothing moved under us just before the manifest is written.
//
// This shrinks the window rather than closing it — the API has no compare-and-swap — but it
// converts the common case of two clients pushing minutes apart into an immediate, legible
// refusal instead of a silently lost update.
func (r *uploadRun) revalidate(ctx context.Context, next *manifest.Manifest, pending map[string]bool) error {
	state, err := r.store.ReadState(ctx)
	if err != nil {
		return err
	}

	if head, ok := manifest.SelectHead(state.Manifests); ok && head.ID != r.parentRef.ID {
		return &ConflictError{
			Expected: r.parentRef.ID,
			Actual:   head.ID,
		}
	}

	// Every pack the manifest is about to reference must still exist, or a concurrent
	// compaction could leave the new head pointing at objects that are gone.
	for _, pack := range next.Packs {
		for _, slice := range pack.Slices {
			if _, present := state.Objects[slice.FileName]; present {
				continue
			}

			if pending[slice.FileName] {
				// Not uploaded yet, which is expected: it goes up in this very commit.
				continue
			}

			return &StaleManifestError{ObjectName: slice.FileName}
		}
	}

	return nil
}

// ConflictError means the container moved between reading the head and writing the next one.
type ConflictError struct {
	Expected string
	Actual   string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("the container advanced from %s to %s while this run was working; "+
		"re-run to plan against the new head (or pass --force)",
		describeGeneration(e.Expected), describeGeneration(e.Actual))
}

func describeGeneration(id string) string {
	if id == "" {
		return "an empty container"
	}

	return id
}

// IsConflict reports whether err is a lost-update or lease conflict.
func IsConflict(err error) bool {
	var (
		conflict *ConflictError
		held     *LeaseHeldError
	)

	return errors.As(err, &conflict) || errors.As(err, &held)
}

// collect deletes what the new manifest no longer needs.
func (r *uploadRun) collect(ctx context.Context, next *manifest.Manifest) (int, string) {
	if r.opts.NoGC {
		return 0, r.compactHint(next)
	}

	state, err := r.store.ReadState(ctx)
	if err != nil {
		r.reporter.Warn("skipping garbage collection: " + err.Error())

		return 0, r.compactHint(next)
	}

	if fork, forked := manifest.DetectFork(state.Manifests); forked {
		// The losing manifest may be the only thing referencing packs that would otherwise
		// look unreferenced, so nothing is collected until the fork is resolved.
		r.reporter.Warn(fmt.Sprintf("two manifests exist at generation %d (%s); "+
			"skipping garbage collection until that is resolved",
			fork.Generation, forkIDs(fork)))

		return 0, r.compactHint(next)
	}

	deleted, err := Collect(ctx, r.store, state, CollectOptions{
		KeepGenerations: r.opts.KeepGenerations,
		Grace:           r.opts.GCGrace,
		Now:             time.Now(),
	})
	if err != nil {
		r.reporter.Warn("garbage collection did not complete: " + err.Error())
	}

	if deleted > 0 {
		r.reporter.Phase("gc", fmt.Sprintf("%d unreferenced objects removed", deleted))
	}

	return deleted, r.compactHint(next)
}

func forkIDs(fork manifest.Fork) string {
	ids := make([]string, 0, len(fork.Refs))
	for _, ref := range fork.Refs {
		ids = append(ids, ref.ID)
	}

	sort.Strings(ids)

	return fmt.Sprintf("%v", ids)
}

// compactHint suggests a compaction when enough dead weight has built up to pay for itself.
func (r *uploadRun) compactHint(next *manifest.Manifest) string {
	report := manifest.NewIndex(next).Garbage(r.compactThreshold())

	const (
		hintRatio = 0.35
		hintBytes = 64 << 20
	)

	if report.DeadRatio() < hintRatio || report.DeadCompressedBytes < hintBytes {
		return ""
	}

	return fmt.Sprintf("%.0f%% of pack bytes are no longer referenced; "+
		"`dr-sync compact` would reclaim about %s",
		report.DeadRatio()*100, progress.HumanBytes(report.DeadCompressedBytes))
}

func (r *uploadRun) level() int {
	if r.opts.Level < 0 {
		return manifest.DefaultLevel
	}

	return r.opts.Level
}

func (r *uploadRun) compactThreshold() float64 {
	if r.opts.CompactThreshold <= 0 {
		return DefaultCompactThreshold
	}

	return r.opts.CompactThreshold
}

func (r *uploadRun) compressJobs() int {
	if r.opts.CompressJobs > 0 {
		return r.opts.CompressJobs
	}

	// The pure-Go LZMA2 encoder is single-threaded, so the only way to use more than one core
	// is to compress several packs at once. That is an independent argument for many medium
	// packs over a few large ones.
	return min(runtime.GOMAXPROCS(0), 4)
}

func (r *uploadRun) uploadJobs() int {
	if r.opts.UploadJobs > 0 {
		return r.opts.UploadJobs
	}

	return DefaultUploadJobs
}

// DefaultCompactThreshold is the live fraction below which a pack is worth rewriting.
const DefaultCompactThreshold = 0.5

func ratio(raw, compressed int64) string {
	if compressed <= 0 || raw <= 0 {
		return ""
	}

	return fmt.Sprintf("  (%.1fx)", float64(raw)/float64(compressed))
}
