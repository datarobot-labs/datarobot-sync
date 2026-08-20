// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Command is one subcommand. Run receives the arguments after the subcommand name.
type Command struct {
	Name    string
	Usage   string // one-line argument summary, e.g. "<path>"
	Short   string // one-line description for the command list
	Run     func(ctx context.Context, env *Env, args []string) error
	Hidden  bool
	NoFlags bool // documented for commands that take no flags at all
}

// Env is the ambient I/O and metadata a command needs. Keeping it explicit rather than
// reaching for globals is what makes the commands testable.
type Env struct {
	Stdout io.Writer
	Stderr io.Writer
	// Program is the name to print in usage text, normally "dr-sync" but "dr sync" when
	// running under the CLI, since that is what the user typed.
	Program string
}

// Registry is the ordered set of subcommands.
type Registry struct {
	commands []*Command
}

// NewRegistry builds a registry from the given commands.
func NewRegistry(commands ...*Command) *Registry {
	return &Registry{commands: commands}
}

// Lookup finds a command by exact name.
func (r *Registry) Lookup(name string) *Command {
	for _, cmd := range r.commands {
		if cmd.Name == name {
			return cmd
		}
	}

	return nil
}

// Dispatch parses argv (excluding the program name) and runs the selected subcommand.
func (r *Registry) Dispatch(ctx context.Context, env *Env, args []string) error {
	if len(args) == 0 {
		r.PrintUsage(env.Stderr, env.Program)

		return Failf(CodeUsage, "no subcommand given")
	}

	name := args[0]

	switch name {
	case "-h", "--help", "help":
		r.PrintUsage(env.Stdout, env.Program)

		return nil
	}

	cmd := r.Lookup(name)
	if cmd == nil {
		if suggestion := r.suggest(name); suggestion != "" {
			return Failf(CodeUsage, "unknown subcommand %q, did you mean %q?", name, suggestion)
		}

		r.PrintUsage(env.Stderr, env.Program)

		return Failf(CodeUsage, "unknown subcommand %q", name)
	}

	err := cmd.Run(ctx, env, args[1:])
	if err == nil {
		return nil
	}

	// flag.ExitOnError is never used, so a parse failure arrives here and is a usage error
	// rather than an internal one.
	if errors.Is(err, flag.ErrHelp) || strings.Contains(err.Error(), "flag provided but not defined") {
		return Fail(CodeUsage, err)
	}

	return err
}

// PrintUsage writes the top-level help.
func (r *Registry) PrintUsage(w io.Writer, program string) {
	fmt.Fprintf(w, "%s — rsync for the DataRobot Files API\n\n", program)
	fmt.Fprintf(w, "Usage:\n  %s <command> [flags]\n\nCommands:\n", program)

	width := 0

	for _, cmd := range r.commands {
		if cmd.Hidden {
			continue
		}

		if n := len(cmd.Name) + len(cmd.Usage) + 1; n > width {
			width = n
		}
	}

	for _, cmd := range r.commands {
		if cmd.Hidden {
			continue
		}

		invocation := cmd.Name
		if cmd.Usage != "" {
			invocation += " " + cmd.Usage
		}

		fmt.Fprintf(w, "  %-*s  %s\n", width, invocation, cmd.Short)
	}

	fmt.Fprintf(w, "\nRun \"%s <command> --help\" for the flags of a single command.\n", program)
}

// suggest returns the closest command name within an edit distance of two, or "".
func (r *Registry) suggest(name string) string {
	type candidate struct {
		name     string
		distance int
	}

	var candidates []candidate

	for _, cmd := range r.commands {
		if distance := editDistance(name, cmd.Name); distance <= 2 {
			candidates = append(candidates, candidate{cmd.Name, distance})
		}
	}

	if len(candidates) == 0 {
		return ""
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].distance != candidates[j].distance {
			return candidates[i].distance < candidates[j].distance
		}

		return candidates[i].name < candidates[j].name
	})

	return candidates[0].name
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
