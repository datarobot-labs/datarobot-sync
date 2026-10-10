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
	"strings"
	"time"

	"github.com/datarobot-labs/datarobot-sync/internal/manifest"
)

// Inspection is a read-only report on a container's dr-sync state.
type Inspection struct {
	ContainerID string
	Head        *manifest.Manifest
	HeadRef     manifest.Ref
	Generations []manifest.Ref
	Leases      []Lease
	Garbage     manifest.GarbageReport
	Liveness    []manifest.PackLiveness
	Fork        *manifest.Fork
	// Orphans are pack objects the head does not reference. They are not necessarily
	// garbage — an older retained manifest may still need them.
	Orphans []string
	// UnreferencedBytes is the size of those orphans.
	UnreferencedBytes int64
}

// Inspect reports the state of a container without changing anything.
func Inspect(ctx context.Context, store *Store, threshold float64) (*Inspection, error) {
	state, err := store.ReadState(ctx)
	if err != nil {
		return nil, err
	}

	head, ref, err := store.Head(ctx, state)
	if err != nil {
		return nil, err
	}

	index := manifest.NewIndex(head)

	inspection := &Inspection{
		ContainerID: store.ContainerID(),
		Head:        head,
		HeadRef:     ref,
		Generations: state.Manifests,
		Leases:      state.Leases,
		Garbage:     index.Garbage(threshold),
		Liveness:    index.Liveness(),
	}

	if fork, forked := manifest.DetectFork(state.Manifests); forked {
		inspection.Fork = &fork
	}

	referenced := map[string]bool{}

	for _, pack := range head.Packs {
		for _, slice := range pack.Slices {
			referenced[slice.FileName] = true
		}
	}

	packsPrefix := manifest.PacksPrefix(store.MetaPrefix())

	for name, entry := range state.Objects {
		if referenced[name] || !strings.HasPrefix(name, packsPrefix) {
			continue
		}

		inspection.Orphans = append(inspection.Orphans, name)
		inspection.UnreferencedBytes += entry.Size
	}

	return inspection, nil
}

// ActiveLeases returns the leases that have not expired.
func (i *Inspection) ActiveLeases(now time.Time) []Lease {
	var active []Lease

	for _, lease := range i.Leases {
		if !lease.Expired(now) {
			active = append(active, lease)
		}
	}

	return active
}
