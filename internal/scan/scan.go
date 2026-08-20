// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

// Package scan derives the local half of a sync: what is on disk, and what its content
// digests are.
//
// dr-sync keeps no persistent local state, so this runs on every invocation. That makes the
// cost of hashing the dominant cost of a no-op run, which is why CheckAuto exists: the remote
// manifest records size, mode and mtime per path, so an unchanged file can be recognized
// without reading it — the same baseline rsync gets from its destination.
package scan

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/datarobot/golang-datarobot-filesync/internal/manifest"
)

// CheckMode decides how much work goes into deciding a file has changed.
type CheckMode string

const (
	// CheckAuto hashes only files whose size, mode or mtime differ from the manifest. The
	// default.
	CheckAuto CheckMode = "auto"
	// CheckFull hashes every file, whatever the metadata says.
	CheckFull CheckMode = "full"
	// CheckSize trusts size alone. Fast and wrong for in-place edits that preserve length.
	CheckSize CheckMode = "size"
	// CheckMTime trusts size and mtime and never hashes. Fast, and wrong for a file whose
	// mtime was restored after an edit.
	CheckMTime CheckMode = "mtime"
)

// ParseCheckMode validates a mode name.
func ParseCheckMode(value string) (CheckMode, error) {
	switch CheckMode(value) {
	case CheckAuto, CheckFull, CheckSize, CheckMTime:
		return CheckMode(value), nil
	default:
		return "", fmt.Errorf("unknown check mode %q: want auto, full, size or mtime", value)
	}
}

// Options configures a scan.
type Options struct {
	// Root is the directory to walk.
	Root string
	// MetaPrefix is dr-sync's reserved directory, excluded at the root only.
	MetaPrefix string
	// Matcher decides exclusions. May be nil.
	Matcher *Matcher
	// Baseline is the manifest to compare metadata against for CheckAuto. May be nil.
	Baseline *manifest.Index
	Check    CheckMode
	// Jobs is the number of concurrent hashers. Zero picks a default.
	Jobs int
	// FollowSymlinks stores the content a link points at instead of the link itself.
	FollowSymlinks bool
	// Warn receives non-fatal problems: skipped device files, unreadable entries.
	Warn func(string)
}

// Result is the local tree as the manifest would describe it.
type Result struct {
	Entries []manifest.Entry
	// ByPath indexes Entries.
	ByPath map[string]manifest.Entry
	// BySHA maps a content digest to every local path holding it. This is what lets a
	// download satisfy a "missing" file by copying from a local sibling instead of
	// downloading it.
	BySHA map[string][]string
	// SourcePath maps a digest to a local absolute path that currently holds it, for the
	// packer to read from.
	SourcePath map[string]string
	// Hashed and Reused count files read versus adopted from the baseline's metadata.
	Hashed     int
	HashedSize int64
	Reused     int
	// Skipped names paths that were deliberately not synced, with the reason.
	Skipped []Skip
}

// Skip records a path that was not synced and why.
type Skip struct {
	Path   string
	Reason string
}

// Walk scans the tree described by opts.
func Walk(ctx context.Context, opts Options) (*Result, error) {
	root, err := filepath.Abs(opts.Root)
	if err != nil {
		return nil, fmt.Errorf("resolving %s: %w", opts.Root, err)
	}

	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", opts.Root, err)
	}

	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", opts.Root)
	}

	candidates, skipped, err := collect(root, opts)
	if err != nil {
		return nil, err
	}

	result := &Result{
		ByPath:     make(map[string]manifest.Entry, len(candidates)),
		BySHA:      map[string][]string{},
		SourcePath: map[string]string{},
		Skipped:    skipped,
	}

	if err := hashAll(ctx, root, candidates, opts, result); err != nil {
		return nil, err
	}

	manifest.SortEntries(result.Entries)

	for _, entry := range result.Entries {
		result.ByPath[entry.Path] = entry
	}

	if err := checkCaseCollisions(result); err != nil {
		return nil, err
	}

	return result, nil
}

// candidate is one path found by the walk, before hashing.
type candidate struct {
	relPath string
	absPath string
	info    fs.FileInfo
	kind    manifest.EntryType
	target  string
}

// collect walks the tree and classifies what it finds, without reading any content.
func collect(root string, opts Options) ([]candidate, []Skip, error) {
	walker := &treeWalker{
		root:    root,
		opts:    opts,
		metaDir: metaDirName(opts.MetaPrefix),
	}

	if err := filepath.WalkDir(root, walker.visit); err != nil {
		return nil, nil, err
	}

	return walker.candidates, walker.skipped, nil
}

// treeWalker accumulates a walk's findings.
type treeWalker struct {
	root       string
	opts       Options
	metaDir    string
	candidates []candidate
	skipped    []Skip
}

