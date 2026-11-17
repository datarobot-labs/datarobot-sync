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

package cli

import (
	"errors"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/datarobot-labs/datarobot-sync/internal/config"
	"github.com/datarobot-labs/datarobot-sync/internal/gitmirror"
)

// container gives the command tree everything it needs to reach the point under test. Nothing
// here makes a request: every case ends before the first one.
func container(t *testing.T) {
	t.Helper()
	isolate(t)
	t.Setenv(config.EnvEndpoint, "https://app.datarobot.com/api/v2")
	t.Setenv(config.EnvToken, "token")
	t.Setenv(config.EnvCatalogID, "container-1")
}

// gitCheckout makes a working tree with one commit, ignoring the machine's own Git identity.
func gitCheckout(t *testing.T) string {
	t.Helper()

	for name, value := range map[string]string{
		"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME": "dr-sync test", "GIT_AUTHOR_EMAIL": "test@example.com",
		"GIT_COMMITTER_NAME": "dr-sync test", "GIT_COMMITTER_EMAIL": "test@example.com",
	} {
		t.Setenv(name, value)
	}

	dir := t.TempDir()
	runGit(t, "init", "--quiet", "--initial-branch=main", dir)
	runGit(t, "-C", dir, "commit", "--quiet", "--allow-empty", "--message=first")

	return dir
}

func runGit(t *testing.T, args ...string) {
	t.Helper()

	out, err := exec.Command("git", args...).CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}

// A mirror missing part of itself is a broken repository, so each of these is refused rather
// than honoured, and before anything reaches the network.
func TestRepoPushRefusesWhatWouldLeaveAPartialMirror(t *testing.T) {
	for want, args := range map[string][]string{
		"--dry-run has nothing to preview": {"repo", "push", "--dry-run"},
		"mix events into git's own output": {"repo", "push", "--output", "json"},
		"which is a broken repository":     {"repo", "push", "--delete=false"},
		"a mirror is sent whole":           {"repo", "push", "--exclude", "*.o"},
	} {
		t.Run(want, func(t *testing.T) {
			container(t)

			code, _, errOut := run(t, args...)

			assert.Equal(t, CodeUsage, code)
			assert.Contains(t, errOut, want)
		})
	}
}

func TestRepoPushTakesOnePathAndThenGitsOwnArguments(t *testing.T) {
	container(t)
	plain := t.TempDir()

	code, _, errOut := run(t, "repo", "push", plain, plain)
	assert.Equal(t, CodeUsage, code)
	assert.Contains(t, errOut, "one path at most")

	// Past the --, the arguments are git's, so this one gets as far as reading the checkout.
	code, _, errOut = run(t, "repo", "push", plain, "--", "--all")
	assert.Equal(t, CodeUsage, code)
	assert.Contains(t, errOut, "not inside a Git checkout")
}

func TestRepoPushOnADetachedHEADNeedsToBeToldWhatToSend(t *testing.T) {
	container(t)
	dir := gitCheckout(t)
	runGit(t, "-C", dir, "checkout", "--quiet", "--detach")

	code, _, errOut := run(t, "repo", "push", dir)

	assert.Equal(t, CodeUsage, code)
	assert.Contains(t, errOut, "HEAD is detached")
}

func TestRepoWithoutASubcommandIsAUsageError(t *testing.T) {
	isolate(t)

	code, out, _ := run(t, "repo")

	assert.Equal(t, CodeUsage, code)
	assert.Contains(t, out, "push")
}

func TestClassifyGitGivesEachFailureItsOwnCode(t *testing.T) {
	for name, probe := range map[string]struct {
		err  error
		code int
	}{
		"a rejected push is the conflict one step earlier": {gitmirror.ErrRejected, CodeConflict},
		"a mirror holding what git did not write":          {gitmirror.ErrUntrusted, CodeIntegrity},
		"any other git failure":                            {errors.New("exit status 1"), CodeCommand},
		"one that already carries a code keeps it":         {Failf(CodeUsage, "no branch"), CodeUsage},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, probe.code, CodeFor(classifyGit(probe.err)))
		})
	}
}

func TestClassifyGitSendsAConflictToRepoPull(t *testing.T) {
	assert.Contains(t, classifyGit(gitmirror.ErrRejected).Error(), "repo pull")
}
