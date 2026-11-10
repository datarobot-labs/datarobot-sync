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

package syncer

import (
	"context"
	"slices"

	"github.com/datarobot-labs/datarobot-sync/internal/drapi"
	"github.com/datarobot-labs/datarobot-sync/internal/manifest"
)

// Claim is a head claim found in the container listing.
type Claim struct {
	ObjectName string
	Generation int64
}

// claimObject returns the claim object that goes into the same commit as next.
func claimObject(next *manifest.Manifest) Object {
	content := []byte(next.ID)

	// The name depends only on the generation, so two writers planning the same generation
	// collide. With overwrite=error, fromStage rejects the second commit whole, which makes the
	// claim a compare-and-swap on the head.
	return Object{
		Name:    manifest.ClaimName(next.Prefix(), next.Generation),
		Content: content,
		Size:    int64(len(content)),
	}
}

// resolveCommitError judges a failed commit by what the container holds afterwards. It returns:
//   - nil when next is the head: the commit landed and only its answer was lost, which the client
//     sees as a failed retry of a stage that no longer exists.
//   - a ConflictError when commitErr is a 409 and the head moved or the generation is claimed.
//   - commitErr unchanged for anything else, including a failed re-read.
func (s *Store) resolveCommitError(ctx context.Context, commitErr error, expectedHeadID string, next *manifest.Manifest) error {
	state, err := s.ReadState(ctx)
	if err != nil {
		return commitErr
	}

	head, ok := manifest.SelectHead(state.Manifests)
	if !ok {
		return commitErr
	}

	if head.ID == next.ID {
		return nil
	}

	if !drapi.IsConflict(commitErr) {
		return commitErr
	}

	moved := head.ID != expectedHeadID
	claimed := slices.ContainsFunc(state.Claims, func(claim Claim) bool {
		return claim.Generation == next.Generation
	})

	// A 409 can also mean a pack name another writer uploaded first, which is not a lost update.
	if !moved && !claimed {
		return commitErr
	}

	return &ConflictError{Expected: expectedHeadID, Actual: head.ID}
}
