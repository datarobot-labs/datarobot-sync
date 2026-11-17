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

package gitmirror

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

// RemoteName is the remote a repo command points at the mirror, so the user's own `git status`
// shows how far ahead or behind the checkout is. It is a convenience: the commands pass the
// mirror path themselves.
const RemoteName = "dr-sync"

// ErrRejected is a ref Git would not fast-forward, because the mirror holds commits the
// checkout does not. It is the conflict a moved head is, one step earlier, so a command
// reports it with exit 4 and the advice to pull first.
var ErrRejected = errors.New("the container holds commits this checkout does not")

// refusedFlag opens the `--porcelain` status line of a ref Git did not take:
// "!\tHEAD:refs/heads/main\t[rejected] (fetch first)".
const refusedFlag = "!\t"

// rejectedSummary is the summary of a ref that is not a fast-forward. The wider
// "[remote rejected]" is a different answer and is not this conflict.
const rejectedSummary = "[rejected]"

// shallowSummary is how Git refuses a push that would leave the mirror without the history its
// commits need. A checkout made with `git clone --depth` is the usual cause, and no pull from
// the container fixes it.
const shallowSummary = "shallow update not allowed"

// Push sends the checkout's commits into the mirror. args replaces the default HEAD, so a
// caller can pass on what followed `--`, such as --all or --tags; nothing is forced that the
// caller did not ask for. Git's porcelain status is copied to Stdout as it arrives and its own
// messages go to Stderr.
func (g *Git) Push(ctx context.Context, checkout, mirror string, args []string) error {
	if len(args) == 0 {
		args = []string{"HEAD"}
	}

	cmd := g.command(ctx, append([]string{"-C", checkout, "push", "--porcelain", mirror}, args...)...)
	cmd.Stderr = g.Stderr

	status, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("running git push: %w", err)
	}

	// Read to the end before Wait, which closes the pipe under a reader still going.
	refused := copyStatus(status, g.Stdout)

	if err := cmd.Wait(); err != nil {
		return pushFailure(refused, err)
	}

	return nil
}

// SetRemote points the checkout's dr-sync remote at the mirror, adding it where the checkout
// has none and rewriting the URL where the mirror moved.
func (g *Git) SetRemote(ctx context.Context, checkout, mirror string) error {
	// set-url fails when the remote is not there yet, which is how its absence is found.
	if _, err := g.query(ctx, "-C", checkout, "remote", "set-url", RemoteName, mirror); err == nil {
		return nil
	}

	if _, err := g.query(ctx, "-C", checkout, "remote", "add", RemoteName, mirror); err != nil {
		return fmt.Errorf("pointing the %s remote at %s: %w", RemoteName, mirror, err)
	}

	return nil
}

// copyStatus copies Git's porcelain status to out, which may be nil, and returns the line of
// the refused ref that says most, empty when Git took them all. It reads to the end whatever
// it finds, so the pipe behind status is always drained.
func copyStatus(status io.Reader, out io.Writer) string {
	worst := ""
	lines := bufio.NewReader(status)

	for {
		// Read by hand rather than with a Scanner, whose line limit would end the copy early
		// and leave the rest of the status unread.
		line, err := lines.ReadString('\n')

		if out != nil {
			fmt.Fprint(out, line)
		}

		// One push of many refs can be refused for several reasons at once, and the ref Git
		// names first is not always the one whose answer the user needs.
		refused := strings.TrimRight(line, "\n")
		if strings.HasPrefix(refused, refusedFlag) && (worst == "" || rank(refused) > rank(worst)) {
			worst = refused
		}

		if err != nil {
			return worst
		}
	}
}

// rank orders the refusals by what each asks of the user. A history the mirror cannot hold
// comes first, because no pull from the container fixes it and sending the user to `repo pull`
// would only loop. A conflict comes next, because a pull does fix that one. Every other
// refusal is Git's own to explain.
func rank(refused string) int {
	switch {
	case strings.Contains(refused, shallowSummary):
		return 2

	case strings.Contains(refused, rejectedSummary):
		return 1
	}

	return 0
}

// pushFailure says why Git refused, reading the status line of the ref whose refusal says
// most. A push that fails before any ref is weighed, such as one naming a ref that does not
// exist, prints no such line and is reported as it came.
func pushFailure(refused string, err error) error {
	switch {
	case strings.Contains(refused, shallowSummary):
		return fmt.Errorf("git push refused %q: this checkout holds only part of its history, "+
			"so run `git fetch --unshallow` first: %w", refused, err)

	case strings.Contains(refused, rejectedSummary):
		return fmt.Errorf("%w: git push refused %q", ErrRejected, refused)

	case refused != "":
		return fmt.Errorf("git push refused %q: %w", refused, err)
	}

	return fmt.Errorf("git push failed: %w", err)
}
