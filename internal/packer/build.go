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
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/datarobot-labs/datarobot-sync/internal/manifest"
	"github.com/datarobot-labs/datarobot-sync/internal/scan"
)

// buildSeq disambiguates concurrent Build calls in the same process: upload.go's buildPacks
// runs several at once, and os.Getpid() alone is identical across all of them.
var buildSeq atomic.Int64

// Blob is one piece of content to store.
type Blob struct {
	SHA256 string
	Size   int64
	// SourcePath is the local file holding this content.
	SourcePath string
}

// Built describes a finished pack sitting in scratch, ready to upload.
type Built struct {
	ID     string
	Codec  manifest.Codec
	SHA256 string
	// CompressedSize is the total across slices.
	CompressedSize   int64
	UncompressedSize int64
	// Members are in tar order, which is what lets a download stop early.
	Members []manifest.Member
	// Slices are the local scratch files, in order.
	Slices []LocalSlice
}

// LocalSlice is one scratch file of a built pack.
type LocalSlice struct {
	LocalPath string
	Size      int64
	SHA256    string
}

// Remove deletes the scratch files backing a built pack.
func (b *Built) Remove() {
	for _, slice := range b.Slices {
		_ = os.Remove(slice.LocalPath)
	}
}

// BuildOptions configures pack construction.
type BuildOptions struct {
	// ScratchDir is where slice files are written.
	ScratchDir string
	Codec      manifest.Codec
	// DictCap is the LZMA2 dictionary capacity; ignored for the store codec.
	DictCap int
	// SliceSize caps one uploaded object. Zero uses DefaultSliceSize.
	SliceSize int64
	// Progress, if set, is called with the number of uncompressed bytes consumed so far.
	Progress func(uncompressedBytes int64)
}

// ErrSourceChanged means a file was modified while it was being packed. The content no longer
// matches the digest the plan was built from, so storing it would put a blob in the container
// under the wrong name.
var ErrSourceChanged = errors.New("file changed while being packed")

// SourceChangedError names the file that moved under us.
type SourceChangedError struct {
	Path     string
	Expected string
	Actual   string
}

func (e *SourceChangedError) Error() string {
	return fmt.Sprintf("%s changed while being packed: expected %s, read %s",
		e.Path, short(e.Expected), short(e.Actual))
}

func (e *SourceChangedError) Unwrap() error {
	return ErrSourceChanged
}

func short(digest string) string {
	if len(digest) <= 12 {
		return digest
	}

	return digest[:12]
}

// Build writes blobs into one pack in scratch and returns its description.
//
// Members are written in the order given, and that order is recorded, because a download uses
// it to stop reading once it has passed the last member it wants.
func Build(ctx context.Context, blobs []Blob, opts BuildOptions) (*Built, error) {
	if len(blobs) == 0 {
		return nil, errors.New("refusing to build an empty pack")
	}

	codec := opts.Codec
	if codec == "" {
		codec = manifest.CodecXZ
	}

	// A provisional name, because the real one is the digest of bytes that do not exist yet.
	provisional := fmt.Sprintf("building-%d-%d", os.Getpid(), buildSeq.Add(1))

	slices := newSliceWriter(opts.ScratchDir, provisional, opts.SliceSize)

	built, err := writePack(ctx, blobs, codec, opts, slices)
	if err != nil {
		slices.Remove()

		return nil, err
	}

	if err := rename(built, opts.ScratchDir); err != nil {
		built.Remove()

		return nil, err
	}

	return built, nil
}

// compressor wraps the slice writer in the pack's codec.
func compressor(codec manifest.Codec, slices *sliceWriter, dictCap int) (io.WriteCloser, error) {
	if codec != manifest.CodecXZ {
		return nopWriteCloser{slices}, nil
	}

	return manifest.XZWriter(slices, dictCap)
}

