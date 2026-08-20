// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

package manifest

import (
	"sort"
)

// Index provides the lookups the sync algorithms need over a manifest.
type Index struct {
	manifest *Manifest
	byPath   map[string]Entry
	// blobPack maps a content digest to the index of the pack holding it. This is what makes
	// "has this content changed?" a map lookup, and what makes a rename cost zero bytes.
	blobPack map[string]int
	blobSize map[string]int64
	packByID map[string]int
}

// NewIndex builds the lookup tables for m.
func NewIndex(m *Manifest) *Index {
	index := &Index{
		manifest: m,
		byPath:   make(map[string]Entry, len(m.Entries)),
		blobPack: make(map[string]int),
		blobSize: make(map[string]int64),
		packByID: make(map[string]int, len(m.Packs)),
	}

	for i, pack := range m.Packs {
		index.packByID[pack.ID] = i

		for _, member := range pack.Members {
			// First pack wins: a blob duplicated across packs is possible after a
			// compaction that has not yet pruned, and either copy is equally valid.
			if _, ok := index.blobPack[member.SHA256]; !ok {
				index.blobPack[member.SHA256] = i
			}

			index.blobSize[member.SHA256] = member.Size
		}
	}

	for _, entry := range m.Entries {
		index.byPath[entry.Path] = entry
	}

	return index
}

// Manifest returns the indexed manifest.
func (i *Index) Manifest() *Manifest {
	return i.manifest
}

// Entry looks up a path.
func (i *Index) Entry(path string) (Entry, bool) {
	entry, ok := i.byPath[path]

	return entry, ok
}

// PackForBlob returns the pack holding a content digest.
func (i *Index) PackForBlob(sha string) (Pack, int, bool) {
	position, ok := i.blobPack[sha]
	if !ok {
		return Pack{}, 0, false
	}

	return i.manifest.Packs[position], position, true
}

// HasBlob reports whether the content is already stored in some pack, which is the test for
// "this file needs no upload" — regardless of what path it appeared at before.
func (i *Index) HasBlob(sha string) bool {
	_, ok := i.blobPack[sha]

	return ok
}

// PackByID returns a pack by its id.
func (i *Index) PackByID(id string) (Pack, int, bool) {
	position, ok := i.packByID[id]
	if !ok {
		return Pack{}, 0, false
	}

	return i.manifest.Packs[position], position, true
}

// LiveBlobs returns the set of content digests still referenced by an entry.
func (i *Index) LiveBlobs() map[string]struct{} {
	live := make(map[string]struct{}, len(i.manifest.Entries))

	for _, entry := range i.manifest.Entries {
		if entry.Type == TypeFile && entry.SHA256 != "" && entry.Size > 0 {
			live[entry.SHA256] = struct{}{}
		}
	}

	return live
}

// PackLiveness describes how much of a pack is still worth keeping.
type PackLiveness struct {
	Pack  Pack
	Index int
	// LiveBytes is the uncompressed size of members still referenced.
	LiveBytes int64
	// LiveCount is how many members are still referenced.
	LiveCount int
	// TotalBytes is the pack's uncompressed size as recorded in its member list.
	TotalBytes int64
	// Ratio is LiveBytes/TotalBytes, or 0 for an empty pack.
	Ratio float64
}

// Dead reports whether nothing in the pack is referenced any more, which makes it deletable
// with no data movement at all.
func (l PackLiveness) Dead() bool {
	return l.LiveCount == 0
}

// Liveness computes per-pack liveness against the manifest's own entries.
func (i *Index) Liveness() []PackLiveness {
	live := i.LiveBlobs()
	result := make([]PackLiveness, 0, len(i.manifest.Packs))

	for position, pack := range i.manifest.Packs {
		stats := PackLiveness{Pack: pack, Index: position}

		for _, member := range pack.Members {
			stats.TotalBytes += member.Size

			if _, referenced := live[member.SHA256]; referenced {
				stats.LiveBytes += member.Size
				stats.LiveCount++
			}
		}

		if stats.TotalBytes > 0 {
			stats.Ratio = float64(stats.LiveBytes) / float64(stats.TotalBytes)
		}

		result = append(result, stats)
	}

	return result
}

// GarbageReport summarizes what could be reclaimed.
type GarbageReport struct {
	// Dead packs hold nothing referenced and can be deleted outright.
	Dead []PackLiveness
	// Sparse packs are below the compaction threshold: reclaiming them requires rewriting
	// their live members, which costs a read.
	Sparse []PackLiveness
	// DeadBytes is the uncompressed size of unreferenced members across all packs.
	DeadBytes int64
	// DeadCompressedBytes estimates the container space those members occupy, prorating each
	// pack's compressed size by its dead fraction.
	DeadCompressedBytes int64
	TotalCompressedSize int64
}

// DeadRatio is the fraction of pack bytes that are no longer referenced.
func (r GarbageReport) DeadRatio() float64 {
	if r.TotalCompressedSize == 0 {
		return 0
	}

	return float64(r.DeadCompressedBytes) / float64(r.TotalCompressedSize)
}

// Garbage classifies packs against a compaction threshold, expressed as the minimum live
// fraction a pack must retain to be left alone.
func (i *Index) Garbage(threshold float64) GarbageReport {
	report := GarbageReport{}

	for _, stats := range i.Liveness() {
		report.TotalCompressedSize += stats.Pack.CompressedSize

		deadBytes := stats.TotalBytes - stats.LiveBytes
		report.DeadBytes += deadBytes

		if stats.TotalBytes > 0 {
			deadFraction := float64(deadBytes) / float64(stats.TotalBytes)
			report.DeadCompressedBytes += int64(deadFraction * float64(stats.Pack.CompressedSize))
		}

		switch {
		case stats.Dead():
			report.Dead = append(report.Dead, stats)
		case stats.Ratio < threshold:
			report.Sparse = append(report.Sparse, stats)
		}
	}

	// Worst first, so a bounded compaction reclaims the most for the least reading.
	sort.Slice(report.Sparse, func(a, b int) bool {
		return report.Sparse[a].Ratio < report.Sparse[b].Ratio
	})

	return report
}

// RecomputeStats refreshes the derived stats block from the entries and packs.
func (m *Manifest) RecomputeStats() {
	stats, blobs := m.entryStats()

	stats.UniqueBlobCount = len(blobs)
	stats.PackCount = len(m.Packs)

	for _, pack := range m.Packs {
		stats.PackCompressedBytes += pack.CompressedSize

		for _, member := range pack.Members {
			if _, referenced := blobs[member.SHA256]; referenced {
				stats.LiveBytes += member.Size
			} else {
				stats.DeadBytes += member.Size
			}
		}
	}

	m.Stats = stats
}

// entryStats counts the entries and collects the set of digests they still reference.
func (m *Manifest) entryStats() (Stats, map[string]struct{}) {
	stats := Stats{}
	blobs := map[string]struct{}{}

	for _, entry := range m.Entries {
		switch entry.Type {
		case TypeFile:
			stats.FileCount++
			stats.TotalSize += entry.Size

			if entry.SHA256 != "" && entry.Size > 0 {
				blobs[entry.SHA256] = struct{}{}
			}

		case TypeDir:
			stats.DirCount++

		case TypeSymlink:
			stats.SymlinkCount++
		}
	}

	return stats, blobs
}
