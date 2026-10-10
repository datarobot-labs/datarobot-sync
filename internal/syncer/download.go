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
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/datarobot-labs/datarobot-sync/internal/manifest"
	"github.com/datarobot-labs/datarobot-sync/internal/packer"
	"github.com/datarobot-labs/datarobot-sync/internal/progress"
	"github.com/datarobot-labs/datarobot-sync/internal/scan"
)

// DownloadOptions configures a pull.
type DownloadOptions struct {
	Root string

	Check   scan.CheckMode
	Matcher *scan.Matcher
	// Prefixes restricts the pull to these subtrees. Empty means the whole tree.
	Prefixes []string
	// Generation and ManifestID pin an older retained manifest instead of the head.
	Generation int64
	ManifestID string

	Delete         bool
	DeleteExcluded bool
	// NoLocalReuse disables satisfying a needed file by copying an identical local sibling.
	NoLocalReuse bool
	// PreserveSpecialModes keeps setuid, setgid and sticky bits.
	PreserveSpecialModes bool
	AllowUnsafeLinks     bool

	Jobs     int
	Parallel int
	Retries  int
	TempDir  string

	DryRun bool
}

// DownloadResult summarizes a pull.
type DownloadResult struct {
	Manifest *manifest.Manifest
	Plan     *DownloadPlan
	// Written counts files materialized, whether transferred or copied locally.
	Written int
	// Failed names files that could not be materialized. What landed is still correct.
	Failed []string
	// BytesTransferred is the compressed volume actually fetched.
	BytesTransferred int64
	Removed          int
	DryRun           bool
}

// Partial reports whether some files failed while others succeeded.
func (r *DownloadResult) Partial() bool {
	return len(r.Failed) > 0
}

// DownloadPlan is what a pull would do.
type DownloadPlan struct {
	// Dirs, Links and Empties come straight from the manifest and need no transfer.
	Dirs    []manifest.Entry
	Links   []manifest.Entry
	Empties []manifest.Entry
	// UpToDate is files whose local content already matches.
	UpToDate []manifest.Entry
	// LocalCopies is files whose content exists locally under a different path.
	LocalCopies []LocalCopy
	// Needed is files whose content must come out of a pack, keyed by digest.
	Needed map[string][]manifest.Entry
	// Packs is the packs to read, in the order they will be read.
	Packs []PackFetch
	// Extraneous is local paths the manifest does not mention.
	Extraneous []string
	// Skipped names manifest entries that cannot be written here, with the reason.
	Skipped []scan.Skip
	// ModeFixups is entries whose content matches but whose mode or mtime does not.
	ModeFixups []manifest.Entry

	NeededBytes  int64
	EstimatedGet int64
}

// LocalCopy is a file that can be produced by cloning an identical local file.
type LocalCopy struct {
	Entry manifest.Entry
	// Source is the local path already holding this content.
	Source string
}

// PackFetch is one pack to read, and how much of it is needed.
type PackFetch struct {
	Pack manifest.Pack
	// Wanted is the digests to extract from it.
	Wanted map[string]struct{}
	// LastNeeded is the index of the last wanted member, so reading can stop there.
	LastNeeded int
	// Fraction is the share of the pack that must be read to reach LastNeeded.
	Fraction float64
}

// Empty reports whether a plan would change nothing.
func (p *DownloadPlan) Empty() bool {
	return len(p.Needed) == 0 && len(p.LocalCopies) == 0 && len(p.Empties) == 0 &&
		len(p.Links) == 0 && len(p.ModeFixups) == 0 && len(p.Dirs) == 0
}

// Download makes the local tree match the container.
func Download(ctx context.Context, store *Store, reporter *progress.Reporter, opts DownloadOptions) (*DownloadResult, error) {
	run := &downloadRun{store: store, reporter: reporter, opts: opts}

	return run.execute(ctx)
}

type downloadRun struct {
	store    *Store
	reporter *progress.Reporter
	opts     DownloadOptions

	state    *State
	manifest *manifest.Manifest
	local    *scan.Result
	tempDir  string
}

func (r *downloadRun) execute(ctx context.Context) (*DownloadResult, error) {
	if err := r.readRemote(ctx); err != nil {
		return nil, err
	}

	if err := r.scanLocal(ctx); err != nil {
		return nil, err
	}

	plan, err := r.plan()
	if err != nil {
		return nil, err
	}

	r.reportPlan(plan)

	result := &DownloadResult{Manifest: r.manifest, Plan: plan, DryRun: r.opts.DryRun}

	if r.opts.DryRun {
		r.reporter.Phase("done", "dry run: nothing was written")

		return result, nil
	}

	if err := r.verifySlices(plan); err != nil {
		return nil, err
	}

	if err := r.prepareTempDir(); err != nil {
		return nil, err
	}

	defer os.RemoveAll(r.tempDir)

	fetched, failures, err := r.fetch(ctx, plan)
	if err != nil {
		return nil, err
	}

	result.BytesTransferred = fetched
	result.Failed = failures

	written, err := r.apply(plan, failures)
	if err != nil {
		return nil, err
	}

	result.Written = written

	removed, err := r.removeExtraneous(plan)
	if err != nil {
		return nil, err
	}

	result.Removed = removed

	return result, nil
}

