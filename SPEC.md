# dr-sync — specification

`dr-sync` is rsync for the DataRobot Files API: a single static Go binary that makes a local
directory and a DataRobot Files container match, transferring only the difference,
xz-compressed. It is also a `dr` CLI plugin, so it is equally `dr sync upload ./dir`.

This document is the source of truth for sync semantics, the on-container layout, and the
guarantees the tool does and does not make. Read it before changing any of those.

## Why

Moving a directory tree in or out of the DataRobot Files API today means either the Python
SDK's fsspec layer (per-file, uncompressed, needs a Python runtime) or hand-rolled `curl`.
Neither drops into CI, and both re-send bytes that have not changed.

### What the API actually gives us (verified live against app.datarobot.com)

- A "container" is a catalog item with `catalogType: files`, holding files at relative paths.
  There are no folder objects — directories are implied by `/` in the path.
- `GET files/{id}/allFiles/` returns `fileName` (full relative path), `fileSize`, and
  **`fileChecksum` = SHA-256 hex of the raw bytes**. Confirmed by downloading a file and
  hashing it: exact match. `prefix=` filtering works (the prefix must end in `/`); `limit`
  defaults to 100 and caps at 1000, so following `next` — an absolute URL, used verbatim —
  is mandatory.
- Downloads are presigned S3 URLs. `POST files/{id}/links/` returns them in batch (validity
  up to 3000s) and they honour `Range` (verified `206`).
- Uploads are multipart, one request per file, and each costs ~0.2 s even at 8-way
  parallelism. A "stage" (`stages/` → N× `stages/{id}/upload/` → `fromStage/`) batches them
  behind **one** commit, which is why packing rather than mirroring is the whole design.
- **DataRobot's intake will not unpack xz** — only zip, tar, tar.gz/tgz, tar.bz2, gz, bz2.
  Server-side extraction is off the table, and we never set `useArchiveContents`.

### Decisions taken

1. **Opaque packs, real xz.** The container holds only `.dr-sync/…` objects: immutable
   manifests plus `tar.xz` packs. The server stores them as opaque bytes; dr-sync does all
   tarring, xz-compressing and extraction locally. This is the only route to genuine LZMA2
   on the wire.
2. **No local state.** Local disk is not trusted between runs. All sync state lives in the
   remote manifest. Scratch temp files during a run are fine and disposable — correctness
   never depends on them surviving.
3. **Catalog id is explicit**: `--catalog-id` flag or `DR_SYNC_CATALOG_ID` env var, and a
   dedicated `dr-sync init` subcommand creates the container.
4. Shipped by goreleaser: per-platform binaries, a ghcr image via `dockers_v2`, and a `dr`
   plugin `.tar.xz` bundling every platform behind a wrapper script.

---

## Invariants

These are load-bearing. Breaking one is a data-loss or corruption bug, not a style choice.

- Nothing in the container is ever overwritten. Manifests are generation-named, packs are
  content-addressed. `overwrite` is `error`, not `replace`.
- A pack is uploaded before any manifest referencing it, and deleted only after the last
  manifest referencing it is gone.
- A manifest is deleted only after a newer one is verified to be the maximum.
- One `fromStage` commit per run wherever possible — it is the atomicity boundary; when
  split, the manifest commits last.
- Everything streams. No `io.ReadAll` and no `bytes.Buffer` on file content, ever.
- The `Authorization` header never reaches a presigned S3 URL.

---

## Verified API behaviour

Every behaviour the design depends on has been settled against a throwaway container
(`POST files/` → probes → `DELETE files/{id}/`, 2026-08-20, `app.datarobot.com`). All seven
open questions came back favourable; two of the answers changed the design and are called out
below.

| # | Question | Answer |
|---|---|---|
| V1 | Can a staged upload's multipart filename contain a leading-dot directory, and round-trip through `allFiles/`? | **Yes.** `.dr-sync/packs/p_x.tar.xz` stored and listed byte-identically. No `--meta-prefix` fallback needed. |
| V2 | Does `prefix=.dr-sync/manifests/` filter correctly (nested + dotted)? | **Yes.** Nested dotted prefixes filter exactly. A prefix **must end in `/`** or the API answers `Prefix must end with a forward slash "/"`. |
| V3 | Largest body `stages/{id}/upload/` accepts? | **≥ 256 MiB** (64 MiB and 256 MiB both `201`). The 64 MiB `--slice-size` default sits comfortably under it. Above 256 MiB untested. |
| V4 | Does `DELETE allFiles/` delete one nested dotted file, not the directory? | **Yes.** `{"numFilesDeleted": 1}`, siblings under the same directory untouched. |
| V5 | Max files per stage, and how `fromStage` latency scales? | 200 files in one stage committed in **0.355 s**. Latency is effectively constant in file count. |
| V6 | Is `fileChecksum` populated for files *we* upload via a stage, and how soon? | **Yes, immediately** on commit, and it equals SHA-256 of the raw bytes exactly. The free pre-flight integrity check is available. |
| V7 | Same name twice in one stage? Does `overwrite=error` fail the whole commit? | Duplicate *within* a stage: accepted, deduplicated, last wins. Duplicate against a name **already in the container**: `409 Duplicate file name(s): <name>` and **nothing from that stage lands**. |

Also established:

