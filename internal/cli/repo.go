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

	"github.com/spf13/cobra"

	"github.com/datarobot-labs/datarobot-sync/internal/gitmirror"
	"github.com/datarobot-labs/datarobot-sync/internal/progress"
	"github.com/datarobot-labs/datarobot-sync/internal/syncer"
)

func newRepoCommand(global *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repo",
		Short: "Move a Git repository between machines that share no Git host",
		Long: "Move a Git repository through a container.\n\n" +
			"The container holds one bare Git repository, the mirror, and nothing else, so one\n" +
			"container is one repository. Each command pulls the mirror into the checkout's own\n" +
			"Git directory, where git status never shows it, runs git against it, and pushes the\n" +
			"result back. The mirror is scratch: deleting it costs one download.",
		Args: rootArgs,
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

	cmd.AddCommand(newRepoPushCommand(global))

	return cmd
}

func newRepoPushCommand(global *globalFlags) *cobra.Command {
	opts := &uploadFlags{}

	cmd := &cobra.Command{
		Use:   "push [<path>] [-- <git push arguments>]",
		Short: "Send a checkout's commits to the container",
		Long: "Send a checkout's commits to the container's Git mirror.\n\n" +
			"<path> is any directory inside a checkout and defaults to the current one. The run\n" +
			"takes the container's lease, pulls the mirror, runs `git push` into it, and pushes\n" +
			"the mirror back as the head's successor. Arguments after -- replace the default\n" +
			"HEAD, so `-- --all` sends every branch; nothing is forced that you did not ask for.\n\n" +
			"When git fails nothing is uploaded and the mirror stays as it was pulled. A push git\n" +
			"rejected exits 4, because the container holds commits this checkout does not: pull\n" +
			"them first. Any other git failure exits 7, and a mirror holding what dr-sync and git\n" +
			"did not write exits 5.",
		Args: rootArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRepoPush(cmd, args, global, opts)
		},
	}

	opts.register(cmd, true)

	return cmd
}

// splitRepoArgs separates the optional path from the Git arguments that followed --. Without a
// --, every argument is the path, so a stray one is a mistake rather than something for Git.
func splitRepoArgs(cmd *cobra.Command, args []string) (path string, gitArgs []string, err error) {
	dash := cmd.ArgsLenAtDash()

	if dash > 1 || (dash < 0 && len(args) > 1) {
		return "", nil, Failf(CodeUsage,
			"want `repo push [<path>] [-- <git push arguments>]`: one path at most, and -- before "+
				"git's own arguments so its flags are not read as ours")
	}

	if dash < 0 {
		if len(args) == 1 {
			path = args[0]
		}

		return path, nil, nil
	}

	if dash == 1 {
		path = args[0]
	}

	return path, args[dash:], nil
}

// checkRepoFlags rejects what cannot work on a mirror before anything touches the network. A
// mirror missing part of itself is a broken repository, not a smaller one, so every flag that
// would leave something out is refused rather than honoured.
func checkRepoFlags(global *globalFlags, opts *uploadFlags) error {
	switch {
	case global.dryRun:
		return Failf(CodeUsage, "repo push runs git, so --dry-run has nothing to preview")

	case global.output != string(progress.ModeText):
		return Failf(CodeUsage, "--output %s would mix events into git's own output", global.output)

	case !opts.delete:
		return Failf(CodeUsage,
			"--delete=false would keep objects the mirror no longer has, which is a broken repository")

	case len(global.exclude)+len(global.include)+len(global.excludeFrom) > 0:
		return Failf(CodeUsage,
			"a mirror is sent whole, so --exclude, --include and --exclude-from would leave a "+
				"broken repository in the container")
	}

	return nil
}

