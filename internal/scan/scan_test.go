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
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/text/unicode/norm"

	"github.com/datarobot/golang-datarobot-filesync/internal/manifest"
)

// tree writes a fixture directory and returns its root.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()

	root := t.TempDir()

	for name, content := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
	}

	return root
}

func walk(t *testing.T, opts Options) *Result {
	t.Helper()

	if opts.Check == "" {
		opts.Check = CheckAuto
	}

	result, err := Walk(context.Background(), opts)
	require.NoError(t, err)

	return result
}

func TestWalkCollectsFilesDirsAndDigests(t *testing.T) {
	t.Parallel()

	root := tree(t, map[string]string{
		"a.txt":       "alpha",
		"src/main.go": "package main",
		"empty.txt":   "",
	})

	result := walk(t, Options{Root: root})

	paths := make([]string, 0, len(result.Entries))
	for _, entry := range result.Entries {
		paths = append(paths, entry.Path)
	}

	// Sorted byte-ascending, so a parent always precedes its children.
	assert.Equal(t, []string{"a.txt", "empty.txt", "src", "src/main.go"}, paths)

	main, ok := result.ByPath["src/main.go"]
	require.True(t, ok)
	assert.Equal(t, manifest.TypeFile, main.Type)
	assert.EqualValues(t, 12, main.Size)
	assert.NotEmpty(t, main.SHA256)

	empty, ok := result.ByPath["empty.txt"]
	require.True(t, ok)
	assert.Empty(t, empty.SHA256, "an empty file needs no digest and no pack member")

	dir, ok := result.ByPath["src"]
	require.True(t, ok)
	assert.Equal(t, manifest.TypeDir, dir.Type)
	assert.Empty(t, dir.SHA256)
}

// Identical content at two paths is one blob, which is what makes a duplicate free to store
// and a rename free to transfer.
func TestWalkIndexesDuplicateContentOnce(t *testing.T) {
	t.Parallel()

	root := tree(t, map[string]string{
		"one/a.txt": "same bytes",
		"two/b.txt": "same bytes",
		"other.txt": "different",
	})

	result := walk(t, Options{Root: root})

	digest, err := HashFile(filepath.Join(root, "one", "a.txt"))
	require.NoError(t, err)

	assert.Equal(t, []string{"one/a.txt", "two/b.txt"}, result.BySHA[digest])
	assert.NotEmpty(t, result.SourcePath[digest])
	assert.Len(t, result.BySHA, 2, "two distinct contents across three files")
}

// A file named with NFD-decomposed Unicode (the form macOS's readdir returns for an accented
// name) must still be readable through SourcePath. The manifest entry is NFC-normalized, but
// SourcePath has to be the path the walker actually found the file at, not a reconstruction
// through the normalized manifest path — that reconstruction only happens to work on a
// filesystem that is itself normalization-insensitive.
func TestWalkSourcePathSurvivesNFDFilename(t *testing.T) {
	t.Parallel()

	decomposed := norm.NFD.String("café.txt")
	require.NotEqual(t, "café.txt", decomposed, "fixture must actually be NFD-decomposed")

	root := t.TempDir()
	full := filepath.Join(root, decomposed)
	require.NoError(t, os.WriteFile(full, []byte("content"), 0o600))

	result := walk(t, Options{Root: root})

	entry, ok := result.ByPath["café.txt"]
	require.True(t, ok, "the manifest path is NFC-normalized regardless of the on-disk form")

	source, ok := result.SourcePath[entry.SHA256]
	require.True(t, ok)
	assert.Equal(t, full, source, "SourcePath must be the walked path, not one rebuilt from the normalized manifest path")

	content, err := os.ReadFile(source)
	require.NoError(t, err)
	assert.Equal(t, "content", string(content))
}

