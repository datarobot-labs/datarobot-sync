// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

package syncer

import (
	"context"
	"strings"
	"time"

	"github.com/datarobot/golang-datarobot-filesync/internal/manifest"
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
