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

package syncer_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/datarobot-labs/datarobot-sync/internal/manifest"
	"github.com/datarobot-labs/datarobot-sync/internal/progress"
	"github.com/datarobot-labs/datarobot-sync/internal/scan"
	"github.com/datarobot-labs/datarobot-sync/internal/syncer"
)

// The head wants p.txt to hold what s.txt holds locally, and wants s.txt emptied. The empty
// s.txt must not be written before p.txt has been copied from it.
func TestDownloadLocalReuseReadsASourceBeforeAnEntryEmptiesIt(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	want := map[string]string{"s.txt": "", "p.txt": "XXXX"}
	h.upload(t, writeTree(t, want))

	local := writeTree(t, map[string]string{"s.txt": "XXXX"})
	h.download(t, local)

	assert.Equal(t, want, readTree(t, local))
}

// Two files trade contents. Each is the other's local source.
func TestDownloadLocalReuseHandlesSwappedContents(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	want := map[string]string{"a.txt": "YYYY", "b.txt": "XXXX"}
	h.upload(t, writeTree(t, want))

	local := writeTree(t, map[string]string{"a.txt": "XXXX", "b.txt": "YYYY"})
	h.download(t, local)

	assert.Equal(t, want, readTree(t, local))
}

// The scan adopts the manifest's recorded digest for a file whose size, mode and mtime match, so
// an edit that keeps all three goes unseen. Copying from it must fail rather than hand the wrong
// bytes to another path.
func TestDownloadRefusesALocalSourceThatNoLongerHoldsTheRecordedContent(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.upload(t, writeTree(t, map[string]string{"x.txt": "XXXX", "y.txt": "XXXX"}))

	state, err := h.store.ReadState(context.Background())
	require.NoError(t, err)

	head, ok := manifest.SelectHead(state.Manifests)
	require.True(t, ok)

	loaded, err := h.store.LoadManifest(context.Background(), head)
	require.NoError(t, err)

	var recorded time.Time

	for _, entry := range loaded.Entries {
		if entry.Path == "x.txt" {
			recorded, err = time.Parse(time.RFC3339Nano, entry.ModTime)
			require.NoError(t, err)
		}
	}

	require.False(t, recorded.IsZero(), "x.txt must be in the manifest")

	local := writeTree(t, map[string]string{"x.txt": "YYYY"})
	require.NoError(t, os.Chtimes(filepath.Join(local, "x.txt"), recorded, recorded))

	_, err = syncer.Download(context.Background(), h.store, progress.Discard(),
		syncer.DownloadOptions{Root: local, Check: scan.CheckAuto})

	require.ErrorContains(t, err, "no longer holds the content")

	_, statErr := os.Stat(filepath.Join(local, "y.txt"))
	require.ErrorIs(t, statErr, os.ErrNotExist, "y.txt must not be written from the wrong bytes")
}
