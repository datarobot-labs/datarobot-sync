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
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/datarobot-labs/datarobot-sync/internal/progress"
	"github.com/datarobot-labs/datarobot-sync/internal/syncer"
)

const (
	helperEnv     = "DR_SYNC_TEST_HELPER"
	helperExitEnv = "DR_SYNC_TEST_EXIT"

	helperWantRootEnv = "DR_SYNC_TEST_WANT_ROOT"
	helperReadyEnv    = "DR_SYNC_TEST_READY_FILE"
)

// TestHelperProcess is the command the execCommand tests run: this test binary again, exiting
// with the status the parent asks for. It does nothing in a normal test run.
func TestHelperProcess(_ *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		return
	}

	// Exit 98 when EnvRoot is not the path the parent expects.
	if want := os.Getenv(helperWantRootEnv); want != "" && os.Getenv(EnvRoot) != want {
		os.Exit(98)
	}

	if ready := os.Getenv(helperReadyEnv); ready != "" {
		terms := make(chan os.Signal, 1)
		signal.Notify(terms, syscall.SIGTERM)

		if err := os.WriteFile(ready, nil, 0o600); err != nil {
			os.Exit(97)
		}

		<-terms
	}

	code, err := strconv.Atoi(os.Getenv(helperExitEnv))
	if err != nil {
		code = 99
	}

	os.Exit(code)
}

// helperCommand is an argv that runs TestHelperProcess and exits with code.
func helperCommand(t *testing.T, code int) []string {
	t.Helper()

	t.Setenv(helperEnv, "1")
	t.Setenv(helperExitEnv, strconv.Itoa(code))

	return []string{os.Args[0], "-test.run=^TestHelperProcess$"}
}

func TestExecCommandSucceedsWhenTheCommandDoes(t *testing.T) {
	err := execCommand(helperCommand(t, 0), "/synced")(context.Background())

	require.NoError(t, err)
}

func TestExecCommandTellsTheCommandWhereTheTreeIs(t *testing.T) {
	t.Setenv(helperWantRootEnv, "/synced/tree")
	t.Setenv(EnvRoot, "/something/stale")

	require.NoError(t, execCommand(helperCommand(t, 0), "/synced/tree")(context.Background()),
		"the command must see the run's root, even over a value inherited from the caller")

	err := execCommand(helperCommand(t, 0), "/elsewhere")(context.Background())

	var exited *exec.ExitError

	require.ErrorAs(t, err, &exited)
	assert.Equal(t, 98, exited.ExitCode(), "a command given a different root must be able to tell")
}

func TestExecCommandReportsTheCommandsExitStatus(t *testing.T) {
	err := execCommand(helperCommand(t, 3), "/synced")(context.Background())

	var exited *exec.ExitError

	require.ErrorAs(t, err, &exited)
	assert.Equal(t, 3, exited.ExitCode())
}

func TestExecCommandReportsACommandThatCannotStart(t *testing.T) {
	err := execCommand([]string{"/nonexistent/dr-sync-test-command"}, "/synced")(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "starting /nonexistent/dr-sync-test-command")
}

func TestRunRequiresADashBeforeTheCommand(t *testing.T) {
	isolate(t)

	for name, args := range map[string][]string{
		"no dash":         {"run", "./state", "pulumi", "up"},
		"nothing after":   {"run", "./state", "--"},
		"dash before dir": {"run", "--", "./state", "pulumi"},
		"no path":         {"run"},
	} {
		code, _, errOut := run(t, args...)

		assert.Equal(t, CodeUsage, code, name)
		assert.Contains(t, errOut, "put -- before the command", name)
	}
}

// The message must name the real mistake, not "no container given".
func TestRunRejectsWhatCannotWorkBeforeTouchingTheNetwork(t *testing.T) {
	isolate(t)

	cases := map[string]struct {
		args []string
		want string
	}{
		"dry run":       {[]string{"run", "./state", "--dry-run", "--", os.Args[0]}, "--dry-run has nothing to preview"},
		"json output":   {[]string{"run", "./state", "--output", "json", "--", os.Args[0]}, "mix events into the command's own stdout"},
		"compact":       {[]string{"run", "./state", "--compact", "--", os.Args[0]}, "--compact is not supported by run"},
		"missing":       {[]string{"run", "./state", "--", "/nonexistent/dr-sync-test-command"}, "cannot run"},
		"expected head": {[]string{"run", "./state", "--expect-head", "0000000001-abc", "--", os.Args[0]}, "unknown flag: --expect-head"},
	}

	for name, tc := range cases {
		code, _, errOut := run(t, tc.args...)

		assert.Equal(t, CodeUsage, code, name)
		assert.Contains(t, errOut, tc.want, name)
	}
}

func TestRunDefaultsToMirroringLikeWatch(t *testing.T) {
	isolate(t)

	_, runHelp, _ := run(t, "run", "--help")

	assert.Contains(t, deleteFlagLine(t, runHelp), "default true",
		"run pulls and pushes a mirror, or a path removed upstream would come back")
}

func TestClassifyMapsTheFailuresRunAdds(t *testing.T) {
	t.Parallel()

	wrapped := func(err error) error {
		return fmt.Errorf("the command's changes were not pushed and remain in ./state: %w", err)
	}

	assert.Equal(t, CodePartial,
		CodeFor(classify(&syncer.IncompleteDownloadError{Paths: []string{"a"}})))
	assert.Equal(t, CodeConflict,
		CodeFor(classify(wrapped(&syncer.StaleBaseError{Expected: "1-a", Actual: "2-b"}))),
		"a stale base found after the command stays a conflict through the wrapping")
	assert.Equal(t, CodeConflict,
		CodeFor(classify(&syncer.LeaseHeldError{})))
	assert.Equal(t, CodeInternal, CodeFor(classify(errors.New("something else"))))
}

