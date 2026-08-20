// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

package manifest

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func packIndex(i int) *int {
	return &i
}

// sample builds a small but structurally complete manifest.
func sample() *Manifest {
	m := New(DefaultMetaPrefix)
	m.Generation = 42
	m.ID = "0000000042-7f3a9c2b"
	m.Packs = []Pack{{
		ID:               "p_9f2a1c8d3e4f5061",
		Codec:            CodecXZ,
		SHA256:           strings.Repeat("a", 64),
		CompressedSize:   1000,
		UncompressedSize: 4000,
		Slices: []Slice{{
			FileName: ".dr-sync/packs/p_9f2a1c8d3e4f5061.tar.xz",
			Size:     1000,
			SHA256:   strings.Repeat("a", 64),
		}},
		Members: []Member{
			{SHA256: strings.Repeat("b", 64), Size: 1500},
			{SHA256: strings.Repeat("c", 64), Size: 2500},
		},
	}}
	m.Entries = []Entry{
		{Path: "docs", Type: TypeDir, Mode: 0o755},
		{Path: "docs/latest", Type: TypeSymlink, Target: "../README.md"},
		{Path: "empty.txt", Type: TypeFile, Size: 0, Mode: 0o644},
		{
			Path: "src/main.go", Type: TypeFile, Size: 1500, Mode: 0o644,
			SHA256: strings.Repeat("b", 64), Pack: packIndex(0),
			ModTime: "2026-08-19T10:00:00.123456789Z",
		},
	}
	SortEntries(m.Entries)
	m.RecomputeStats()

	return m
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()

	original := sample()

	encoded, err := Encode(original)
	require.NoError(t, err)

	// The manifest is fetched on every run, so its compression is not decorative.
	assert.Less(t, len(encoded), 1200, "an xz-compressed manifest should be compact")

	decoded, err := DecodeBytes(encoded)
	require.NoError(t, err)

	assert.Equal(t, original.Generation, decoded.Generation)
	assert.Equal(t, original.ID, decoded.ID)
	assert.Len(t, decoded.Entries, 4)
	assert.Equal(t, original.Packs, decoded.Packs)
}

// An empty file carries no digest and no pack, so it must survive validation and round-trip
// without one.
func TestEmptyFileNeedsNoPack(t *testing.T) {
	t.Parallel()

	m := New(DefaultMetaPrefix)
	m.Entries = []Entry{{Path: "empty", Type: TypeFile, Size: 0, Mode: 0o644}}

	require.NoError(t, m.Validate())

	encoded, err := Encode(m)
	require.NoError(t, err)

	decoded, err := DecodeBytes(encoded)
	require.NoError(t, err)
	assert.True(t, decoded.Entries[0].IsEmptyFile())
}

func TestDecodeRejectsNewerFormat(t *testing.T) {
	t.Parallel()

	m := sample()
	m.FormatVersion = FormatVersion + 1

	// Encode validates, so build the payload by hand to simulate a future writer.
	var compressed bytes.Buffer

	writer, err := XZWriter(&compressed, 0)
	require.NoError(t, err)

	_, err = io.WriteString(writer, `{"formatVersion":99,"entries":[],"packs":[]}`)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	_, err = DecodeBytes(compressed.Bytes())
	require.ErrorIs(t, err, ErrUnsupportedVersion)
}

func TestDecodeRejectsNonXZ(t *testing.T) {
	t.Parallel()

	_, err := DecodeBytes([]byte(`{"formatVersion":1}`))
	require.ErrorContains(t, err, "not valid xz")
}

