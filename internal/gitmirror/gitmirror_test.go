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

package gitmirror

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMain lets the test binary stand in for git, so a test can choose the status that
// symbolic-ref exits with. No real repository gives a status other than 0 or 1 on demand.
func TestMain(m *testing.M) {
	status, err := strconv.Atoi(os.Getenv("FAKE_SYMBOLIC_REF_STATUS"))
	if err != nil {
		os.Exit(m.Run())
	}

	if slices.Contains(os.Args, "rev-parse") {
		fmt.Print("true\n/repo/.git\nsha1\n")
		os.Exit(0)
	}

	fmt.Fprintln(os.Stderr, "fatal: unable to read HEAD")
	os.Exit(status)
}

// newGit returns a Git that ignores the machine's own configuration and identity.
func newGit(t *testing.T) *Git {
	t.Helper()

	for name, value := range map[string]string{
		"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME": "dr-sync test", "GIT_AUTHOR_EMAIL": "test@example.com",
		"GIT_COMMITTER_NAME": "dr-sync test", "GIT_COMMITTER_EMAIL": "test@example.com",
	} {
		t.Setenv(name, value)
	}

	git, err := Find()
	require.NoError(t, err)

	return git
}

// checkout makes a working tree on branch main with one commit and a subdirectory.
func checkout(t *testing.T, git *Git, objectFormat string) string {
	t.Helper()

	dir := t.TempDir()
	run(t, git, "init", "--quiet", "--initial-branch=main", "--object-format="+objectFormat, dir)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub", "deep"), 0o755))
	run(t, git, "-C", dir, "commit", "--quiet", "--allow-empty", "--message=first")

	return dir
}

func run(t *testing.T, git *Git, args ...string) {
	t.Helper()
	require.NoError(t, git.Run(context.Background(), args...))
}

func TestFindWithoutGitIsAUsageError(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	_, err := Find()
	require.ErrorIs(t, err, ErrNoGit)
}

func TestRunUsesTheCallersStreams(t *testing.T) {
	git := newGit(t)

	stdout := &bytes.Buffer{}
	git.Stdout = stdout

	run(t, git, "version")
	assert.Contains(t, stdout.String(), "git version")
}

func TestCheckoutIsSharedBySubdirectoriesAndLinkedWorktrees(t *testing.T) {
	git := newGit(t)
	dir := checkout(t, git, SHA1)
	linked := filepath.Join(t.TempDir(), "linked")
	run(t, git, "-C", dir, "worktree", "add", "--quiet", "--detach", linked)

	fromSubdir, err := git.Checkout(context.Background(), filepath.Join(dir, "sub", "deep"))
	require.NoError(t, err)

	fromLinked, err := git.Checkout(context.Background(), linked)
	require.NoError(t, err)

	assert.True(t, filepath.IsAbs(fromSubdir.CommonDir))
	assert.FileExists(t, filepath.Join(fromSubdir.CommonDir, "HEAD"))
	assert.Equal(t, "main", fromSubdir.Branch)
	assert.Equal(t, SHA1, fromSubdir.ObjectFormat)

	assert.Equal(t, fromSubdir.CommonDir, fromLinked.CommonDir)
	assert.Empty(t, fromLinked.Branch, "a detached HEAD has no branch")
	assert.Equal(t,
		filepath.Join(fromSubdir.CommonDir, "dr-sync", "abc123.git"), fromLinked.MirrorPath("abc123"))
}

func TestCheckoutIgnoresRepositoryEnvironment(t *testing.T) {
	git := newGit(t)
	dir := checkout(t, git, SHA1)
	other := checkout(t, git, SHA1)

	before, err := git.Checkout(context.Background(), dir)
	require.NoError(t, err)

	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_COMMON_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_OBJECT_DIRECTORY", t.TempDir())
	t.Setenv("GIT_REFERENCE_BACKEND", "reftable")

	after, err := git.Checkout(context.Background(), dir)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

func TestCheckoutTakesOnlyStatusOneAsDetached(t *testing.T) {
	git := &Git{bin: os.Args[0]}

	t.Setenv("FAKE_SYMBOLIC_REF_STATUS", "1")

	detached, err := git.Checkout(context.Background(), t.TempDir())
	require.NoError(t, err)
	assert.Empty(t, detached.Branch)

	t.Setenv("FAKE_SYMBOLIC_REF_STATUS", "128")

	_, err = git.Checkout(context.Background(), t.TempDir())
	require.ErrorContains(t, err, "unable to read HEAD")
	require.NotErrorIs(t, err, ErrNotCheckout)
}

func TestCheckoutOutsideAWorkingTreeIsAUsageError(t *testing.T) {
	git := newGit(t)
	dir := checkout(t, git, SHA1)

	for name, path := range map[string]string{
		"plain directory":     t.TempDir(),
		"missing path":        filepath.Join(t.TempDir(), "missing"),
		"inside the Git data": filepath.Join(dir, ".git"),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := git.Checkout(context.Background(), path)
			require.ErrorIs(t, err, ErrNotCheckout)
		})
	}
}

func TestCreateMakesAMirrorThatTakesAPush(t *testing.T) {
	for _, objectFormat := range []string{SHA1, SHA256} {
		t.Run(objectFormat, func(t *testing.T) {
			git := newGit(t)
			// A reftable default from any source must still give a files mirror, and the
			// objects must not follow GIT_OBJECT_DIRECTORY out of it.
			outside := t.TempDir()
			t.Setenv("GIT_CONFIG_COUNT", "1")
			t.Setenv("GIT_CONFIG_KEY_0", "init.defaultRefFormat")
			t.Setenv("GIT_CONFIG_VALUE_0", "reftable")
			t.Setenv("GIT_DEFAULT_REF_FORMAT", "reftable")
			t.Setenv("GIT_REFERENCE_BACKEND", "reftable")
			t.Setenv("GIT_OBJECT_DIRECTORY", outside)

			dir := checkout(t, git, objectFormat)
			mirror := filepath.Join(t.TempDir(), "mirror.git")
			require.NoError(t, git.Create(context.Background(), mirror, "main", objectFormat))

			entries, err := filepath.Glob(filepath.Join(mirror, "*"))
			require.NoError(t, err)
			assert.Len(t, entries, 4, "only HEAD, config, objects and refs: %v", entries)
			assert.NoDirExists(t, filepath.Join(outside, "pack"))

			config, err := os.ReadFile(filepath.Join(mirror, "config"))
			require.NoError(t, err)
			assert.Equal(t, Config(objectFormat), string(config))

			run(t, git, "-C", dir, "push", "--quiet", mirror, "HEAD")
			run(t, git, "--git-dir", mirror, "rev-parse", "--quiet", "--verify", "refs/heads/main")
		})
	}
}
