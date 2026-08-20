// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

// Package cli builds dr-sync's command tree.
package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/datarobot/golang-datarobot-filesync/internal/config"
	"github.com/datarobot/golang-datarobot-filesync/internal/version"
)

// NewRoot builds the command tree.
func NewRoot() *cobra.Command {
	global := &globalFlags{}

	root := &cobra.Command{
		Use:   programName(),
		Short: "rsync for the DataRobot Files API",
		Long: "dr-sync makes a local directory and a DataRobot Files container match,\n" +
			"transferring only the difference, xz-compressed.",
		Version: version.Short(),
		// Errors and usage are printed by Execute, which knows the exit code that goes with
		// them; cobra printing them too would double every message.
		SilenceErrors: true,
		SilenceUsage:  true,
		// Cobra's own "unknown command" arrives uncoded, which would report a typo as an
		// internal error. Validating here also lets the message suggest the nearest command.
		Args: rootArgs,
		// A bare invocation should show help rather than claim success.
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return unknownCommand(cmd, args[0])
			}

			if err := cmd.Help(); err != nil {
				return err
			}

			return Failf(CodeUsage, "no subcommand given")
		},
	}

	global.register(root.PersistentFlags())

	// Cobra's own parse failures arrive uncoded, which would report a mistyped flag as an
	// internal error. Both this and the Args validators below make them usage errors.
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return Fail(CodeUsage, err)
	})

	// Not "{{.Name}}": cobra takes only the first word of Use, which under the dr CLI would
	// print "dr" rather than "dr sync".
	root.SetVersionTemplate(programName() + " {{.Version}}\n")

	root.AddCommand(
		newInitCommand(global),
		newUploadCommand(global),
		newDownloadCommand(global),
		newStatusCommand(global),
		newLSCommand(global),
		newInspectCommand(global),
		newCompactCommand(global),
		newGCCommand(global),
		newVersionCommand(),
	)

	return root
}

// Execute runs the command tree and returns the process exit code.
func Execute(root *cobra.Command, args []string) int {
	root.SetArgs(args)

	err := root.Execute()
	if err == nil {
		return CodeOK
	}

	code := CodeFor(err)

	// A usage error is the one case where the command's own help is worth more than the
	// message alone.
	if code == CodeUsage {
		fmt.Fprintln(root.ErrOrStderr())
	}

	fmt.Fprintf(root.ErrOrStderr(), "%s: %v\n", programName(), err)

	return code
}

// programName reports how the user invoked us, so usage text reads back what they typed.
// Under the `dr` CLI that is "dr sync"; standalone it is the binary's own name.
func programName() string {
	if config.RunningAsPlugin() {
		return "dr sync"
	}

	if len(os.Args) > 0 {
		return filepath.Base(os.Args[0])
	}

	return "dr-sync"
}

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version and build metadata",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), version.Long())

			return nil
		},
	}
}

// exactArgs is cobra.ExactArgs with the usage exit code attached.
func exactArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		return Fail(CodeUsage, cobra.ExactArgs(n)(cmd, args))
	}
}

// noArgs is cobra.NoArgs with the usage exit code attached.
func noArgs(cmd *cobra.Command, args []string) error {
	return Fail(CodeUsage, cobra.NoArgs(cmd, args))
}

// rootArgs accepts anything, leaving the root's RunE to report a mistyped subcommand with a
// suggestion attached.
func rootArgs(*cobra.Command, []string) error {
	return nil
}

// unknownCommand reports a subcommand that does not exist, suggesting the closest match.
func unknownCommand(root *cobra.Command, name string) error {
	if suggestion := closestCommand(root, name); suggestion != "" {
		return Failf(CodeUsage, "unknown command %q, did you mean %q?", name, suggestion)
	}

	return Failf(CodeUsage, "unknown command %q for %q", name, root.Name())
}

// closestCommand returns the command name within an edit distance of two, or "".
func closestCommand(root *cobra.Command, name string) string {
	best, bestDistance := "", 3

	for _, candidate := range root.Commands() {
		if candidate.Hidden {
			continue
		}

		distance := editDistance(name, candidate.Name())
		if distance < bestDistance || (distance == bestDistance && candidate.Name() < best) {
			best, bestDistance = candidate.Name(), distance
		}
	}

	return best
}

// editDistance is Levenshtein distance over bytes, adequate for ASCII command names.
func editDistance(a, b string) int {
	previous := make([]int, len(b)+1)
	current := make([]int, len(b)+1)

	for j := range previous {
		previous[j] = j
	}

	for i := 1; i <= len(a); i++ {
		current[0] = i

		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}

			current[j] = min(previous[j]+1, min(current[j-1]+1, previous[j-1]+cost))
		}

		previous, current = current, previous
	}

	return previous[len(b)]
}
