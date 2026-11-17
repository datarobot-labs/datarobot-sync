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
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func TestPushOfAShallowCheckoutIsRefusedWhereTheMachineWouldAllowIt(t *testing.T) {
	git := newGit(t)
	ctx := context.Background()

	// A ~/.gitconfig that lets shallow pushes through, which the mirror's own config has to
	// outrank: otherwise the push lands and leaves a shallow file the trust check then refuses,
	// making the mirror unusable on every machine after this one.
	global := filepath.Join(t.TempDir(), "gitconfig")
	require.NoError(t, os.WriteFile(global, []byte("[receive]\n\tshallowUpdate = true\n"), 0o600))
	t.Setenv("GIT_CONFIG_GLOBAL", global)

	dir, mirror := mirrored(t, git)
	run(t, git, "-C", dir, "commit", "--quiet", "--allow-empty", "--message=second")

	tip := says(t, git, "-C", dir, "rev-parse", "HEAD")
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "shallow"), []byte(tip+"\n"), 0o600))

	require.Error(t, git.Push(ctx, dir, mirror, nil))
	assert.NoFileExists(t, filepath.Join(mirror, "shallow"))
}

func TestCopyStatusKeepsTheRefusalThatSaysMost(t *testing.T) {
	// Git's own wording, taken from a push it refused for each of these reasons.
	hook := "!\trefs/heads/a:refs/heads/a\t[remote rejected] (pre-receive hook declined)\n"
	conflict := "!\trefs/heads/b:refs/heads/b\t[rejected] (fetch first)\n"
	shallow := "!\trefs/heads/c:refs/heads/c\t[remote rejected] (shallow update not allowed)\n"

	for name, probe := range map[string]struct{ refusals, saysInTheError string }{
		"a conflict after another refusal": {hook + conflict, "the container holds commits"},
		"a shallow push after a conflict":  {conflict + shallow, "--unshallow"},
		"neither":                          {hook, "pre-receive hook declined"},
	} {
		t.Run(name, func(t *testing.T) {
			status := "To /mirror\n" + probe.refusals + "Done\n"

			out := &bytes.Buffer{}
			worst := copyStatus(strings.NewReader(status), out)

			assert.Equal(t, status, out.String(), "every line still reaches the caller")
			assert.ErrorContains(t, pushFailure(worst, errors.New("exit status 1")), probe.saysInTheError)
		})
	}
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