- **`fromStage` is synchronous.** It answers `200` with
  `{catalogId, catalogVersionId, numFiles}` and **no `Location` header** — for a 1-file stage
  and for a 200-file stage alike. Async `Location` polling is therefore a defensive fallback
  for large stages, not the main path, and the async design must not assume it fires.
- **`fromStage` is atomic.** On the `409` above, none of the stage's files landed, including
  the ones with no conflict. Partial application is not a case to handle.
- Per-file upload round-trips cost ~0.2 s each at 8-way parallelism (200 files in 43 s). This
  is the whole case for packing: 200 files in one pack is one request, not 200.
- `allFiles/` `limit` **caps at 1000** (higher → `value is greater than 1000`); the default is
  100, so pagination is mandatory, not optional.
- `fileCreatedAt` is server-side UTC, so it is safe to base GC grace on rather than a local
  clock.
- Sending an `Authorization` header alongside a presigned S3 URL returns **400**. Confirmed,
  not theoretical — hence invariant 6.
- **Presigned URLs carry an explicit `:443`**, and SigV4 signs the *canonical* host, which
  omits the scheme's default port. Go sends the `Host` header exactly as the URL spells it,
  so the port must be stripped before the request or S3 answers `SignatureDoesNotMatch`. curl
  strips it silently, which is why the URL appears to work by hand and nowhere else. No
  `httptest`-based test can catch this, because those always listen on a non-default port —
  it took a run against the live service.
- **Catalog tags cannot contain a dash.** `PATCH catalogItems/{id}/` answers
  `422 Tag cannot contain \`-\`` — undocumented, and found by having a tag rejected. Tags are
  validated before the container is created so the failure names the rule instead of leaving an
  orphan behind.
- `POST files/` returns `{catalogId, catalogVersionId}`; `POST files/{id}/stages/` returns
  `{catalogId, stageId}` and needs an empty body; `stages/{id}/upload/` returns `201`;
  `DELETE files/{id}/` returns `204` and later reads return `410 Gone` (soft delete).

### What these answers changed

0. **Presigned URLs need their default port stripped.** Without it every download fails with a
   signature mismatch, and no unit test can see it.
1. **Skipping already-present packs is mandatory, not an optimization.** Because
   `overwrite=error` rejects the entire commit on any pre-existing name, a resumed run that
   re-uploads a pack it already landed would fail the whole push. The content-addressed name
   plus the `.dr-sync/` listing is what makes the retry correct, not merely cheap.
2. **The async poller is a fallback, not the hot path.** `fromStage` returns synchronously,
   so the "one commit per run" invariant is about *atomicity*, not about amortizing a 5 s
   poll. Keep the poller for the case where a large stage does return a `Location`, but do
   not build the flow around it.

---

## Design

### Content-addressed packs, immutable manifests

Tar members are named `blobs/<sha256hex>`, not by path. The manifest maps
path → sha256 → pack. This buys: identical content stored once, renames and reverts that
transfer zero bytes, "did this change?" as a map lookup, and no path-traversal surface
inside the tar.

Nothing is ever overwritten. There is no mutable pointer object, because the API has no
compare-and-swap and a mutable head is exactly where lost updates become corruption. The
head is **derived**: list `.dr-sync/manifests/` and take the lexicographic max.

```
.dr-sync/README.txt                                  written once, explains the layout to
                                                     whoever finds this container in the UI
.dr-sync/manifests/0000000042-7f3a9c2b.json.xz       gen zero-padded to 10 → lexicographic
                                                     order == numeric order
.dr-sync/packs/p_9f2a1c8d3e4f5061.tar.xz             p_<sha256(blob)[0:16]>
.dr-sync/packs/p_1b2c3d4e5f607182.tar.xz.000         sliced when compressed size exceeds
.dr-sync/packs/p_1b2c3d4e5f607182.tar.xz.001         --slice-size
.dr-sync/locks/<clientId>-<seq>.lock                 advisory lease, 30 min TTL
.dr-sync/writers/<clientId>-<seq>.json               resident-writer registry (see Watch)
```

Manifest (xz-compressed JSON, camelCase to match the API wire format so `internal/drapi`
and `internal/manifest` share one convention):

```json
{
  "formatVersion": 1,
  "generation": 42,
  "manifestId": "0000000042-7f3a9c2b",
  "parentManifestId": "0000000041-11ab90ff",
  "generator": "dr-sync/0.1.0 go1.26.5 darwin/arm64 xz-go/v0.5.16",
  "createdAt": "2026-08-20T15:04:05.123456789Z",
  "createdBy": { "clientId": "a1b2c3d4e5f60718", "host": "…", "user": "…" },
  "observedCatalogVersionId": "68a1f0c9d2e3b4a5c6d7e8f9",
  "metaPrefix": ".dr-sync/",
  "pathEncoding": "nfc",
  "stats": { "fileCount": 12043, "uniqueBlobCount": 11890, "totalSize": 8123456789,
             "packCount": 31, "packCompressedBytes": 1402653184,
             "liveBytes": 8010000000, "deadBytes": 640000000 },
  "packs": [
    { "id": "p_9f2a1c8d3e4f5061", "codec": "xz",
      "slices": [ { "fileName": ".dr-sync/packs/p_9f2a1c8d3e4f5061.tar.xz",
                    "size": 67108864, "sha256": "…" } ],
      "compressedSize": 67108864, "uncompressedSize": 314572800, "sha256": "…",
      "members": [ { "sha256": "ab12…", "size": 1234 } ] }
  ],
  "entries": [
    { "path": "src/main.go", "type": "file", "size": 1234, "sha256": "ab12…",
      "mode": 420, "modTime": "2026-08-19T10:00:00.123456789Z", "pack": 0 },
    { "path": "src",         "type": "dir",     "mode": 493, "modTime": "…" },
    { "path": "docs/latest", "type": "symlink", "target": "../v3/README.md" }
  ]
}
```

