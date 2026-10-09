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

//go:build integration

// Package integration exercises dr-sync against a real DataRobot instance.
//
// Every test creates its own throwaway container and deletes it afterwards, so a run leaves
// nothing behind. Run with:
//
//	task test:integration
package integration

import (
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/datarobot/golang-datarobot-filesync/internal/config"
	"github.com/datarobot/golang-datarobot-filesync/internal/drapi"
	"github.com/datarobot/golang-datarobot-filesync/internal/manifest"
	"github.com/datarobot/golang-datarobot-filesync/internal/progress"
	"github.com/datarobot/golang-datarobot-filesync/internal/syncer"
)

// live holds a client and a scratch container that is deleted when the test ends.
type live struct {
	client      *drapi.Client
	store       *syncer.Store
	containerID string
}

func newLive(t *testing.T) *live {
	t.Helper()

	resolved, err := config.Resolve(config.Overrides{})
	if err != nil {
		t.Skipf("no DataRobot credentials available: %v", err)
	}

	client, err := drapi.New(drapi.Config{
		Endpoint:           resolved.Endpoint,
		Token:              resolved.Token,
		CACertFile:         resolved.CACertFile,
		InsecureSkipVerify: resolved.InsecureSkipVerify,
		UserAgentSuffix:    "integration-test",
	})
	require.NoError(t, err)

	ctx := context.Background()

	result, err := syncer.Init(ctx, client, progress.Discard(), syncer.InitOptions{
		Name:        "dr-sync integration test " + time.Now().UTC().Format(time.RFC3339),
		Description: "created by an automated test; safe to delete",
		Tags:        []string{"drsyncintegrationtest"},
	})
	require.NoError(t, err)

	t.Logf("scratch container %s", result.ContainerID)

	t.Cleanup(func() {
		// Deleting is best effort: leaving a soft-deleted catalog item behind is untidy, but
		// failing an otherwise-green test over cleanup is worse.
		if err := client.DeleteContainer(context.Background(), result.ContainerID); err != nil {
			t.Logf("could not delete scratch container %s: %v", result.ContainerID, err)
		}
	})

	return &live{
		client:      client,
		store:       syncer.NewStore(client, result.ContainerID, manifest.DefaultMetaPrefix),
		containerID: result.ContainerID,
	}
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()

	for name, content := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
	}
}

func readTree(t *testing.T, root string) map[string]string {
	t.Helper()

	files := map[string]string{}

	require.NoError(t, filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() || !info.Mode().IsRegular() {
			return nil
		}

		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}

		if strings.HasPrefix(relative, ".dr-sync") {
			return nil
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		files[filepath.ToSlash(relative)] = string(content)

		return nil
	}))

	return files
}

var fixture = map[string]string{
	"README.md":           "# integration fixture\n",
	"src/main.go":         "package main\n\nfunc main() {}\n",
	"src/deep/a/b/leaf.c": "int main(void) { return 0; }\n",
	"docs/guide.md":       strings.Repeat("prose that compresses well. ", 400),
	"empty.txt":           "",
	"data/blob.bin":       strings.Repeat("\x00\x01\x02\x03\x04\x05\x06\x07", 2048),
}

// The whole tool, against the real service.
func TestLiveRoundTrip(t *testing.T) {
	live := newLive(t)
	ctx := context.Background()

	source := t.TempDir()
	writeTree(t, source, fixture)

	pushed, err := syncer.Upload(ctx, live.store, progress.Discard(), syncer.UploadOptions{
		Root:  source,
		Level: manifest.DefaultLevel,
		Lock:  true,
	})
	require.NoError(t, err)
	require.NotNil(t, pushed.Manifest)

	assert.Equal(t, len(fixture), pushed.Manifest.Stats.FileCount)
	assert.Positive(t, pushed.BytesUploaded)
	assert.Less(t, pushed.BytesUploaded, pushed.BytesCompressed, "the packs should compress")

	// The container must hold dr-sync objects and nothing else.
	entries, err := live.client.ListFiles(ctx, live.containerID, drapi.ListOptions{})
	require.NoError(t, err)

	for _, entry := range entries {
		assert.True(t, strings.HasPrefix(entry.Name, ".dr-sync/"), "unexpected object %q", entry.Name)
	}

	// Every pack's server-computed checksum must match what the manifest recorded. This is the
	// free integrity check the API gives us.
	byName := map[string]drapi.FileEntry{}
	for _, entry := range entries {
		byName[entry.Name] = entry
	}

	for _, pack := range pushed.Manifest.Packs {
		for _, slice := range pack.Slices {
			remote, ok := byName[slice.FileName]
			require.True(t, ok, "%s should exist in the container", slice.FileName)
			assert.Equal(t, slice.SHA256, remote.Checksum,
				"the server's checksum for %s must match the manifest", slice.FileName)
			assert.Equal(t, slice.Size, remote.Size)
		}
	}

	destination := t.TempDir()

	pulled, err := syncer.Download(ctx, live.store, progress.Discard(), syncer.DownloadOptions{
		Root: destination,
	})
	require.NoError(t, err)
	assert.Empty(t, pulled.Failed)

	assert.Equal(t, fixture, readTree(t, destination))
}