// readRemote loads the manifest this pull will reproduce.
func (r *downloadRun) readRemote(ctx context.Context) error {
	state, err := r.store.ReadState(ctx)
	if err != nil {
		return err
	}

	r.state = state

	ref, err := r.selectManifest(state)
	if err != nil {
		return err
	}

	loaded, err := r.store.LoadManifest(ctx, ref)
	if err != nil {
		return err
	}

	r.manifest = loaded

	r.reporter.Phase("remote", loaded.Describe())

	return nil
}

// selectManifest picks the head, or the specific generation the caller pinned.
func (r *downloadRun) selectManifest(state *State) (manifest.Ref, error) {
	if r.opts.ManifestID != "" {
		for _, ref := range state.Manifests {
			if ref.ID == r.opts.ManifestID {
				return ref, nil
			}
		}

		return manifest.Ref{}, fmt.Errorf("manifest %s is not retained in this container", r.opts.ManifestID)
	}

	if r.opts.Generation > 0 {
		var matches []manifest.Ref

		for _, ref := range state.Manifests {
			if ref.Generation == r.opts.Generation {
				matches = append(matches, ref)
			}
		}

		if len(matches) == 0 {
			return manifest.Ref{}, fmt.Errorf("generation %d is not retained in this container", r.opts.Generation)
		}

		head, _ := manifest.SelectHead(matches)

		return head, nil
	}

	ref, ok := manifest.SelectHead(state.Manifests)
	if !ok {
		return manifest.Ref{}, ErrNoManifest
	}

	return ref, nil
}

func (r *downloadRun) scanLocal(ctx context.Context) error {
	if err := os.MkdirAll(r.opts.Root, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", r.opts.Root, err)
	}

	// Clear any scratch a previous interrupted run left behind. It carries no state, so its
	// only possible contribution is confusion.
	_ = os.RemoveAll(filepath.Join(r.opts.Root, scan.TempDirName))

	scanned, err := scan.Walk(ctx, scan.Options{
		Root:       r.opts.Root,
		MetaPrefix: r.store.MetaPrefix(),
		Matcher:    r.opts.Matcher,
		Baseline:   manifest.NewIndex(r.manifest),
		Check:      r.opts.Check,
		Jobs:       r.opts.Jobs,
		Warn:       r.reporter.Warn,
	})
	if err != nil {
		return err
	}

	r.local = scanned

	return nil
}

func (r *downloadRun) reportPlan(plan *DownloadPlan) {
	r.reporter.Phase("scan", fmt.Sprintf("up-to-date %s  stale/missing %s  extraneous %s",
		progress.Count(len(plan.UpToDate)),
		progress.Count(countEntries(plan.Needed)+len(plan.LocalCopies)),
		progress.Count(len(plan.Extraneous))))

	detail := fmt.Sprintf("%s files from %s packs (~%s to transfer)",
		progress.Count(countEntries(plan.Needed)),
		progress.Count(len(plan.Packs)),
		progress.HumanBytes(plan.EstimatedGet))

	if len(plan.LocalCopies) > 0 {
		detail = progress.Count(len(plan.LocalCopies)) + " from local copies, " + detail
	}

	r.reporter.Phase("plan", detail)
}

func countEntries(needed map[string][]manifest.Entry) int {
	total := 0
	for _, entries := range needed {
		total += len(entries)
	}

	return total
}

func (r *downloadRun) prepareTempDir() error {
	// The scratch directory lives under the sync root on purpose: the final step is
	// os.Rename, which is only atomic within one filesystem.
	base := filepath.Join(r.opts.Root, scan.TempDirName)

	if r.opts.TempDir != "" {
		base = r.opts.TempDir
	}

	if err := os.MkdirAll(base, 0o700); err != nil {
		return fmt.Errorf("creating scratch directory %s: %w", base, err)
	}

	r.tempDir = base

	return nil
}

// verifySlices confirms every needed object still exists with the digest the manifest claims.
//
// This costs one already-fetched listing and no bandwidth, and it turns "the remote was
// compacted while we were reading it" from a mid-transfer surprise into an upfront refusal.
func (r *downloadRun) verifySlices(plan *DownloadPlan) error {
	for _, fetch := range plan.Packs {
		for _, slice := range fetch.Pack.Slices {
			remote, present := r.state.Objects[slice.FileName]
			if !present {
				return &StaleManifestError{ObjectName: slice.FileName}
			}

			if remote.Size != slice.Size {
				return &packer.IntegrityError{
					Digest: slice.SHA256,
					Reason: fmt.Sprintf("%s is %d bytes in the container but the manifest says %d",
						slice.FileName, remote.Size, slice.Size),
				}
			}

			if remote.Checksum != "" && slice.SHA256 != "" && remote.Checksum != slice.SHA256 {
				return &packer.IntegrityError{
					Digest: slice.SHA256,
					Reason: slice.FileName + " has a different checksum in the container than the manifest records",
				}
			}
		}
	}

	return nil
}