Field rules:

- `path`: POSIX separators, relative to root, no leading `./`, no `..`, no trailing slash,
  **NFC-normalized**, sorted byte-ascending so parents precede children. NFC matters: macOS
  `readdir` yields NFD, so without it a Mac↔Linux round trip renames and re-uploads every
  accented filename on every run.
- `type` ∈ `file | dir | symlink`. Every directory gets an entry (~50 bytes) so mode
  restoration is complete and empty dirs are free. Fifos, sockets, devices are skipped with
  a warning; a hardlink is stored as an independent file (dedup makes it free).
- `mode`: decimal `uint32` of the POSIX permission bits (JSON has no octal literal; always
  *rendered* octal in output).
- `modTime`: RFC3339 nanos, UTC. Load-bearing for `--check=auto`, not for correctness.
- `pack`: index into `packs[]`. Absent for dirs, symlinks, and **zero-byte files** — those
  need no pack member.
- `packs[].members` is in **tar order**. This is what makes download's early abort and
  dead-byte accounting possible, at ~5 bytes per blob after xz.
- **Determinism:** tar headers carry `name=blobs/<sha>`, `size`, `mode=0600`, `mtime=0`,
  `uid/gid=0`, empty `uname/gname`, `FormatPAX` with no PAX records. Fixed xz params +
  deterministic member ordering ⇒ a rebuild is byte-identical ⇒ the same `p_<sha16>` name ⇒
  crash-resume is a listing lookup, not a re-upload.
- `entriesShards` is reserved (currently `null`): above ~200k files, `entries` moves to
  sharded side objects with `firstPath`/`lastPath` so a subtree pull fetches only what it
  needs. Forward-compatible under `formatVersion: 1`; not built in v1.

### `dr-sync init`

`POST files/` → new container. Optional `--name`, `--description`, `--tag` applied with
`PATCH catalogItems/{id}/` (always sending `tags` explicitly — omitting it *clears* them).
Writes `.dr-sync/README.txt` and a generation-0 empty manifest, so the container is
immediately valid rather than a special case everywhere. Prints the id and the
`export DR_SYNC_CATALOG_ID=…` line.

### Upload

1. **Resolve.** Catalog id from `--catalog-id` then `$DR_SYNC_CATALOG_ID`. Endpoint/token
   from env, then the `dr` config file. Refuse (exit 2) if `<root>/.dr-sync` exists locally,
   naming `--meta-prefix` in the message — silently declining to sync a real user directory
   is a data-loss bug that surfaces months later.
2. **Read head** (concurrent with 3). `GET files/{id}/allFiles/?prefix=.dr-sync/&limit=1000`
   with pagination → the objects, their sizes and `fileChecksum`s. Head = lexicographic max
   manifest; fetch via `/links/`, xz-decode. Also `GET catalogItems/{id}/extendedDetails/`
   for `catalogVersionId`.
3. **Walk + hash** (concurrent with 2). One walker feeding `min(GOMAXPROCS,8)` hashers.
   Under `--check=auto` (default) a hasher first consults the parent manifest: matching
   `size` + `modTime` + `mode` ⇒ adopt the recorded SHA without reading the file. This is
   the same baseline rsync gets from its destination, and without it every no-op run on a
   200 GB tree costs ten minutes of SHA-256. `--check=full` forces a read.
   Build `byPath` and `bySha` indexes.
4. **Classify.** Three-way over `union(local, manifest)`: unchanged / changed / new /
   remote-only. Remote-only is *kept* by default (additive and safe); `--delete` drops it
   from the new manifest, which is a manifest-only edit — no API call.
5. **Determine bytes needed.** For every new/changed file, if its SHA is already in the
   parent manifest's blob set, emit an entry pointing at the existing pack — **zero bytes**.
   Renames, copies, and reverts are free. The remainder, deduplicated by SHA, is `needUpload`.
