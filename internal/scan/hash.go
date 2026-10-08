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

package scan

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
)

// hashBufferSize is the streaming chunk. Everything here streams: a single file may be larger
// than available memory, and the API has no chunked upload to fall back on.
const hashBufferSize = 1 << 20

// HashFile returns the SHA-256 hex digest of a file's contents.
//
// SHA-256 is not a choice: it is what the Files API reports as fileChecksum for every stored
// object, so using anything else would forfeit a free server-side integrity check.
func HashFile(path string) (string, error) {
	handle, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("hashing %s: %w", path, err)
	}

	defer handle.Close()

	digest, err := HashReader(handle)
	if err != nil {
		return "", fmt.Errorf("hashing %s: %w", path, err)
	}

	return digest, nil
}

// HashReader returns the SHA-256 hex digest of everything r yields.
func HashReader(r io.Reader) (string, error) {
	hasher := sha256.New()
	buf := make([]byte, hashBufferSize)

	if _, err := io.CopyBuffer(hasher, r, buf); err != nil {
		return "", err
	}

	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// NewHasher returns a SHA-256 hasher, so callers that are already streaming bytes can digest
// them in passing rather than reading twice.
func NewHasher() hash.Hash {
	return sha256.New()
}

// Digest renders a hasher's result the way the manifest and the API both spell it.
func Digest(h hash.Hash) string {
	return hex.EncodeToString(h.Sum(nil))
}

// EmptyDigest is the SHA-256 of no bytes at all.
const EmptyDigest = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
