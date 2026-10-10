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
	"sort"

	"github.com/datarobot-labs/datarobot-sync/internal/manifest"
	"github.com/datarobot-labs/datarobot-sync/internal/packer"
	"github.com/datarobot-labs/datarobot-sync/internal/scan"
)

// UploadPlan is the decided outcome of comparing a local tree against the remote manifest.
type UploadPlan struct {
	// Entries is what the next manifest will contain: exactly the local tree, plus any
	// remote-only paths that are being kept.
	Entries []manifest.Entry

	New        int
	Changed    int
	Unchanged  int
	RemoteOnly int
	// Dropped counts remote-only paths removed from the manifest, which happens only with
	// --delete.
	Dropped int

	// NeedUpload is the deduplicated content that is not stored anywhere yet.
	NeedUpload []packer.Blob
	NeedBytes  int64

	// ReusedBlobs counts content that is new at this path but already stored — a rename, a
	// copy, or a revert. These transfer nothing at all.
	ReusedBlobs int
	ReusedBytes int64
}

// Empty reports whether the plan would change nothing.
func (p *UploadPlan) Empty() bool {
	return p.New == 0 && p.Changed == 0 && p.Dropped == 0 && len(p.NeedUpload) == 0
}

// ClassifyOptions configures the comparison.
type ClassifyOptions struct {
	// Delete drops remote-only paths from the new manifest. Without it, an upload is purely
	// additive, which is the safer default for a tool that can be pointed at a partial tree.
	Delete bool
}

// Classify compares a scanned local tree against the remote manifest.
//
// The interesting decision is not new-versus-changed but whether any bytes have to move: a
// file that is new at this path, whose content is already stored under some other path, needs
// nothing uploaded. That is what makes a rename or a revert free.
func Classify(local *scan.Result, remote *manifest.Index, opts ClassifyOptions) *UploadPlan {
	plan := &UploadPlan{}

	needed := map[string]packer.Blob{}

	for _, entry := range local.Entries {
		plan.count(entry, remote)
		plan.Entries = append(plan.Entries, entry)
		plan.considerContent(entry, local, remote, needed)
	}

	plan.RemoteOnly, plan.Dropped = carryRemoteOnly(plan, local, remote, opts)

	manifest.SortEntries(plan.Entries)

	plan.NeedUpload = make([]packer.Blob, 0, len(needed))
	for _, blob := range needed {
		plan.NeedUpload = append(plan.NeedUpload, blob)
		plan.NeedBytes += blob.Size
	}

	sort.Slice(plan.NeedUpload, func(i, j int) bool {
		return plan.NeedUpload[i].SHA256 < plan.NeedUpload[j].SHA256
	})

	return plan
}

// count tallies one local entry against the remote state.
func (p *UploadPlan) count(entry manifest.Entry, remote *manifest.Index) {
	previous, existed := remote.Entry(entry.Path)

	switch {
	case !existed:
		p.New++
	case sameEntry(previous, entry):
		p.Unchanged++
	default:
		p.Changed++
	}
}

// considerContent decides whether an entry's bytes have to move, adding them to needed when
// they do.
func (p *UploadPlan) considerContent(entry manifest.Entry, local *scan.Result, remote *manifest.Index, needed map[string]packer.Blob) {
	if entry.Type != manifest.TypeFile || entry.Size == 0 {
		return
	}

	if remote.HasBlob(entry.SHA256) {
		previous, existed := remote.Entry(entry.Path)

		if !existed || previous.SHA256 != entry.SHA256 {
			// Already stored, just not at this path: a rename, a copy or a revert, and no
			// bytes move.
			p.ReusedBlobs++
			p.ReusedBytes += entry.Size
		}

		return
	}

	if _, already := needed[entry.SHA256]; already {
		return
	}

	needed[entry.SHA256] = packer.Blob{
		SHA256:     entry.SHA256,
		Size:       entry.Size,
		SourcePath: local.SourcePath[entry.SHA256],
	}
}

// carryRemoteOnly decides what happens to paths the remote has and the local tree does not.
func carryRemoteOnly(plan *UploadPlan, local *scan.Result, remote *manifest.Index, opts ClassifyOptions) (remoteOnly, dropped int) {
	for _, entry := range remote.Manifest().Entries {
		if _, present := local.ByPath[entry.Path]; present {
			continue
		}

		remoteOnly++

		if opts.Delete {
			dropped++

			continue
		}

		plan.Entries = append(plan.Entries, entry)
	}

	return remoteOnly, dropped
}

// sameEntry reports whether two entries describe identical state.
func sameEntry(a, b manifest.Entry) bool {
	if a.Type != b.Type {
		return false
	}

	switch a.Type {
	case manifest.TypeFile:
		return a.SHA256 == b.SHA256 && a.Size == b.Size && a.Mode == b.Mode
	case manifest.TypeSymlink:
		return a.Target == b.Target
	case manifest.TypeDir:
		return a.Mode == b.Mode
	default:
		return false
	}
}

// BuildManifest assembles the next manifest from a plan, the packs that were just written, and
// the parent's packs.
//
// Only packs that still hold referenced content are carried forward. A pack whose every member
// has been superseded simply stops being mentioned, which is what turns it into collectable
// garbage without any explicit bookkeeping.
func BuildManifest(parent *manifest.Manifest, plan *UploadPlan, fresh []manifest.Pack, compression manifest.Compression) *manifest.Manifest {
	next := parent.Child()
	next.Compression = compression

	parentIndex := manifest.NewIndex(parent)

	freshByBlob := map[string]int{}

	for i, pack := range fresh {
		for _, member := range pack.Members {
			freshByBlob[member.SHA256] = i
		}
	}

	positions := map[string]int{}

	packFor := func(digest string) (int, bool) {
		if i, ok := freshByBlob[digest]; ok {
			return appendPack(next, positions, fresh[i]), true
		}

		if pack, _, ok := parentIndex.PackForBlob(digest); ok {
			return appendPack(next, positions, pack), true
		}

		return 0, false
	}

	next.Entries = make([]manifest.Entry, 0, len(plan.Entries))

	for _, entry := range plan.Entries {
		entry.Pack = nil

		if entry.Type == manifest.TypeFile && entry.Size > 0 {
			position, ok := packFor(entry.SHA256)
			if !ok {
				// Content with nowhere to live cannot be recorded; dropping the entry keeps
				// the manifest self-consistent, and validation would otherwise reject it.
				continue
			}

			entry.Pack = &position
		}

		next.Entries = append(next.Entries, entry)
	}

	manifest.SortEntries(next.Entries)
	next.RecomputeStats()

	return next
}

// appendPack adds a pack to the manifest if it is not already there, returning its index.
func appendPack(m *manifest.Manifest, positions map[string]int, pack manifest.Pack) int {
	if position, ok := positions[pack.ID]; ok {
		return position
	}

	position := len(m.Packs)
	m.Packs = append(m.Packs, pack)
	positions[pack.ID] = position

	return position
}
