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

package syncer_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/datarobot-labs/datarobot-sync/internal/syncer"
)

// The operating system words this error differently: ENOTEMPTY on Unix, ERROR_DIR_NOT_EMPTY on
// Windows. The test takes the error from a real removal, so each platform's CI covers its own.
func TestIsNotEmptyRecognizesTheErrorOfRemovingANonEmptyDirectory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "held.txt"), []byte("x"), 0o600))

	assert.True(t, syncer.IsNotEmpty(os.Remove(dir)))
}

func TestIsNotEmptyRejectsOtherRemovalErrors(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "missing")

	assert.False(t, syncer.IsNotEmpty(os.Remove(missing)))
	assert.False(t, syncer.IsNotEmpty(nil))
}
