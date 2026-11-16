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
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/datarobot-labs/datarobot-sync/internal/scan"
)

// pushedMirror makes a mirror that holds one pushed commit.
func pushedMirror(t *testing.T, git *Git, objectFormat string) string {
	t.Helper()

	dir := checkout(t, git, objectFormat)
	mirror := filepath.Join(t.TempDir(), "mirror.git")
	require.NoError(t, git.Create(context.Background(), mirror, "main", objectFormat))
	run(t, git, "-C", dir, "push", "--quiet", mirror, "HEAD")

	return mirror
}

// plant returns a change that adds a file to a mirror.
func plant(name string) func(*testing.T, string) {
	return func(t *testing.T, mirror string) {
		t.Helper()

		path := filepath.Join(mirror, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))
	}
}

func TestTrustTakesWhatGitWrites(t *testing.T) {
	for _, objectFormat := range []string{SHA1, SHA256} {
		t.Run(objectFormat, func(t *testing.T) {
			git := newGit(t)
			mirror := pushedMirror(t, git, objectFormat)

			empty, err := Trust(mirror)
			require.NoError(t, err)
			assert.False(t, empty)

			run(t, git, "--git-dir", mirror, "gc", "--quiet")
			run(t, git, "--git-dir", mirror, "maintenance", "run", "--quiet",
				"--task=commit-graph", "--task=incremental-repack")
			require.FileExists(t, filepath.Join(mirror, "packed-refs"))

			_, err = Trust(mirror)
			require.NoError(t, err)
		})
	}
}

func TestTrustReportsAMirrorWithOnlyScratchAsEmpty(t *testing.T) {
	mirror := t.TempDir()
	plant(scan.TempDirName+"/hooks/pre-receive")(t, mirror)
	plant(scan.BackupDirName+"/config")(t, mirror)

	empty, err := Trust(mirror)
	require.NoError(t, err)
	assert.True(t, empty)
}

func TestTrustRefusesWhatGitWouldObey(t *testing.T) {
	git := newGit(t)

	for name, tamper := range map[string]func(*testing.T, string){
		"a hook":               plant("hooks/pre-receive"),
		"a commondir file":     plant("commondir"),
		"a nested .git":        plant(".git/config"),
		"a shallow file":       plant("shallow"),
		"an alternates file":   plant("objects/info/alternates"),
		"http alternates":      plant("objects/info/http-alternates"),
		"info with attributes": plant("info/attributes"),
		"an edited config": func(t *testing.T, mirror string) {
			t.Helper()
			require.NoError(t, os.WriteFile(filepath.Join(mirror, "config"), []byte(Config(SHA1)+"\n"), 0o600))
		},
		"no HEAD": func(t *testing.T, mirror string) {
			t.Helper()
			require.NoError(t, os.Remove(filepath.Join(mirror, "HEAD")))
		},
		"a symlink below refs": func(t *testing.T, mirror string) {
			t.Helper()

			if runtime.GOOS == "windows" {
				t.Skip("making a symlink on Windows needs a privilege")
			}

			require.NoError(t, os.Symlink(mirror, filepath.Join(mirror, "refs", "heads", "link")))
		},
	} {
		t.Run(name, func(t *testing.T) {
			mirror := pushedMirror(t, git, SHA1)
			tamper(t, mirror)

			_, err := Trust(mirror)
			require.ErrorIs(t, err, ErrUntrusted)
		})
	}
}
