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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mirrored returns a checkout holding one commit and the empty mirror a first pull leaves.
func mirrored(t *testing.T, git *Git) (dir, mirror string) {
	t.Helper()

	dir = checkout(t, git, SHA1)
	mirror = filepath.Join(t.TempDir(), "mirror.git")
	require.NoError(t, git.Create(context.Background(), mirror, "main", SHA1))

	return dir, mirror
}

// says returns what git printed, so a test can compare two repositories.
func says(t *testing.T, git *Git, args ...string) string {
	t.Helper()

	out, err := git.query(context.Background(), args...)
	require.NoError(t, err)

	return out
}

func TestPushSendsHEADAndPrintsWhatGitDid(t *testing.T) {
	git := newGit(t)
	dir, mirror := mirrored(t, git)

	stdout := &bytes.Buffer{}
	git.Stdout = stdout

	require.NoError(t, git.Push(context.Background(), dir, mirror, nil))

	assert.Equal(t,
		says(t, git, "-C", dir, "rev-parse", "HEAD"),
		says(t, git, "--git-dir", mirror, "rev-parse", "refs/heads/main"))
	assert.Contains(t, stdout.String(), "HEAD:refs/heads/main")
}

func TestPushSendsWhatTheCallerAskedForInsteadOfHEAD(t *testing.T) {
	git := newGit(t)
	dir, mirror := mirrored(t, git)
	run(t, git, "-C", dir, "branch", "side")

	require.NoError(t, git.Push(context.Background(), dir, mirror, []string{"--all"}))

	assert.Equal(t,
		says(t, git, "-C", dir, "rev-parse", "refs/heads/side"),
		says(t, git, "--git-dir", mirror, "rev-parse", "refs/heads/side"))
}

func TestPushIsRejectedWhenTheMirrorIsAhead(t *testing.T) {
	git := newGit(t)
	ctx := context.Background()
	dir, mirror := mirrored(t, git)

	run(t, git, "-C", dir, "commit", "--quiet", "--allow-empty", "--message=second")
	require.NoError(t, git.Push(ctx, dir, mirror, nil))

	// The checkout drops the commit the mirror now holds, so its HEAD is no longer ahead.
	run(t, git, "-C", dir, "reset", "--quiet", "--hard", "HEAD~1")

	require.ErrorIs(t, git.Push(ctx, dir, mirror, nil), ErrRejected)
}

func TestPushOfAShallowCheckoutSaysToUnshallowIt(t *testing.T) {
	git := newGit(t)
	ctx := context.Background()
	dir, mirror := mirrored(t, git)
	run(t, git, "-C", dir, "commit", "--quiet", "--allow-empty", "--message=second")

	// All that `git clone --depth` leaves behind: the listed commit's parents count as absent,
	// so the mirror would be left without the history the pushed commit needs.
	tip := says(t, git, "-C", dir, "rev-parse", "HEAD")
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "shallow"), []byte(tip+"\n"), 0o600))

	err := git.Push(ctx, dir, mirror, nil)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrRejected, "no pull from the container would fix it")
	assert.Contains(t, err.Error(), "--unshallow")
}

func TestPushOfAMissingRefIsNotAConflict(t *testing.T) {
	git := newGit(t)
	dir, mirror := mirrored(t, git)

	err := git.Push(context.Background(), dir, mirror, []string{"refs/heads/absent"})
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrRejected, "git weighed no ref, so nothing was refused")
}

func TestSetRemoteAddsTheRemoteAndThenMovesIt(t *testing.T) {
	git := newGit(t)
	ctx := context.Background()
	dir, mirror := mirrored(t, git)

	require.NoError(t, git.SetRemote(ctx, dir, mirror))
	assert.Equal(t, mirror, says(t, git, "-C", dir, "remote", "get-url", RemoteName))

	moved := filepath.Join(t.TempDir(), "moved.git")
	require.NoError(t, git.SetRemote(ctx, dir, moved))
	assert.Equal(t, moved, says(t, git, "-C", dir, "remote", "get-url", RemoteName))
}
