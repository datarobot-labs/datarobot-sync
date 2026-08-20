// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

// Package cli holds the subcommand dispatcher, shared flag plumbing, and the exit-code
// contract described in SPEC.md.
package cli

import (
	"errors"
	"fmt"
)

// Exit codes. These are part of the tool's contract with CI, so they are append-only:
// never renumber one.
const (
	CodeOK          = 0   // success, including "nothing to do"
	CodeInternal    = 1   // unexpected failure
	CodeUsage       = 2   // bad flags, missing config, failed precondition
	CodeAPI         = 3   // API or authentication failure
	CodeConflict    = 4   // foreign lease, generation moved, or a fork was detected
	CodeIntegrity   = 5   // checksum mismatch, missing pack, unsupported formatVersion
	CodePartial     = 6   // some files transferred, some failed
	CodeInterrupted = 130 // SIGINT/SIGTERM
)

// ExitError carries the exit code a failure should produce alongside its message.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string {
	return e.Err.Error()
}

func (e *ExitError) Unwrap() error {
	return e.Err
}

// Fail wraps err with an exit code. A nil err yields nil so callers can pass through.
func Fail(code int, err error) error {
	if err == nil {
		return nil
	}

	return &ExitError{Code: code, Err: err}
}

// Failf builds a coded error from a format string.
func Failf(code int, format string, args ...any) error {
	return &ExitError{Code: code, Err: fmt.Errorf(format, args...)}
}

// CodeFor reports the exit code for err, defaulting to CodeInternal for anything that did
// not declare one. A nil error is CodeOK.
func CodeFor(err error) int {
	if err == nil {
		return CodeOK
	}

	var exitErr *ExitError
	if errors.As(err, &exitErr) {
		return exitErr.Code
	}

	return CodeInternal
}
