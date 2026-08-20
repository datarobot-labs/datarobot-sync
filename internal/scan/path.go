// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

package scan

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/text/unicode/norm"

	"github.com/datarobot/golang-datarobot-filesync/internal/manifest"
)

// Normalize converts a local relative path to the manifest's canonical form: slash-separated
// and NFC-normalized.
//
// The NFC step is not cosmetic. macOS returns decomposed (NFD) names from readdir while Linux
// returns whatever bytes were written, so without normalizing, a Mac-to-Linux round trip
// renames every file with an accent in it and re-uploads the whole set on every run.
func Normalize(relPath string) string {
	slashed := filepath.ToSlash(relPath)

	return norm.NFC.String(slashed)
}

// Denormalize converts a manifest path back to a local path for the current platform.
func Denormalize(manifestPath string) string {
	return filepath.FromSlash(manifestPath)
}

// LocalPath joins a manifest path onto the sync root, refusing anything that would escape.
//
// Validation happens before the join, not after, because a path that has already escaped is
// harder to reason about than one that never got the chance.
func LocalPath(root, manifestPath string) (string, error) {
	if err := manifest.ValidatePath(manifestPath); err != nil {
		return "", err
	}

	joined := filepath.Join(root, Denormalize(manifestPath))

	// Belt and braces: confirm the result is still inside the root even though validation
	// already rejected traversal.
	relative, err := filepath.Rel(root, joined)
	if err != nil {
		return "", fmt.Errorf("resolving %q under %s: %w", manifestPath, root, err)
	}

	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the sync root", manifestPath)
	}

	return joined, nil
}

// CaseInsensitive reports whether dir lives on a filesystem that ignores case.
//
// It matters because two entries differing only in case cannot both exist there: writing
// both would silently leave one of them holding the other's content.
func CaseInsensitive(dir string) (bool, error) {
	probe, err := os.CreateTemp(dir, ".dr-sync-case-probe-*")
	if err != nil {
		return false, fmt.Errorf("probing case sensitivity of %s: %w", dir, err)
	}

	name := probe.Name()

	if err := probe.Close(); err != nil {
		return false, fmt.Errorf("probing case sensitivity of %s: %w", dir, err)
	}

	defer os.Remove(name)

	// The probe name is lowercase by construction, so asking for the uppercase spelling
	// answers the question directly.
	_, err = os.Stat(strings.ToUpper(name))

	return err == nil, nil
}

// CaseCollisions returns groups of paths that differ only in case. Empty unless there is a
// problem, so a caller can report every conflict at once rather than one per run.
func CaseCollisions(paths []string) [][]string {
	byFolded := map[string][]string{}

	for _, p := range paths {
		folded := strings.ToLower(p)
		byFolded[folded] = append(byFolded[folded], p)
	}

	var collisions [][]string

	for _, group := range byFolded {
		if len(group) > 1 {
			collisions = append(collisions, group)
		}
	}

	return collisions
}

// reservedWindowsNames cannot be used as a path element on Windows, whatever the extension.
var reservedWindowsNames = map[string]struct{}{
	"CON": {}, "PRN": {}, "AUX": {}, "NUL": {},
	"COM1": {}, "COM2": {}, "COM3": {}, "COM4": {}, "COM5": {},
	"COM6": {}, "COM7": {}, "COM8": {}, "COM9": {},
	"LPT1": {}, "LPT2": {}, "LPT3": {}, "LPT4": {}, "LPT5": {},
	"LPT6": {}, "LPT7": {}, "LPT8": {}, "LPT9": {},
}

// UnwritableOnWindows reports why a path cannot be created on Windows, or "" if it can.
//
// Checked on every platform so a Linux producer learns that a Windows consumer will not be
// able to extract what it just uploaded.
func UnwritableOnWindows(manifestPath string) string {
	for _, element := range strings.Split(manifestPath, "/") {
		base, _, _ := strings.Cut(element, ".")

		if _, reserved := reservedWindowsNames[strings.ToUpper(base)]; reserved {
			return fmt.Sprintf("%q is a reserved device name on Windows", element)
		}

		if strings.HasSuffix(element, ".") || strings.HasSuffix(element, " ") {
			return fmt.Sprintf("%q ends with a dot or space, which Windows silently strips", element)
		}

		if idx := strings.IndexAny(element, `<>:"|?*`); idx >= 0 {
			return fmt.Sprintf("%q contains %q, which is not allowed on Windows", element, element[idx])
		}
	}

	return ""
}