func (w *treeWalker) visit(absPath string, dirEntry fs.DirEntry, err error) error {
	if err != nil {
		// An unreadable directory should not abort a whole sync; report it and continue.
		w.skip(Skip{Path: absPath, Reason: err.Error()})

		return nil
	}

	if absPath == w.root {
		return nil
	}

	relPath, relErr := filepath.Rel(w.root, absPath)
	if relErr != nil {
		return fmt.Errorf("resolving %s under %s: %w", absPath, w.root, relErr)
	}

	normalized := Normalize(relPath)

	if w.excluded(normalized, dirEntry.IsDir()) {
		if dirEntry.IsDir() {
			return filepath.SkipDir
		}

		return nil
	}

	found, skip, keepErr := classify(normalized, absPath, dirEntry, w.opts)
	if keepErr != nil {
		return keepErr
	}

	if skip != nil {
		w.skip(*skip)

		return nil
	}

	w.candidates = append(w.candidates, found)

	return nil
}

// excluded reports whether a path is out of scope: dr-sync's own state, the download scratch
// directory, or something the ignore rules exclude.
func (w *treeWalker) excluded(normalized string, isDir bool) bool {
	// The reserved directory and the scratch directory are matched at the root only, so a
	// nested vendor/x/.dr-sync/ of the user's own still syncs normally.
	for _, reserved := range [...]string{w.metaDir, TempDirName} {
		if normalized == reserved || strings.HasPrefix(normalized, reserved+"/") {
			return true
		}
	}

	return w.opts.Matcher != nil && w.opts.Matcher.Match(normalized, isDir)
}

func (w *treeWalker) skip(skip Skip) {
	w.skipped = append(w.skipped, skip)
	warn(w.opts, fmt.Sprintf("skipping %s: %s", skip.Path, skip.Reason))
}

// metaDirName returns the reserved directory name without its trailing slash.
func metaDirName(metaPrefix string) string {
	trimmed := strings.TrimSuffix(metaPrefix, "/")
	if trimmed == "" {
		return strings.TrimSuffix(manifest.DefaultMetaPrefix, "/")
	}

	return trimmed
}

// classify decides what kind of entry a directory entry becomes, or why it is skipped.
func classify(relPath, absPath string, dirEntry fs.DirEntry, opts Options) (candidate, *Skip, error) {
	if err := manifest.ValidatePath(relPath); err != nil {
		return candidate{}, &Skip{Path: relPath, Reason: err.Error()}, nil
	}

	if reason := UnwritableOnWindows(relPath); reason != "" {
		warn(opts, fmt.Sprintf("%s: %s; a Windows client will not be able to extract it", relPath, reason))
	}

	if dirEntry.IsDir() {
		info, err := dirEntry.Info()
		if err != nil {
			return candidate{}, &Skip{Path: relPath, Reason: err.Error()}, nil
		}

		return candidate{relPath: relPath, absPath: absPath, info: info, kind: manifest.TypeDir}, nil, nil
	}

	if dirEntry.Type()&fs.ModeSymlink != 0 && !opts.FollowSymlinks {
		target, err := os.Readlink(absPath)
		if err != nil {
			return candidate{}, &Skip{Path: relPath, Reason: err.Error()}, nil
		}

		return candidate{relPath: relPath, absPath: absPath, kind: manifest.TypeSymlink, target: target}, nil, nil
	}

	return classifyFile(relPath, absPath)
}

// classifyFile stats a non-directory entry and accepts it only if it is a regular file.
// Stat rather than Lstat, so --follow-symlinks reaches the target's metadata.
func classifyFile(relPath, absPath string) (candidate, *Skip, error) {
	info, err := os.Stat(absPath)
	if err != nil {
		return candidate{}, &Skip{Path: relPath, Reason: err.Error()}, nil
	}

	if info.IsDir() {
		// A followed symlink to a directory. Descending would risk a cycle and duplicated
		// content, so it is reported rather than traversed.
		return candidate{}, &Skip{Path: relPath, Reason: "symlink to a directory; not followed"}, nil
	}

	if !info.Mode().IsRegular() {
		// Fifos, sockets and devices have no content to sync. A hardlink is a regular file
		// and is stored independently, which content addressing makes free anyway.
		return candidate{}, &Skip{
			Path:   relPath,
			Reason: fmt.Sprintf("not a regular file (%s)", info.Mode().Type()),
		}, nil
	}

	return candidate{relPath: relPath, absPath: absPath, info: info, kind: manifest.TypeFile}, nil, nil
}

// checkCaseCollisions rejects a tree that cannot be materialized faithfully on a
// case-insensitive filesystem.
func checkCaseCollisions(result *Result) error {
	paths := make([]string, 0, len(result.Entries))
	for _, entry := range result.Entries {
		paths = append(paths, entry.Path)
	}

	collisions := CaseCollisions(paths)
	if len(collisions) == 0 {
		return nil
	}

	sort.Slice(collisions, func(i, j int) bool { return collisions[i][0] < collisions[j][0] })

	var report strings.Builder

	report.WriteString("paths differ only in case, so they cannot coexist on a case-insensitive filesystem:")

	for _, group := range collisions {
		sort.Strings(group)
		fmt.Fprintf(&report, "\n  %s", strings.Join(group, "  vs  "))
	}

	return errors.New(report.String())
}

