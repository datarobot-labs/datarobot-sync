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
	"sort"
	"strconv"
	"strings"
)

// Ref names one manifest object in the container.
type Ref struct {
	ID         string
	ObjectName string
	Generation int64
	Size       int64
	Checksum   string
}

// ParseManifestObjectName recognizes a manifest object and extracts its id and generation.
// A name that does not match the pattern is not a manifest and is reported as such, so
// unrelated objects under the prefix are ignored rather than misread.
func ParseManifestObjectName(metaPrefix, objectName string) (Ref, bool) {
	const suffix = ".json.xz"

	prefix := ManifestsPrefix(metaPrefix)

	// Both affixes are required, not merely trimmed: TrimSuffix is a no-op when absent, so
	// a stray ".json" object would otherwise parse as a manifest and could win the head
	// election.
	if !strings.HasPrefix(objectName, prefix) || !strings.HasSuffix(objectName, suffix) {
		return Ref{}, false
	}

	id := strings.TrimSuffix(strings.TrimPrefix(objectName, prefix), suffix)
	if id == "" || strings.Contains(id, "/") {
		return Ref{}, false
	}

	generation, ok := generationOf(id)
	if !ok {
		return Ref{}, false
	}

	return Ref{ID: id, ObjectName: objectName, Generation: generation}, true
}

// generationOf reads the numeric prefix of a manifest id.
func generationOf(id string) (int64, bool) {
	digits, _, found := strings.Cut(id, "-")
	if !found || len(digits) != 10 {
		return 0, false
	}

	generation, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || generation < 0 {
		return 0, false
	}

	return generation, true
}

// SelectHead picks the current manifest from the objects found under the manifests prefix.
//
// The winner is the lexicographic maximum, which is why the generation is zero-padded. Two
// writers landing on the same generation both survive as objects; one wins deterministically
// and the loser is detectable rather than lost silently.
func SelectHead(refs []Ref) (Ref, bool) {
	if len(refs) == 0 {
		return Ref{}, false
	}

	sorted := make([]Ref, len(refs))
	copy(sorted, refs)

	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].ID < sorted[j].ID
	})

	return sorted[len(sorted)-1], true
}

// Fork describes two or more manifests written at the same generation, meaning two clients
// pushed concurrently. Only one is the head; the others' paths are not visible to a puller.
type Fork struct {
	Generation int64
	Refs       []Ref
}

// DetectFork reports the highest generation that has more than one manifest.
//
// This is not a race that can be prevented — the API has no compare-and-swap — so it is
// detected and reported instead, and it suppresses garbage collection: the losing manifest
// may be the only reference to packs that are about to look unreferenced.
func DetectFork(refs []Ref) (Fork, bool) {
	byGeneration := map[int64][]Ref{}

	for _, ref := range refs {
		byGeneration[ref.Generation] = append(byGeneration[ref.Generation], ref)
	}

	best := Fork{}

	for generation, group := range byGeneration {
		if len(group) < 2 || generation < best.Generation {
			continue
		}

		sort.Slice(group, func(i, j int) bool { return group[i].ID < group[j].ID })
		best = Fork{Generation: generation, Refs: group}
	}

	return best, len(best.Refs) > 1
}

// Retain returns the manifest refs to keep and the ones to prune, newest first.
//
// Retention is the real garbage dial: keeping an old manifest pins every pack it references,
// so a large keep count on a churning tree keeps a lot of dead bytes alive.
func Retain(refs []Ref, keep int) (retained, prunable []Ref) {
	if keep < 1 {
		keep = 1
	}

	sorted := make([]Ref, len(refs))
	copy(sorted, refs)

	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].ID > sorted[j].ID
	})

	if len(sorted) <= keep {
		return sorted, nil
	}

	return sorted[:keep], sorted[keep:]
}

// NextGeneration returns the generation a child of this manifest should carry.
func (m *Manifest) NextGeneration() int64 {
	if m == nil {
		return 0
	}

	return m.Generation + 1
}

// Child returns an empty manifest that will succeed m: same prefix and compression settings,
// next generation, and a parent pointer so a fork can be recognized later.
func (m *Manifest) Child() *Manifest {
	child := New(m.Prefix())
	child.Generation = m.NextGeneration()
	child.ParentManifestID = m.ID
	child.Compression = m.Compression

	return child
}

// Describe renders a one-line summary for progress output.
func (m *Manifest) Describe() string {
	if m == nil {
		return "no manifest"
	}

	return fmt.Sprintf("generation %d  %d files  %s  %d packs",
		m.Generation, m.Stats.FileCount, HumanBytes(m.Stats.TotalSize), m.Stats.PackCount)
}

// HumanBytes renders a byte count in binary units.
func HumanBytes(n int64) string {
	const unit = 1024

	if n < unit {
		return fmt.Sprintf("%d B", n)
	}

	value := float64(n)
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}

	for _, suffix := range units {
		value /= unit

		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}

	return fmt.Sprintf("%.1f EiB", value/unit)
}
