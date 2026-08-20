// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

package cli

import (
	"context"
	"fmt"

	"github.com/datarobot/golang-datarobot-filesync/internal/version"
)

// Commands returns the subcommand registry. The order here is the order shown in help.
func Commands() *Registry {
	return NewRegistry(
		versionCommand(),
	)
}

func versionCommand() *Command {
	return &Command{
		Name:    "version",
		Short:   "Print the version and build metadata",
		NoFlags: true,
		Run: func(_ context.Context, env *Env, args []string) error {
			if len(args) > 0 {
				return Failf(CodeUsage, "version takes no arguments")
			}

			fmt.Fprintln(env.Stdout, version.Long())

			return nil
		},
	}
}
