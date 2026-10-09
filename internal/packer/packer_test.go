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

package packer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/datarobot/golang-datarobot-filesync/internal/manifest"
	"github.com/datarobot/golang-datarobot-filesync/internal/scan"
)

// blobFrom writes content to a scratch file and describes it as a blob.
func blobFrom(t *testing.T, dir, name, content string) Blob {
	t.Helper()

	path := filepath.Join(dir, name)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	digest, err := scan.HashFile(path)
	require.NoError(t, err)

	return Blob{SHA256: digest, Size: int64(len(content)), SourcePath: path}
}

// readSlices stitches a built pack's slices back into one byte stream.
func readSlices(t *testing.T, built *Built) []byte {
	t.Helper()

	reader := NewSliceReader(len(built.Slices), func(index int) (io.ReadCloser, error) {
		return os.Open(built.Slices[index].LocalPath)
	})

	defer reader.Close()

	content, err := io.ReadAll(reader)
	require.NoError(t, err)

	return content
}

func TestBuildRoundTrip(t *testing.T) {
	t.Parallel()

	sources := t.TempDir()
	scratch := t.TempDir()

	blobs := []Blob{
		blobFrom(t, sources, "a.txt", "alpha content"),
		blobFrom(t, sources, "b.txt", "beta content"),
	}

	built, err := Build(context.Background(), blobs, BuildOptions{ScratchDir: scratch})
	require.NoError(t, err)

	assert.Equal(t, manifest.CodecXZ, built.Codec)
	assert.Len(t, built.Members, 2)
	assert.EqualValues(t, 25, built.UncompressedSize)
	assert.Equal(t, PackID(built.SHA256), built.ID)
	require.Len(t, built.Slices, 1)

	// The pack name is derived from its own bytes, which is what makes a resumed run able to
	// recognize an already-uploaded pack from the listing alone.
	assert.Equal(t, built.SHA256[:16], built.ID[len("p_"):])

	recovered := map[string]string{}

	err = Read(context.Background(), bytes.NewReader(readSlices(t, built)), built.Codec, func(member Member) error {
		var buf bytes.Buffer
		if err := ExtractMember(&buf, member); err != nil {
			return err
		}

		recovered[member.SHA256] = buf.String()

		return nil
	})
	require.NoError(t, err)

	assert.Equal(t, "alpha content", recovered[blobs[0].SHA256])
	assert.Equal(t, "beta content", recovered[blobs[1].SHA256])
}

// A rebuild must produce byte-identical output. Everything about resume depends on it: if the
// bytes drifted, the pack name would drift, and because the commit rejects any pre-existing
// name, every retry would either re-upload the world or fail outright.
func TestBuildIsDeterministic(t *testing.T) {
	t.Parallel()

	sources := t.TempDir()

	blobs := []Blob{
		blobFrom(t, sources, "a.txt", "alpha content that is long enough to compress a little"),
		blobFrom(t, sources, "b.txt", "beta content that is also reasonably long and similar"),
	}

	first, err := Build(context.Background(), blobs, BuildOptions{ScratchDir: t.TempDir()})
	require.NoError(t, err)

	second, err := Build(context.Background(), blobs, BuildOptions{ScratchDir: t.TempDir()})
	require.NoError(t, err)

	assert.Equal(t, first.SHA256, second.SHA256)
	assert.Equal(t, first.ID, second.ID)
	assert.Equal(t, readSlices(t, first), readSlices(t, second))
}

// Changing the dictionary size changes the bytes, so the level belongs in the manifest — a
// puller must not assume the settings it would have used.
func TestCompressionLevelChangesPackIdentity(t *testing.T) {
	t.Parallel()

	sources := t.TempDir()
	blobs := []Blob{blobFrom(t, sources, "a.txt", "some content worth compressing")}

	small, err := Build(context.Background(), blobs, BuildOptions{
		ScratchDir: t.TempDir(), DictCap: manifest.DictCapForLevel(1),
	})
	require.NoError(t, err)

	large, err := Build(context.Background(), blobs, BuildOptions{
		ScratchDir: t.TempDir(), DictCap: manifest.DictCapForLevel(9),
	})
	require.NoError(t, err)

	assert.NotEqual(t, small.SHA256, large.SHA256)
}

