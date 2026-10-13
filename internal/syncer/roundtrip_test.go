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
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/datarobot-labs/datarobot-sync/internal/manifest"
	"github.com/datarobot-labs/datarobot-sync/internal/progress"
	"github.com/datarobot-labs/datarobot-sync/internal/scan"
	"github.com/datarobot-labs/datarobot-sync/internal/syncer"
)

// harness is a fake API plus a container ready to sync into.
type harness struct {
	api         *fakeAPI
	store       *syncer.Store
	containerID string
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	api := newFakeAPI(t)
	client := api.client(t)

	result, err := syncer.Init(context.Background(), client, progress.Discard(), syncer.InitOptions{
		Name: "test container",
	})
	require.NoError(t, err)

	return &harness{
		api:         api,
		store:       syncer.NewStore(client, result.ContainerID, manifest.DefaultMetaPrefix),
		containerID: result.ContainerID,
	}
}

func (h *harness) upload(t *testing.T, root string, mutate ...func(*syncer.UploadOptions)) *syncer.UploadResult {
	t.Helper()

	opts := syncer.UploadOptions{Root: root, Level: manifest.DefaultLevel, Lock: true}
	for _, apply := range mutate {
		apply(&opts)
	}

	result, err := syncer.Upload(context.Background(), h.store, progress.Discard(), opts)
	require.NoError(t, err)

	return result
}

func (h *harness) download(t *testing.T, root string, mutate ...func(*syncer.DownloadOptions)) *syncer.DownloadResult {
	t.Helper()

	opts := syncer.DownloadOptions{Root: root}
	for _, apply := range mutate {
		apply(&opts)
	}

	result, err := syncer.Download(context.Background(), h.store, progress.Discard(), opts)
	require.NoError(t, err)

	return result
}

// writeTree materializes a fixture directory.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()

	root := t.TempDir()
	writeInto(t, root, files)

	return root
}

func writeInto(t *testing.T, root string, files map[string]string) {
	t.Helper()

	for name, content := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
	}
}

// readTree reads a directory into a map, so two trees can be compared directly.
func readTree(t *testing.T, root string) map[string]string {
	t.Helper()

	files := map[string]string{}

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
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
	})
	require.NoError(t, err)

	return files
}

var fixture = map[string]string{
	"README.md":           "# a project\n",
	"src/main.go":         "package main\n\nfunc main() {}\n",
	"src/util/helper.go":  "package util\n",
	"docs/guide.md":       strings.Repeat("documentation that compresses nicely. ", 200),
	"empty.txt":           "",
	"assets/logo.bin":     strings.Repeat("\x00\x01\x02\x03", 500),
	"deep/a/b/c/leaf.txt": "leaf content\n",
}

// The whole point of the tool, end to end: a tree goes up and comes back identical.
func TestRoundTrip(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := writeTree(t, fixture)

	result := h.upload(t, source)
	require.NotNil(t, result.Manifest)
	assert.EqualValues(t, 1, result.Manifest.Generation)
	assert.Equal(t, len(fixture), result.Manifest.Stats.FileCount)

	// The container holds only dr-sync's own objects: no user paths leak into it.
	for _, name := range h.api.objectNames(h.containerID) {
		assert.True(t, strings.HasPrefix(name, ".dr-sync/"), "unexpected object %q", name)
	}

	destination := t.TempDir()
	pulled := h.download(t, destination)

	assert.Equal(t, len(fixture), pulled.Written)
	assert.Equal(t, fixture, readTree(t, destination))
}

// A second push of an unchanged tree must move nothing at all.
func TestSecondPushIsANoOp(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := writeTree(t, fixture)

	h.upload(t, source)
	before := h.api.objectNames(h.containerID)

	second := h.upload(t, source)

	assert.Nil(t, second.Manifest, "an unchanged tree writes no new manifest")
	assert.Zero(t, second.BytesUploaded)
	assert.Equal(t, before, h.api.objectNames(h.containerID))
}