// removeExtraneous deletes local paths the manifest does not mention, when asked to.
func (r *downloadRun) removeExtraneous(plan *DownloadPlan) (int, error) {
	if len(plan.Extraneous) == 0 {
		return 0, nil
	}

	if !r.opts.Delete {
		r.reporter.Warn(fmt.Sprintf("%s local paths are not in the manifest and were left alone "+
			"(pass --delete to remove them)", progress.Count(len(plan.Extraneous))))

		for _, path := range plan.Extraneous {
			r.reporter.Note("extraneous: " + path)
		}

		return 0, nil
	}

	// Deepest first, so a directory is empty by the time it is reached.
	ordered := make([]string, len(plan.Extraneous))
	copy(ordered, plan.Extraneous)

	sort.Sort(sort.Reverse(sort.StringSlice(ordered)))

	removed := 0

	for _, relPath := range ordered {
		gone, err := r.removeOne(relPath)
		if err != nil {
			return removed, err
		}

		if gone {
			removed++
		}
	}

	r.reporter.Phase("delete", progress.Count(removed)+" extraneous paths removed")

	return removed, nil
}

// removeOne deletes one extraneous path, reporting whether anything went away.
func (r *downloadRun) removeOne(relPath string) (bool, error) {
	full, err := scan.LocalPath(r.opts.Root, relPath)
	if err != nil {
		return false, err
	}

	err = os.Remove(full)

	switch {
	case err == nil:
		return true, nil

	case errors.Is(err, os.ErrNotExist):
		return false, nil

	case isNotEmpty(err):
		// A directory still holding excluded files is left alone; only --delete-excluded is
		// licence to take those too.
		if !r.opts.DeleteExcluded {
			return false, nil
		}

		if err := os.RemoveAll(full); err != nil {
			return false, fmt.Errorf("removing %s: %w", full, err)
		}

		return true, nil

	default:
		return false, fmt.Errorf("removing %s: %w", full, err)
	}
}

// isNotEmpty reports whether a removal failed because the directory still has contents.
func isNotEmpty(err error) bool {
	return errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST)
}

func (r *downloadRun) parallel() int {
	if r.opts.Parallel > 0 {
		return r.opts.Parallel
	}

	return DefaultUploadJobs
}

// fetch reads the needed packs and stages their members.
func (r *downloadRun) fetch(ctx context.Context, plan *DownloadPlan) (int64, []string, error) {
	if len(plan.Packs) == 0 {
		return 0, nil, nil
	}

	names := make([]string, 0, len(plan.Packs))
	for _, fetch := range plan.Packs {
		for _, slice := range fetch.Pack.Slices {
			names = append(names, slice.FileName)
		}
	}

	links, err := r.store.SignedLinks(ctx, names)
	if err != nil {
		return 0, nil, err
	}

	var (
		transferred atomic.Int64
		done        atomic.Int64
		failures    failureList
		started     = time.Now()
	)

	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(r.parallel())

	for _, fetch := range plan.Packs {
		group.Go(func() error {
			bytes, err := r.fetchPack(groupCtx, fetch, links)
			if err != nil {
				if ctxErr := groupCtx.Err(); ctxErr != nil {
					return ctxErr
				}

				// One pack failing must not abandon the others: what lands is verified and
				// complete, and a re-run picks up exactly the remainder.
				r.reporter.Warn(fmt.Sprintf("pack %s: %v", fetch.Pack.ID, err))
				failures.addAll(pathsOf(fetch, plan))

				return nil
			}

			transferred.Add(bytes)

			r.reporter.Update("fetch", fmt.Sprintf("[%s] %d/%d packs  %s  %s",
				progress.Bar(done.Add(1), int64(len(plan.Packs)), 24),
				done.Load(), len(plan.Packs),
				progress.HumanBytes(transferred.Load()),
				progress.Rate(transferred.Load(), time.Since(started))))

			return nil
		})
	}

	if err := group.Wait(); err != nil {
		return transferred.Load(), failures.list(), err
	}

	r.reporter.Phase("fetch", fmt.Sprintf("%s transferred from %s packs",
		progress.HumanBytes(transferred.Load()), progress.Count(len(plan.Packs))))

	return transferred.Load(), failures.list(), nil
}

// pathsOf returns the manifest paths that depend on a pack, so a failure can be reported in
// terms the user recognizes.
func pathsOf(fetch PackFetch, plan *DownloadPlan) []string {
	var paths []string

	for digest := range fetch.Wanted {
		for _, entry := range plan.Needed[digest] {
			paths = append(paths, entry.Path)
		}
	}

	return paths
}

// failureList collects the paths a concurrent fetch could not produce.
type failureList struct {
	mu    sync.Mutex
	paths []string
}

func (f *failureList) addAll(paths []string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.paths = append(f.paths, paths...)
}

func (f *failureList) list() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	sorted := make([]string, len(f.paths))
	copy(sorted, f.paths)
	sort.Strings(sorted)

	return sorted
}