// dr-sync's own state must not be swept into the sync, but a user's identically named nested
// directory must be.
func TestWalkExcludesReservedDirectoriesAtRootOnly(t *testing.T) {
	t.Parallel()

	root := tree(t, map[string]string{
		".dr-sync/manifests/x.json.xz": "state",
		".dr-sync-tmp/pending.part":    "scratch",
		"vendor/lib/.dr-sync/keep.txt": "a user's own directory that happens to share the name",
		"real.txt":                     "content",
	})

	result := walk(t, Options{Root: root, MetaPrefix: manifest.DefaultMetaPrefix})

	assert.Contains(t, result.ByPath, "real.txt")
	assert.Contains(t, result.ByPath, "vendor/lib/.dr-sync/keep.txt")

	for path := range result.ByPath {
		assert.NotContains(t, path, ".dr-sync/manifests")
		assert.NotContains(t, path, ".dr-sync-tmp")
	}
}

func TestWalkAppliesIgnorePatterns(t *testing.T) {
	t.Parallel()

	root := tree(t, map[string]string{
		"keep.txt":        "keep",
		"skip.log":        "skip",
		"build/out.o":     "skip",
		"nested/keep.log": "re-included",
	})

	matcher, err := NewMatcher([]string{"*.log", "!nested/keep.log", "build/"})
	require.NoError(t, err)

	result := walk(t, Options{Root: root, Matcher: matcher})

	assert.Contains(t, result.ByPath, "keep.txt")
	assert.Contains(t, result.ByPath, "nested/keep.log")
	assert.NotContains(t, result.ByPath, "skip.log")
	assert.NotContains(t, result.ByPath, "build/out.o")
	assert.NotContains(t, result.ByPath, "build")
}

func TestWalkRecordsSymlinksWithoutFollowing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}

	t.Parallel()

	root := tree(t, map[string]string{"target.txt": "pointed at"})
	require.NoError(t, os.Symlink("target.txt", filepath.Join(root, "link.txt")))

	result := walk(t, Options{Root: root})

	link, ok := result.ByPath["link.txt"]
	require.True(t, ok)
	assert.Equal(t, manifest.TypeSymlink, link.Type)
	assert.Equal(t, "target.txt", link.Target)
	assert.Empty(t, link.SHA256, "a link's content is its target, which lives in the manifest")
}

func TestWalkFollowSymlinksStoresContent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}

	t.Parallel()

	root := tree(t, map[string]string{"target.txt": "pointed at"})
	require.NoError(t, os.Symlink("target.txt", filepath.Join(root, "link.txt")))

	result := walk(t, Options{Root: root, FollowSymlinks: true})

	link, ok := result.ByPath["link.txt"]
	require.True(t, ok)
	assert.Equal(t, manifest.TypeFile, link.Type)
	assert.NotEmpty(t, link.SHA256)
}

func TestWalkSkipsSymlinkedDirectories(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}

	t.Parallel()

	root := tree(t, map[string]string{"real/inside.txt": "content"})
	require.NoError(t, os.Symlink("real", filepath.Join(root, "loop")))

	var warnings []string

	result := walk(t, Options{
		Root:           root,
		FollowSymlinks: true,
		Warn:           func(message string) { warnings = append(warnings, message) },
	})

	assert.NotContains(t, result.ByPath, "loop")
	assert.NotEmpty(t, result.Skipped)
	assert.Contains(t, warnings[0], "symlink to a directory")
}

// The whole point of the metadata fast path: a no-op run must not re-read the tree.
func TestCheckAutoAdoptsBaselineDigests(t *testing.T) {
	t.Parallel()

	root := tree(t, map[string]string{"a.txt": "alpha", "b.txt": "beta"})

	first := walk(t, Options{Root: root, Check: CheckFull})
	assert.Equal(t, 2, first.Hashed)
	assert.Zero(t, first.Reused)

	baseline := &manifest.Manifest{FormatVersion: 1, Entries: first.Entries}
	index := manifest.NewIndex(baseline)

	second := walk(t, Options{Root: root, Baseline: index, Check: CheckAuto})
	assert.Zero(t, second.Hashed, "unchanged files must not be re-read")
	assert.Equal(t, 2, second.Reused)
	assert.Equal(t, first.ByPath["a.txt"].SHA256, second.ByPath["a.txt"].SHA256)

	// CheckFull ignores the baseline entirely.
	third := walk(t, Options{Root: root, Baseline: index, Check: CheckFull})
	assert.Equal(t, 2, third.Hashed)
}