func runRepoPush(cmd *cobra.Command, args []string, global *globalFlags, opts *uploadFlags) error {
	path, gitArgs, err := splitRepoArgs(cmd, args)
	if err != nil {
		return err
	}

	if err := checkRepoFlags(global, opts); err != nil {
		return err
	}

	git, checkout, err := openCheckout(cmd, path)
	if err != nil {
		return err
	}

	if checkout.Branch == "" && len(gitArgs) == 0 {
		return Failf(CodeUsage, "HEAD is detached, so there is no branch to push: say what to "+
			"send after --, as in `-- --all`")
	}

	// The mirror is the sync root, and it holds nothing of the user's own, so no ignore file is
	// read for it and the refusals above keep the matcher empty.
	session, err := global.open(cmd, true, "")
	if err != nil {
		return err
	}

	defer session.reporter.Close()

	overwrite, err := parseOverwrite(opts.overwrite)
	if err != nil {
		return err
	}

	mirror := checkout.MirrorPath(session.resolved.CatalogID)

	result, err := syncer.Run(cmd.Context(), session.store, session.reporter, syncer.RunOptions{
		Download: syncer.DownloadOptions{
			Root:    mirror,
			Check:   session.check,
			Delete:  true,
			Jobs:    global.jobs,
			Retries: global.retries,
		},
		Upload: opts.uploadOptions(mirror, global, session, overwrite),
		// The mirror came from the container or from this checkout, so there is nothing to keep
		// a copy of, and a failed git push must leave the container's head where it was.
		NoBackup:          true,
		SkipPushOnFailure: true,
		Lock:              !opts.noLock,
		LeaseTTL:          opts.leaseTTL,
		Force:             opts.force,
		Timeout:           global.timeout,
		Command:           pushToMirror(git, checkout, path, mirror, gitArgs),
	})

	return finishRepoPush(cmd, session.reporter, result, err)
}

// openCheckout finds git and describes the checkout holding path, which defaults to the
// current directory. Neither being absent is dr-sync's failure, so both are usage errors.
func openCheckout(cmd *cobra.Command, path string) (*gitmirror.Git, gitmirror.Checkout, error) {
	git, err := gitmirror.Find()
	if err != nil {
		return nil, gitmirror.Checkout{}, Fail(CodeUsage, err)
	}

	git.Stdin, git.Stdout, git.Stderr = cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()

	if path == "" {
		path, err = os.Getwd()
		if err != nil {
			return nil, gitmirror.Checkout{}, Fail(CodeUsage, err)
		}
	}

	checkout, err := git.Checkout(cmd.Context(), path)
	if err != nil {
		return nil, gitmirror.Checkout{}, Fail(CodeUsage, err)
	}

	return git, checkout, nil
}

// pushToMirror is the work between the pull and the push: check what the pull left, create the
// mirror when the container held none, and send the commits.
func pushToMirror(git *gitmirror.Git, checkout gitmirror.Checkout, path, mirror string,
	args []string,
) func(context.Context) error {
	return func(ctx context.Context) error {
		empty, err := gitmirror.Trust(mirror)
		if err != nil {
			return err
		}

		if empty {
			if checkout.Branch == "" {
				return Failf(CodeUsage, "the container holds no repository yet, and a detached "+
					"HEAD does not name the branch to start it from: push from a branch first")
			}

			if err := git.Create(ctx, mirror, checkout.Branch, checkout.ObjectFormat); err != nil {
				return err
			}
		}

		if err := git.SetRemote(ctx, path, mirror); err != nil {
			return err
		}

		return git.Push(ctx, path, mirror, args)
	}
}

// finishRepoPush turns what Run returned into the exit status. A sync failure wins over a git
// failure, because it is the one that says whether the container is safe.
func finishRepoPush(cmd *cobra.Command, reporter *progress.Reporter, result *syncer.RunResult, err error) error {
	if err != nil {
		return classify(err)
	}

	if cmd.Context().Err() != nil {
		return Fail(CodeInterrupted, errors.New("interrupted"))
	}

	if result.CommandErr != nil {
		return classifyGit(result.CommandErr)
	}

	if result.Uploaded != nil {
		reportUpload(reporter, result.Uploaded)
	}

	return nil
}

// classifyGit gives a failure from the mirror its exit code. A rejected push is the conflict a
// moved head is, one step earlier, so it shares that code and the advice to pull first.
func classifyGit(err error) error {
	var coded *ExitError

	switch {
	case errors.As(err, &coded):
		return err

	case errors.Is(err, gitmirror.ErrRejected):
		return Fail(CodeConflict, fmt.Errorf("%w: run `%s repo pull` first, then push again",
			err, programName()))

	case errors.Is(err, gitmirror.ErrUntrusted):
		return Fail(CodeIntegrity, err)
	}

	return Fail(CodeCommand, err)
}
