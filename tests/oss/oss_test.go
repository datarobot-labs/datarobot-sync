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

// Package oss checks the repository files DataRobot's open source policy requires.
package oss

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const repoRoot = "../.."

func read(t *testing.T, name string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(repoRoot, name))
	require.NoError(t, err, name)

	return string(data)
}

func TestRequiredFilesAreFilledIn(t *testing.T) {
	for _, name := range []string{
		"LICENSE.txt", "AUTHORS", "CONTRIBUTING.md", "CODE_OF_CONDUCT.md", "SECURITY.md",
		".github/PULL_REQUEST_TEMPLATE.md", ".github/dependabot.yml", ".pre-commit-config.yaml",
	} {
		info, err := os.Stat(filepath.Join(repoRoot, name))
		require.NoError(t, err, name)
		require.NotZero(t, info.Size(), name)
	}
}

func TestLicenseIsApache(t *testing.T) {
	license := read(t, "LICENSE.txt")
	require.Contains(t, license, "Apache License")
	require.Regexp(t, `Copyright 20\d\d DataRobot`, license)
	require.NotContains(t, license, "[yyyy]")
}

func TestNoProprietaryLicenseText(t *testing.T) {
	for _, name := range []string{"LICENSE.txt", "README.md"} {
		require.NotContains(t, read(t, name), "Tool and Utility Agreement", name)
	}
}

func TestWorkflowActionsArePinnedToACommit(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(repoRoot, ".github/workflows/*.y*ml"))
	require.NoError(t, err)
	require.NotEmpty(t, files)

	pinned := regexp.MustCompile(`@[0-9a-f]{40}( |$)`)

	for _, file := range files {
		data, err := os.ReadFile(file)
		require.NoError(t, err)

		for _, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "- "))
			if !strings.HasPrefix(trimmed, "uses: ") {
				continue
			}

			require.Regexp(t, pinned, trimmed, "%s: %s", file, trimmed)
		}
	}
}

func TestReleaseInstallsAPinnedDRCLI(t *testing.T) {
	release := read(t, ".github/workflows/release.yaml")
	require.NotContains(t, release, "cli.datarobot.com/install")
	require.Regexp(t, `datarobot-oss/cli/[0-9a-f]{40}/install\.sh`, release)
}

func TestDocsHoldNoRealContainerIDs(t *testing.T) {
	// Documentation examples use placeholder IDs; a real one would name a container in a live org.
	known := "6a88a0e0de792a9d144a69c9"
	for _, name := range []string{"README.md", "docs/quickstart.md", "docs/content/docs/quick-start.md"} {
		require.NotContains(t, read(t, name), known, name)
	}
}

func TestDockerBaseImageIsPinnedByDigest(t *testing.T) {
	require.Regexp(t, `(?m)^FROM \S+@sha256:[0-9a-f]{64}$`, read(t, "Dockerfile.goreleaser"))
}

func TestNoSourceFileCarriesTheProprietaryHeader(t *testing.T) {
	err := filepath.WalkDir(repoRoot, func(path string, entry fs.DirEntry, err error) error {
		require.NoError(t, err)

		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}

			return nil
		}

		if filepath.Ext(path) != ".go" || entry.Name() == "oss_test.go" {
			return nil
		}

		data, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Contains(t, string(data), "Licensed under the Apache License", path)
		require.NotContains(t, string(data), "proprietary source code", path)

		return nil
	})
	require.NoError(t, err)
}
