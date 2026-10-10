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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"testing"

	"github.com/ulikunitz/xz"

	"github.com/datarobot-labs/datarobot-sync/internal/manifest"
)

// countingHasher hashes what ExtractMember writes and counts its bytes, without keeping them:
// a compressed pack can expand far beyond its size, and the fuzzer must not exhaust memory.
type countingHasher struct {
	sum   io.Writer
	count int64
}

func (c *countingHasher) Write(p []byte) (int, error) {
	c.count += int64(len(p))

	return c.sum.Write(p)
}

// tarWithBlob builds a pack's tar stream holding one blob under the given member name.
func tarWithBlob(tb testing.TB, name string, content []byte) []byte {
	tb.Helper()

	var buf bytes.Buffer

	writer := tar.NewWriter(&buf)

	header := &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content))}
	if err := writer.WriteHeader(header); err != nil {
		tb.Fatal(err)
	}

	if _, err := writer.Write(content); err != nil {
		tb.Fatal(err)
	}

	if err := writer.Close(); err != nil {
		tb.Fatal(err)
	}

	return buf.Bytes()
}

func compressXZ(tb testing.TB, plain []byte) []byte {
	tb.Helper()

	var buf bytes.Buffer

	writer, err := xz.NewWriter(&buf)
	if err != nil {
		tb.Fatal(err)
	}

	if _, err := writer.Write(plain); err != nil {
		tb.Fatal(err)
	}

	if err := writer.Close(); err != nil {
		tb.Fatal(err)
	}

	return buf.Bytes()
}

// FuzzReadAndExtract reads arbitrary bytes as a pack in both codecs. It must never panic, and
// ExtractMember must never report success for content whose digest or size is not the one the
// member claims: that check is the integrity guarantee for every download.
func FuzzReadAndExtract(f *testing.F) {
	content := []byte("blob content\n")
	digest := sha256.Sum256(content)
	good := tarWithBlob(f, MemberName(hex.EncodeToString(digest[:])), content)
	wrongDigest := tarWithBlob(f, MemberName(hex.EncodeToString(make([]byte, 32))), content)

	f.Add(good, false)
	f.Add(compressXZ(f, good), true)
	f.Add(wrongDigest, false)
	f.Add(good[:len(good)/2], false)
	f.Add([]byte("not a tar"), false)
	f.Add([]byte("not xz"), true)

	f.Fuzz(func(t *testing.T, data []byte, compressed bool) {
		codec := manifest.CodecStore
		if compressed {
			codec = manifest.CodecXZ
		}

		handle := func(member Member) error {
			hasher := sha256.New()
			sink := &countingHasher{sum: hasher}

			if err := ExtractMember(sink, member); err != nil {
				return nil
			}

			if got := hex.EncodeToString(hasher.Sum(nil)); got != member.SHA256 {
				t.Fatalf("extract reported success for content with digest %s, member claims %s", got, member.SHA256)
			}

			if sink.count != member.Size {
				t.Fatalf("extract reported success after %d bytes, member claims %d", sink.count, member.Size)
			}

			return nil
		}

		// Errors are expected for malformed input; a panic or a false success is the bug.
		_ = Read(context.Background(), bytes.NewReader(data), codec, handle)
	})
}
