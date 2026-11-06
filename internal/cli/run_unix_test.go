//go:build !windows

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
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Only this process receives a SIGTERM sent to it, so it has to reach the command.
func TestExecCommandForwardsSIGTERMToTheCommand(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	argv := helperCommand(t, 17)

	t.Setenv(helperReadyEnv, ready)

	done := make(chan error, 1)

	go func() { done <- execCommand(argv, "/synced")(context.Background()) }()

	require.Eventually(t, func() bool { _, err := os.Stat(ready); return err == nil },
		10*time.Second, 20*time.Millisecond, "the command never started")

	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGTERM))

	var exited *exec.ExitError

	select {
	case err := <-done:
		require.ErrorAs(t, err, &exited)
		assert.Equal(t, 17, exited.ExitCode(), "the command must have received the SIGTERM and exited itself")
	case <-time.After(10 * time.Second):
		t.Fatal("the command did not exit after SIGTERM")
	}
}
