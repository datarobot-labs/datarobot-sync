// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

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
