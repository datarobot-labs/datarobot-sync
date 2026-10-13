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
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/datarobot-labs/datarobot-sync/internal/manifest"
)

// DefaultKeepGenerations is how many manifests an upload retains.
//
// Retention is the real garbage dial: an old manifest pins every pack it references, so a
// large keep count on a churning tree keeps a lot of dead bytes alive. Five is enough to roll
// back a bad push without hoarding.
const DefaultKeepGenerations = 5

// DefaultGCGrace is how long an unreferenced object is left alone before it is deleted.
//
// It protects a client that has uploaded its packs but has not committed its manifest yet:
// without the grace window, a concurrent garbage collection could delete the packs out from
// under it and leave it committing a manifest that points at nothing.
const DefaultGCGrace = 6 * time.Hour

// CollectOptions bounds a garbage collection pass.
type CollectOptions struct {
	KeepGenerations int
	Grace           time.Duration
	// Now is the reference time; injected so the grace window is testable.
	Now time.Time
	// DryRun reports what would be deleted without deleting it.
	DryRun bool
}

// CollectPlan is what a pass would remove.
type CollectPlan struct {
	Manifests []string
	Packs     []string
	Leases    []string
	// Writers is stale entries from the resident-writer registry. They sit outside the
	// pack/manifest reference graph, so nothing else will ever reclaim them.
	Writers []string
	// Retained is the manifests that will survive, newest first.
	Retained []manifest.Ref
	Bytes    int64
}

// Names returns every object the plan would delete.
func (p *CollectPlan) Names() []string {
	names := make([]string, 0, len(p.Manifests)+len(p.Packs)+len(p.Leases)+len(p.Writers))

	// Manifests first. The ordering invariant is that a pack may only be deleted after the
	// last manifest referencing it is gone, so a pass that is interrupted halfway leaves
	// packs without a manifest rather than a manifest without its packs. Leases and writers
	// are both advisory and unreferenced by anything, so their order relative to the rest
	// does not matter.
	names = append(names, p.Manifests...)
	names = append(names, p.Packs...)
	names = append(names, p.Leases...)
	names = append(names, p.Writers...)

	return names
}

// Empty reports whether there is nothing to do.
func (p *CollectPlan) Empty() bool {
	return len(p.Manifests) == 0 && len(p.Packs) == 0 && len(p.Leases) == 0 && len(p.Writers) == 0
}

// PlanCollect decides what can be removed, given the current listing.
//
// Only two things are ever deleted: manifests beyond the retention count, and objects no
// retained manifest references. Nothing is rewritten, so this pass moves no data and can run
// after every push.
func PlanCollect(ctx context.Context, store *Store, state *State, opts CollectOptions) (*CollectPlan, error) {
	keep := opts.KeepGenerations
	if keep <= 0 {
		keep = DefaultKeepGenerations
	}

	grace := opts.Grace
	if grace <= 0 {
		grace = DefaultGCGrace
	}

	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}

	retained, prunable := manifest.Retain(state.Manifests, keep)

	plan := &CollectPlan{Retained: retained}

	for _, ref := range prunable {
		// The grace window applies to manifests as well as packs. A reader that has just
		// selected a manifest and started fetching from it should not have it deleted out
		// from under them, and a manifest costs about a megabyte to keep.
		if entry, ok := state.Objects[ref.ObjectName]; ok && entry.CreatedAt != nil &&
			now.Sub(*entry.CreatedAt) < grace {
			plan.Retained = append(plan.Retained, ref)

			continue
		}

		plan.Manifests = append(plan.Manifests, ref.ObjectName)
	}

	// Anything still standing pins the packs it references, whether it was retained by count
	// or reprieved by the grace window.
	referenced, err := referencedObjects(ctx, store, plan.Retained)
	if err != nil {
		return nil, err
	}

	collectUnreferencedPacks(plan, store, state, referenced, now, grace)

	plan.Leases = StaleLeaseNames(state.Leases, now)
	plan.Writers = StaleWriterNames(state.Writers, now)

	sort.Strings(plan.Manifests)
	sort.Strings(plan.Packs)

	return plan, nil
}

// collectUnreferencedPacks adds pack objects no retained manifest points at.
func collectUnreferencedPacks(plan *CollectPlan, store *Store, state *State, referenced map[string]bool, now time.Time, grace time.Duration) {
	packsPrefix := manifest.PacksPrefix(store.MetaPrefix())

	for name, entry := range state.Objects {
		if !strings.HasPrefix(name, packsPrefix) || referenced[name] {
			continue
		}

		// An object younger than the grace window may belong to a push that has uploaded its
		// packs but has not committed its manifest yet. Deleting it would leave that client
		// committing a manifest that points at nothing.
		if entry.CreatedAt != nil && now.Sub(*entry.CreatedAt) < grace {
			continue
		}

		plan.Packs = append(plan.Packs, name)
		plan.Bytes += entry.Size
	}
}

// referencedObjects collects every object name the retained manifests point at.
func referencedObjects(ctx context.Context, store *Store, retained []manifest.Ref) (map[string]bool, error) {
	referenced := map[string]bool{}

	for _, ref := range retained {
		loaded, err := store.LoadManifest(ctx, ref)
		if err != nil {
			// Failing closed is the only safe option: if a retained manifest cannot be read,
			// its packs must be assumed live rather than deleted.
			return nil, fmt.Errorf("cannot determine what %s references, so nothing will be deleted: %w", ref.ID, err)
		}

		referenced[ref.ObjectName] = true

		for _, pack := range loaded.Packs {
			for _, slice := range pack.Slices {
				referenced[slice.FileName] = true
			}
		}
	}

	return referenced, nil
}

// Collect removes what PlanCollect identifies, returning how many objects were deleted.
func Collect(ctx context.Context, store *Store, state *State, opts CollectOptions) (int, error) {
	plan, err := PlanCollect(ctx, store, state, opts)
	if err != nil {
		return 0, err
	}

	if plan.Empty() || opts.DryRun {
		return 0, nil
	}

	return store.Delete(ctx, plan.Names())
}
