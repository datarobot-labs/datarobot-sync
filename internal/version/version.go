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

// Package version carries build metadata injected at link time.
package version

import (
	"fmt"
	"runtime"
)

// Injected via -ldflags at build time; the defaults describe a `go build` with no flags.
var (
	Version   = "dev"
	GitCommit = "unknown"
	BuildDate = "unknown"
)

// Short returns just the version string.
func Short() string {
	return Version
}

// Long returns a multi-token description for `dr-sync version`.
func Long() string {
	return fmt.Sprintf("dr-sync %s (commit %s, built %s, %s %s/%s)",
		Version, GitCommit, BuildDate, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

// Generator returns the string recorded in a manifest's `generator` field, so a container
// carries a record of what wrote it.
func Generator() string {
	return fmt.Sprintf("dr-sync/%s %s %s/%s", Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
