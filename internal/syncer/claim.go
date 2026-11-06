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

// claimObject returns the claim that goes into the same commit as a manifest.
//
// The claim name depends only on the generation, so two writers that plan the same next
// generation stage the same name. `fromStage` rejects a commit that names an existing object
// and applies none of it, which makes the claim the compare-and-swap on the head: exactly one
// of the two commits lands, and the loser's manifest never becomes visible.
func claimObject(next *manifest.Manifest) Object {
	content := []byte(next.ID)

	return Object{
		Name:    manifest.ClaimName(next.Prefix(), next.Generation),
		Content: content,
		Size:    int64(len(content)),
	}
}

// conflictFromCommit turns a rejected manifest commit into a ConflictError when another writer
// got there first.
//
// A 409 can also come from a pack name that a concurrent writer uploaded first, which is not
// a lost update, so the container is read again and the error is only converted when the head
// moved or the generation is already claimed. Anything else is returned as it came.
func (s *Store) conflictFromCommit(ctx context.Context, commitErr error, expectedHeadID string, generation int64) error {
	if !drapi.IsConflict(commitErr) {
		return commitErr
	}

	state, err := s.ReadState(ctx)
	if err != nil {
		return commitErr
	}

	head, _ := manifest.SelectHead(state.Manifests)
	moved := head.ID != expectedHeadID
	claimed := slices.ContainsFunc(state.Claims, func(claim Claim) bool {
		return claim.Generation == generation
	})

	if !moved && !claimed {
		return commitErr
	}

	return &ConflictError{Expected: expectedHeadID, Actual: head.ID}
}