func TestCheckAutoRehashesWhenMetadataMoves(t *testing.T) {
	t.Parallel()

	root := tree(t, map[string]string{"a.txt": "alpha"})

	first := walk(t, Options{Root: root, Check: CheckFull})
	index := manifest.NewIndex(&manifest.Manifest{FormatVersion: 1, Entries: first.Entries})

	// Same length, different content, and the mtime moves: exactly the case size alone
	// would miss.
	target := filepath.Join(root, "a.txt")
	require.NoError(t, os.WriteFile(target, []byte("ALPHA"), 0o600))
	require.NoError(t, os.Chtimes(target, time.Now().Add(time.Hour), time.Now().Add(time.Hour)))

	second := walk(t, Options{Root: root, Baseline: index, Check: CheckAuto})
	assert.Equal(t, 1, second.Hashed)
	assert.NotEqual(t, first.ByPath["a.txt"].SHA256, second.ByPath["a.txt"].SHA256)
}

func TestCheckSizeIgnoresMTime(t *testing.T) {
	t.Parallel()

	root := tree(t, map[string]string{"a.txt": "alpha"})

	first := walk(t, Options{Root: root, Check: CheckFull})
	index := manifest.NewIndex(&manifest.Manifest{FormatVersion: 1, Entries: first.Entries})

	target := filepath.Join(root, "a.txt")
	require.NoError(t, os.WriteFile(target, []byte("ALPHA"), 0o600))
	require.NoError(t, os.Chtimes(target, time.Now().Add(time.Hour), time.Now().Add(time.Hour)))

	// Documented, deliberate blind spot: --check=size trusts length alone.
	second := walk(t, Options{Root: root, Baseline: index, Check: CheckSize})
	assert.Equal(t, 1, second.Reused)
	assert.Equal(t, first.ByPath["a.txt"].SHA256, second.ByPath["a.txt"].SHA256)
}

func TestWalkRejectsCaseCollisions(t *testing.T) {
	t.Parallel()

	insensitive, err := CaseInsensitive(t.TempDir())
	require.NoError(t, err)

	if insensitive {
		t.Skip("this filesystem cannot hold two paths differing only in case")
	}

	root := tree(t, map[string]string{"README": "one", "readme": "two"})

	_, err = Walk(context.Background(), Options{Root: root, Check: CheckAuto})
	require.ErrorContains(t, err, "differ only in case")
	assert.ErrorContains(t, err, "README")
}

func TestWalkRejectsNonDirectoryRoot(t *testing.T) {
	t.Parallel()

	root := tree(t, map[string]string{"a.txt": "alpha"})

	_, err := Walk(context.Background(), Options{Root: filepath.Join(root, "a.txt")})
	require.ErrorContains(t, err, "not a directory")

	_, err = Walk(context.Background(), Options{Root: filepath.Join(root, "missing")})
	require.Error(t, err)
}

func TestWalkHonoursContextCancellation(t *testing.T) {
	t.Parallel()

	root := tree(t, map[string]string{"a.txt": "alpha", "b.txt": "beta", "c.txt": "gamma"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Walk(ctx, Options{Root: root, Check: CheckFull})
	require.ErrorIs(t, err, context.Canceled)
}

func TestParseCheckMode(t *testing.T) {
	t.Parallel()

	for _, valid := range []string{"auto", "full", "size", "mtime"} {
		mode, err := ParseCheckMode(valid)
		require.NoError(t, err)
		assert.EqualValues(t, valid, mode)
	}

	_, err := ParseCheckMode("vibes")
	require.ErrorContains(t, err, "unknown check mode")
}

func TestHashHelpers(t *testing.T) {
	t.Parallel()

	root := tree(t, map[string]string{"empty": ""})

	digest, err := HashFile(filepath.Join(root, "empty"))
	require.NoError(t, err)
	assert.Equal(t, EmptyDigest, digest)

	_, err = HashFile(filepath.Join(root, "missing"))
	require.Error(t, err)

	hasher := NewHasher()
	assert.Equal(t, EmptyDigest, Digest(hasher))
}