// A second push of an unchanged tree must transfer nothing.
func TestLiveSecondPushIsANoOp(t *testing.T) {
	live := newLive(t)
	ctx := context.Background()

	source := t.TempDir()
	writeTree(t, source, fixture)

	options := syncer.UploadOptions{Root: source, Level: manifest.DefaultLevel, Lock: true}

	_, err := syncer.Upload(ctx, live.store, progress.Discard(), options)
	require.NoError(t, err)

	before, err := live.client.ListFiles(ctx, live.containerID, drapi.ListOptions{})
	require.NoError(t, err)

	second, err := syncer.Upload(ctx, live.store, progress.Discard(), options)
	require.NoError(t, err)

	assert.Nil(t, second.Manifest, "nothing changed, so no new manifest")
	assert.Zero(t, second.BytesUploaded)

	after, err := live.client.ListFiles(ctx, live.containerID, drapi.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, after, len(before))
}

// Editing one file must produce exactly one new pack.
func TestLiveIncrementalPush(t *testing.T) {
	live := newLive(t)
	ctx := context.Background()

	source := t.TempDir()
	writeTree(t, source, fixture)

	options := syncer.UploadOptions{Root: source, Level: manifest.DefaultLevel, Lock: true}

	_, err := syncer.Upload(ctx, live.store, progress.Discard(), options)
	require.NoError(t, err)

	writeTree(t, source, map[string]string{"src/main.go": "package main\n\nfunc main() { println(1) }\n"})

	second, err := syncer.Upload(ctx, live.store, progress.Discard(), options)
	require.NoError(t, err)
	require.NotNil(t, second.Manifest)

	assert.Equal(t, 1, second.PacksBuilt)
	assert.Equal(t, 1, second.Plan.Changed)

	destination := t.TempDir()

	_, err = syncer.Download(ctx, live.store, progress.Discard(), syncer.DownloadOptions{Root: destination})
	require.NoError(t, err)

	assert.Equal(t, readTree(t, source), readTree(t, destination))
}

// A rename is a manifest edit, not a transfer.
func TestLiveRenameTransfersNothing(t *testing.T) {
	live := newLive(t)
	ctx := context.Background()

	source := t.TempDir()
	writeTree(t, source, fixture)

	_, err := syncer.Upload(ctx, live.store, progress.Discard(), syncer.UploadOptions{
		Root: source, Level: manifest.DefaultLevel, Lock: true,
	})
	require.NoError(t, err)

	require.NoError(t, os.Rename(
		filepath.Join(source, "docs", "guide.md"),
		filepath.Join(source, "docs", "manual.md")))

	second, err := syncer.Upload(ctx, live.store, progress.Discard(), syncer.UploadOptions{
		Root: source, Level: manifest.DefaultLevel, Lock: true, Delete: true,
	})
	require.NoError(t, err)
	require.NotNil(t, second.Manifest)

	assert.Zero(t, second.BytesUploaded)
	assert.Equal(t, 1, second.Plan.ReusedBlobs)
}

// A file larger than one slice has to round-trip, since slicing is what removes the file-size
// ceiling the API would otherwise impose.
func TestLiveSlicedPackRoundTrip(t *testing.T) {
	live := newLive(t)
	ctx := context.Background()

	source := t.TempDir()

	// Random bytes, because xz would flatten anything patterned: the pack has to genuinely
	// exceed the slice size rather than collapse under it.
	payload := make([]byte, 3<<20)
	_, err := rand.Read(payload)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(source, "big.bin"), payload, 0o600))

	pushed, err := syncer.Upload(ctx, live.store, progress.Discard(), syncer.UploadOptions{
		Root:      source,
		Level:     manifest.DefaultLevel,
		Lock:      true,
		SliceSize: 512 << 10,
	})
	require.NoError(t, err)
	require.NotNil(t, pushed.Manifest)
	require.Len(t, pushed.Manifest.Packs, 1)

	assert.Greater(t, len(pushed.Manifest.Packs[0].Slices), 1, "the pack must actually be split")

	destination := t.TempDir()

	_, err = syncer.Download(ctx, live.store, progress.Discard(), syncer.DownloadOptions{Root: destination})
	require.NoError(t, err)

	recovered, err := os.ReadFile(filepath.Join(destination, "big.bin"))
	require.NoError(t, err)
	assert.Equal(t, payload, recovered)
}
