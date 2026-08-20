// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

package scan

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMatcherGitignoreSemantics(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		patterns []string
		path     string
		isDir    bool
		want     bool
	}{
		{name: "no patterns", path: "a/b.txt"},
		{name: "exact name at any depth", patterns: []string{"node_modules"}, path: "web/node_modules", isDir: true, want: true},
		{name: "name matches nested file", patterns: []string{"*.log"}, path: "var/logs/app.log", want: true},
		// git excludes the contents of any directory whose name matches, so a component
		// named "app.log" takes "keep.txt" with it.
		{name: "matched dir takes its contents", patterns: []string{"*.log"}, path: "var/app.log/keep.txt", want: true},
		{name: "star does not cross a separator", patterns: []string{"a*c"}, path: "a/b/c"},
		{name: "doublestar crosses separators", patterns: []string{"**/*.log"}, path: "a/b/c.log", want: true},
		{name: "doublestar matches at depth zero", patterns: []string{"**/x"}, path: "x", want: true},
		{name: "anchored by leading slash", patterns: []string{"/build"}, path: "build", isDir: true, want: true},
		{name: "anchored pattern misses nested", patterns: []string{"/build"}, path: "web/build", isDir: true},
		{name: "slash anywhere anchors", patterns: []string{"web/build"}, path: "web/build", isDir: true, want: true},
		{name: "slash anywhere is not floating", patterns: []string{"web/build"}, path: "app/web/build", isDir: true},
		{name: "dir only ignores files", patterns: []string{"dist/"}, path: "dist", want: false},
		{name: "dir only matches dirs", patterns: []string{"dist/"}, path: "dist", isDir: true, want: true},
		{name: "negation re-includes", patterns: []string{"*.log", "!keep.log"}, path: "keep.log"},
		{name: "negation order matters", patterns: []string{"!keep.log", "*.log"}, path: "keep.log", want: true},
		{name: "character class", patterns: []string{"file[0-9].txt"}, path: "file7.txt", want: true},
		{name: "question mark", patterns: []string{"a?.txt"}, path: "ab.txt", want: true},
		{name: "prefix directory excludes contents", patterns: []string{"build"}, path: "build/out/x.o", want: true},
		{name: "malformed pattern matches nothing", patterns: []string{"a[b"}, path: "a[b"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			matcher, err := NewMatcher(tc.patterns)
			require.NoError(t, err)

			assert.Equal(t, tc.want, matcher.Match(tc.path, tc.isDir))
		})
	}
}

func TestMatcherIgnoresCommentsAndBlanks(t *testing.T) {
	t.Parallel()

	matcher, err := NewMatcher([]string{"", "   ", "# a comment", "*.tmp"})
	require.NoError(t, err)

	assert.True(t, matcher.Match("x.tmp", false))
	assert.False(t, matcher.Match("a comment", false))
}

func TestMatcherRejectsEmptySelector(t *testing.T) {
	t.Parallel()

	_, err := NewMatcher([]string{"/"})
	require.ErrorContains(t, err, "selects nothing")
}

func TestMatchesAncestor(t *testing.T) {
	t.Parallel()

	matcher, err := NewMatcher([]string{"build/"})
	require.NoError(t, err)

	// The file itself does not match the dir-only rule; its parent does.
	assert.False(t, matcher.Match("build/out/x.o", false))
	assert.True(t, matcher.MatchesAncestor("build/out/x.o"))
	assert.False(t, matcher.MatchesAncestor("src/x.go"))
}

func TestLoadIgnoreFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, ".drsyncignore")

	require.NoError(t, os.WriteFile(path, []byte("# comment\n\n*.log\n!keep.log\n"), 0o600))

	patterns, err := LoadIgnoreFile(path)
	require.NoError(t, err)
	assert.Equal(t, []string{"*.log", "!keep.log"}, patterns)

	// A missing ignore file is a convenience that was not used, not an error.
	missing, err := LoadIgnoreFile(filepath.Join(dir, "nope"))
	require.NoError(t, err)
	assert.Empty(t, missing)
}

func TestNilMatcherMatchesNothing(t *testing.T) {
	t.Parallel()

	var matcher *Matcher

	assert.False(t, matcher.Match("anything", false))
	assert.False(t, matcher.MatchesAncestor("a/b"))
}