func TestStoreCodecRoundTrip(t *testing.T) {
	t.Parallel()

	sources := t.TempDir()
	blobs := []Blob{blobFrom(t, sources, "photo.jpg", "not really a jpeg but pretend")}

	built, err := Build(context.Background(), blobs, BuildOptions{
		ScratchDir: t.TempDir(), Codec: manifest.CodecStore,
	})
	require.NoError(t, err)

	assert.Equal(t, manifest.CodecStore, built.Codec)

	// A stored pack is a plain tar, so its compressed size is the tar's own size.
	assert.Greater(t, built.CompressedSize, built.UncompressedSize)

	var recovered bytes.Buffer

	err = Read(context.Background(), bytes.NewReader(readSlices(t, built)), built.Codec, func(member Member) error {
		return ExtractMember(&recovered, member)
	})
	require.NoError(t, err)
	assert.Equal(t, "not really a jpeg but pretend", recovered.String())
}

// Slicing is what removes the file-size ceiling, and it has to be invisible to the layers
// above: the tar and xz readers must never learn the bytes arrived as several objects.
func TestSlicingIsTransparent(t *testing.T) {
	t.Parallel()

	sources := t.TempDir()
	content := bytes.Repeat([]byte("dr-sync slicing test payload "), 4000)

	path := filepath.Join(sources, "big.bin")
	require.NoError(t, os.WriteFile(path, content, 0o600))

	digest, err := scan.HashFile(path)
	require.NoError(t, err)

	blobs := []Blob{{SHA256: digest, Size: int64(len(content)), SourcePath: path}}

	built, err := Build(context.Background(), blobs, BuildOptions{
		ScratchDir: t.TempDir(),
		Codec:      manifest.CodecStore,
		SliceSize:  4096,
	})
	require.NoError(t, err)

	require.Greater(t, len(built.Slices), 2, "the payload must actually span several slices")

	// Each slice carries its own digest, so a resume can verify what it already has.
	var total int64

	for _, slice := range built.Slices {
		assert.NotEmpty(t, slice.SHA256)
		assert.LessOrEqual(t, slice.Size, int64(4096))
		total += slice.Size
	}

	assert.Equal(t, built.CompressedSize, total)

	var recovered bytes.Buffer

	err = Read(context.Background(), bytes.NewReader(readSlices(t, built)), built.Codec, func(member Member) error {
		return ExtractMember(&recovered, member)
	})
	require.NoError(t, err)
	assert.Equal(t, content, recovered.Bytes())
}

// Storing content under a name that no longer describes it is the one corruption content
// addressing exists to prevent, so a file that moves mid-pack is a hard failure.
func TestBuildDetectsSourceChangedUnderIt(t *testing.T) {
	t.Parallel()

	sources := t.TempDir()
	blob := blobFrom(t, sources, "a.txt", "original content")

	require.NoError(t, os.WriteFile(blob.SourcePath, []byte("REPLACED content"), 0o600))

	_, err := Build(context.Background(), []Blob{blob}, BuildOptions{ScratchDir: t.TempDir()})
	require.ErrorIs(t, err, ErrSourceChanged)
	require.ErrorContains(t, err, "a.txt")
}

func TestBuildLeavesNoScratchBehindOnFailure(t *testing.T) {
	t.Parallel()

	sources := t.TempDir()
	scratch := t.TempDir()

	blob := blobFrom(t, sources, "a.txt", "original")
	require.NoError(t, os.Remove(blob.SourcePath))

	_, err := Build(context.Background(), []Blob{blob}, BuildOptions{ScratchDir: scratch})
	require.Error(t, err)

	remaining, err := os.ReadDir(scratch)
	require.NoError(t, err)
	assert.Empty(t, remaining, "a failed build must not leave slices behind")
}

func TestBuildRejectsEmptyPack(t *testing.T) {
	t.Parallel()

	_, err := Build(context.Background(), nil, BuildOptions{ScratchDir: t.TempDir()})
	require.ErrorContains(t, err, "empty pack")
}

