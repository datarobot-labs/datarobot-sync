+++
title = 'Concurrency and garbage collection'
weight = 8
+++

## Concurrency: what is guaranteed, what is not

Two clients pushing to one container is guarded. datarobot-sync takes an advisory lease,
revalidates immediately before committing, and writes a head claim in the same atomic
commit as the manifest. Two pushes that plan the same generation collide on the claim:
one lands, and the other exits with code 4 and changes nothing readers can see. Re-run
the push to plan against the new head.

Five layers, cheapest first: immutable generation-named objects (structural, so no
interleaving can produce a dangling or corrupt state), advisory leases (prevents the
common case), pre-commit revalidation (shrinks the window), the head claim (closes it),
and post-commit fork detection (names both manifest IDs and both writers, and never GCs
when a fork is seen).

{{< callout type="warning" >}}
**Lost update.** The head claim protects writers that write claims. A writer from before
claims existed can still commit the same generation as another writer, and the
lexicographic max wins. The loser's packs and manifest both survive intact, but its
paths are not in the head, so the next puller won't see them. datarobot-sync reports the
fork after the fact. Re-running upload heals it: it re-classifies against the new head and
reuses the already-uploaded packs.
{{< /callout >}}

{{< callout type="warning" >}}
**Stale tree.** The claim stops two writers that plan the same generation. It does not stop
a tree that is older than the head. The stale files look like local changes, so a push
overwrites a teammate's newer version in the new manifest. Pass `--expect-head <id>`, the
manifest ID the last `download` printed, and the push exits with code 4 when the container
has moved on. `--force` does not override it. [`run`]({{% relref "docs/run" %}}) does both around a
command, under a lease. Pull with `--backup` to keep your own edits:
every local file the pull replaces or removes is moved to `.dr-sync-backup/<timestamp>/` in
the sync root, and datarobot-sync never uploads that directory.
{{< /callout >}}

A push that loses the race can leave orphan packs, because new packs commit in a stage
before the manifest. Tier-1 garbage collection removes them, and a re-run reuses them.

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
  push. `datarobot sync gc` runs tier-1 only — no local walk, fast, scriptable in CI.
- **Tier 2 (expensive):** `0 < liveRatio < --compact-threshold` (default 0.5) needs live
  members rewritten into a fresh pack. Never automatic. `datarobot sync compact <path>` runs
  from the local tree: after hashing, most live members are already present locally, so
  on a machine with a current checkout, compaction usually downloads nothing.

Compact verifies it is still the maximum generation before deleting anything — if
someone else committed concurrently, it aborts entirely rather than deleting packs a
newer manifest might still reference.

A resident `watch` session changes the *shape* of the garbage, not the mechanism: many
small pushes produce many mostly-live packs that tier-1's `liveRatio == 0` rule cannot
reclaim, so dead weight accrues over a long session and wants an occasional
`datarobot sync compact` — exactly what the compaction hint in `watch`'s own output suggests
when dead bytes exceed the threshold.
