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
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/datarobot-labs/datarobot-sync/internal/manifest"
	"github.com/datarobot-labs/datarobot-sync/internal/progress"
	"github.com/datarobot-labs/datarobot-sync/internal/scan"
	"github.com/datarobot-labs/datarobot-sync/internal/syncer"
)

func headID(t *testing.T, h *harness) string {
	t.Helper()

	state, err := h.store.ReadState(context.Background())
	require.NoError(t, err)

	head, ok := manifest.SelectHead(state.Manifests)
	require.True(t, ok)

	return head.ID
}

func TestUploadAcceptsTheExpectedHead(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := writeTree(t, map[string]string{"a.txt": "one"})
	h.upload(t, source)

	expected := headID(t, h)

	writeInto(t, source, map[string]string{"a.txt": "two"})
	result := h.upload(t, source, func(o *syncer.UploadOptions) { o.ExpectHead = expected })

	require.NotNil(t, result.Manifest)
	assert.NotEqual(t, expected, headID(t, h), "the push must have moved the head")
}

func TestUploadRefusesAStaleBase(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	mine := writeTree(t, map[string]string{"a.txt": "base"})
	h.upload(t, mine)

	based := headID(t, h)

	theirs := t.TempDir()
	h.download(t, theirs)
	writeInto(t, theirs, map[string]string{"a.txt": "a teammate's edit"})
	h.upload(t, theirs)

	afterTeammate := headID(t, h)
	manifestsBefore := h.api.countObjects(h.containerID, ".dr-sync/manifests/")

	writeInto(t, mine, map[string]string{"a.txt": "my stale edit"})

	_, err := syncer.Upload(context.Background(), h.store, progress.Discard(),
		syncer.UploadOptions{Root: mine, Level: manifest.DefaultLevel, Lock: true, ExpectHead: based})

	var stale *syncer.StaleBaseError

	require.ErrorAs(t, err, &stale)
	assert.True(t, syncer.IsConflict(err), "a stale base exits as a conflict, like a moved head")
	assert.Equal(t, stale.Expected, based)
	assert.Equal(t, afterTeammate, stale.Actual)

	assert.Equal(t, afterTeammate, headID(t, h), "the refused push must not move the head")
	assert.Equal(t, manifestsBefore, h.api.countObjects(h.containerID, ".dr-sync/manifests/"))

	check := t.TempDir()
	h.download(t, check)
	assert.Equal(t, map[string]string{"a.txt": "a teammate's edit"}, readTree(t, check),
		"the teammate's edit must survive the refused push")
}

func TestUploadStaleBaseIsNotOverriddenByForce(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := writeTree(t, map[string]string{"a.txt": "base"})
	h.upload(t, source)

	based := headID(t, h)

	other := t.TempDir()
	h.download(t, other)
	writeInto(t, other, map[string]string{"a.txt": "newer"})
	h.upload(t, other)

	writeInto(t, source, map[string]string{"a.txt": "stale"})

	_, err := syncer.Upload(context.Background(), h.store, progress.Discard(),
		syncer.UploadOptions{Root: source, Level: manifest.DefaultLevel, Lock: true, Force: true, ExpectHead: based})

	var stale *syncer.StaleBaseError

	require.ErrorAs(t, err, &stale)
}

func TestUploadRefusesAnExpectedHeadThatNeverExisted(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := writeTree(t, map[string]string{"a.txt": "one"})

	_, err := syncer.Upload(context.Background(), h.store, progress.Discard(),
		syncer.UploadOptions{Root: source, Level: manifest.DefaultLevel, Lock: true, ExpectHead: "0000000007-deadbeef"})

	var stale *syncer.StaleBaseError

	require.ErrorAs(t, err, &stale)
}

// bobEdits returns a harness and bob's tree, where bob edited a.txt locally and alice has since
// pushed a newer a.txt.
func bobEdits(t *testing.T) (h *harness, bob string) {
	t.Helper()

	h = newHarness(t)
	alice := writeTree(t, map[string]string{"a.txt": "v1", "keep.txt": "same everywhere"})
	h.upload(t, alice)

	bob = t.TempDir()
	h.download(t, bob)
	writeInto(t, bob, map[string]string{"a.txt": "bob's local edit, longer than v1"})

	writeInto(t, alice, map[string]string{"a.txt": "v2 from alice"})
	h.upload(t, alice)

	return h, bob
}

