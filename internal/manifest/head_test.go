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

package manifest

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseManifestObjectName(t *testing.T) {
	t.Parallel()

	ref, ok := ParseManifestObjectName(DefaultMetaPrefix,
		".dr-sync/manifests/0000000042-7f3a9c2b.json.xz")
	require.True(t, ok)
	assert.Equal(t, "0000000042-7f3a9c2b", ref.ID)
	assert.EqualValues(t, 42, ref.Generation)

	// Anything that is not a manifest object must be ignored rather than misread, so an
	// unrelated file under the prefix cannot become the head.
	notManifests := []string{
		".dr-sync/packs/p_abc.tar.xz",
		".dr-sync/manifests/README.txt",
		".dr-sync/manifests/nested/0000000001-aaaaaaaa.json.xz",
		".dr-sync/manifests/42-7f3a9c2b.json.xz",
		".dr-sync/manifests/xxxxxxxxxx-7f3a9c2b.json.xz",
		".dr-sync/manifests/0000000042-7f3a9c2b.json",
		"manifests/0000000042-7f3a9c2b.json.xz",
	}

	for _, name := range notManifests {
		_, ok := ParseManifestObjectName(DefaultMetaPrefix, name)
		assert.False(t, ok, "%q must not parse as a manifest", name)
	}
}

// The head is derived from the listing, so generation 10 must beat generation 9 — which is
// only true because the number is zero-padded.
func TestSelectHeadIsHighestGeneration(t *testing.T) {
	t.Parallel()

	refs := []Ref{
		{ID: "0000000009-ffffffff", Generation: 9},
		{ID: "0000000010-00000000", Generation: 10},
		{ID: "0000000002-aaaaaaaa", Generation: 2},
	}

	head, ok := SelectHead(refs)
	require.True(t, ok)
	assert.Equal(t, "0000000010-00000000", head.ID)

	_, ok = SelectHead(nil)
	assert.False(t, ok, "an empty container has no head")
}

// Two writers at the same generation both persist; the tie breaks deterministically on the
// random suffix so every reader agrees which one is the head.
func TestSelectHeadBreaksTiesDeterministically(t *testing.T) {
	t.Parallel()

	refs := []Ref{
		{ID: "0000000007-bbbbbbbb", Generation: 7},
		{ID: "0000000007-aaaaaaaa", Generation: 7},
	}

	head, ok := SelectHead(refs)
	require.True(t, ok)
	assert.Equal(t, "0000000007-bbbbbbbb", head.ID)

	reversed, ok := SelectHead([]Ref{refs[1], refs[0]})
	require.True(t, ok)
	assert.Equal(t, head.ID, reversed.ID, "the winner must not depend on listing order")
}

func TestDetectFork(t *testing.T) {
	t.Parallel()

	_, forked := DetectFork([]Ref{
		{ID: "0000000001-aaaaaaaa", Generation: 1},
		{ID: "0000000002-bbbbbbbb", Generation: 2},
	})
	assert.False(t, forked, "a linear history is not a fork")

	fork, forked := DetectFork([]Ref{
		{ID: "0000000001-aaaaaaaa", Generation: 1},
		{ID: "0000000002-bbbbbbbb", Generation: 2},
		{ID: "0000000002-cccccccc", Generation: 2},
	})
	require.True(t, forked)
	assert.EqualValues(t, 2, fork.Generation)
	assert.Len(t, fork.Refs, 2)
	assert.Equal(t, "0000000002-bbbbbbbb", fork.Refs[0].ID, "fork refs are reported in a stable order")
}

func TestRetainKeepsNewest(t *testing.T) {
	t.Parallel()

	refs := []Ref{
		{ID: "0000000001-aaaaaaaa", Generation: 1},
		{ID: "0000000003-cccccccc", Generation: 3},
		{ID: "0000000002-bbbbbbbb", Generation: 2},
	}

	retained, prunable := Retain(refs, 2)

	require.Len(t, retained, 2)
	assert.Equal(t, "0000000003-cccccccc", retained[0].ID)
	assert.Equal(t, "0000000002-bbbbbbbb", retained[1].ID)

	require.Len(t, prunable, 1)
	assert.Equal(t, "0000000001-aaaaaaaa", prunable[0].ID)

	// Keeping fewer than one manifest would leave the container unreadable.
	retained, prunable = Retain(refs, 0)
	assert.Len(t, retained, 1)
	assert.Len(t, prunable, 2)

	retained, prunable = Retain(refs, 10)
	assert.Len(t, retained, 3)
	assert.Empty(t, prunable)
}
