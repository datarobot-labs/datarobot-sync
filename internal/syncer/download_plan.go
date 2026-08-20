// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

package syncer

import (
	"fmt"
	"sort"
	"strings"

	"github.com/datarobot/golang-datarobot-filesync/internal/manifest"
	"github.com/datarobot/golang-datarobot-filesync/internal/packer"
	"github.com/datarobot/golang-datarobot-filesync/internal/scan"
)

// plan decides what a pull has to do, before anything is transferred.
func (r *downloadRun) plan() (*DownloadPlan, error) {
	plan := &DownloadPlan{Needed: map[string][]manifest.Entry{}}

	index := manifest.NewIndex(r.manifest)
	wanted := map[string]struct{}{}
	included := map[string]bool{}

	for _, entry := range r.manifest.Entries {
		if !r.wants(entry.Path) {
			continue
		}

		if reason := r.unusable(entry); reason != "" {
			plan.Skipped = append(plan.Skipped, scan.Skip{Path: entry.Path, Reason: reason})

			continue
		}

		included[entry.Path] = true

		r.classifyEntry(entry, plan, wanted)
	}

	r.collectPacks(plan, index, wanted)
	plan.Extraneous = r.extraneous(included)

	return plan, nil
}

// wants reports whether a manifest path is in scope for this pull.
func (r *downloadRun) wants(path string) bool {
	if r.opts.Matcher != nil && (r.opts.Matcher.Match(path, false) || r.opts.Matcher.MatchesAncestor(path)) {
		return false
	}

	if len(r.opts.Prefixes) == 0 {
		return true
	}

	for _, prefix := range r.opts.Prefixes {
		trimmed := strings.TrimSuffix(prefix, "/")

		if path == trimmed || strings.HasPrefix(path, trimmed+"/") {
			return true
		}
	}

	return false
}

// unusable reports why an entry cannot be written on this machine, or "".
//
// Every one of these is a way a crafted or merely inconvenient manifest could otherwise write
// somewhere it should not, or silently produce the wrong tree.
func (r *downloadRun) unusable(entry manifest.Entry) string {
	if err := manifest.ValidatePath(entry.Path); err != nil {
		return err.Error()
	}

	if strings.HasPrefix(entry.Path, strings.TrimSuffix(r.store.MetaPrefix(), "/")+"/") {
		return "path is inside the reserved dr-sync prefix"
	}

	if entry.Path == scan.TempDirName || strings.HasPrefix(entry.Path, scan.TempDirName+"/") {
		return "path collides with the download scratch directory"
	}

	if entry.Type == manifest.TypeSymlink && !r.opts.AllowUnsafeLinks {
		if reason := unsafeLinkTarget(entry); reason != "" {
			return reason
		}
	}

	return ""
}

// unsafeLinkTarget reports why a symlink target is not safe to create, or "".
func unsafeLinkTarget(entry manifest.Entry) string {
	target := entry.Target

	if strings.HasPrefix(target, "/") {
		return "symlink target is absolute"
	}

	// Resolve the target against the link's own directory to see where it actually lands.
	base := ""
	if idx := strings.LastIndex(entry.Path, "/"); idx >= 0 {
		base = entry.Path[:idx]
	}

	depth := 0

	for _, element := range strings.Split(base, "/") {
		if element != "" {
			depth++
		}
	}

	for _, element := range strings.Split(target, "/") {
		switch element {
		case "", ".":
		case "..":
			depth--

			if depth < 0 {
				return "symlink target escapes the sync root"
			}
		default:
			depth++
		}
	}

	return ""
}

// classifyEntry decides what has to happen for one manifest entry.
func (r *downloadRun) classifyEntry(entry manifest.Entry, plan *DownloadPlan, wanted map[string]struct{}) {
	local, present := r.local.ByPath[entry.Path]

	switch entry.Type {
	case manifest.TypeDir:
		if present && local.Type == manifest.TypeDir && local.Mode == entry.Mode {
			return
		}

		plan.Dirs = append(plan.Dirs, entry)

		return

	case manifest.TypeSymlink:
		if present && local.Type == manifest.TypeSymlink && local.Target == entry.Target {
			return
		}

		plan.Links = append(plan.Links, entry)

		return

	case manifest.TypeFile:
		r.classifyFile(entry, local, present, plan, wanted)
	}
}

