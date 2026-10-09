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
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// macOS hands back decomposed names from readdir while Linux returns whatever was written.
// Without normalizing, a Mac-to-Linux round trip renames every accented file and re-uploads
// the entire set on every run.
func TestNormalizeComposesUnicode(t *testing.T) {
	t.Parallel()

	decomposed := "café/résumé.txt" // e + combining acute
	composed := "café/résumé.txt"      // precomposed e-acute

	assert.NotEqual(t, decomposed, composed, "the fixture must actually differ byte-wise")
	assert.Equal(t, composed, Normalize(decomposed))
	assert.Equal(t, composed, Normalize(composed), "normalization is idempotent")
}

func TestLocalPathRefusesEscapes(t *testing.T) {
	t.Parallel()

	root := filepath.Join(string(filepath.Separator), "sync", "root")

	joined, err := LocalPath(root, "a/b.txt")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, "a", "b.txt"), joined)

	for _, bad := range []string{"../outside", "/etc/passwd", "a/../../b", ""} {
		_, err := LocalPath(root, bad)
		require.Error(t, err, "path %q must be refused", bad)
	}
}

func TestCaseCollisions(t *testing.T) {
	t.Parallel()

	collisions := CaseCollisions([]string{"README", "readme", "src/a.go", "SRC/a.go", "unique"})

	require.Len(t, collisions, 2)

	flattened := map[string]bool{}

	for _, group := range collisions {
		assert.Len(t, group, 2)

		for _, path := range group {
			flattened[path] = true
		}
	}

	assert.True(t, flattened["README"] && flattened["readme"])
	assert.False(t, flattened["unique"])

	assert.Empty(t, CaseCollisions([]string{"a", "b", "c"}))
}

func TestUnwritableOnWindows(t *testing.T) {
	t.Parallel()

	// Checked on every platform, so a Linux producer learns that a Windows consumer will
	// not be able to extract what it just uploaded.
	problems := map[string]string{
		"CON":              "reserved device name",
		"a/nul.txt":        "reserved device name",
		"a/COM1.log":       "reserved device name",
		"trailing.":        "dot or space",
		"trailing ":        "dot or space",
		"a/what?.txt":      "not allowed on Windows",
		`a/pipe|name`:      "not allowed on Windows",
		"a/star*.txt":      "not allowed on Windows",
		"a/colon:name.txt": "not allowed on Windows",
	}

	for path, want := range problems {
		assert.Contains(t, UnwritableOnWindows(path), want, "path %q", path)
	}

	for _, fine := range []string{"a/b.txt", "console.log", "nullable.go", "a-b_c.1.txt"} {
		assert.Empty(t, UnwritableOnWindows(fine), "path %q should be fine", fine)
	}
}

func TestCaseInsensitiveProbeRuns(t *testing.T) {
	t.Parallel()

	// The answer is platform-dependent; what matters is that probing is non-destructive and
	// does not error on a normal directory.
	_, err := CaseInsensitive(t.TempDir())
	require.NoError(t, err)

	_, err = CaseInsensitive(filepath.Join(t.TempDir(), "missing"))
	require.Error(t, err)
}

func TestDenormalizeRoundTrip(t *testing.T) {
	t.Parallel()

	assert.Equal(t, filepath.Join("a", "b.txt"), Denormalize("a/b.txt"))
	assert.Equal(t, "a/b.txt", Normalize(filepath.Join("a", "b.txt")))
}
