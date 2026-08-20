// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

package scan

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

// Matcher decides which paths a sync includes, using gitignore-style patterns.
//
// The semantics people already know from .gitignore are the ones implemented: later patterns
// win, "!" re-includes, a leading "/" anchors to the sync root, a trailing "/" matches only
// directories, "*" stops at a separator and "**" crosses them.
type Matcher struct {
	rules []rule
}

type rule struct {
	pattern string
	// negate re-includes a path an earlier rule excluded.
	negate bool
	// dirOnly matches only directories, from a trailing slash.
	dirOnly bool
	// anchored matches from the sync root rather than at any depth.
	anchored bool
	// segments is the pattern split on "/", pre-split so matching does no allocation.
	segments []string
}

// NewMatcher compiles patterns in order. An empty pattern list matches nothing.
func NewMatcher(patterns []string) (*Matcher, error) {
	matcher := &Matcher{}

	for _, raw := range patterns {
		compiled, ok, err := compileRule(raw)
		if err != nil {
			return nil, err
		}

		if ok {
			matcher.rules = append(matcher.rules, compiled)
		}
	}

	return matcher, nil
}

// LoadIgnoreFile reads patterns from a gitignore-style file. A missing file is not an error:
// the default ignore file is a convenience, not a requirement.
func LoadIgnoreFile(filename string) ([]string, error) {
	handle, err := os.Open(filename)
	if os.IsNotExist(err) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("reading ignore file %s: %w", filename, err)
	}

	defer handle.Close()

	return readPatterns(handle)
}

func readPatterns(r io.Reader) ([]string, error) {
	var patterns []string

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		patterns = append(patterns, line)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading ignore patterns: %w", err)
	}

	return patterns, nil
}

func compileRule(raw string) (rule, bool, error) {
	pattern := strings.TrimSpace(raw)
	if pattern == "" || strings.HasPrefix(pattern, "#") {
		return rule{}, false, nil
	}

	compiled := rule{pattern: raw}

	if strings.HasPrefix(pattern, "!") {
		compiled.negate = true
		pattern = pattern[1:]
	}

	if strings.HasSuffix(pattern, "/") {
		compiled.dirOnly = true
		pattern = strings.TrimSuffix(pattern, "/")
	}

	if strings.HasPrefix(pattern, "/") {
		compiled.anchored = true
		pattern = strings.TrimPrefix(pattern, "/")
	} else if strings.Contains(pattern, "/") {
		// git treats a pattern containing a slash anywhere as anchored to the root.
		compiled.anchored = true
	}

	if pattern == "" {
		return rule{}, false, fmt.Errorf("ignore pattern %q selects nothing", raw)
	}

	compiled.segments = strings.Split(pattern, "/")

	return compiled, true, nil
}

// Match reports whether relPath is excluded. relPath is slash-separated and relative to the
// sync root.
func (m *Matcher) Match(relPath string, isDir bool) bool {
	if m == nil || len(m.rules) == 0 {
		return false
	}

	excluded := false

	// Last match wins, so a "!" rule can re-include something an earlier rule excluded.
	for _, r := range m.rules {
		if r.dirOnly && !isDir {
			continue
		}

		if r.matches(relPath) {
			excluded = !r.negate
		}
	}

	return excluded
}

// MatchesAncestor reports whether any ancestor directory of relPath is excluded, which is
// how an exclusion on a directory reaches the files inside it.
func (m *Matcher) MatchesAncestor(relPath string) bool {
	if m == nil || len(m.rules) == 0 {
		return false
	}

	for dir := path.Dir(relPath); dir != "." && dir != "/"; dir = path.Dir(dir) {
		if m.Match(dir, true) {
			return true
		}
	}

	return false
}

func (r rule) matches(relPath string) bool {
	segments := strings.Split(relPath, "/")

	if r.anchored {
		return matchSegments(r.segments, segments)
	}

	// An unanchored pattern applies at any depth, so try every suffix of the path.
	for start := range segments {
		if matchSegments(r.segments, segments[start:]) {
			return true
		}
	}

	return false
}

// matchSegments matches a pattern's segments against a path's segments, where "**" spans any
// number of segments.
func matchSegments(pattern, segments []string) bool {
	if len(pattern) == 0 {
		return true
	}

	if pattern[0] == "**" {
		// "**" may consume nothing at all, so every split point has to be tried.
		for skip := 0; skip <= len(segments); skip++ {
			if matchSegments(pattern[1:], segments[skip:]) {
				return true
			}
		}

		return false
	}

	if len(segments) == 0 {
		return false
	}

	if !matchSegment(pattern[0], segments[0]) {
		return false
	}

	// A pattern that runs out mid-path still matches: excluding "build" excludes everything
	// beneath it.
	if len(pattern) == 1 {
		return true
	}

	return matchSegments(pattern[1:], segments[1:])
}

// matchSegment matches one path element against one pattern element, where "*" and "?" do
// not cross separators.
func matchSegment(pattern, name string) bool {
	matched, err := path.Match(pattern, name)
	if err != nil {
		// A malformed pattern matches nothing rather than everything: over-excluding is the
		// less recoverable mistake.
		return false
	}

	return matched
}
