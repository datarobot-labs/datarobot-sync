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

// claimDigits is the zero-padded generation width, the same as in a manifest id.
const claimDigits = 10

const claimSuffix = ".claim"

// ClaimsPrefix is the directory holding head claims, with a trailing slash.
func ClaimsPrefix(metaPrefix string) string {
	return path.Join(strings.TrimSuffix(metaPrefix, "/"), claimsDir) + "/"
}

// ClaimName returns the container path of the claim for a generation.
func ClaimName(metaPrefix string, generation int64) string {
	return fmt.Sprintf("%s%0*d%s", ClaimsPrefix(metaPrefix), claimDigits, generation, claimSuffix)
}

// ParseClaimObjectName reads the generation from a claim object name and reports false for any
// other object.
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
