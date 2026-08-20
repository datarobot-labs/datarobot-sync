// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

// Command dr-sync is rsync for the DataRobot Files API, and a `dr` CLI plugin.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/datarobot/golang-datarobot-filesync/internal/cli"
)

func main() {
	// Answer plugin discovery before anything else: no flag parsing, no config, no network.
	// It has to come before cobra, which would reject the flag as unknown.
	handlePluginManifest(os.Args[1:])

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	root := cli.NewRoot()
	root.SetContext(ctx)

	os.Exit(cli.Execute(root, os.Args[1:]))
}
