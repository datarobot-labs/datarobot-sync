// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

// Command dr-sync is rsync for the DataRobot Files API, and a `dr` CLI plugin.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/datarobot/golang-datarobot-filesync/internal/cli"
)

func main() {
	// Answer plugin discovery before anything else: no flag parsing, no config, no network.
	handlePluginManifest(os.Args[1:])

	env := &cli.Env{Stdout: os.Stdout, Stderr: os.Stderr, Program: programName()}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := cli.Commands().Dispatch(ctx, env, os.Args[1:])
	if err == nil {
		return
	}

	if errors.Is(err, context.Canceled) {
		fmt.Fprintln(env.Stderr, "dr-sync: interrupted")
		os.Exit(cli.CodeInterrupted)
	}

	fmt.Fprintf(env.Stderr, "dr-sync: %v\n", err)
	os.Exit(cli.CodeFor(err))
}

// programName reports how the user invoked us, so usage text reads back what they typed.
// Under the `dr` CLI that is "dr sync"; standalone it is the binary's own name.
func programName() string {
	if os.Getenv("DR_PLUGIN_MODE") == "1" {
		return "dr sync"
	}

	return filepath.Base(os.Args[0])
}