// writePack streams the blobs through tar and the codec into the slice writer.
func writePack(ctx context.Context, blobs []Blob, codec manifest.Codec, opts BuildOptions, slices *sliceWriter) (*Built, error) {
	compressed, err := compressor(codec, slices, opts.DictCap)
	if err != nil {
		return nil, err
	}

	tarWriter := tar.NewWriter(compressed)

	members, uncompressed, err := writeMembers(ctx, tarWriter, blobs, opts)
	if err != nil {
		return nil, err
	}

	// Order matters: tar's trailer, then the codec's, then the slice files.
	if err := tarWriter.Close(); err != nil {
		return nil, fmt.Errorf("finishing pack tar: %w", err)
	}

	if err := compressed.Close(); err != nil {
		return nil, fmt.Errorf("finishing pack compression: %w", err)
	}

	if err := slices.Close(); err != nil {
		return nil, err
	}

	digest := slices.Digest()

	built := &Built{
		ID:               PackID(digest),
		Codec:            codec,
		SHA256:           digest,
		CompressedSize:   slices.Size(),
		UncompressedSize: uncompressed,
		Members:          members,
	}

	for _, slice := range slices.Slices() {
		built.Slices = append(built.Slices, LocalSlice(slice))
	}

	return built, nil
}

// writeMembers copies every blob into the tar in order, reporting the member list and the
// total uncompressed size.
func writeMembers(ctx context.Context, tarWriter *tar.Writer, blobs []Blob, opts BuildOptions) ([]manifest.Member, int64, error) {
	members := make([]manifest.Member, 0, len(blobs))

	var uncompressed int64

	for _, blob := range blobs {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}

		if err := writeMember(tarWriter, blob); err != nil {
			return nil, 0, err
		}

		members = append(members, manifest.Member{SHA256: blob.SHA256, Size: blob.Size})
		uncompressed += blob.Size

		if opts.Progress != nil {
			opts.Progress(uncompressed)
		}
	}

	return members, uncompressed, nil
}

// writeMember copies one blob into the tar, re-verifying its digest on the way through.
//
// The digest was computed during the scan, and the file could have been rewritten since. Not
// checking would mean storing content under a name that does not describe it, which is
// exactly the kind of corruption content addressing is supposed to make impossible.
func writeMember(tarWriter *tar.Writer, blob Blob) error {
	source, err := os.Open(blob.SourcePath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", blob.SourcePath, err)
	}

	defer source.Close()

	if err := tarWriter.WriteHeader(memberHeader(blob.SHA256, blob.Size)); err != nil {
		return fmt.Errorf("writing pack header for %s: %w", blob.SourcePath, err)
	}

	hasher := scan.NewHasher()
	buf := make([]byte, 1<<20)

	copied, err := io.CopyBuffer(tarWriter, io.TeeReader(io.LimitReader(source, blob.Size), hasher), buf)
	if err != nil {
		return fmt.Errorf("packing %s: %w", blob.SourcePath, err)
	}

	if copied != blob.Size {
		return &SourceChangedError{Path: blob.SourcePath, Expected: blob.SHA256, Actual: "short read"}
	}

	if actual := scan.Digest(hasher); actual != blob.SHA256 {
		return &SourceChangedError{Path: blob.SourcePath, Expected: blob.SHA256, Actual: actual}
	}

	return nil
}

// rename moves the scratch slices to names derived from the finished pack's id, so a resumed
// run can recognize its own work.
func rename(built *Built, scratchDir string) error {
	for i := range built.Slices {
		target := filepath.Join(scratchDir, built.ID+SliceSuffix(i))

		if err := os.Rename(built.Slices[i].LocalPath, target); err != nil {
			return fmt.Errorf("naming pack slice: %w", err)
		}

		built.Slices[i].LocalPath = target
	}

	return nil
}

// nopWriteCloser lets the store codec share the compressed-writer code path.
type nopWriteCloser struct {
	io.Writer
}

func (nopWriteCloser) Close() error {
	return nil
}