func TestValidateCatchesStructuralProblems(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		mutate  func(*Manifest)
		wantErr string
	}{
		{
			name:    "unsorted entries",
			mutate:  func(m *Manifest) { m.Entries[0], m.Entries[3] = m.Entries[3], m.Entries[0] },
			wantErr: "must be sorted",
		},
		{
			name: "duplicate paths",
			mutate: func(m *Manifest) {
				m.Entries = []Entry{
					{Path: "a", Type: TypeDir, Mode: 0o755},
					{Path: "a", Type: TypeDir, Mode: 0o755},
				}
			},
			wantErr: "sorted and unique",
		},
		{
			name:    "pack index out of range",
			mutate:  func(m *Manifest) { m.Entries[3].Pack = packIndex(7) },
			wantErr: "out of 1 packs",
		},
		{
			name:    "file with no digest",
			mutate:  func(m *Manifest) { m.Entries[3].SHA256 = "" },
			wantErr: "has no sha256",
		},
		{
			name:    "symlink with no target",
			mutate:  func(m *Manifest) { m.Entries[1].Target = "" },
			wantErr: "has no target",
		},
		{
			name:    "unknown entry type",
			mutate:  func(m *Manifest) { m.Entries[0].Type = "socket" },
			wantErr: "unknown type",
		},
		{
			name:    "pack with no slices",
			mutate:  func(m *Manifest) { m.Packs[0].Slices = nil },
			wantErr: "no slices",
		},
		{
			name:    "pack with unknown codec",
			mutate:  func(m *Manifest) { m.Packs[0].Codec = "brotli" },
			wantErr: "unknown codec",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			m := sample()
			tc.mutate(m)

			require.ErrorContains(t, m.Validate(), tc.wantErr)
		})
	}
}

// Every one of these is a way a crafted manifest could write outside the sync root.
func TestValidatePathRejectsEscapes(t *testing.T) {
	t.Parallel()

	bad := []string{
		"", "/etc/passwd", "../outside", "a/../../b", "./a", ".", "..",
		"a/", "a//b", "a\x00b", `a\b`,
	}

	for _, p := range bad {
		require.Error(t, ValidatePath(p), "path %q must be rejected", p)
	}

	good := []string{"a", "a/b", "a/b/c.txt", ".hidden", "a/.hidden/b", "spaces in name"}

	for _, p := range good {
		require.NoError(t, ValidatePath(p), "path %q must be accepted", p)
	}
}

func TestObjectNaming(t *testing.T) {
	t.Parallel()

	assert.Equal(t, ".dr-sync/manifests/", ManifestsPrefix(DefaultMetaPrefix))
	assert.Equal(t, ".dr-sync/packs/", PacksPrefix(DefaultMetaPrefix))
	assert.Equal(t, ".dr-sync/locks/", LocksPrefix(DefaultMetaPrefix))
	assert.Equal(t, ".dr-sync/README.txt", ReadmePath(DefaultMetaPrefix))

	// A configured prefix must work with or without its trailing slash.
	assert.Equal(t, "dr-sync/packs/", PacksPrefix("dr-sync"))
	assert.Equal(t, "dr-sync/packs/", PacksPrefix("dr-sync/"))

	assert.Equal(t, ".dr-sync/manifests/0000000042-7f3a9c2b.json.xz",
		ObjectName(DefaultMetaPrefix, "0000000042-7f3a9c2b"))

	// A single-slice pack keeps the plain name; only a split pack gets numeric suffixes.
	assert.Equal(t, ".dr-sync/packs/p_abc.tar.xz", SliceName(DefaultMetaPrefix, "p_abc", 0, 1))
	assert.Equal(t, ".dr-sync/packs/p_abc.tar.xz.000", SliceName(DefaultMetaPrefix, "p_abc", 0, 3))
	assert.Equal(t, ".dr-sync/packs/p_abc.tar.xz.012", SliceName(DefaultMetaPrefix, "p_abc", 12, 20))
}

func TestNewIDSortsLexicographicallyByGeneration(t *testing.T) {
	t.Parallel()

	// Zero padding is what makes the lexicographic maximum the numeric maximum; without it
	// generation 9 would beat generation 10.
	nine, err := NewID(9)
	require.NoError(t, err)

	ten, err := NewID(10)
	require.NoError(t, err)

	assert.Less(t, nine, ten)
	assert.True(t, strings.HasPrefix(nine, "0000000009-"))
	assert.Len(t, nine, 10+1+8)
}

