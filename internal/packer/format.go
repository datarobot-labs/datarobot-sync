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

// Package packer writes and reads dr-sync packs: tar streams of content-addressed blobs,
// compressed with xz and split into fixed-size slices.
//
// Two properties of the format do all the work:
//
// Members are named by their SHA-256 rather than by path, so identical content is stored
// once, a rename transfers nothing, and there is no path inside the archive that extraction
// could be tricked into honouring.
//
// The bytes are deterministic. Given the same members in the same order and the same
// compression settings, a rebuild produces a byte-identical pack and therefore the same
// content-derived name. That is what turns crash recovery into a listing lookup instead of a
// re-upload — and it is required, not merely nice, because the commit rejects any name that
// already exists.
package packer

import (
	"archive/tar"
	"fmt"
	"strings"
	"time"
)

// memberPrefix namespaces blobs inside the tar, leaving room for other member kinds later
// without ambiguity.
const memberPrefix = "blobs/"

// packIDPrefix marks a pack object name.
const packIDPrefix = "p_"

// packIDDigestChars is how much of the digest goes into the pack id. 64 bits of a SHA-256 is
// ample for naming: a collision would need on the order of 2^32 distinct packs.
const packIDDigestChars = 16

// MemberName returns the tar member name for a content digest.
func MemberName(digest string) string {
	return memberPrefix + digest
}

// DigestFromMemberName extracts the digest from a member name.
func DigestFromMemberName(name string) (string, bool) {
	if !strings.HasPrefix(name, memberPrefix) {
		return "", false
	}

	digest := strings.TrimPrefix(name, memberPrefix)
	if len(digest) != 64 || !isHex(digest) {
		return "", false
	}

	return digest, true
}

func isHex(s string) bool {
	for i := range len(s) {
		c := s[i]

		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
		default:
			return false
		}
	}

	return true
}

// PackID derives a pack's id from the digest of its compressed bytes.
func PackID(compressedDigest string) string {
	if len(compressedDigest) < packIDDigestChars {
		return packIDPrefix + compressedDigest
	}

	return packIDPrefix + compressedDigest[:packIDDigestChars]
}

// epoch is the fixed modification time written into every tar header. A real timestamp would
// make the pack bytes, and therefore the pack name, differ between runs over identical
// content.
var epoch = time.Unix(0, 0).UTC()

// memberHeader builds the tar header for a blob.
//
// Every field that could vary between machines or runs is pinned: a fixed mode, a fixed
// mtime, no owner, no group, no names. PAX is chosen over USTAR only because USTAR cannot
// express a member larger than 8 GiB; for everything smaller Go emits no PAX records, so the
// output stays byte-stable.
func memberHeader(digest string, size int64) *tar.Header {
	return &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     MemberName(digest),
		Size:     size,
		Mode:     0o600,
		ModTime:  epoch,
		Uid:      0,
		Gid:      0,
		Uname:    "",
		Gname:    "",
		Format:   tar.FormatPAX,
	}
}

// SliceSuffix renders the numeric suffix of a multi-slice pack object.
func SliceSuffix(index int) string {
	return fmt.Sprintf(".%03d", index)
}
