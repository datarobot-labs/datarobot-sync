// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

package manifest

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIndexLookups(t *testing.T) {
	t.Parallel()

	index := NewIndex(sample())

	entry, ok := index.Entry("src/main.go")
	require.True(t, ok)
	assert.EqualValues(t, 1500, entry.Size)

	_, ok = index.Entry("nope")
	assert.False(t, ok)

	pack, position, ok := index.PackByID("p_9f2a1c8d3e4f5061")
	require.True(t, ok)
	assert.Equal(t, 0, position)
	assert.Equal(t, CodecXZ, pack.Codec)

	_, _, ok = index.PackByID("p_missing")
	assert.False(t, ok)
}

// This is the lookup that makes a rename free: content already stored anywhere needs no
// upload, no matter which path it used to live at.
func TestHasBlobIgnoresPath(t *testing.T) {
	t.Parallel()

	index := NewIndex(sample())

	assert.True(t, index.HasBlob(strings.Repeat("b", 64)), "content referenced by an entry")
	assert.True(t, index.HasBlob(strings.Repeat("c", 64)),
		"content stored but unreferenced is still reusable without uploading it again")
	assert.False(t, index.HasBlob(strings.Repeat("d", 64)))

	_, position, ok := index.PackForBlob(strings.Repeat("b", 64))
	require.True(t, ok)
	assert.Equal(t, 0, position)
}

func TestLiveness(t *testing.T) {
	t.Parallel()

	stats := NewIndex(sample()).Liveness()
	require.Len(t, stats, 1)

	assert.EqualValues(t, 1500, stats[0].LiveBytes)
	assert.Equal(t, 1, stats[0].LiveCount)
	assert.EqualValues(t, 4000, stats[0].TotalBytes)
	assert.InDelta(t, 0.375, stats[0].Ratio, 0.001)
	assert.False(t, stats[0].Dead())
}

func TestGarbageClassifiesDeadAndSparsePacks(t *testing.T) {
	t.Parallel()

	m := New(DefaultMetaPrefix)
	m.Packs = []Pack{
		{
			ID: "p_dead", Codec: CodecXZ, CompressedSize: 100,
			Slices:  []Slice{{FileName: "x", Size: 100}},
			Members: []Member{{SHA256: strings.Repeat("1", 64), Size: 400}},
		},
		{
			ID: "p_sparse", Codec: CodecXZ, CompressedSize: 200,
			Slices: []Slice{{FileName: "y", Size: 200}},
			Members: []Member{
				{SHA256: strings.Repeat("2", 64), Size: 100},
				{SHA256: strings.Repeat("3", 64), Size: 900},
			},
		},
		{
			ID: "p_full", Codec: CodecXZ, CompressedSize: 300,
			Slices:  []Slice{{FileName: "z", Size: 300}},
			Members: []Member{{SHA256: strings.Repeat("4", 64), Size: 500}},
		},
	}
	m.Entries = []Entry{
		{Path: "kept-full", Type: TypeFile, Size: 500, SHA256: strings.Repeat("4", 64), Pack: packIndex(2)},
		{Path: "kept-sparse", Type: TypeFile, Size: 100, SHA256: strings.Repeat("2", 64), Pack: packIndex(1)},
	}
	SortEntries(m.Entries)
	require.NoError(t, m.Validate())

	report := NewIndex(m).Garbage(0.5)

	require.Len(t, report.Dead, 1)
	assert.Equal(t, "p_dead", report.Dead[0].Pack.ID)

	require.Len(t, report.Sparse, 1)
	assert.Equal(t, "p_sparse", report.Sparse[0].Pack.ID)

	// 400 dead in p_dead plus 900 dead in p_sparse.
	assert.EqualValues(t, 1300, report.DeadBytes)
	assert.EqualValues(t, 600, report.TotalCompressedSize)
	assert.Positive(t, report.DeadRatio())
}

func TestGarbageSortsSparsePacksWorstFirst(t *testing.T) {
	t.Parallel()

	m := New(DefaultMetaPrefix)
	m.Packs = []Pack{
		{
			ID: "p_half", Codec: CodecXZ, CompressedSize: 10,
			Slices: []Slice{{FileName: "a"}},
			Members: []Member{
				{SHA256: strings.Repeat("1", 64), Size: 50},
				{SHA256: strings.Repeat("2", 64), Size: 50},
			},
		},
		{
			ID: "p_tenth", Codec: CodecXZ, CompressedSize: 10,
			Slices: []Slice{{FileName: "b"}},
			Members: []Member{
				{SHA256: strings.Repeat("3", 64), Size: 10},
				{SHA256: strings.Repeat("4", 64), Size: 90},
			},
		},
	}
	m.Entries = []Entry{
		{Path: "a", Type: TypeFile, Size: 50, SHA256: strings.Repeat("1", 64), Pack: packIndex(0)},
		{Path: "b", Type: TypeFile, Size: 10, SHA256: strings.Repeat("3", 64), Pack: packIndex(1)},
	}
	require.NoError(t, m.Validate())

	report := NewIndex(m).Garbage(0.9)

	require.Len(t, report.Sparse, 2)
	assert.Equal(t, "p_tenth", report.Sparse[0].Pack.ID,
		"a bounded compaction should reclaim the most for the least reading")
}

func TestGarbageOnEmptyManifest(t *testing.T) {
	t.Parallel()

	report := NewIndex(New(DefaultMetaPrefix)).Garbage(0.5)

	assert.Empty(t, report.Dead)
	assert.Empty(t, report.Sparse)
	assert.Zero(t, report.DeadRatio())
}
