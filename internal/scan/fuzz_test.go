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
	"strings"
	"testing"
)

// pathSeeds are the shapes the path rules exist for: traversal, absolute paths, empty
// elements, NUL and backslash bytes, and decomposed Unicode.
var pathSeeds = []string{
	"a/b/c.txt",
	"../escape",
	"a/../../escape",
	"/etc/passwd",
	"a//b",
	"./a",
	"a/",
	"",
	"a\x00b",
	`a\..\b`,
	"cafe\u0301/re\u0301sume\u0301.txt", // decomposed: e + combining acute
	"caf\u00e9/r\u00e9sum\u00e9.txt",    // precomposed
}

// FuzzLocalPath checks the property that keeps a crafted manifest from writing outside the
// sync root: whenever LocalPath accepts a path, the result is inside the root.
func FuzzLocalPath(f *testing.F) {
	for _, seed := range pathSeeds {
		f.Add(seed)
	}

	root := filepath.Join(string(filepath.Separator), "sync", "root")

	f.Fuzz(func(t *testing.T, manifestPath string) {
		local, err := LocalPath(root, manifestPath)
		if err != nil {
			return
		}

		relative, err := filepath.Rel(root, local)
		if err != nil {
			t.Fatalf("accepted %q, but %q has no relative form under the root: %v", manifestPath, local, err)
		}

		if relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			t.Fatalf("accepted %q, but it resolves to %q outside the root", manifestPath, local)
		}
	})
}

// FuzzNormalize checks that normalizing twice changes nothing. A path that kept changing on
// every pass would be re-uploaded on every run.
func FuzzNormalize(f *testing.F) {
	for _, seed := range pathSeeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, path string) {
		once := Normalize(path)

		if twice := Normalize(once); twice != once {
			t.Fatalf("Normalize is not idempotent for %q: %q then %q", path, once, twice)
		}
	})
}
