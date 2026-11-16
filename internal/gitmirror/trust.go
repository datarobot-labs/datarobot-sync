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
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"

	"github.com/datarobot-labs/datarobot-sync/internal/scan"
)

// ErrUntrusted is a mirror that holds what dr-sync and Git did not write there. A command
// reports it with exit 5.
var ErrUntrusted = errors.New("refusing the mirror")

// allowed is everything a mirror may hold outside objects/ and refs/, where Git picks the
// names. `git gc` writes packed-refs and info/refs.
var allowed = []string{"HEAD", "config", "packed-refs", "objects", "refs", "info", "info/refs"}

// alternates are the two files that make Git read objects from outside the mirror.
var alternates = []string{"objects/info/alternates", "objects/info/http-alternates"}

// Trust checks the mirror a pull left on disk, and runs before any Git command does. Git runs
// hooks and reads config from the repository it pushes into, so a writer to the container could
// run code on this machine. A mirror may therefore hold only what the "Trust" paragraph of
// SPEC.md lists. empty reports that the container held no repository, which is when Create runs.
func Trust(mirror string) (empty bool, err error) {
	files := os.DirFS(mirror)
	entries := 0

	err = fs.WalkDir(files, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || name == "." {
			return err
		}

		if entry.IsDir() && scan.IsScratchPath(name) {
			return fs.SkipDir
		}

		entries++

		if !isAllowed(name, entry) {
			return fmt.Errorf("%w %s: it holds %s", ErrUntrusted, mirror, name)
		}

		return nil
	})
	if err != nil {
		return false, err
	}

	if entries == 0 {
		return true, nil
	}

	if fault := fault(files); fault != "" {
		return false, fmt.Errorf("%w %s: %s", ErrUntrusted, mirror, fault)
	}

	return false, nil
}

// isAllowed reports whether a mirror may hold the entry: a regular file or a directory, under
// a slash-separated name that is listed or lies below objects/ or refs/.
func isAllowed(name string, entry fs.DirEntry) bool {
	if !entry.IsDir() && !entry.Type().IsRegular() {
		return false
	}

	return strings.HasPrefix(name, "objects/") || strings.HasPrefix(name, "refs/") ||
		slices.Contains(allowed, name)
}

// fault says what is wrong with a mirror whose names all passed, or nothing when it is sound.
func fault(files fs.FS) string {
	for _, name := range alternates {
		// Asked of the filesystem, so a spelling it treats as the same name is found too.
		if _, err := fs.Stat(files, name); !errors.Is(err, fs.ErrNotExist) {
			return "it holds " + name
		}
	}

	if _, err := fs.Stat(files, "HEAD"); err != nil {
		return "it has no HEAD"
	}

	if !hasFixedConfig(files) {
		return "its config is not the one dr-sync writes"
	}

	return ""
}

// hasFixedConfig reports whether config is, byte for byte, one of the two texts Create writes.
func hasFixedConfig(files fs.FS) bool {
	// The size comes first, so a config of any size is never read into memory.
	info, err := fs.Stat(files, "config")
	if err != nil || info.Size() > int64(len(Config(SHA256))) {
		return false
	}

	text, err := fs.ReadFile(files, "config")

	return err == nil && (string(text) == Config(SHA1) || string(text) == Config(SHA256))
}