// Editing one file must produce exactly one new pack, not re-send the tree.
func TestIncrementalPushSendsOnlyTheChange(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := writeTree(t, fixture)

	h.upload(t, source)
	packsBefore := h.api.countObjects(h.containerID, ".dr-sync/packs/")

	writeInto(t, source, map[string]string{"src/main.go": "package main\n\nfunc main() { println(1) }\n"})

	second := h.upload(t, source)
	require.NotNil(t, second.Manifest)

	assert.Equal(t, 1, second.PacksBuilt, "one changed file, one new pack")
	assert.Equal(t, 1, second.Plan.Changed)
	assert.Len(t, second.Plan.NeedUpload, 1, "only the edited file's content moves")
	// Unchanged covers directories too, so it is compared against the scanned entry count.
	assert.Equal(t, len(second.Scan.Entries)-1, second.Plan.Unchanged)
	assert.Equal(t, packsBefore+1, h.api.countObjects(h.containerID, ".dr-sync/packs/"))

	destination := t.TempDir()
	h.download(t, destination)
	assert.Equal(t, readTree(t, source), readTree(t, destination))
}

// Content addressing means a rename is a manifest edit, not a transfer.
func TestRenameTransfersNothing(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := writeTree(t, fixture)

	h.upload(t, source)
	packsBefore := h.api.countObjects(h.containerID, ".dr-sync/packs/")

	require.NoError(t, os.Rename(
		filepath.Join(source, "docs", "guide.md"),
		filepath.Join(source, "docs", "manual.md")))

	second := h.upload(t, source, func(o *syncer.UploadOptions) { o.Delete = true })
	require.NotNil(t, second.Manifest)

	assert.Zero(t, second.BytesUploaded, "the content is already stored, only its path changed")
	assert.Equal(t, 1, second.Plan.ReusedBlobs)
	assert.Equal(t, packsBefore, h.api.countObjects(h.containerID, ".dr-sync/packs/"))

	destination := t.TempDir()
	h.download(t, destination)

	pulled := readTree(t, destination)
	assert.Contains(t, pulled, "docs/manual.md")
	assert.NotContains(t, pulled, "docs/guide.md")
}

// A pull only needs the packs holding content it does not already have.
func TestDownloadReusesLocalContent(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := writeTree(t, fixture)
	h.upload(t, source)

	// A destination that already holds the same content under a different name: the file has
	// to be produced, but not fetched.
	destination := t.TempDir()
	writeInto(t, destination, map[string]string{"copied-guide.md": fixture["docs/guide.md"]})

	result := h.download(t, destination)

	var reused bool

	for _, copy := range result.Plan.LocalCopies {
		if copy.Entry.Path == "docs/guide.md" {
			reused = true
		}
	}

	assert.True(t, reused, "identical local content should be cloned rather than downloaded")
	assert.Equal(t, fixture["docs/guide.md"], readTree(t, destination)["docs/guide.md"])
}

// Without --delete an upload is additive: a path removed locally stays in the manifest.
func TestDeleteSemantics(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := writeTree(t, fixture)
	h.upload(t, source)

	require.NoError(t, os.Remove(filepath.Join(source, "README.md")))

	additive := h.upload(t, source)
	assert.Nil(t, additive.Manifest, "an additive push with nothing new to add changes nothing")

	pruning := h.upload(t, source, func(o *syncer.UploadOptions) { o.Delete = true })
	require.NotNil(t, pruning.Manifest)
	assert.Equal(t, 1, pruning.Plan.Dropped)

	destination := t.TempDir()
	h.download(t, destination)
	assert.NotContains(t, readTree(t, destination), "README.md")
}

// A pull leaves local files alone unless told otherwise.
func TestDownloadDeleteExtraneous(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := writeTree(t, fixture)
	h.upload(t, source)

	destination := t.TempDir()
	writeInto(t, destination, map[string]string{"local-only.txt": "mine"})

	kept := h.download(t, destination)
	assert.Contains(t, kept.Plan.Extraneous, "local-only.txt")
	assert.Contains(t, readTree(t, destination), "local-only.txt")

	h.download(t, destination, func(o *syncer.DownloadOptions) { o.Delete = true })
	assert.NotContains(t, readTree(t, destination), "local-only.txt")
}