func TestDownloadBackupKeepsALocalEditItReplaces(t *testing.T) {
	t.Parallel()

	h, bob := bobEdits(t)

	result := h.download(t, bob, func(o *syncer.DownloadOptions) { o.Backup = true })

	content, err := os.ReadFile(filepath.Join(bob, "a.txt"))
	require.NoError(t, err)
	assert.Equal(t, "v2 from alice", string(content))

	assert.Equal(t, 1, result.BackedUp)
	require.NotEmpty(t, result.BackupDir)

	kept, err := os.ReadFile(filepath.Join(result.BackupDir, "a.txt"))
	require.NoError(t, err)
	assert.Equal(t, "bob's local edit, longer than v1", string(kept), "the replaced edit must be recoverable")

	_, err = os.Stat(filepath.Join(result.BackupDir, "keep.txt"))
	assert.ErrorIs(t, err, os.ErrNotExist, "a file the pull left alone must not be copied")
}

func TestDownloadWithoutBackupKeepsNoCopy(t *testing.T) {
	t.Parallel()

	h, bob := bobEdits(t)

	result := h.download(t, bob)

	assert.Zero(t, result.BackedUp)
	assert.Empty(t, result.BackupDir)

	_, err := os.Stat(filepath.Join(bob, ".dr-sync-backup"))
	assert.ErrorIs(t, err, os.ErrNotExist, "the default pull must not create the backup directory")
}

func TestDownloadBackupKeepsWhatDeleteRemoves(t *testing.T) {
	t.Parallel()

	h, bob := bobEdits(t)
	writeInto(t, bob, map[string]string{"scratch/notes.txt": "only on bob's disk"})

	result := h.download(t, bob, func(o *syncer.DownloadOptions) {
		o.Backup = true
		o.Delete = true
	})

	_, err := os.Stat(filepath.Join(bob, "scratch", "notes.txt"))
	require.ErrorIs(t, err, os.ErrNotExist, "--delete still removes it from the tree")

	kept, err := os.ReadFile(filepath.Join(result.BackupDir, "scratch", "notes.txt"))
	require.NoError(t, err)
	assert.Equal(t, "only on bob's disk", string(kept))
}

func TestBackupDirectoryIsNotUploaded(t *testing.T) {
	t.Parallel()

	h, bob := bobEdits(t)
	h.download(t, bob, func(o *syncer.DownloadOptions) { o.Backup = true })

	_, err := os.Stat(filepath.Join(bob, ".dr-sync-backup"))
	require.NoError(t, err, "the backup must exist for this test to mean anything")

	writeInto(t, bob, map[string]string{"new.txt": "a real change"})
	h.upload(t, bob)

	check := t.TempDir()
	h.download(t, check)

	_, err = os.Stat(filepath.Join(check, ".dr-sync-backup"))
	require.ErrorIs(t, err, os.ErrNotExist, "backup copies must never reach the container")
	assert.Equal(t, "a real change", readTree(t, check)["new.txt"])
}

func TestDownloadBackupKeepsALocalFileThatBecomesEmpty(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.upload(t, writeTree(t, map[string]string{"e.txt": ""}))

	local := writeTree(t, map[string]string{"e.txt": "local content"})
	result := h.download(t, local, func(o *syncer.DownloadOptions) { o.Backup = true })

	assert.Equal(t, map[string]string{"e.txt": ""}, readTree(t, local))

	kept, err := os.ReadFile(filepath.Join(result.BackupDir, "e.txt"))
	require.NoError(t, err)
	assert.Equal(t, "local content", string(kept))
}

func TestDownloadBackupKeepsALocalFileReplacedByACopyOfAnotherLocalFile(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.upload(t, writeTree(t, map[string]string{"p.txt": "XXXX"}))

	local := writeTree(t, map[string]string{"s.txt": "XXXX", "p.txt": "old"})
	result := h.download(t, local, func(o *syncer.DownloadOptions) { o.Backup = true })

	assert.Equal(t, map[string]string{"s.txt": "XXXX", "p.txt": "XXXX"}, readTree(t, local))

	kept, err := os.ReadFile(filepath.Join(result.BackupDir, "p.txt"))
	require.NoError(t, err)
	assert.Equal(t, "old", string(kept))
}

func TestDownloadBackupKeepsALocalFileReplacedBySymlink(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}

	h := newHarness(t)
	source := writeTree(t, map[string]string{"a.txt": "target"})
	require.NoError(t, os.Symlink("a.txt", filepath.Join(source, "link")))
	h.upload(t, source)

	local := writeTree(t, map[string]string{"link": "a regular file"})
	result := h.download(t, local, func(o *syncer.DownloadOptions) { o.Backup = true })

	target, err := os.Readlink(filepath.Join(local, "link"))
	require.NoError(t, err)
	assert.Equal(t, "a.txt", target)

	kept, err := os.ReadFile(filepath.Join(result.BackupDir, "link"))
	require.NoError(t, err)
	assert.Equal(t, "a regular file", string(kept))
}