6. **Plan packs.** Sort by `(extension, dir, basename, sha)` — extension-major because
   same-extension files are the cheapest good proxy for LZMA match reuse. Greedily fill to
   `--pack-size` (64 MiB *compressed*, detected by watching the writer's output counter) or
   `--pack-input-max` (256 MiB uncompressed), whichever binds. Any blob ≥
   `--big-file-threshold` (32 MiB) gets its own pack, so big-file churn never drags
   neighbours along on the next pull. If ≥80% of a pack's input bytes are extensions on the
   incompressible list (jpg/png/mp4/zip/gz/parquet/…), use `codec: "store"` — a bare tar —
   rather than burning minutes to shrink JPEGs by 1%.
7. **Build** with `min(GOMAXPROCS,4)` workers: `tar → xz.Writer → sliceWriter → sha256 tee`
   into `--tmpdir`, re-verifying each source SHA as it streams (a file changing under us is
   requeued once, then reported). Name the pack after the fact from its own digest. If that
   name already appears in the step-2 listing, **skip the upload** — and note this is
   *required*, not an optimization: `overwrite=error` rejects the entire commit on any
   pre-existing name, so a resumed run that re-uploaded a pack it already landed would fail
   the whole push. Compare `fileChecksum` too; a name present with a different digest is a
   corrupted prior upload and is reported, not silently reused.
   Slicing at `--slice-size` (64 MiB) is what removes the file-size ceiling: the API has no
   chunked upload, so a 40 GiB file becomes ~600 sibling objects reassembled by
   `io.MultiReader` on read. Cap scratch at `--max-scratch` (4 GiB) with backpressure.
8. **Lease.** Write `.dr-sync/locks/<clientId>.lock` (15 min TTL, renewed every 5, released
   on clean exit and on SIGINT); refuse if an unexpired foreign lease exists unless
   `--force`. Advisory, one extra listing and one small write — but it converts "two CI jobs
   push the same tree and one silently loses six minutes of work" into an immediate, legible
   failure. That is the race that actually happens, not the millisecond one at commit time.
9. **Commit.** One stage: create, upload every not-already-present slice (4 at a time), then
   the manifest, then `fromStage` with `overwrite: "error"` — every name written is unique by
   construction, so a duplicate means a bug or a concurrent writer and we want to hear about
   it. `fromStage` normally answers `200` synchronously with
   `{catalogId, catalogVersionId, numFiles}` (0.355 s for a 200-file stage), and it is
   **atomic**: on a `409` nothing from the stage lands, so there is no partial-application
   case. If it ever answers with a `Location` instead, fall back to polling with redirects
   disabled: 307 → follow, 303 → done, 200 → read `status` (`error`/`abort` prefix → fail and
   surface `message`; `completed` → done; anything unrecognized → hard error, never loop),
   on a 250 ms ×1.6 backoff capped at 2 s with a 600 s deadline. On deadline do **not**
   re-commit — exit 6 printing the status URL and let a re-run reconcile from the listing.
   Above `--max-stage-bytes` (4 GiB) or `--max-stage-files` (500), split: pack stages commit
   first, manifest last and alone. Immediately before the manifest commit, re-list manifests,
   re-read `catalogVersionId`, and re-verify every pack the manifest references still exists;
   any movement aborts with exit 4 (`--force` re-plans from the new parent and retries — the
   packs are already uploaded and content-addressed, so the retry is nearly free).
10. **Tier-1 GC** (unless `--no-gc`): delete packs referenced by no retained manifest, older
    than `--gc-grace` (6 h), when no foreign lease is live. Prune manifests beyond
    `--keep-generations` (5) — manifests first, then their orphaned packs, never the reverse.
11. **Advise.** If dead bytes exceed 35% and 64 MiB, print a `dr-sync compact` hint. Never
    auto-compact unless `--auto-compact`.

**Crash between packs and manifest**, three layers: single-stage runs make nothing visible
until commit; multi-stage runs leave orphan packs that no reader can see and that a re-run
recognizes by content-addressed name and skips; anything never re-run is reaped by tier-1 GC.

### Download

1. Read head as above. Empty container → warn, exit 0.
2. Walk + hash local, same code path and same `--check=auto` semantics.
3. **Plan.** Dirs and symlinks come straight from the manifest. Zero-byte files are created
   directly. A file whose local SHA matches is skipped (mode/mtime fixed if needed). A file
   whose SHA exists locally *at another path* is cloned from disk (`copy_file_range` /
   `clonefile` where available) — renames are free on the pull side too, unless
   `--no-local-reuse`. Everything else needs its blob from a pack. Per pack, compute
   `lastNeededIndex` over `members`.
4. **Verify before transferring.** From the listing already in hand, confirm every needed
   slice exists with the manifest's size and `fileChecksum`. A mismatch is exit 5 with a
   precise message ("pack … slice 003 missing; the remote was compacted, re-run") at zero
   bandwidth cost.
5. **Fetch** with `--parallel` (4) workers. `POST /links/` in batches of 100 at
   `duration: 3000`, recording `issuedAt`. Each worker chains slices through
   `io.MultiReader` over presigned GETs made by a **separate client whose transport strips
   `Authorization`** (S3 returns 403 when a presigned signature and an auth header both
   appear), wraps in `xz.Reader` then `tar.Reader`, streams wanted members to
   `<root>/.dr-sync-tmp/<sha>.part` while hashing, `io.Copy(io.Discard)` past unwanted ones
   (xz cannot seek), and **aborts at `lastNeededIndex`** — which is where the "solid archive
   but stop early" win is actually realized. Because of the early abort the pack-level SHA
   is only checked when the whole blob was read; **per-member SHA is the integrity
   mechanism**. Refresh any link within 300 s of expiry before starting a slice; on a 403 or
   mid-stream error, re-issue `/links/` for the remainder and resume with
   `Range: bytes=<sliceOffset>-`. A `/links/` batch fails wholesale if any one name is
   missing, so on that 4xx re-issue singly to name the culprit.
6. **Apply** single-threaded in sorted order: `MkdirAll` first, `os.Rename` from the tmp dir
   (which lives under `<root>` precisely so the rename is same-filesystem and atomic),
   symlinks replaced, then `Chmod` then `Chtimes` (mtime last or the write resets it).
   **Strip setuid/setgid/sticky** unless `--preserve-special-modes` — materializing a setuid
   binary out of a remote blob is a security hole. Reject any manifest path that is
   absolute, contains `..` or NUL, starts with the meta prefix, or case-collides on a
   case-insensitive filesystem (probed once; collisions warn-and-skip).
7. Extraneous local files are listed and kept; `--delete` removes them, `--delete-excluded`
   also prunes emptied dirs. Never touch anything outside root or under the meta prefix.
8. A pack that fails after `--retries` marks its blobs failed and the rest continue: what
   landed is SHA-verified and atomically renamed, what didn't is untouched. Exit 6 with the
   list. Re-running picks up exactly the remainder, because local truth is re-derived every
   time — this is where "no local state" pays for itself.

### Watch

`dr-sync watch` is a resident process that keeps a container mirroring a local tree,
upload-only — it never pulls. It is deliberately not the FUSE mode; it is the groundwork
for it: a stable per-process identity and a remote registry of who else is writing, so the
eventual multi-writer case has somewhere to look rather than being bolted on later.

**A filesystem event is a hint, never a source of truth.** dr-sync keeps no persistent
local state (invariant 7), so every trigger — whether from the OS watcher or the periodic
poll — re-walks and re-hashes the tree exactly as `upload` does under `--check=auto`. A
missed inotify event, a kqueue watch-descriptor limit, or a network filesystem that never
raises one at all cost latency, never correctness. `internal/watcher` owns this: it debounces
a burst of events into one trigger (`--debounce`, 2 s), enforces a floor between the start of
two pushes (`--min-interval`, 15 s) so a busy tree cannot mint a generation per keystroke, and
runs a periodic rescan (`--poll-interval`, 60 s) as a backstop regardless of what the OS
watcher did or didn't see.

Unlike `upload`, **`--delete` defaults on**: watch exists to mirror, not to accrete.

**The resident-writer registry** (`.dr-sync/writers/`) is what a "well-known metadata file"
becomes once invariant 1 is taken seriously: nothing in the container is ever overwritten,
and the API has no compare-and-swap, so a single mutable pointer file is exactly the bug
invariant 1 exists to prevent. Instead each resident writer owns a sequence-named object,
refreshed the same way a lease is — write the next sequence, delete the previous one — and
the registry is the union of whatever is currently live. It costs nothing extra to read:
`Store.ReadState` already lists every object under the meta prefix for the lease listing, so
parsing the writers alongside it is free. Entries outside the pack/manifest reference graph
are never touched by tier-1 GC's allowlist logic, so a stale one is swept explicitly
(`StaleWriterNames`), on the same cadence as `--gc-interval`.

**On a dirty remote — the head moved, or a fork was detected — watch suppresses `--delete`
for that cycle and warns**, rather than deleting through it. Two mirroring writers each
deleting the other's paths from the manifest every cycle is a real failure mode once a
second writer exists (as FUSE will introduce), and it is cheap to avoid: content is never
lost by holding a delete back for one cycle, only re-evaluated on the next. `--force`
overrides.

**GC does not run per push.** Tier-1 GC is the expensive tail of a commit — a full listing
plus up to `--keep-generations` manifest downloads — and a daemon pushing every few seconds
would pay that far more often than it needs to, for less benefit: many small pushes produce
many *mostly*-live packs, which tier-1's `liveRatio == 0` rule cannot reclaim regardless of
how often it runs. `watch` passes `NoGC` on every cycle and instead runs one
`syncer.Collect` sweep on a timer (`--gc-interval`, 10 min, independent of any push) and once
more on clean shutdown. A long watch session still accrues *sparse* packs that only
`dr-sync compact` reclaims; the usual compaction hint still fires, rate-limited to once per
sweep interval so it does not repeat every cycle.

**A stable lease identity is required, not optional.** `Upload` used to mint a fresh random
lease client id on every call; a daemon whose previous cycle died mid-commit would then see
its own orphaned lease as foreign and self-block for the full lease TTL. `UploadOptions`
therefore takes an optional `ClientID`, and watch generates one client id for its entire
lifetime rather than one per push.

A stable id creates a second problem a fresh random one never could: `AcquireLease` used to
start every `LeaseHolder` at sequence 0, so every call for the same client id wrote the same
object name, `<clientId>-1.lock`. That is invisible while each cycle's `Release` runs before
the next cycle's `AcquireLease` — but a cycle that dies mid-commit, or whose `Release` itself
fails, leaves that name occupied, and `Overwrite: error` then rejects the very next cycle's
lease outright (not a foreign-lease refusal — this client id is never foreign to itself — a
collision with its own leftover). `AcquireLease` therefore seeds the new holder's sequence
from the highest one already on record for that client id, so a resumed run always picks an
unused name.

### Garbage and compaction

Per pack, `liveBytes` = Σ sizes of members still referenced; `liveRatio` = that over
`uncompressedSize`. Both computable from the manifest alone, no network.

- **Tier 1 (free):** `liveRatio == 0` ⇒ deletable with no data movement. Runs automatically
  after every successful commit, subject to grace + lease. Handles most garbage, because
  pack contents correlate with a single push.
- **Tier 2 (expensive):** `0 < liveRatio < --compact-threshold` (0.5) needs live members
  rewritten. Never automatic by default. `dr-sync compact <path>` runs **from the local
  tree**: after hashing, most live members are already present locally, so on a machine with
  a current checkout compaction usually downloads nothing. Only live-but-not-local members
  stream from their old pack.

Compact then verifies it is still the maximum generation before deleting anything — if
someone else committed concurrently, abort GC entirely (exit 4), because their manifest may
reference packs we were about to delete. `dr-sync gc` is tier-1 only: no local walk, fast,
scriptable in CI.

A resident `watch` session changes the shape of the garbage, not the mechanism: many small
pushes produce many *mostly*-live packs — tier 1's `liveRatio == 0` rule reclaims little of
that — so dead weight accrues as sparse packs over a long session and wants an occasional
`dr-sync compact`, exactly as the usual hint suggests.

### Concurrency: what is guaranteed, what is not

The API has no conditional write, no ETag, no CAS. `catalogVersionId` is a *change detector*,
not a guard. Four layers, cheapest first: immutable generation-named objects (structural — no
interleaving can produce a dangling or corrupt state), advisory leases (prevents the common
case), pre-commit revalidation (shrinks the window), post-commit fork detection (names both
`manifestId`s and both `createdBy`s, and never GCs when a fork is seen).

**Races that remain, stated plainly:**

- *Lost update.* Two writers both commit generation N+1; lexicographic max wins. The loser's
  packs and manifest both survive intact — nothing is corrupted — but its paths are not in
  the head, so the next puller won't see them. Detected post-commit; healed by re-running
  upload, which re-classifies against the new head and reuses the already-uploaded packs.
  Unfixable without CAS.
- *Read during compact.* A reader holding manifest N whose packs get deleted gets a missing
  name from `/links/` → exit 5, "re-run". Bounded by the grace window, not eliminated.
- *Multi-stage commit.* Packs land in earlier catalog versions than the manifest; between
  commits the container has unreferenced packs — invisible to readers, reaped by tier-1 GC.
- *Clock skew* makes lease expiry and GC grace approximate; prefer server `fileCreatedAt`
  for grace decisions and keep the lease TTL generous.

Explicitly rejected: `catalogVersionId` as a CAS token (nothing to attach it to), and using
`PATCH catalogItems/{id}/` tags or description as a pointer or lock (same last-write-wins
problem plus the tags-clearing footgun). Any description we write is display-only, e.g.
`dr-sync gen 42 · 12043 files · 7.6 GiB`.

The resident-writer registry `watch` maintains (`.dr-sync/writers/`) is advisory in exactly
this sense — presence, not a lock — and changes none of the above: it lets one writer notice
another and hold back a `--delete` for a cycle, nothing more.

### Two API details the client must get right

- The 303 from `POST files/{id}/downloads/` must be followed **without** `Authorization`.
  Go's client strips it cross-host, but the redirect policy should assert it, not assume.
- `useArchiveContents` is `"True"`/`"False"` (Python-capitalized) in form bodies, while
  `recursive` is `"true"`/`"false"` in query strings. Do not unify them. Trailing slashes on
  every path are load-bearing.

---

## Implementation layout and CLI

```
cmd/dr-sync/main.go          --dr-plugin-manifest fast path, then cobra; error → exit code
cmd/dr-sync/manifest.json    go:embed'd plugin manifest (name "sync", authentication true)
internal/cli/                cobra command tree, shared persistent flags, exit-code mapping
internal/config/             endpoint/token/catalog resolution; the only package that uses viper
internal/drapi/              client, retry, typed errors, allFiles iterator, links (no-auth
                             transport), stage create/upload/commit, catalog, async poll
internal/manifest/           schema + Validate, xz codec, head selection & fork detect,
                             byPath/bySha indexes, liveness
internal/scan/               walk, parallel SHA-256 pipeline, --check modes, NFC + path safety
internal/packer/             pack planning, deterministic tar+xz+slice+sha build, scratch mgmt
internal/extract/            needed-blob planning, slice-chained streaming with early abort
                             and Range resume, atomic apply
internal/syncer/             the upload/download phase machines, classify, gc, compact, lease,
                             the resident-writer registry
internal/watcher/            watch's debounce/coalesce state machine and OS filesystem watch
                             management; a trigger is a hint, the re-scan is the truth
internal/progress/           Reporter interface; tty / json / quiet renderers
internal/version/            ldflags-injected version, commit, date
```

Direct dependencies: `github.com/spf13/cobra` and `github.com/spf13/viper` for the command
tree and configuration, `github.com/ulikunitz/xz v0.5.16` for compression,
`github.com/fsnotify/fsnotify` for `watch`'s filesystem events, `golang.org/x/sync/errgroup`,
`golang.org/x/text/unicode/norm`, and `golang.org/x/term`. Pin `ulikunitz/xz` exactly and check
`WriterConfig` field names against the pinned version; that API has shifted across releases
and it is the one moving target here. fsnotify was already pulled in transitively by viper, so
using it directly adds no new module to the build — and it is the only watcher library
available under `CGO_ENABLED=0` (`rjeczalik/notify` needs CGO for macOS FSEvents). It has no
public recursive watch even in newer releases, so `internal/watcher` manages one watch per
directory itself.

**cobra and viper**, matching the `dr` CLI. cobra permutes arguments, so `upload ./dir --delete`
works the way anyone would type it — the stdlib `flag` package stops at the first positional and
fails that invocation. It also contributes shell completions and grouped per-command help for
free. Two things it needs care with: `--dr-plugin-manifest` is answered in `main` *before* cobra
sees it, since cobra would reject an unknown flag in `argv[1]`; and cobra's own parse, argument
and unknown-command failures arrive without an exit code, so they are wrapped as usage errors
rather than being reported as internal ones.

viper owns the flag → environment → config-file precedence, and lives behind `internal/config`
only. A `depguard` rule enforces that: reading viper from a command would reintroduce exactly
the precedence bugs a single owner exists to prevent. Because the flag names double as viper
keys, `drconfig.yaml` can carry any of them — a `catalog-id:` key, say — with no per-command
special case.

```
dr-sync init                 [--name N] [--description D] [--tag T]...   → prints catalog id
dr-sync upload   <path>
dr-sync download <path>
dr-sync watch    <path>      resident, upload-only, --delete defaults on
dr-sync status   <path>      classify + plan, no writes (same as --dry-run, read-only)
dr-sync ls                   [--prefix P] [--long] [--generation N]      manifest contents
dr-sync inspect              [--packs]    generations, liveness, dead bytes, leases
dr-sync compact  <path>      tier-2, local-tree-driven
dr-sync gc                   tier-1 only
dr-sync completion <shell>   contributed by cobra
dr-sync version
dr-sync --dr-plugin-manifest works with no env and no network
```

Shared flags: `--catalog-id` (`DR_SYNC_CATALOG_ID`), `--endpoint` (`DATAROBOT_ENDPOINT`),
`--config` (`DATAROBOT_CONFIG`, default `~/.config/datarobot/drconfig.yaml`),
`--meta-prefix` (`.dr-sync/`), `--check auto|full|size|mtime`, `--exclude`/`--include`/
`--exclude-from` (gitignore-style, matched on the relative POSIX path), `--jobs`,
`--timeout`, `--retries`, `-v/--verbose`, `-q/--quiet`, `--output text|json`, `--no-color`,
`--dry-run`, `--yes`. The token comes from `DATAROBOT_API_TOKEN` or the config file only —
no flag, so it stays out of `ps` and shell history. `--insecure-skip-verify` / `--ca-cert`
mirror the `dr` global flags (also arriving as `DATAROBOT_CLI_*` env vars).

Upload adds `--delete`, `--level 0|1|3|6|9` (6), `--pack-size`, `--pack-input-max`,
`--big-file-threshold`, `--slice-size`, `--max-stage-bytes`, `--max-stage-files`,
`--compress-jobs`, `--upload-jobs`, `--tmpdir`, `--max-scratch`, `--keep-generations`,
`--gc-grace`, `--no-gc`, `--auto-compact`, `--compact-threshold`, `--no-lock`, `--force`,
`--follow-symlinks`, `--overwrite error|replace`.

Download adds `--delete`, `--delete-excluded`, `--prefix` (repeatable),
`--generation`/`--manifest` (pull a retained older generation), `--parallel`,
`--link-duration`, `--no-local-reuse`, `--preserve-special-modes`, `--allow-unsafe-links`,
`--tmpdir` (must share the filesystem with root).

Watch shares every upload flag — including `--timeout`, which bounds one cycle, not the
daemon — but registers `--delete` with a default of `true` rather than `upload`'s `false`.
It adds `--debounce` (2s), `--min-interval` (15s), `--poll-interval` (60s; `0` disables it),
`--poll-only` (skip the OS watcher entirely), `--gc-interval` (10m; `0` disables it), and
`--writer-ttl` (5m, how long its writers-registry entry is honoured).

**Exit codes:** 0 success (including nothing to do) · 1 internal · 2 usage/config/precondition
· 3 API or auth failure · 4 conflict (foreign lease, generation moved, fork) · 5 integrity
(SHA mismatch, missing pack, unsupported `formatVersion`) · 6 partial success · 130
interrupted (lease released, scratch cleaned).

`--output json` emits one JSON object per line on stdout with human text on stderr, so it
composes in CI. Human progress is per-phase — `remote` / `scan` / `plan` / `pack` / `upload`
/ `commit` / `gc` / `done` — with byte counts, achieved compression ratio, rate and ETA;
non-TTY collapses the bars to one line per phase.

---

## Release engineering

Modelled on `/Users/carson.gee/code/cDX/cli` and
`/Users/carson.gee/skunk/ard/agentic-resource-discovery-server`, both already on `dockers_v2`
with goreleaser OSS v2 (2.15.4 locally supports it).

- `.goreleaser.yaml`: `version: 2`, `project_name: dr-sync`; builds for linux/darwin/windows
  × amd64/arm64 plus linux/riscv64 (plugin convention), `CGO_ENABLED=0`,
  `mod_timestamp: "{{ .CommitTimestamp }}"`, ldflags into `internal/version`; tar.gz archives
  with a zip override for windows; `checksum`; `dockers_v2` → `ghcr.io/datarobot-labs/dr-sync`,
  tags `{{ .Version }}` and `{{ if not .Prerelease }}latest{{ end }}`, platforms
  linux/amd64 + linux/arm64, OCI labels.
- `Dockerfile.goreleaser`: `FROM cgr.dev/chainguard/static:latest`, `ARG TARGETPLATFORM`,
  `COPY $TARGETPLATFORM/dr-sync /dr-sync`, `ENTRYPOINT ["/dr-sync"]`. The `$TARGETPLATFORM`
  subdirectory is load-bearing — `dockers_v2` stages binaries there, unlike legacy `dockers:`.
  Pure-Go xz is what lets this be a scratch-style image with no `xz` binary.
- Plugin discovery, both forms verified against dr v0.2.86: a `dr-`-prefixed executable on
  `PATH`, which is what `task install:local` produces and where the other `dr-*` plugins live;
  and a managed install under `$XDG_CONFIG_HOME/datarobot/plugins/<name>/`, which is what
  `dr plugin install` writes from the archive `task plugin:package` builds.
- Cobra derives `Name()` from the first word of `Use`, and every usage line is built from
  `CommandPath()`, which is built from `Name()`. A two-word invocation like `dr sync` therefore
  cannot go in `Use`: it goes in `cobra.CommandDisplayNameAnnotation`, which `UseLine`
  substitutes. Without it the help under the plugin reads `dr upload <path>`.
- Plugin archive via `task plugin:package`: stage `manifest.json`, `LICENSE.txt` (required by
  the packager), `dr-sync.sh` + `dr-sync.ps1` wrappers that `exec` the right
  `dr-sync-<goos>-<goarch>` binary from `bin/`, then
  `dr self plugin package "$STAGING" -o dist/ --index-output dist/index-fragment.json`.
  The binaries must live under `bin/`, not flat at the plugin root: the CLI's
  `FindPluginScript` scans the root for a `dr`-prefixed file with no extension (or `.sh`/
  `.ps1`) and returns the first match in directory order, so a flat layout lets it pick a
  wrong-platform binary directly — e.g. `dr-sync-darwin-amd64` on a Linux CI runner — instead
  of the wrapper, which fails with `exec format error`.
  The packager execs the wrapper with `--dr-plugin-manifest` and diffs it against
  `manifest.json`, so the two must agree exactly on name/version/description/authentication —
  `task lint` gets a check asserting the manifest version matches the git tag.
- `.github/workflows/release.yaml` on `v*` tags: checkout `fetch-depth: 0`, setup-go from
  `go.mod`, setup-qemu, setup-buildx (the default builder cannot multi-platform push), ghcr
  login with `${{ github.actor }}` / `GITHUB_TOKEN`, `goreleaser release --clean`, then
  `task plugin:package` and `gh release upload`. `permissions: contents: write,
  packages: write`. SHA-pin third-party actions with a version comment, per house style.
- `Taskfile.yml`: `lint` (`go mod tidy -diff`, `golangci-lint fmt --diff`, `go vet`,
  `golangci-lint run` once per GOOS so build-tagged files are covered, `goreleaser check`,
  manifest-version check), `test`, `build`, `build:dist`, `plugin:package`, `install:local`
  (install onto `PATH` for the dev loop), `release:snapshot`. Pin tool versions in `vars:`.
- `.golangci.yaml`: copy `/Users/carson.gee/code/opencode-cli-plugin/.golangci.yaml` (v2
  schema, gofumpt formatter) and re-enable `gosec` — this tool writes files from remote
  input, so the path-traversal and permission checks earn their keep here.

---

## Verification

1. **Unit.** Manifest classify tables; head selection and fork detection; pack round-trip
   including dedup, slicing, `store` codec, and byte-identical rebuild; NFC and path-safety
   rejection cases; mode mapping; drconfig parsing. `internal/drapi` against
   `httptest.Server`: paginated `allFiles` following `next`, a 303 download asserting **no**
   `Authorization` reaches the redirect target, DELETE-with-body, `/links/` whole-batch
   failure, async poll 200→200→303 and the `error`-status path.
2. **Integration** (`-tags=integration`, real credentials, scratch container deleted at the
   end): `init`; upload a fixture tree; assert `allFiles` shows only `.dr-sync/*`; download
   into an empty dir and `diff -r`. Touch one file, re-upload, assert exactly one new pack
   and no re-upload of the others. Rename a large file, assert zero bytes uploaded. Kill the
   process between pack upload and commit, re-run, assert the packs are recognized and
   skipped. Force a compaction, assert old packs are gone and a fresh download still
   round-trips. Run two uploads concurrently and assert one exits 4 rather than corrupting.
3. **Plugin.** `task install:local && dr plugin list` shows `sync`; `dr sync init` and
   `dr sync upload ./testdata/tree` work end to end; `dr self plugin package` passes
   validation on the staged directory.
4. **Performance.** Benchmark compression against system `xz` on a fixed corpus in CI and
   fail the build on regression — pure-Go LZMA2 runs ~5–15 MB/s per core versus 30–80 for
   `xz -T0`, so the concurrency and pack-size defaults are load-bearing, not cosmetic, and
   need a regression guard.
5. **Release.** `goreleaser check` inside `task lint`; `goreleaser release --snapshot --clean`
   locally to prove the multi-arch Docker build before tagging.