// This is where the "solid archive, but stop early" saving is actually realized.
func TestReadStopsEarly(t *testing.T) {
	t.Parallel()

	sources := t.TempDir()

	blobs := []Blob{
		blobFrom(t, sources, "a.txt", "first"),
		blobFrom(t, sources, "b.txt", "second"),
		blobFrom(t, sources, "c.txt", "third"),
	}

	built, err := Build(context.Background(), blobs, BuildOptions{
		ScratchDir: t.TempDir(), Codec: manifest.CodecStore,
	})
	require.NoError(t, err)

	visited := 0

	err = Read(context.Background(), bytes.NewReader(readSlices(t, built)), built.Codec, func(Member) error {
		visited++

		if visited == 2 {
			return ErrStopReading
		}

		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 2, visited, "iteration must stop without reading the rest of the pack")
}

func TestReadPropagatesHandlerError(t *testing.T) {
	t.Parallel()

	sources := t.TempDir()
	blobs := []Blob{blobFrom(t, sources, "a.txt", "content")}

	built, err := Build(context.Background(), blobs, BuildOptions{
		ScratchDir: t.TempDir(), Codec: manifest.CodecStore,
	})
	require.NoError(t, err)

	sentinel := errors.New("handler said no")

	err = Read(context.Background(), bytes.NewReader(readSlices(t, built)), built.Codec, func(Member) error {
		return sentinel
	})
	require.ErrorIs(t, err, sentinel)
}

// Per-member verification is the integrity mechanism, because an early abort means the
// pack-level digest was never fully read.
func TestExtractMemberRejectsCorruptedContent(t *testing.T) {
	t.Parallel()

	member := Member{
		SHA256:  scan.EmptyDigest,
		Size:    5,
		Content: bytes.NewReader([]byte("wrong")),
	}

	err := ExtractMember(io.Discard, member)
	require.Error(t, err)
	assert.True(t, IsIntegrityError(err))
	require.ErrorContains(t, err, "content digest is")

	short := Member{SHA256: scan.EmptyDigest, Size: 10, Content: bytes.NewReader([]byte("abc"))}
	err = ExtractMember(io.Discard, short)
	require.ErrorContains(t, err, "expected 10 bytes, read 3")
}

func TestMemberNaming(t *testing.T) {
	t.Parallel()

	digest := scan.EmptyDigest

	assert.Equal(t, "blobs/"+digest, MemberName(digest))

	recovered, ok := DigestFromMemberName(MemberName(digest))
	require.True(t, ok)
	assert.Equal(t, digest, recovered)

	// Anything that is not a blob member is ignored rather than misread, so a future writer
	// can add other member kinds.
	for _, name := range []string{"other/thing", "blobs/short", "blobs/" + digest + "x", "blobs/ZZZZ", digest} {
		_, ok := DigestFromMemberName(name)
		assert.False(t, ok, "%q must not parse as a blob member", name)
	}
}

func TestPackIDDerivation(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "p_e3b0c44298fc1c14", PackID(scan.EmptyDigest))
	assert.Equal(t, "p_short", PackID("short"))
}

func TestLastNeededIndexAndFraction(t *testing.T) {
	t.Parallel()

	pack := manifest.Pack{Members: []manifest.Member{
		{SHA256: "a", Size: 100},
		{SHA256: "b", Size: 100},
		{SHA256: "c", Size: 800},
	}}

	assert.Equal(t, -1, LastNeededIndex(pack, map[string]struct{}{"z": {}}))
	assert.Equal(t, 0, LastNeededIndex(pack, map[string]struct{}{"a": {}}))
	assert.Equal(t, 2, LastNeededIndex(pack, map[string]struct{}{"a": {}, "c": {}}))

	// Wanting only the first member means one tenth of the pack has to be read.
	assert.InDelta(t, 0.1, NeededFraction(pack, 0), 0.001)
	assert.InDelta(t, 1.0, NeededFraction(pack, 2), 0.001)
	assert.Zero(t, NeededFraction(pack, -1))
	assert.InDelta(t, 1.0, NeededFraction(manifest.Pack{}, 0), 0.001)
}