// classifyFile decides where a file's content will come from: nowhere, a local sibling, or a
// pack.
func (r *downloadRun) classifyFile(entry manifest.Entry, local manifest.Entry, present bool, plan *DownloadPlan, wanted map[string]struct{}) {
	if entry.Size == 0 {
		if present && local.Type == manifest.TypeFile && local.Size == 0 {
			r.noteFixup(entry, local, plan)

			return
		}

		plan.Empties = append(plan.Empties, entry)

		return
	}

	if present && local.Type == manifest.TypeFile && local.SHA256 == entry.SHA256 {
		plan.UpToDate = append(plan.UpToDate, entry)
		r.noteFixup(entry, local, plan)

		return
	}

	// The content may already be on disk under a different path: a rename upstream, a
	// duplicated asset, or a revert. Cloning it locally beats fetching it again.
	if !r.opts.NoLocalReuse {
		if sources := r.local.BySHA[entry.SHA256]; len(sources) > 0 {
			plan.LocalCopies = append(plan.LocalCopies, LocalCopy{
				Entry:  entry,
				Source: r.local.SourcePath[entry.SHA256],
			})

			return
		}
	}

	plan.Needed[entry.SHA256] = append(plan.Needed[entry.SHA256], entry)
	plan.NeededBytes += entry.Size
	wanted[entry.SHA256] = struct{}{}
}

// noteFixup records an entry whose content is right but whose metadata is not.
func (r *downloadRun) noteFixup(entry, local manifest.Entry, plan *DownloadPlan) {
	if local.Mode == entry.Mode && local.ModTime == entry.ModTime {
		return
	}

	plan.ModeFixups = append(plan.ModeFixups, entry)
}

// collectPacks works out which packs have to be read, and how much of each.
func (r *downloadRun) collectPacks(plan *DownloadPlan, index *manifest.Index, wanted map[string]struct{}) {
	byPack := map[int]map[string]struct{}{}

	for digest := range wanted {
		_, position, ok := index.PackForBlob(digest)
		if !ok {
			// The manifest passed validation, so this cannot normally happen; treat it as a
			// missing file rather than pretending it will appear.
			for _, entry := range plan.Needed[digest] {
				plan.Skipped = append(plan.Skipped, scan.Skip{
					Path:   entry.Path,
					Reason: "no pack in the manifest holds this content",
				})
			}

			delete(plan.Needed, digest)

			continue
		}

		if byPack[position] == nil {
			byPack[position] = map[string]struct{}{}
		}

		byPack[position][digest] = struct{}{}
	}

	positions := make([]int, 0, len(byPack))
	for position := range byPack {
		positions = append(positions, position)
	}

	sort.Ints(positions)

	for _, position := range positions {
		pack := r.manifest.Packs[position]
		digests := byPack[position]

		lastNeeded := packer.LastNeededIndex(pack, digests)
		fraction := packer.NeededFraction(pack, lastNeeded)

		plan.Packs = append(plan.Packs, PackFetch{
			Pack:       pack,
			Wanted:     digests,
			LastNeeded: lastNeeded,
			Fraction:   fraction,
		})

		plan.EstimatedGet += int64(fraction * float64(pack.CompressedSize))
	}
}

// extraneous lists local paths the manifest does not mention.
func (r *downloadRun) extraneous(included map[string]bool) []string {
	var paths []string

	for _, entry := range r.local.Entries {
		if included[entry.Path] || !r.wants(entry.Path) {
			continue
		}

		paths = append(paths, entry.Path)
	}

	sort.Strings(paths)

	return paths
}

// describeFetch renders one pack's early-abort saving, for verbose output.
func describeFetch(fetch PackFetch) string {
	return fmt.Sprintf("%s  needs %d/%d members  reads %.0f%% of %s",
		fetch.Pack.ID, len(fetch.Wanted), len(fetch.Pack.Members),
		fetch.Fraction*100, manifest.HumanBytes(fetch.Pack.CompressedSize))
}