// TempDirName is the scratch directory a download uses under the sync root. It has to be
// under the root so the final rename is same-filesystem and therefore atomic, which in turn
// means the walker must know to skip it.
const TempDirName = ".dr-sync-tmp"

// DefaultJobs is the hashing concurrency when none is requested. Capped because hashing is
// I/O bound and more readers past a point just add seek contention.
func DefaultJobs() int {
	return min(runtime.GOMAXPROCS(0), 8)
}

// hashAll fills in digests for the candidate files, in parallel.
func hashAll(ctx context.Context, root string, candidates []candidate, opts Options, result *Result) error {
	jobs := opts.Jobs
	if jobs <= 0 {
		jobs = DefaultJobs()
	}

	entries := make([]manifest.Entry, len(candidates))
	reused := make([]bool, len(candidates))
	hashedSizes := make([]int64, len(candidates))

	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(jobs)

	for i := range candidates {
		group.Go(func() error {
			if groupCtx.Err() != nil {
				return groupCtx.Err()
			}

			entry, wasReused, err := hashCandidate(candidates[i], opts)
			if err != nil {
				return err
			}

			entries[i] = entry
			reused[i] = wasReused

			if !wasReused && entry.Type == manifest.TypeFile {
				hashedSizes[i] = entry.Size
			}

			return nil
		})
	}

	if err := group.Wait(); err != nil {
		return err
	}

	result.Entries = entries
	tally(root, entries, reused, hashedSizes, result)

	return nil
}

// tally builds the digest indexes and the hashed/reused counters.
func tally(root string, entries []manifest.Entry, reused []bool, hashedSizes []int64, result *Result) {
	for i, entry := range entries {
		if entry.Type != manifest.TypeFile || entry.SHA256 == "" {
			continue
		}

		result.BySHA[entry.SHA256] = append(result.BySHA[entry.SHA256], entry.Path)

		if _, known := result.SourcePath[entry.SHA256]; !known {
			result.SourcePath[entry.SHA256] = filepath.Join(root, Denormalize(entry.Path))
		}

		if reused[i] {
			result.Reused++

			continue
		}

		result.Hashed++
		result.HashedSize += hashedSizes[i]
	}

	for digest := range result.BySHA {
		sort.Strings(result.BySHA[digest])
	}
}

// hashCandidate turns one candidate into a manifest entry, hashing only when it has to.
func hashCandidate(c candidate, opts Options) (manifest.Entry, bool, error) {
	switch c.kind {
	case manifest.TypeDir:
		return manifest.Entry{
			Path:    c.relPath,
			Type:    manifest.TypeDir,
			Mode:    uint32(c.info.Mode().Perm()),
			ModTime: formatModTime(c.info.ModTime()),
		}, false, nil

	case manifest.TypeSymlink:
		return manifest.Entry{
			Path:   c.relPath,
			Type:   manifest.TypeSymlink,
			Target: filepath.ToSlash(c.target),
		}, false, nil

	case manifest.TypeFile:
		// Handled below.
	}

	entry := manifest.Entry{
		Path:    c.relPath,
		Type:    manifest.TypeFile,
		Size:    c.info.Size(),
		Mode:    uint32(c.info.Mode().Perm()),
		ModTime: formatModTime(c.info.ModTime()),
	}

	if entry.Size == 0 {
		// An empty file needs no digest and no pack member.
		return entry, true, nil
	}

	if adopted, ok := adoptBaseline(entry, opts); ok {
		return adopted, true, nil
	}

	digest, err := HashFile(c.absPath)
	if err != nil {
		return manifest.Entry{}, false, err
	}

	entry.SHA256 = digest

	return entry, false, nil
}

// adoptBaseline reuses the digest the manifest already recorded when the metadata says the
// file cannot have changed.
func adoptBaseline(entry manifest.Entry, opts Options) (manifest.Entry, bool) {
	if opts.Check == CheckFull || opts.Baseline == nil {
		return entry, false
	}

	previous, ok := opts.Baseline.Entry(entry.Path)
	if !ok || previous.Type != manifest.TypeFile || previous.SHA256 == "" {
		return entry, false
	}

	if previous.Size != entry.Size {
		return entry, false
	}

	if !metadataMatches(opts.Check, previous, entry) {
		return entry, false
	}

	entry.SHA256 = previous.SHA256

	return entry, true
}

// metadataMatches reports whether the metadata is close enough, for the requested mode, to
// trust the recorded digest without reading the file.
func metadataMatches(mode CheckMode, previous, current manifest.Entry) bool {
	switch mode {
	case CheckSize:
		// Size alone was asked for; the caller has accepted that an edit preserving length
		// is invisible.
		return true

	case CheckMTime:
		return previous.ModTime == current.ModTime

	case CheckAuto:
		return previous.ModTime == current.ModTime && previous.Mode == current.Mode

	case CheckFull:
		// Never reached: CheckFull is rejected before we get here.
		return false

	default:
		return false
	}
}

// formatModTime renders a timestamp the way the manifest stores it.
func formatModTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func warn(opts Options, message string) {
	if opts.Warn != nil {
		opts.Warn(message)
	}
}
