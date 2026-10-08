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

	"github.com/datarobot/golang-datarobot-filesync/internal/manifest"
	"github.com/datarobot/golang-datarobot-filesync/internal/scan"
)

// Member is one blob encountered while reading a pack.
type Member struct {
	SHA256 string
	Size   int64
	// Content streams the blob. It is only valid until the next iteration step, so a handler
	// must consume it before returning.
	Content io.Reader
}

// ErrStopReading tells Read to stop iterating without reporting a failure. A handler returns
// it once it has everything it wanted, which is what realizes the early-abort saving on a
// solid archive.
var ErrStopReading = errors.New("stop reading pack")

// Read iterates the members of a pack, calling handle for each.
//
// The stream is read once, forwards, because xz cannot seek: unwanted members are discarded
// rather than skipped over. Returning ErrStopReading from handle abandons the rest of the
// pack, and for a pack whose wanted members are near the front that is the difference between
// transferring a few megabytes and transferring all of it.
func Read(ctx context.Context, r io.Reader, codec manifest.Codec, handle func(Member) error) error {
	stream := r

	if codec == manifest.CodecXZ {
		decompressed, err := manifest.XZReader(r)
		if err != nil {
			return err
		}

		stream = decompressed
	}

	tarReader := tar.NewReader(stream)

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}

		if err != nil {
			return fmt.Errorf("reading pack: %w", err)
		}

		stop, err := visitMember(header, tarReader, handle)
		if err != nil {
			return err
		}

		if stop {
			return nil
		}
	}
}

// visitMember hands one tar entry to the handler, reporting whether iteration should stop.
func visitMember(header *tar.Header, content io.Reader, handle func(Member) error) (bool, error) {
	if header.Typeflag != tar.TypeReg {
		return false, nil
	}

	digest, ok := DigestFromMemberName(header.Name)
	if !ok {
		// Not a blob. An unrecognized member kind is skipped rather than fatal, so a future
		// writer can add one without breaking this reader.
		return false, nil
	}

	err := handle(Member{SHA256: digest, Size: header.Size, Content: content})

	if errors.Is(err, ErrStopReading) {
		return true, nil
	}

	return false, err
}

// ExtractMember copies one member's content to dst while verifying its digest.
//
// Per-member verification is the integrity mechanism, not the pack-level digest: a download
// that aborts early has deliberately not read the whole pack, so the pack digest cannot be
// checked in that case.
func ExtractMember(dst io.Writer, member Member) error {
	hasher := scan.NewHasher()
	buf := make([]byte, 1<<20)

	copied, err := io.CopyBuffer(dst, io.TeeReader(member.Content, hasher), buf)
	if err != nil {
		return fmt.Errorf("extracting %s: %w", short(member.SHA256), err)
	}

	if copied != member.Size {
		return &IntegrityError{
			Digest: member.SHA256,
			Reason: fmt.Sprintf("expected %d bytes, read %d", member.Size, copied),
		}
	}

	if actual := scan.Digest(hasher); actual != member.SHA256 {
		return &IntegrityError{
			Digest: member.SHA256,
			Reason: "content digest is " + short(actual),
		}
	}

	return nil
}

// IntegrityError is content that does not match the digest it was stored under.
type IntegrityError struct {
	Digest string
	Reason string
}

func (e *IntegrityError) Error() string {
	return fmt.Sprintf("integrity failure for %s: %s", short(e.Digest), e.Reason)
}

// IsIntegrityError reports whether err is a content mismatch.
func IsIntegrityError(err error) bool {
	var integrityErr *IntegrityError

	return errors.As(err, &integrityErr)
}

// LastNeededIndex returns the position of the last wanted member in a pack's member list, or
// -1 when none are wanted.
//
// This is what a caller uses to decide when to stop: past this index the rest of the pack is
// of no interest, however much of it remains.
func LastNeededIndex(pack manifest.Pack, wanted map[string]struct{}) int {
	last := -1

	for i, member := range pack.Members {
		if _, ok := wanted[member.SHA256]; ok {
			last = i
		}
	}

	return last
}

// NeededFraction estimates how much of a pack must be transferred to reach the last wanted
// member, as a fraction of its uncompressed size. It drives the progress estimate only.
func NeededFraction(pack manifest.Pack, lastNeeded int) float64 {
	if lastNeeded < 0 {
		return 0
	}

	var (
		through int64
		total   int64
	)

	for i, member := range pack.Members {
		total += member.Size

		if i <= lastNeeded {
			through += member.Size
		}
	}

	if total == 0 {
		return 1
	}

	return float64(through) / float64(total)
}