// The resume path: a push that lands its packs but dies before the manifest must be able to
// finish, which only works because the pack name is derived from its content and the commit
// would otherwise reject the duplicate.
func TestResumeAfterPacksLandedButManifestDidNot(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := writeTree(t, fixture)

	// Force the manifest into its own stage, then fail that stage.
	failManifest := true

	h.api.beforeCommit = func(_ string, names []string) error {
		if !failManifest {
			return nil
		}

		for _, name := range names {
			if strings.HasPrefix(name, ".dr-sync/manifests/") {
				return errors.New("simulated crash before the manifest committed")
			}
		}

		return nil
	}

	_, err := syncer.Upload(context.Background(), h.store, progress.Discard(), syncer.UploadOptions{
		Root:          source,
		Level:         manifest.DefaultLevel,
		Lock:          true,
		MaxStageFiles: 1, // one object per stage, so packs commit before the manifest
	})
	require.Error(t, err)

	packsAfterCrash := h.api.countObjects(h.containerID, ".dr-sync/packs/")
	require.Positive(t, packsAfterCrash, "the packs should have landed before the crash")

	// The head is still generation 0, so no reader ever saw the half-finished push.
	state, err := h.store.ReadState(context.Background())
	require.NoError(t, err)

	head, _, err := h.store.Head(context.Background(), state)
	require.NoError(t, err)
	assert.EqualValues(t, 0, head.Generation)

	failManifest = false

	resumed := h.upload(t, source, func(o *syncer.UploadOptions) { o.MaxStageFiles = 1 })
	require.NotNil(t, resumed.Manifest)

	assert.Positive(t, resumed.PacksSkipped, "the packs already in the container must be recognized, not re-sent")
	assert.Zero(t, resumed.BytesUploaded, "a resume that re-uploads nothing is the point")

	destination := t.TempDir()
	h.download(t, destination)
	assert.Equal(t, fixture, readTree(t, destination))
}

// Two clients pushing at once: one must be refused rather than silently losing its work.
func TestSecondWriterIsRefusedWhileALeaseIsHeld(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := writeTree(t, fixture)

	state, err := h.store.ReadState(context.Background())
	require.NoError(t, err)

	holder, err := syncer.AcquireLease(context.Background(), h.store, state, "other-client", time.Hour, false)
	require.NoError(t, err)

	_, err = syncer.Upload(context.Background(), h.store, progress.Discard(), syncer.UploadOptions{
		Root:  source,
		Level: manifest.DefaultLevel,
		Lock:  true,
	})
	require.Error(t, err)
	assert.True(t, syncer.IsConflict(err), "a held lease must be a conflict, not a generic failure")
	require.ErrorContains(t, err, "--force")

	// --force overrides it, for the case where the holder is known to be gone.
	_, err = syncer.Upload(context.Background(), h.store, progress.Discard(), syncer.UploadOptions{
		Root:  source,
		Level: manifest.DefaultLevel,
		Lock:  true,
		Force: true,
	})
	require.NoError(t, err)

	require.NoError(t, holder.Release(context.Background()))
}

// A stable ClientID reused across many AcquireLease calls (watch's whole point) must not try
// to recreate the exact lock name an earlier, unreleased call already holds — Overwrite: error
// would otherwise reject that commit outright, exactly as if it crashed mid-push or Release
// itself failed.
func TestStableClientIDDoesNotCollideWithItsOwnOrphanedLease(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	state, err := h.store.ReadState(context.Background())
	require.NoError(t, err)

	orphan, err := syncer.AcquireLease(context.Background(), h.store, state, "watch-client", time.Hour, false)
	require.NoError(t, err)

	// orphan is deliberately never released, simulating a crashed cycle or a Release call
	// that itself failed.
	state, err = h.store.ReadState(context.Background())
	require.NoError(t, err)

	next, err := syncer.AcquireLease(context.Background(), h.store, state, "watch-client", time.Hour, false)
	require.NoError(t, err, "a stable ClientID must not collide with its own orphaned lease object")

	state, err = h.store.ReadState(context.Background())
	require.NoError(t, err)
	assert.Len(t, state.Leases, 2, "both the orphan and the new lease must exist as distinct objects")

	require.NoError(t, orphan.Release(context.Background()))
	require.NoError(t, next.Release(context.Background()))
}

// A lease whose body cannot be read must still expose the clientId and sequence recovered from
// its object name: ownMaxSequence needs both to keep a stable watch ClientID from retrying the
// exact lock name it already holds, exactly as it must for a readable orphan (see above).
func TestStableClientIDDoesNotCollideWithAnUnreadableOwnLease(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	state, err := h.store.ReadState(context.Background())
	require.NoError(t, err)

	orphan, err := syncer.AcquireLease(context.Background(), h.store, state, "watch-client", time.Hour, false)
	require.NoError(t, err)

	state, err = h.store.ReadState(context.Background())
	require.NoError(t, err)

	var objectName string

	for _, lease := range state.Leases {
		if lease.ClientID == "watch-client" {
			objectName = lease.ObjectName
		}
	}

	require.NotEmpty(t, objectName, "the orphan lease must be visible before it is corrupted")

	h.api.corruptObject(h.containerID, objectName)

	state, err = h.store.ReadState(context.Background())
	require.NoError(t, err)

	var (
		corrupted syncer.Lease
		found     bool
	)

	for _, lease := range state.Leases {
		if lease.ObjectName == objectName {
			corrupted, found = lease, true
		}
	}

	require.True(t, found, "an unreadable lease must still be listed")
	assert.Equal(t, "watch-client", corrupted.ClientID,
		"the clientId parsed from the object name must survive an unreadable body")

	next, err := syncer.AcquireLease(context.Background(), h.store, state, "watch-client", time.Hour, false)
	require.NoError(t, err,
		"a stable ClientID must not collide with its own lease even when that lease's body cannot be read")

	require.NoError(t, orphan.Release(context.Background()))
	require.NoError(t, next.Release(context.Background()))
}