// A directory that became a file upstream: the pull moves the old directory aside, and the files
// that were inside it must not be reported as failures to remove.
func TestDownloadBackupWithDeleteSurvivesADirectoryBecomingAFile(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.upload(t, writeTree(t, map[string]string{"d": "now a file"}))

	local := writeTree(t, map[string]string{"d/x": "old child"})
	result := h.download(t, local, func(o *syncer.DownloadOptions) {
		o.Backup = true
		o.Delete = true
	})

	assert.Equal(t, map[string]string{"d": "now a file"}, readTree(t, local))

	kept, err := os.ReadFile(filepath.Join(result.BackupDir, "d", "x"))
	require.NoError(t, err)
	assert.Equal(t, "old child", string(kept))
}

// --delete-excluded takes a directory that only holds excluded files. With --backup it moves the
// whole directory aside, even when part of it is already in the backup.
func TestDownloadBackupWithDeleteExcludedMovesADirectoryHoldingExcludedFiles(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.upload(t, writeTree(t, map[string]string{"keep.txt": "k"}))

	local := writeTree(t, map[string]string{"keep.txt": "k", "old/a.txt": "a", "old/b.log": "excluded"})

	matcher, err := scan.NewMatcher([]string{"*.log"})
	require.NoError(t, err)

	result := h.download(t, local, func(o *syncer.DownloadOptions) {
		o.Backup = true
		o.Delete = true
		o.DeleteExcluded = true
		o.Matcher = matcher
	})

	_, statErr := os.Stat(filepath.Join(local, "old"))
	require.ErrorIs(t, statErr, os.ErrNotExist)

	for name, want := range map[string]string{"a.txt": "a", "b.log": "excluded"} {
		kept, err := os.ReadFile(filepath.Join(result.BackupDir, "old", name))
		require.NoError(t, err, name)
		assert.Equal(t, want, string(kept), name)
	}
}

// An older or foreign writer can record a path under the backup directory. The pull must not
// write into it, or a later --backup would mix its copies with the manifest's.
func TestDownloadSkipsAManifestPathUnderTheBackupDirectory(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	next := manifest.New(manifest.DefaultMetaPrefix)
	next.Generation = 1
	next.Entries = []manifest.Entry{
		{Path: ".dr-sync-backup/x.txt", Type: manifest.TypeFile},
		{Path: "ok.txt", Type: manifest.TypeFile},
	}

	require.NoError(t, next.Stamp("hand-built", time.Now()))

	encoded, err := manifest.Encode(next)
	require.NoError(t, err)

	claim := []byte(next.ID)

	require.NoError(t, h.store.Commit(context.Background(), nil, []syncer.Object{
		{Name: next.ObjectName(), Content: encoded, Size: int64(len(encoded))},
		{Name: manifest.ClaimName(manifest.DefaultMetaPrefix, 1), Content: claim, Size: int64(len(claim))},
	}, syncer.CommitOptions{}))

	local := t.TempDir()
	result := h.download(t, local)

	require.Len(t, result.Plan.Skipped, 1)
	assert.Equal(t, ".dr-sync-backup/x.txt", result.Plan.Skipped[0].Path)
	assert.Equal(t, map[string]string{"ok.txt": ""}, readTree(t, local))

	_, statErr := os.Stat(filepath.Join(local, ".dr-sync-backup"))
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

// The pull moves local directory d aside and puts a symlink to target in its place, so d's old
// children are extraneous. Cleaning them up must not follow the new symlink into target, which
// the manifest owns.
func TestDownloadBackupWithDeleteDoesNotCleanUpThroughANewSymlink(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}

	h := newHarness(t)
	remote := writeTree(t, map[string]string{"target/child/x": "keep"})
	require.NoError(t, os.Symlink("target", filepath.Join(remote, "d")))
	h.upload(t, remote)

	local := writeTree(t, map[string]string{"d/child/x": "old"})
	result := h.download(t, local, func(o *syncer.DownloadOptions) {
		o.Backup = true
		o.Delete = true
	})

	kept, err := os.ReadFile(filepath.Join(local, "target", "child", "x"))
	require.NoError(t, err)
	assert.Equal(t, "keep", string(kept))

	old, err := os.ReadFile(filepath.Join(result.BackupDir, "d", "child", "x"))
	require.NoError(t, err)
	assert.Equal(t, "old", string(old))
}

