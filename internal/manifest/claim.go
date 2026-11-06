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
	"fmt"
	"path"
	"strconv"
	"strings"
)

// claimDigits is the zero-padded width of a generation in a claim name, the same width the
// manifest id uses, so claims sort like manifests.
const claimDigits = 10

// claimSuffix ends every claim object name.
const claimSuffix = ".claim"

// ClaimsPrefix is the directory holding head claims, with a trailing slash.
//
// A claim is a small object named only by generation. It is written in the same atomic
// commit as the manifest for that generation, and a commit that names an existing object is
// rejected whole. Two writers that both plan the next generation therefore collide on the
// claim name and exactly one commit lands, which is the compare-and-swap that the unique
// manifest names cannot provide.
func ClaimsPrefix(metaPrefix string) string {
	return path.Join(strings.TrimSuffix(metaPrefix, "/"), claimsDir) + "/"
}

// ClaimName returns the container path of the claim for a generation.
func ClaimName(metaPrefix string, generation int64) string {
	return fmt.Sprintf("%s%0*d%s", ClaimsPrefix(metaPrefix), claimDigits, generation, claimSuffix)
}

// ParseClaimObjectName reads the generation from a claim object name. It reports false for
// anything that is not a claim under this prefix, so a stray object never reads as one.
func ParseClaimObjectName(metaPrefix, objectName string) (int64, bool) {
	prefix := ClaimsPrefix(metaPrefix)

	if !strings.HasPrefix(objectName, prefix) || !strings.HasSuffix(objectName, claimSuffix) {
		return 0, false
	}

	digits := strings.TrimSuffix(strings.TrimPrefix(objectName, prefix), claimSuffix)
	if len(digits) != claimDigits {
		return 0, false
	}

	generation, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || generation < 0 {
		return 0, false
	}

	return generation, true
}
