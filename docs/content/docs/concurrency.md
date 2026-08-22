+++
title = 'Concurrency and Garbage Collection'
weight = 8
+++

## Concurrency: what is guaranteed, what is not

Two clients pushing to one container is guarded, not prevented: the API has no
compare-and-swap. `dr-sync` takes an advisory lease, revalidates immediately before
committing, and reports a fork afterwards if one happened. A genuine simultaneous push
means one side's update is not in the head — detected, never silently corrupted, and
healed by re-running.

Four layers, cheapest first: immutable generation-named objects (structural — no
interleaving can produce a dangling or corrupt state), advisory leases (prevents the
common case), pre-commit revalidation (shrinks the window), post-commit fork detection
(names both manifest IDs and both writers, and never GCs when a fork is seen).

{{< callout type="warning" >}}
**Lost update.** Two writers both commit generation N+1; lexicographic max wins. The
loser's packs and manifest both survive intact — nothing is corrupted — but its paths
are not in the head, so the next puller won't see them. Detected post-commit; healed by
re-running upload, which re-classifies against the new head and reuses the
already-uploaded packs. Unfixable without a compare-and-swap primitive.
{{< /callout >}}

`--force` overrides an advisory lease when the holder is known to be gone. A resident
`watch` process additionally announces itself in `.dr-sync/writers/` — see
[Watch]({{% relref "docs/watch" %}}) — so a peer can tell it is active, and holds back
`--delete` for a cycle rather than racing another writer's paths.

## Garbage and compaction

Per pack, `liveBytes` is the sum of member sizes still referenced by the current
manifest; `liveRatio` is that over the pack's uncompressed size. Both are computable
from the manifest alone, with no network call.

- **Tier 1 (free):** `liveRatio == 0` → deletable with no data movement. Runs
  automatically after every successful commit, subject to a grace period and no live
  foreign lease. Handles most garbage, because pack contents correlate with a single
  push. `dr-sync gc` runs tier-1 only — no local walk, fast, scriptable in CI.
- **Tier 2 (expensive):** `0 < liveRatio < --compact-threshold` (default 0.5) needs live
  members rewritten into a fresh pack. Never automatic. `dr-sync compact <path>` runs
  from the local tree: after hashing, most live members are already present locally, so
  on a machine with a current checkout, compaction usually downloads nothing.

Compact verifies it is still the maximum generation before deleting anything — if
someone else committed concurrently, it aborts entirely rather than deleting packs a
newer manifest might still reference.

A resident `watch` session changes the *shape* of the garbage, not the mechanism: many
small pushes produce many mostly-live packs that tier-1's `liveRatio == 0` rule cannot
reclaim, so dead weight accrues over a long session and wants an occasional
`dr-sync compact` — exactly what the compaction hint in `watch`'s own output suggests
when dead bytes exceed the threshold.
