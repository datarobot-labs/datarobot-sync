// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

package packer

import (
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"

	"github.com/datarobot/golang-datarobot-filesync/internal/scan"
)

// DefaultSliceSize is how much of a pack goes into one uploaded object.
//
// This is what removes the file-size ceiling. The API has no chunked upload, so a pack is one
// request body; splitting the compressed stream into siblings that a reader stitches back
// together means a 40 GiB file becomes several hundred ordinary uploads instead of one
// impossible one. 64 MiB is comfortably under the largest body observed to be accepted.
const DefaultSliceSize = 64 << 20

// sliceWriter spreads a byte stream across numbered scratch files, digesting each slice and
// the whole stream as it goes.
type sliceWriter struct {
	dir      string
	basename string
	limit    int64

	slices []writtenSlice
	// total digests the logical concatenation, which is what names the pack.
	total hash.Hash

	current      *os.File
	currentHash  hash.Hash
	currentBytes int64
	totalBytes   int64
	closed       bool
}

// writtenSlice is one scratch file holding part of a pack.
type writtenSlice struct {
	LocalPath string
	Size      int64
	SHA256    string
}

func newSliceWriter(dir, basename string, limit int64) *sliceWriter {
	if limit <= 0 {
		limit = DefaultSliceSize
	}

	return &sliceWriter{
		dir:      dir,
		basename: basename,
		limit:    limit,
		total:    scan.NewHasher(),
	}
}

func (w *sliceWriter) Write(p []byte) (int, error) {
	written := 0

	for len(p) > 0 {
		if w.current == nil {
			if err := w.rollover(); err != nil {
				return written, err
			}
		}

		room := w.limit - w.currentBytes

		chunk := p
		if int64(len(chunk)) > room {
			chunk = chunk[:room]
		}

		n, err := w.current.Write(chunk)
		if n > 0 {
			// Both digests see exactly the bytes that reached the file, so a short write
			// cannot desynchronize them.
			_, _ = w.currentHash.Write(chunk[:n])
			_, _ = w.total.Write(chunk[:n])
			w.currentBytes += int64(n)
			w.totalBytes += int64(n)
			written += n
			p = p[n:]
		}

		if err != nil {
			return written, fmt.Errorf("writing pack slice: %w", err)
		}

		if w.currentBytes >= w.limit {
			if err := w.finishCurrent(); err != nil {
				return written, err
			}
		}
	}

	return written, nil
}

// rollover opens the next slice file.
func (w *sliceWriter) rollover() error {
	name := filepath.Join(w.dir, w.basename+SliceSuffix(len(w.slices)))

	handle, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("creating pack slice %s: %w", name, err)
	}

	w.current = handle
	w.currentHash = scan.NewHasher()
	w.currentBytes = 0

	return nil
}

func (w *sliceWriter) finishCurrent() error {
	if w.current == nil {
		return nil
	}

	name := w.current.Name()

	if err := w.current.Sync(); err != nil {
		return fmt.Errorf("flushing pack slice %s: %w", name, err)
	}

	if err := w.current.Close(); err != nil {
		return fmt.Errorf("closing pack slice %s: %w", name, err)
	}

	w.slices = append(w.slices, writtenSlice{
		LocalPath: name,
		Size:      w.currentBytes,
		SHA256:    scan.Digest(w.currentHash),
	})

	w.current = nil
	w.currentHash = nil
	w.currentBytes = 0

	return nil
}

// Close finalizes the last slice.
func (w *sliceWriter) Close() error {
	if w.closed {
		return nil
	}

	w.closed = true

	return w.finishCurrent()
}

// Digest returns the digest of the logical concatenation of every slice.
func (w *sliceWriter) Digest() string {
	return scan.Digest(w.total)
}

// Size returns the total compressed size.
func (w *sliceWriter) Size() int64 {
	return w.totalBytes
}

// Slices returns the written slices in order.
func (w *sliceWriter) Slices() []writtenSlice {
	return w.slices
}

// Remove deletes the scratch files. Used to abandon a partially built pack.
func (w *sliceWriter) Remove() {
	if w.current != nil {
		name := w.current.Name()
		_ = w.current.Close()
		_ = os.Remove(name)
		w.current = nil
	}

	for _, slice := range w.slices {
		_ = os.Remove(slice.LocalPath)
	}

	w.slices = nil
}

// SliceOpener yields the reader for one slice of a pack, by index.
type SliceOpener func(index int) (io.ReadCloser, error)

// multiSliceReader stitches a pack's slices back into one stream.
//
// This is the read side of what makes slicing invisible: the tar and xz layers above never
// learn that the bytes arrived as several objects.
type multiSliceReader struct {
	open    SliceOpener
	count   int
	index   int
	current io.ReadCloser
}

// NewSliceReader returns a reader over count slices, opened on demand.
func NewSliceReader(count int, open SliceOpener) io.ReadCloser {
	return &multiSliceReader{open: open, count: count}
}

func (r *multiSliceReader) Read(p []byte) (int, error) {
	for {
		if r.current == nil {
			if r.index >= r.count {
				return 0, io.EOF
			}

			next, err := r.open(r.index)
			if err != nil {
				return 0, fmt.Errorf("opening pack slice %d: %w", r.index, err)
			}

			r.current = next
			r.index++
		}

		n, err := r.current.Read(p)
		if n > 0 {
			return n, nil
		}

		if errors.Is(err, io.EOF) {
			// Move to the next slice; the caller must not see the boundary.
			if closeErr := r.current.Close(); closeErr != nil {
				return 0, closeErr
			}

			r.current = nil

			continue
		}

		if err != nil {
			return 0, err
		}
	}
}

func (r *multiSliceReader) Close() error {
	if r.current == nil {
		return nil
	}

	err := r.current.Close()
	r.current = nil

	return err
}