func TestFinishRunPicksTheExitStatus(t *testing.T) {
	t.Parallel()

	failure := errors.New("exit status 255")
	interrupted, cancel := context.WithCancel(context.Background())

	cancel()

	cases := map[string]struct {
		ctx    context.Context //nolint:containedctx // a table of contexts is the point
		result *syncer.RunResult
		err    error
		want   int
	}{
		"clean run":                    {context.Background(), &syncer.RunResult{}, nil, CodeOK},
		"command failed":               {context.Background(), &syncer.RunResult{CommandErr: failure}, nil, CodeCommand},
		"interrupted and failed":       {interrupted, &syncer.RunResult{CommandErr: failure}, nil, CodeInterrupted},
		"interrupted but exited clean": {interrupted, &syncer.RunResult{}, nil, CodeInterrupted},
		"push refused":                 {context.Background(), &syncer.RunResult{CommandErr: failure}, &syncer.StaleBaseError{}, CodeConflict},
		"interrupted and push refused": {interrupted, &syncer.RunResult{CommandErr: failure}, &syncer.StaleBaseError{}, CodeInterrupted},
		"pull failed before the run":   {context.Background(), nil, errors.New("boom"), CodeInternal},
	}

	for name, tc := range cases {
		cmd := &cobra.Command{}
		cmd.SetContext(tc.ctx)

		got := CodeFor(finishRun(cmd, progress.Discard(), tc.result, tc.err))

		assert.Equal(t, tc.want, got, name)
	}
}

func TestFinishRunKeepsThePushErrorWhenAnInterruptSetsTheStatus(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())

	cancel()

	cmd := &cobra.Command{}
	cmd.SetContext(ctx)

	refused := fmt.Errorf("the command's changes were not pushed and remain in ./state: %w", &syncer.StaleBaseError{})

	err := finishRun(cmd, progress.Discard(), &syncer.RunResult{}, refused)

	require.ErrorIs(t, err, refused, "the user must still learn that the changes remain local")
}

// The first interrupt of a run is spent on the command. Only the second one is for the push.
func TestOnlyTheSecondInterruptCancelsThePush(t *testing.T) {
	t.Parallel()

	signals := make(chan os.Signal, 2)
	second, stop := secondInterrupt(signals)

	defer stop()

	pushCtx, cancel := pushUntil(second)(context.Background())
	defer cancel()

	signals <- os.Interrupt

	assert.Never(t, func() bool { return pushCtx.Err() != nil }, 150*time.Millisecond, 10*time.Millisecond,
		"one interrupt must not cancel the push")

	signals <- os.Interrupt

	assert.Eventually(t, func() bool { return pushCtx.Err() != nil }, 5*time.Second, 10*time.Millisecond,
		"the second interrupt must cancel the push")
}

// `dr` cancels its own context on Ctrl-C and sends the plugin SIGTERM, while the terminal sends
// SIGINT to the whole process group. One Ctrl-C then reaches the run as both signals, in either
// order, and must not count as two interrupts.
func TestOneInterruptThatArrivesAsSigintAndSigtermDoesNotCancelThePush(t *testing.T) {
	t.Parallel()

	orders := map[string][]os.Signal{
		"sigint then sigterm": {os.Interrupt, syscall.SIGTERM},
		"sigterm then sigint": {syscall.SIGTERM, os.Interrupt},
		"sigterm twice":       {syscall.SIGTERM, syscall.SIGTERM},
	}

	for name, sent := range orders {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			signals := make(chan os.Signal, 2)
			second, stop := secondInterrupt(signals)

			defer stop()

			pushCtx, cancel := pushUntil(second)(context.Background())
			defer cancel()

			for _, sig := range sent {
				signals <- sig
			}

			assert.Never(t, func() bool { return pushCtx.Err() != nil }, 150*time.Millisecond, 10*time.Millisecond,
				"only two SIGINTs may cancel the push")
		})
	}
}

// An interrupt that arrived during the pull and another during the command already count.
func TestAPushStartedAfterTwoInterruptsIsCanceledAtOnce(t *testing.T) {
	t.Parallel()

	signals := make(chan os.Signal, 2)
	second, stop := secondInterrupt(signals)

	defer stop()

	signals <- os.Interrupt

	signals <- os.Interrupt

	require.Eventually(t, func() bool {
		select {
		case <-second:
			return true
		default:
			return false
		}
	}, 5*time.Second, 10*time.Millisecond)

	pushCtx, cancel := pushUntil(second)(context.Background())
	defer cancel()

	assert.Eventually(t, func() bool { return pushCtx.Err() != nil }, 5*time.Second, 10*time.Millisecond)
}

func TestThePushContextIgnoresTheCancellationOfItsParent(t *testing.T) {
	t.Parallel()

	signals := make(chan os.Signal, 2)
	second, stop := secondInterrupt(signals)

	defer stop()

	parent, cancelParent := context.WithCancel(context.Background())

	pushCtx, cancel := pushUntil(second)(parent)
	defer cancel()

	cancelParent()

	assert.NoError(t, pushCtx.Err(), "the first interrupt cancels the run's context, not the push")
}
