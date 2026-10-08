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

package packer

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/datarobot/golang-datarobot-filesync/internal/manifest"
)

func blob(path string, size int64) Blob {
	return Blob{SHA256: path, Size: size, SourcePath: path}
}

func TestPlanGroupsUnderInputCap(t *testing.T) {
	t.Parallel()

	packs := Plan([]Blob{
		blob("a.txt", 40),
		blob("b.txt", 40),
		blob("c.txt", 40),
	}, PlanOptions{PackInputMax: 100, BigFileThreshold: 1000, Level: manifest.DefaultLevel})

	require.Len(t, packs, 2)
	assert.EqualValues(t, 80, packs[0].InputBytes)
	assert.EqualValues(t, 40, packs[1].InputBytes)
}

// A big file in a shared pack would drag every neighbour across the wire whenever it changed,
// and would be dragged along whenever they did.
func TestPlanIsolatesBigFiles(t *testing.T) {
	t.Parallel()

	packs := Plan([]Blob{
		blob("small1.txt", 10),
		blob("huge.bin", 5000),
		blob("small2.txt", 10),
	}, PlanOptions{PackInputMax: 1000, BigFileThreshold: 1000, Level: manifest.DefaultLevel})

	var solo *PlannedPack

	for i := range packs {
		if len(packs[i].Blobs) == 1 && packs[i].Blobs[0].SourcePath == "huge.bin" {
			solo = &packs[i]
		}
	}

	require.NotNil(t, solo, "a big file must get a pack to itself")
	assert.EqualValues(t, 5000, solo.InputBytes)

	// A blob larger than the input cap must still be packable at all.
	packs = Plan([]Blob{blob("enormous.bin", 10_000)},
		PlanOptions{PackInputMax: 100, BigFileThreshold: 1000, Level: manifest.DefaultLevel})
	require.Len(t, packs, 1)
	assert.Len(t, packs[0].Blobs, 1)
}

// Extension-major ordering is the cheapest good proxy for the content similarity LZMA's
// window rewards.
func TestPlanOrdersByExtensionThenDirectory(t *testing.T) {
	t.Parallel()

	packs := Plan([]Blob{
		blob("z/one.go", 1),
		blob("a/two.txt", 1),
		blob("a/one.go", 1),
		blob("z/two.txt", 1),
	}, PlanOptions{PackInputMax: 1000, BigFileThreshold: 1000, Level: manifest.DefaultLevel})

	require.Len(t, packs, 1)

	order := make([]string, 0, 4)
	for _, b := range packs[0].Blobs {
		order = append(order, b.SourcePath)
	}

	assert.Equal(t, []string{"a/one.go", "z/one.go", "a/two.txt", "z/two.txt"}, order)
}

func TestPlanIsStableForIdenticalPaths(t *testing.T) {
	t.Parallel()

	blobs := []Blob{
		{SHA256: "bbbb", Size: 1, SourcePath: "same.txt"},
		{SHA256: "aaaa", Size: 1, SourcePath: "same.txt"},
	}

	first := Plan(blobs, PlanOptions{Level: manifest.DefaultLevel})
	second := Plan([]Blob{blobs[1], blobs[0]}, PlanOptions{Level: manifest.DefaultLevel})

	// The order determines the pack's bytes and therefore its name, so it must not depend on
	// input order.
	assert.Equal(t, first[0].Blobs, second[0].Blobs)
	assert.Equal(t, "aaaa", first[0].Blobs[0].SHA256)
}

func TestChooseCodecSkipsCompressionForIncompressibleCorpora(t *testing.T) {
	t.Parallel()

	photos := Plan([]Blob{
		blob("a.jpg", 900),
		blob("b.png", 900),
		blob("notes.txt", 10),
	}, PlanOptions{PackInputMax: 1 << 30, BigFileThreshold: 1 << 30, Level: manifest.DefaultLevel})

	require.Len(t, photos, 1)
	assert.Equal(t, manifest.CodecStore, photos[0].Codec,
		"a pack that is almost all already-compressed data is not worth recompressing")

	// A meaningful share of compressible content tips it back.
	mixed := Plan([]Blob{
		blob("a.jpg", 500),
		blob("notes.txt", 500),
	}, PlanOptions{PackInputMax: 1 << 30, BigFileThreshold: 1 << 30, Level: manifest.DefaultLevel})

	require.Len(t, mixed, 1)
	assert.Equal(t, manifest.CodecXZ, mixed[0].Codec)
}

func TestLevelZeroForcesStore(t *testing.T) {
	t.Parallel()

	packs := Plan([]Blob{blob("notes.txt", 100)}, PlanOptions{Level: 0})

	require.Len(t, packs, 1)
	assert.Equal(t, manifest.CodecStore, packs[0].Codec)
}

func TestPlanEmptyInput(t *testing.T) {
	t.Parallel()

	assert.Empty(t, Plan(nil, PlanOptions{}))
}

func TestIsIncompressible(t *testing.T) {
	t.Parallel()

	assert.True(t, IsIncompressible("photo.JPG"), "the check is case-insensitive")
	assert.True(t, IsIncompressible("a/b/archive.tar.gz"))
	assert.False(t, IsIncompressible("main.go"))
	assert.False(t, IsIncompressible("no-extension"))
}
