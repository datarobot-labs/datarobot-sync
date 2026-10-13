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
	"path"
	"sort"
	"strings"

	"github.com/datarobot-labs/datarobot-sync/internal/manifest"
)

// Planning defaults.
const (
	// DefaultPackInputMax caps the uncompressed input of one pack.
	//
	// Packs are kept modest on purpose, and the reason is on the download side rather than
	// the upload side: a pack is read from the start, so one huge pack means changing one
	// byte of one file drags every unrelated file in that pack across the wire for the next
	// puller.
	DefaultPackInputMax = 256 << 20

	// DefaultPackTarget is the compressed size a pack aims for. It is a target rather than a
	// limit because the compressed size is not known until the bytes are compressed.
	DefaultPackTarget = 64 << 20

	// DefaultBigFileThreshold gives any blob at or above this size a pack to itself, so a
	// large churning file never drags its neighbours along and they never drag it.
	DefaultBigFileThreshold = 32 << 20

	// incompressibleShare is the fraction of a pack's input that must be already-compressed
	// formats before the pack is stored uncompressed.
	incompressibleShare = 0.8
)

// incompressibleExtensions are formats where xz reliably buys nothing. Spending minutes of
// single-threaded LZMA2 to shrink a directory of JPEGs by one percent is not a good trade.
var incompressibleExtensions = map[string]struct{}{
	".7z": {}, ".avi": {}, ".avro": {}, ".br": {}, ".bz2": {}, ".flac": {},
	".gif": {}, ".gz": {}, ".heic": {}, ".jpeg": {}, ".jpg": {}, ".lz4": {},
	".m4a": {}, ".mkv": {}, ".mov": {}, ".mp3": {}, ".mp4": {}, ".ogg": {},
	".opus": {}, ".pack": {}, ".parquet": {}, ".pdf": {}, ".png": {}, ".rar": {},
	".webm": {}, ".webp": {}, ".whl": {}, ".woff": {}, ".woff2": {}, ".xz": {},
	".zip": {}, ".zst": {},
}

// PlanOptions configures how blobs are grouped into packs.
type PlanOptions struct {
	PackInputMax     int64
	BigFileThreshold int64
	// Level is the requested compression level; 0 forces the store codec everywhere.
	Level int
}

// PlannedPack is one pack's worth of blobs, with the codec chosen for it.
type PlannedPack struct {
	Blobs []Blob
	Codec manifest.Codec
	// InputBytes is the uncompressed total.
	InputBytes int64
}

// Plan groups blobs into packs.
//
// Ordering is by extension first, then directory, then name. Extension-major because
// same-extension files are the cheapest good proxy for the content similarity LZMA's window
// rewards; directory second because files that live together usually resemble each other too.
func Plan(blobs []Blob, opts PlanOptions) []PlannedPack {
	if len(blobs) == 0 {
		return nil
	}

	inputMax := opts.PackInputMax
	if inputMax <= 0 {
		inputMax = DefaultPackInputMax
	}

	bigFile := opts.BigFileThreshold
	if bigFile <= 0 {
		bigFile = DefaultBigFileThreshold
	}

	ordered := make([]Blob, len(blobs))
	copy(ordered, blobs)
	sortBlobs(ordered)

	var (
		packs   []PlannedPack
		current PlannedPack
	)

	flush := func() {
		if len(current.Blobs) == 0 {
			return
		}

		current.Codec = chooseCodec(current.Blobs, opts.Level)
		packs = append(packs, current)
		current = PlannedPack{}
	}

	for _, blob := range ordered {
		if blob.Size >= bigFile {
			flush()

			solo := PlannedPack{Blobs: []Blob{blob}, InputBytes: blob.Size}
			solo.Codec = chooseCodec(solo.Blobs, opts.Level)
			packs = append(packs, solo)

			continue
		}

		if current.InputBytes+blob.Size > inputMax && len(current.Blobs) > 0 {
			flush()
		}

		current.Blobs = append(current.Blobs, blob)
		current.InputBytes += blob.Size
	}

	flush()

	return packs
}

// sortBlobs orders blobs to cluster similar content together.
func sortBlobs(blobs []Blob) {
	sort.Slice(blobs, func(i, j int) bool {
		left, right := blobs[i], blobs[j]

		leftExt, rightExt := strings.ToLower(path.Ext(left.SourcePath)), strings.ToLower(path.Ext(right.SourcePath))
		if leftExt != rightExt {
			return leftExt < rightExt
		}

		leftDir, rightDir := path.Dir(left.SourcePath), path.Dir(right.SourcePath)
		if leftDir != rightDir {
			return leftDir < rightDir
		}

		if left.SourcePath != right.SourcePath {
			return left.SourcePath < right.SourcePath
		}

		// Final tiebreak on the digest, so the order — and therefore the pack's name — is
		// reproducible even for two identical paths.
		return left.SHA256 < right.SHA256
	})
}

// chooseCodec decides whether a pack is worth compressing.
func chooseCodec(blobs []Blob, level int) manifest.Codec {
	if level == 0 {
		return manifest.CodecStore
	}

	var incompressible, total int64

	for _, blob := range blobs {
		total += blob.Size

		if _, ok := incompressibleExtensions[strings.ToLower(path.Ext(blob.SourcePath))]; ok {
			incompressible += blob.Size
		}
	}

	if total > 0 && float64(incompressible)/float64(total) >= incompressibleShare {
		return manifest.CodecStore
	}

	return manifest.CodecXZ
}

// IsIncompressible reports whether a filename's extension is one xz cannot help.
func IsIncompressible(name string) bool {
	_, ok := incompressibleExtensions[strings.ToLower(path.Ext(name))]

	return ok
}
