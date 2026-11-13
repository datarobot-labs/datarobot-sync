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

// Package gitmirror finds and creates the bare Git repository that `dr-sync repo` keeps in a
// container, and runs git for it. The "Repo" section of SPEC.md is the contract.
package gitmirror

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// Usage errors: a command reports either one with exit 2.
var (
	ErrNoGit       = errors.New("git is not on PATH")
	ErrNotCheckout = errors.New("not inside a Git checkout")
)

// Object formats Git can store a repository in.
const (
	SHA1   = "sha1"
	SHA256 = "sha256"
)

// Git runs the git binary. Run uses the three streams, which the caller sets.
type Git struct {
	bin    string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Checkout is what the repo commands need to know about a working tree.
type Checkout struct {
	CommonDir    string // absolute Git directory, shared by linked worktrees
	Branch       string // empty on a detached HEAD
	ObjectFormat string // SHA1 or SHA256
}

// Find locates git on PATH.
func Find() (*Git, error) {
	bin, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNoGit, err)
	}

	return &Git{bin: bin}, nil
}

// Run runs git with args on the caller's streams.
func (g *Git) Run(ctx context.Context, args ...string) error {
	cmd := g.command(ctx, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = g.Stdin, g.Stdout, g.Stderr

	return cmd.Run()
}

// Checkout describes the working tree that holds path, which may be any directory inside it.
func (g *Git) Checkout(ctx context.Context, path string) (Checkout, error) {
	out, err := g.query(ctx, "-C", path, "rev-parse", "--path-format=absolute",
		"--is-inside-work-tree", "--git-common-dir", "--show-object-format")

	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return Checkout{}, fmt.Errorf("%w: %s: %w", ErrNotCheckout, path, err)
	}

	if err != nil {
		return Checkout{}, err
	}

	lines := strings.Split(out, "\n")
	if lines[0] != "true" {
		return Checkout{}, fmt.Errorf("%w: %s", ErrNotCheckout, path)
	}

	// With --quiet, symbolic-ref exits 1 and prints nothing on a detached HEAD, and it still
	// names a branch that has no commit yet. Any other failure is a real one.
	branch, err := g.query(ctx, "-C", path, "symbolic-ref", "--short", "--quiet", "HEAD")
	if err != nil && (!errors.As(err, &exit) || exit.ExitCode() != 1) {
		return Checkout{}, fmt.Errorf("reading HEAD in %s: %w", path, err)
	}

	return Checkout{CommonDir: filepath.FromSlash(lines[1]), Branch: branch, ObjectFormat: lines[2]}, nil
}

// MirrorPath is where the mirror of the container catalogID lives. It is inside the Git
// directory, so `git status` never shows it.
func (c Checkout) MirrorPath(catalogID string) string {
	return filepath.Join(c.CommonDir, "dr-sync", catalogID+".git")
}

// Config is the whole config file of a mirror. The trust check compares the file with this
// text byte for byte, so changing it is a spec change.
func Config(objectFormat string) string {
	if objectFormat == SHA256 {
		return "[core]\n\trepositoryformatversion = 1\n\tbare = true\n" +
			"[extensions]\n\tobjectformat = sha256\n[receive]\n\tautoGc = false\n"
	}

	return "[core]\n\trepositoryformatversion = 0\n\tbare = true\n[receive]\n\tautoGc = false\n"
}

// Create makes an empty mirror whose HEAD points at branch. The caller has checked that the
// pull left the directory empty.
func (g *Git) Create(ctx context.Context, mirror, branch, objectFormat string) error {
	cmd := g.command(ctx, "init", "--quiet", "--bare", "--template=",
		"--initial-branch="+branch, "--object-format="+objectFormat, mirror)
	cmd.Stdout, cmd.Stderr = g.Stdout, g.Stderr
	// The fixed config names no ref format, so the mirror must use the files one even where
	// reftable is the default. The variable wins over init.defaultRefFormat and over an
	// inherited copy of itself, because os/exec passes on the last duplicate. A Git too old to
	// know it has only the files format.
	cmd.Env = append(cmd.Env, "GIT_DEFAULT_REF_FORMAT=files")

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("creating the mirror %s: %w", mirror, err)
	}

	return os.WriteFile(filepath.Join(mirror, "config"), []byte(Config(objectFormat)), 0o600)
}

// query runs git and returns its trimmed standard output. A failure unwraps to the
// *exec.ExitError, and its text carries what git printed.
func (g *Git) query(ctx context.Context, args ...string) (string, error) {
	out, err := g.command(ctx, args...).Output()

	var exit *exec.ExitError
	if errors.As(err, &exit) {
		err = fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exit.Stderr)))
	}

	return strings.TrimSpace(string(out)), err
}

// inheritedEnv is what an enclosing Git process, such as a hook, exports to name its own
// repository. Every command here names its repository itself, so these would point it at
// another one or put the mirror's objects outside the mirror. GIT_REFERENCE_BACKEND forces
// a ref format on a repository that has another, and it outranks GIT_DEFAULT_REF_FORMAT.
var inheritedEnv = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_REFERENCE_BACKEND",
}

func (g *Git) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, g.bin, args...) //nolint:gosec // running git with the caller's arguments is the point
	cmd.Env = slices.DeleteFunc(os.Environ(), func(variable string) bool {
		name, _, _ := strings.Cut(variable, "=")

		return slices.Contains(inheritedEnv, name)
	})

	return cmd
}