// An expired lease must not block anyone.
func TestExpiredLeaseIsIgnored(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := writeTree(t, fixture)

	state, err := h.store.ReadState(context.Background())
	require.NoError(t, err)

	_, err = syncer.AcquireLease(context.Background(), h.store, state, "stale-client", time.Nanosecond, false)
	require.NoError(t, err)

	time.Sleep(2 * time.Millisecond)

	result := h.upload(t, source)
	assert.NotNil(t, result.Manifest)
}

func TestSymlinksAndEmptyDirectoriesRoundTrip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}

	t.Parallel()

	h := newHarness(t)
	source := writeTree(t, map[string]string{"target.txt": "pointed at"})

	require.NoError(t, os.Symlink("target.txt", filepath.Join(source, "link.txt")))
	require.NoError(t, os.MkdirAll(filepath.Join(source, "empty", "nested"), 0o755))

	h.upload(t, source)

	destination := t.TempDir()
	h.download(t, destination)

	link, err := os.Readlink(filepath.Join(destination, "link.txt"))
	require.NoError(t, err)
	assert.Equal(t, "target.txt", link)

	info, err := os.Stat(filepath.Join(destination, "empty", "nested"))
	require.NoError(t, err)
	assert.True(t, info.IsDir(), "an empty directory must survive the round trip")
}

func TestExecutableBitSurvives(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no executable bit")
	}

	t.Parallel()

	h := newHarness(t)
	source := writeTree(t, map[string]string{"script.sh": "#!/bin/sh\necho hi\n"})
	require.NoError(t, os.Chmod(filepath.Join(source, "script.sh"), 0o755))

	h.upload(t, source)

	destination := t.TempDir()
	h.download(t, destination)

	info, err := os.Stat(filepath.Join(destination, "script.sh"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())
}

// dr-sync must refuse to run rather than quietly declining to sync a real directory of the
// user's that happens to share the reserved name.
func TestRefusesWhenReservedNameIsTaken(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := writeTree(t, map[string]string{".dr-sync/mine.txt": "a directory of my own"})

	_, err := syncer.Upload(context.Background(), h.store, progress.Discard(), syncer.UploadOptions{
		Root:  source,
		Level: manifest.DefaultLevel,
	})
	require.ErrorContains(t, err, "--meta-prefix")
}

func TestExcludePatternsAreHonoured(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := writeTree(t, map[string]string{
		"keep.txt":    "keep",
		"skip.log":    "skip",
		"build/out.o": "skip",
	})

	matcher := mustMatcher(t, []string{"*.log", "build/"})

	h.upload(t, source, func(o *syncer.UploadOptions) { o.Matcher = matcher })

	destination := t.TempDir()
	h.download(t, destination)

	pulled := readTree(t, destination)
	assert.Equal(t, map[string]string{"keep.txt": "keep"}, pulled)
}

// A dry run must plan exactly as the real push would and change nothing.
func TestDryRunWritesNothing(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := writeTree(t, fixture)

	before := h.api.objectNames(h.containerID)

	result := h.upload(t, source, func(o *syncer.UploadOptions) { o.DryRun = true })

	assert.True(t, result.DryRun)
	assert.Nil(t, result.Manifest)
	assert.Equal(t, len(result.Scan.Entries), result.Plan.New, "everything is new on a first push")
	assert.Len(t, result.Plan.NeedUpload, len(fixture)-1, "the empty file needs no pack member")
	assert.Equal(t, before, h.api.objectNames(h.containerID))
}

// mustMatcher compiles ignore patterns for a test.
func mustMatcher(t *testing.T, patterns []string) *scan.Matcher {
	t.Helper()

	matcher, err := scan.NewMatcher(patterns)
	require.NoError(t, err)

	return matcher
}
