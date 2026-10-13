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

package manifest

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/user"
	"time"

	"github.com/ulikunitz/xz"

	"github.com/datarobot-labs/datarobot-sync/internal/version"
)

// maxManifestBytes bounds how much a decoder will expand, so a hostile or corrupt object
// cannot drive the process out of memory. Ten million entries would be well under this.
const maxManifestBytes = 1 << 30

// Encode serializes a manifest as xz-compressed JSON.
//
// JSON of this shape compresses by an order of magnitude, which matters: the manifest is
// fetched at the start of every single run, so its transfer cost is paid constantly.
func Encode(m *Manifest) ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("refusing to write an invalid manifest: %w", err)
	}

	// Indented, because the one thing worse than an opaque container is an opaque container
	// whose only readable object is minified.
	plain, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding manifest: %w", err)
	}

	var compressed bytes.Buffer

	writer, err := xzWriterConfig(defaultDictCap).NewWriter(&compressed)
	if err != nil {
		return nil, fmt.Errorf("starting xz writer: %w", err)
	}

	if _, err := writer.Write(plain); err != nil {
		return nil, fmt.Errorf("compressing manifest: %w", err)
	}

	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("finishing manifest compression: %w", err)
	}

	return compressed.Bytes(), nil
}

// Decode parses an xz-compressed manifest.
func Decode(r io.Reader) (*Manifest, error) {
	reader, err := xz.ReaderConfig{}.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("reading manifest: not valid xz: %w", err)
	}

	plain, err := io.ReadAll(io.LimitReader(reader, maxManifestBytes))
	if err != nil {
		return nil, fmt.Errorf("decompressing manifest: %w", err)
	}

	if len(plain) == maxManifestBytes {
		return nil, fmt.Errorf("manifest exceeds the %d byte limit", maxManifestBytes)
	}

	var m Manifest

	decoder := json.NewDecoder(bytes.NewReader(plain))
	if err := decoder.Decode(&m); err != nil {
		return nil, fmt.Errorf("parsing manifest: %w", err)
	}

	if err := m.Validate(); err != nil {
		return nil, err
	}

	return &m, nil
}

// DecodeBytes parses an xz-compressed manifest from memory.
func DecodeBytes(b []byte) (*Manifest, error) {
	return Decode(bytes.NewReader(b))
}

// Compression presets. The level maps to an LZMA2 dictionary size; a bigger dictionary finds
// more distant matches at the cost of memory and time.
//
// The default is 6 rather than 9 deliberately: the pure-Go encoder is single-threaded and
// several times slower than liblzma, and level 9 buys a few percent of ratio for a large
// constant-factor slowdown and 64 MiB of working set per concurrent pack.
const (
	dictCapLevel1  = 1 << 20
	dictCapLevel3  = 4 << 20
	defaultDictCap = 8 << 20
	dictCapLevel9  = 64 << 20

	// DefaultLevel is the compression level used when none is requested.
	DefaultLevel = 6
)

// DictCapForLevel maps a compression level to an LZMA2 dictionary capacity. Level 0 means
// "store" and has no dictionary.
func DictCapForLevel(level int) int {
	switch {
	case level <= 0:
		return 0
	case level <= 1:
		return dictCapLevel1
	case level <= 3:
		return dictCapLevel3
	case level <= 6:
		return defaultDictCap
	default:
		return dictCapLevel9
	}
}

// xzWriterConfig returns a fully pinned writer configuration.
//
// Every field is set explicitly, including the ones whose defaults are already what we want,
// because the pack name is the digest of the compressed bytes: if a library default shifted,
// every pack would silently get a new name and every resume would re-upload the world.
func xzWriterConfig(dictCap int) xz.WriterConfig {
	if dictCap <= 0 {
		dictCap = defaultDictCap
	}

	return xz.WriterConfig{
		DictCap:   dictCap,
		BufSize:   4096,
		BlockSize: 1 << 62, // effectively one block, matching the library default
		CheckSum:  xz.CRC64,
	}
}

// XZWriter wraps w in an xz encoder with dr-sync's pinned settings.
func XZWriter(w io.Writer, dictCap int) (io.WriteCloser, error) {
	writer, err := xzWriterConfig(dictCap).NewWriter(w)
	if err != nil {
		return nil, fmt.Errorf("starting xz writer: %w", err)
	}

	return writer, nil
}

// XZReader wraps r in an xz decoder.
func XZReader(r io.Reader) (io.Reader, error) {
	reader, err := xz.ReaderConfig{}.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("starting xz reader: %w", err)
	}

	return reader, nil
}

// NewID builds a manifest id for a generation: the number zero-padded to ten digits so
// lexicographic order equals numeric order, plus random hex so two writers landing on the
// same generation produce two distinct objects rather than clobbering each other.
func NewID(generation int64) (string, error) {
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return "", fmt.Errorf("generating manifest id: %w", err)
	}

	return fmt.Sprintf("%010d-%s", generation, hex.EncodeToString(suffix)), nil
}

// NewClientID returns a random identifier for this process, used in leases and recorded in
// manifests so a fork report can name who wrote what.
func NewClientID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating client id: %w", err)
	}

	return hex.EncodeToString(buf), nil
}

// LocalAuthor describes this process for the createdBy field.
func LocalAuthor(clientID string) Author {
	author := Author{ClientID: clientID}

	if host, err := os.Hostname(); err == nil {
		author.Host = host
	}

	if current, err := user.Current(); err == nil {
		author.User = current.Username
	}

	return author
}

// Stamp fills in the identity and provenance fields of a manifest about to be written.
func (m *Manifest) Stamp(clientID string, now time.Time) error {
	id, err := NewID(m.Generation)
	if err != nil {
		return err
	}

	m.FormatVersion = FormatVersion
	m.ID = id
	m.Generator = version.Generator()
	m.CreatedAt = now.UTC().Format(time.RFC3339Nano)
	m.CreatedBy = LocalAuthor(clientID)

	if m.PathEncoding == "" {
		m.PathEncoding = "nfc"
	}

	if m.MetaPrefix == "" {
		m.MetaPrefix = DefaultMetaPrefix
	}

	return nil
}