func TestStampFillsProvenance(t *testing.T) {
	t.Parallel()

	m := New("")
	m.Generation = 7

	require.NoError(t, m.Stamp("client-1", time.Date(2026, 8, 20, 15, 4, 5, 0, time.UTC)))

	assert.Equal(t, FormatVersion, m.FormatVersion)
	assert.True(t, strings.HasPrefix(m.ID, "0000000007-"))
	assert.Equal(t, "2026-08-20T15:04:05Z", m.CreatedAt)
	assert.Equal(t, "client-1", m.CreatedBy.ClientID)
	assert.Contains(t, m.Generator, "dr-sync/")
	assert.Equal(t, "nfc", m.PathEncoding)
	assert.Equal(t, DefaultMetaPrefix, m.MetaPrefix)
}

func TestChildInheritsSettingsAndAdvancesGeneration(t *testing.T) {
	t.Parallel()

	parent := sample()
	parent.Compression = Compression{Codec: CodecXZ, Level: 6, DictCap: 8 << 20}

	child := parent.Child()

	assert.Equal(t, int64(43), child.Generation)
	assert.Equal(t, parent.ID, child.ParentManifestID)
	assert.Equal(t, parent.Compression, child.Compression)
	assert.Empty(t, child.Entries)
}

func TestRecomputeStats(t *testing.T) {
	t.Parallel()

	m := sample()

	assert.Equal(t, 2, m.Stats.FileCount)
	assert.Equal(t, 1, m.Stats.DirCount)
	assert.Equal(t, 1, m.Stats.SymlinkCount)
	assert.Equal(t, 1, m.Stats.UniqueBlobCount, "the empty file contributes no blob")
	assert.EqualValues(t, 1500, m.Stats.TotalSize)
	assert.EqualValues(t, 1500, m.Stats.LiveBytes)
	// Member "c" is stored but no entry points at it any more.
	assert.EqualValues(t, 2500, m.Stats.DeadBytes)
}

func TestDictCapForLevel(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 0, DictCapForLevel(0), "level 0 means store, with no dictionary")
	assert.Equal(t, 1<<20, DictCapForLevel(1))
	assert.Equal(t, 4<<20, DictCapForLevel(3))
	assert.Equal(t, 8<<20, DictCapForLevel(DefaultLevel))
	assert.Equal(t, 64<<20, DictCapForLevel(9))
}

func TestHumanBytes(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "0 B", HumanBytes(0))
	assert.Equal(t, "512 B", HumanBytes(512))
	assert.Equal(t, "1.0 KiB", HumanBytes(1024))
	assert.Equal(t, "1.5 MiB", HumanBytes(1024*1024*3/2))
	assert.Equal(t, "2.0 GiB", HumanBytes(2*1024*1024*1024))
}

func TestEntryHelpers(t *testing.T) {
	t.Parallel()

	entry := Entry{Type: TypeFile, Mode: 0o755, ModTime: "2026-08-19T10:00:00Z"}

	assert.Equal(t, "-rwxr-xr-x", entry.FileMode().String())
	assert.Equal(t, 2026, entry.ModifiedAt().Year())
	assert.True(t, Entry{Type: TypeFile}.IsEmptyFile())
	assert.False(t, Entry{Type: TypeFile, Size: 1}.IsEmptyFile())
	assert.True(t, Entry{Type: TypeFile, ModTime: "nonsense"}.ModifiedAt().IsZero())
}

func TestEncodeRefusesInvalidManifest(t *testing.T) {
	t.Parallel()

	m := New(DefaultMetaPrefix)
	m.Entries = []Entry{{Path: "../escape", Type: TypeFile, Size: 1, SHA256: "x", Pack: packIndex(0)}}

	_, err := Encode(m)
	require.ErrorContains(t, err, "refusing to write an invalid manifest")
}

func TestValidateRejectsMissingFormatVersion(t *testing.T) {
	t.Parallel()

	m := sample()
	m.FormatVersion = 0

	require.ErrorContains(t, m.Validate(), "no formatVersion")
}
