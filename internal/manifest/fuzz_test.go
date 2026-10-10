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
	"encoding/json"
	"testing"

	"github.com/ulikunitz/xz"
)

// assertAcceptedManifestIsSafe checks what a decoder that said "valid" has promised: every
// path is one extraction may write, and the manifest survives a re-encode.
func assertAcceptedManifestIsSafe(t *testing.T, m *Manifest) {
	t.Helper()

	for _, entry := range m.Entries {
		if err := ValidatePath(entry.Path); err != nil {
			t.Fatalf("decoder accepted an entry whose path is refused by ValidatePath: %v", err)
		}
	}

	again, err := Encode(m)
	if err != nil {
		t.Fatalf("decoder accepted a manifest that Encode refuses: %v", err)
	}

	if _, err := DecodeBytes(again); err != nil {
		t.Fatalf("re-encoded manifest does not decode: %v", err)
	}
}

// fuzzDecodeLimit caps how far the fuzzer lets a manifest expand. The production cap is 1 GiB,
// and a tiny xz stream of zeros reaches it, which would exhaust memory in every fuzz worker.
const fuzzDecodeLimit = 16 << 20

// FuzzDecodeBytes feeds arbitrary bytes to the container layer. Random bytes almost never
// form a valid xz stream, so this guards the xz and size-limit handling; the JSON layer is
// covered by FuzzDecodeManifestJSON.
func FuzzDecodeBytes(f *testing.F) {
	valid, err := Encode(sample())
	if err != nil {
		f.Fatal(err)
	}

	f.Add(valid)
	f.Add(valid[:len(valid)/2])
	f.Add([]byte{})
	f.Add([]byte("not xz at all"))

	f.Fuzz(func(t *testing.T, data []byte) {
		m, err := decode(bytes.NewReader(data), fuzzDecodeLimit)
		if err != nil {
			return
		}

		assertAcceptedManifestIsSafe(t, m)
	})
}

// FuzzDecodeManifestJSON compresses arbitrary bytes as a valid xz stream first, so the fuzzer
// reaches the JSON parser and Validate instead of stopping at the xz header.
func FuzzDecodeManifestJSON(f *testing.F) {
	valid, err := json.Marshal(sample())
	if err != nil {
		f.Fatal(err)
	}

	f.Add(valid)
	f.Add([]byte(`{"formatVersion":1}`))
	f.Add([]byte(`{"formatVersion":1,"entries":[{"path":"../escape","type":"file"}]}`))
	f.Add([]byte(`{"formatVersion":1,"entries":[{"path":"/abs","type":"file"}]}`))
	f.Add([]byte(`not json`))

	f.Fuzz(func(t *testing.T, plain []byte) {
		var compressed bytes.Buffer

		writer, err := xz.NewWriter(&compressed)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := writer.Write(plain); err != nil {
			t.Fatal(err)
		}

		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}

		m, err := DecodeBytes(compressed.Bytes())
		if err != nil {
			return
		}

		assertAcceptedManifestIsSafe(t, m)
	})
}