// A local file where the manifest has a directory: the pull moves the file aside and creates the
// directory.
func TestDownloadBackupMovesAFileThatBecomesADirectory(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.upload(t, writeTree(t, map[string]string{"d/x": "new child"}))

	local := writeTree(t, map[string]string{"d": "old file"})
	result := h.download(t, local, func(o *syncer.DownloadOptions) { o.Backup = true })

	assert.Equal(t, map[string]string{"d/x": "new child"}, readTree(t, local))

	kept, err := os.ReadFile(filepath.Join(result.BackupDir, "d"))
	require.NoError(t, err)
	assert.Equal(t, "old file", string(kept))
}

// A symlink in place of the backup directory would send the moved files wherever it points.
func TestDownloadBackupRefusesASymlinkedBackupDirectory(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}

	h := newHarness(t)
	h.upload(t, writeTree(t, map[string]string{"a.txt": "remote"}))

	local := writeTree(t, map[string]string{"a.txt": "local edit"})
	outside := t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(local, scan.BackupDirName)))

	_, err := syncer.Download(context.Background(), h.store, progress.Discard(),
		syncer.DownloadOptions{Root: local, Backup: true})
	require.Error(t, err)

	moved, readErr := os.ReadDir(outside)
	require.NoError(t, readErr)
	assert.Empty(t, moved, "nothing may be moved outside the sync root")

	kept, readErr := os.ReadFile(filepath.Join(local, "a.txt"))
	require.NoError(t, readErr)
	assert.Equal(t, "local edit", string(kept))
}

// On a filesystem that ignores case, .DR-SYNC-BACKUP resolves into .dr-sync-backup, so a manifest
// path under that spelling would write into saved local edits.
func TestDownloadSkipsADifferentlyCasedBackupDirectoryPath(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	next := manifest.New(manifest.DefaultMetaPrefix)
	next.Generation = 1
	next.Entries = []manifest.Entry{
		{Path: ".DR-SYNC-BACKUP/x.txt", Type: manifest.TypeFile},
		{Path: "ok.txt", Type: manifest.TypeFile},
	}

	require.NoError(t, next.Stamp("hand-built", time.Now()))

	encoded, err := manifest.Encode(next)
	require.NoError(t, err)

	claim := []byte(next.ID)

	require.NoError(t, h.store.Commit(context.Background(), nil, []syncer.Object{
		{Name: next.ObjectName(), Content: encoded, Size: int64(len(encoded))},
		{Name: manifest.ClaimName(manifest.DefaultMetaPrefix, 1), Content: claim, Size: int64(len(claim))},
	}, syncer.CommitOptions{}))

	local := t.TempDir()
	result := h.download(t, local)

	require.Len(t, result.Plan.Skipped, 1)
	assert.Equal(t, ".DR-SYNC-BACKUP/x.txt", result.Plan.Skipped[0].Path)
	assert.Equal(t, map[string]string{"ok.txt": ""}, readTree(t, local))
}

// A scratch directory that overlaps the backup directory must not take the saved edits with it
// when the pull cleans up.
func TestDownloadBackupSurvivesScratchInsideTheBackupDirectory(t *testing.T) {
	t.Parallel()

	h, bob := bobEdits(t)

	older := filepath.Join(bob, scan.BackupDirName, "20200101T000000Z")
	require.NoError(t, os.MkdirAll(older, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(older, "old.txt"), []byte("from an earlier pull"), 0o600))

	result := h.download(t, bob, func(o *syncer.DownloadOptions) {
		o.Backup = true
		o.TempDir = filepath.Join(bob, scan.BackupDirName)
	})

	kept, err := os.ReadFile(filepath.Join(result.BackupDir, "a.txt"))
	require.NoError(t, err, "the backup of this pull must survive the cleanup")
	assert.Equal(t, "bob's local edit, longer than v1", string(kept))

	_, err = os.Stat(filepath.Join(older, "old.txt"))
	assert.NoError(t, err, "a backup from an earlier pull must survive the cleanup")
}

func TestDownloadLeavesUnrelatedFilesInACustomScratchDirectory(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.upload(t, writeTree(t, map[string]string{"a.txt": "remote"}))

	scratch := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(scratch, "mine.txt"), []byte("not ours"), 0o600))

	h.download(t, t.TempDir(), func(o *syncer.DownloadOptions) { o.TempDir = scratch })

	_, err := os.Stat(filepath.Join(scratch, "mine.txt"))
	assert.NoError(t, err, "cleanup must remove only what the pull created")
}
