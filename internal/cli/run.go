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

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/datarobot-labs/datarobot-sync/internal/progress"
	"github.com/datarobot-labs/datarobot-sync/internal/syncer"
)

// EnvRoot is the variable `run` sets to the absolute path of the synced directory. The command
// keeps the caller's working directory, so this is how it finds the tree.
const EnvRoot = "DR_SYNC_ROOT"

// runArgs requires `<path> -- <command> [args...]`, so the command's flags are never read as
// dr-sync's.
func runArgs(cmd *cobra.Command, args []string) error {
	if cmd.ArgsLenAtDash() != 1 || len(args) < 2 {
		return Failf(CodeUsage, "want <path> -- <command> [args...]: put -- before the command "+
			"so its flags are not read as ours")
	}

	return nil
}

func newRunCommand(global *globalFlags) *cobra.Command {
	opts := &uploadFlags{}

	cmd := &cobra.Command{
		Use:   "run <path> -- <command> [args...]",
		Short: "Pull a directory, run a command against it, and push the result",
		Long: "Make a directory a working copy of the container for one command.\n\n" +
			"run takes the container's lease, pulls the head into <path>, runs the command, and\n" +
			"pushes the result as the head's successor. The command runs in the current directory\n" +
			"with " + EnvRoot + " set to the absolute path of <path>. The pull removes paths the\n" +
			"container does not hold (--delete, on by default) and keeps every local file it replaces\n" +
			"or removes under .dr-sync-backup/. The push is refused with exit code 4 if the head moved\n" +
			"in the meantime, and the command's changes stay in <path>.\n\n" +
			"The push happens even when the command fails, because a half-finished run's state is\n" +
			"what the next run starts from. A failed command exits with code 7. Ctrl-C reaches the\n" +
			"command through the terminal and the push still follows. A second interrupt cancels the\n" +
			"push, and an interrupt before the command starts ends the run without running it.\n" +
			"SIGTERM is passed to the command only, so a shell wrapper should exec the last command.",
		Args: runArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRun(cmd, args, global, opts)
		},
	}

	opts.register(cmd, true)

	return cmd
}

// checkRunFlags rejects what cannot work with `run` before anything touches the network.
func checkRunFlags(global *globalFlags, opts *uploadFlags, command []string) error {
	switch {
	case global.dryRun:
		return Failf(CodeUsage, "run executes a command, so --dry-run has nothing to preview; "+
			"use `status` for a push")

	case global.output != string(progress.ModeText):
		return Failf(CodeUsage, "--output %s would mix events into the command's own stdout", global.output)

	case opts.compact:
		return Failf(CodeUsage, "--compact is not supported by run; run `compact` afterwards")
	}

	if _, err := exec.LookPath(command[0]); err != nil {
		return Failf(CodeUsage, "cannot run %q: %v", command[0], err)
	}

	return nil
}

func runRun(cmd *cobra.Command, args []string, global *globalFlags, opts *uploadFlags) error {
	command := args[1:]

	if err := checkRunFlags(global, opts, command); err != nil {
		return err
	}

	root, err := syncRoot(args[:1])
	if err != nil {
		return err
	}

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return Fail(CodeUsage, err)
	}

	session, err := global.open(cmd, true, root)
	if err != nil {
		return err
	}

	defer session.reporter.Close()

	overwrite, err := parseOverwrite(opts.overwrite)
	if err != nil {
		return err
	}

	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

	defer signal.Stop(signals)

	second, stopCounting := secondInterrupt(signals)
	defer stopCounting()

	result, err := syncer.Run(cmd.Context(), session.store, session.reporter, syncer.RunOptions{
		Download: syncer.DownloadOptions{
			Root:    root,
			Check:   session.check,
			Matcher: session.matcher,
			Delete:  opts.delete,
			Jobs:    global.jobs,
			Retries: global.retries,
		},
		Upload:      opts.uploadOptions(root, global, session, overwrite),
		Lock:        !opts.noLock,
		LeaseTTL:    opts.leaseTTL,
		Force:       opts.force,
		Timeout:     global.timeout,
		Command:     execCommand(command, absRoot),
		PushContext: pushUntil(second),
	})

	return finishRun(cmd, session.reporter, result, err)
}

// finishRun turns what Run returned into the exit status. A sync failure wins over a command
// failure, which is then only mentioned, because the sync failure says whether the state is safe.
func finishRun(cmd *cobra.Command, reporter *progress.Reporter, result *syncer.RunResult, err error) error {
	if err != nil {
		if result != nil && result.CommandErr != nil {
			reporter.Warn(fmt.Sprintf("the command also failed: %v", result.CommandErr))
		}

		return classify(err)
	}

	if result.Uploaded != nil {
		reportUpload(reporter, result.Uploaded)
	}

	switch {
	case cmd.Context().Err() != nil:
		// A command that traps the interrupt and exits cleanly must not make the run look normal.
		return Fail(CodeInterrupted, errors.New("interrupted; the tree was pushed as it stood"))

	case result.CommandErr != nil:
		return Fail(CodeCommand, fmt.Errorf("the command failed and its changes were pushed: %w", result.CommandErr))
	}

	return nil
}

// execCommand returns the Command that runs argv with this process's standard streams and EnvRoot
// set to root. It ignores the run's context because the terminal delivers Ctrl-C to the command,
// which must be left to wind down and write its state. SIGTERM is forwarded because only this
// process receives it.
func execCommand(argv []string, root string) func(context.Context) error {
	return func(context.Context) error {
		child := exec.Command(argv[0], argv[1:]...) //nolint:gosec // the user's own command is the point of run
		child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr

		child.Env = append(os.Environ(), EnvRoot+"="+root)

		// Registered before the start, so a SIGTERM that arrives while the command launches is
		// held in the channel and forwarded, not lost.
		forwarded := make(chan os.Signal, 1)
		signal.Notify(forwarded, syscall.SIGTERM)

		defer signal.Stop(forwarded)

		if err := child.Start(); err != nil {
			return fmt.Errorf("starting %s: %w", argv[0], err)
		}

		finished := make(chan struct{})
		defer close(finished)

		go forwardSignals(child.Process, forwarded, finished)

		return child.Wait()
	}
}

// forwardSignals passes each signal to process until finished closes.
func forwardSignals(process *os.Process, signals <-chan os.Signal, finished <-chan struct{}) {
	for {
		select {
		case sig := <-signals:
			if err := process.Signal(sig); err != nil {
				fmt.Fprintf(os.Stderr, "could not pass %v to the command: %v\n", sig, err)
			}
		case <-finished:
			return
		}
	}
}

// secondInterrupt closes the returned channel on the second signal it reads. The first interrupt
// of a run is spent on the command, so only the second one is meant for the push. The stop
// function ends the counting.
func secondInterrupt(signals <-chan os.Signal) (second <-chan struct{}, stop func()) {
	seconded := make(chan struct{})
	done := make(chan struct{})

	go func() {
		seen := 0

		for {
			select {
			case <-signals:
				seen++

				if seen == 2 {
					close(seconded)

					return
				}
			case <-done:
				return
			}
		}
	}()

	return seconded, sync.OnceFunc(func() { close(done) })
}

// pushUntil returns the PushContext that outlives the first interrupt and ends at the second.
func pushUntil(second <-chan struct{}) func(context.Context) (context.Context, context.CancelFunc) {
	return func(ctx context.Context) (context.Context, context.CancelFunc) {
		pushCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))

		go func() {
			select {
			case <-second:
				cancel()
			case <-pushCtx.Done():
			}
		}()

		return pushCtx, cancel
	}
}
