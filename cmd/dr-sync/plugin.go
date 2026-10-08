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

package main

import (
	_ "embed"
	"fmt"
	"io"
	"os"
)

// pluginManifestFlag is the argument the `dr` CLI uses to interrogate a plugin during
// discovery. It must be answered with a single JSON object on stdout and exit 0, fast,
// deterministically, and without touching the network.
const pluginManifestFlag = "--dr-plugin-manifest"

//go:embed manifest.json
var pluginManifest []byte

// writePluginManifest emits the embedded manifest. It is the same file the release archive
// ships, so the binary and the archive can never disagree — `dr self plugin package`
// validates that they match exactly.
func writePluginManifest(w io.Writer) error {
	_, err := w.Write(pluginManifest)

	return err
}

// handlePluginManifest answers the discovery flag and exits, or returns if the flag is
// absent. It runs before flag parsing so an unknown-flag error can never shadow discovery.
func handlePluginManifest(args []string) {
	for _, arg := range args {
		if arg != pluginManifestFlag {
			continue
		}

		if err := writePluginManifest(os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "dr-sync: writing plugin manifest: %v\n", err)
			os.Exit(1)
		}

		os.Exit(0)
	}
}
