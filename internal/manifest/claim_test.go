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

func TestClaimNameRoundTrips(t *testing.T) {
	t.Parallel()

	name := ClaimName(DefaultMetaPrefix, 42)
	assert.Equal(t, ".dr-sync/claims/0000000042.claim", name)

	generation, ok := ParseClaimObjectName(DefaultMetaPrefix, name)
	require.True(t, ok)
	assert.EqualValues(t, 42, generation)
}

func TestClaimNameHonorsCustomPrefix(t *testing.T) {
	t.Parallel()

	name := ClaimName(".sync-meta/", 7)
	assert.Equal(t, ".sync-meta/claims/0000000007.claim", name)

	_, ok := ParseClaimObjectName(DefaultMetaPrefix, name)
	assert.False(t, ok, "a claim under another prefix must not parse")
}

func TestClaimNamesSortLikeGenerations(t *testing.T) {
	t.Parallel()

	assert.Less(t, ClaimName(DefaultMetaPrefix, 9), ClaimName(DefaultMetaPrefix, 10))
}

func TestParseClaimObjectNameRejectsStrays(t *testing.T) {
	t.Parallel()

	strays := []string{
		".dr-sync/claims/42.claim",
		".dr-sync/claims/0000000042.json",
		".dr-sync/claims/0000000042",
		".dr-sync/claims/xxxxxxxxxx.claim",
		".dr-sync/claims/-000000042.claim",
		".dr-sync/claims/nested/0000000042.claim",
		".dr-sync/claims/00000000042.claim",
		".dr-sync/manifests/0000000042.claim",
		".dr-sync/claims/",
		"",
	}

	for _, name := range strays {
		_, ok := ParseClaimObjectName(DefaultMetaPrefix, name)
		assert.False(t, ok, "%q must not parse as a claim", name)
	}
}
