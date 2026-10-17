+++
title = 'Architecture'
weight = 10
+++

Moving a directory tree in or out of the DataRobot Files API otherwise means either the
Python SDK's `fsspec` layer (per-file, uncompressed, needs a Python runtime) or
hand-rolled `curl` — neither drops into CI, and both re-send bytes that have not
changed. See
[SPEC.md](https://github.com/datarobot-labs/datarobot-sync/blob/main/SPEC.md) for
the full design; this page is the summary.

## Invariants

These are load-bearing. Breaking one is a data-loss or corruption bug, not a style
choice.

1. Nothing in the container is ever overwritten. Manifests are generation-named, packs
   are content-addressed. `overwrite` is `error`, not `replace`.
2. A pack is uploaded before any manifest referencing it, and deleted only after the
   last manifest referencing it is gone.
3. A manifest is deleted only after a newer one is verified to be the maximum.
4. One `fromStage` commit per run wherever possible — it is the atomicity boundary; when
   split, the manifest commits last.
5. Everything streams. No `io.ReadAll` and no `bytes.Buffer` on file content, ever — a
   single file may be larger than RAM.
6. The `Authorization` header never reaches a presigned S3 URL (S3 returns 403 when a
   presigned signature and an auth header both appear).
7. No persistent local state. Local truth is re-derived by walking and hashing every
   run; scratch files are disposable and correctness never depends on them surviving.
8. Extraction never writes outside the sync root, never writes under the meta prefix,
   and strips setuid/setgid/sticky unless explicitly asked not to.

## What the API actually gives us

Verified live against `app.datarobot.com`:

- A "container" is a catalog item with `catalogType: files`, holding files at relative
  paths. There are no folder objects — directories are implied by `/` in the path.
- `GET files/{id}/allFiles/` returns `fileChecksum` = SHA-256 hex of the raw bytes,
  confirmed by downloading a file and hashing it. `limit` defaults to 100 and caps at
  1000, so following `next` is mandatory.
- Downloads are presigned S3 URLs, and they honor `Range`.
- Uploads are multipart, one request per file, at ~0.2s each even at 8-way parallelism.
  A "stage" batches many of them behind **one** commit — the whole reason packing,
  rather than mirroring, is the design.
- DataRobot's intake will not unpack xz — only zip, tar, tar.gz/tgz, tar.bz2, gz, bz2.
  Server-side extraction is off the table, so datarobot-sync does all tarring, compressing,
  and extraction locally.

## Decisions this led to

1. **Opaque packs, real xz.** The container holds only `.dr-sync/…` objects: immutable
   manifests plus `tar.xz` packs, stored as opaque bytes. This is the only route to
   genuine LZMA2 on the wire.
2. **No local state.** All sync state lives in the remote manifest; local disk is not
   trusted between runs.
3. **Catalog ID is explicit** — `--catalog-id` flag or `DR_SYNC_CATALOG_ID` — with a
   dedicated `datarobot sync init` subcommand to create the container.
4. Shipped by goreleaser: per-platform binaries, a ghcr image, and a `dr` plugin
   `.tar.xz` bundling every platform behind a wrapper script.
5. **fsnotify for `watch`, not `rjeczalik/notify`.** `rjeczalik/notify` needs CGO for
   macOS FSEvents, which rules it out under this project's `CGO_ENABLED=0` release build.
   fsnotify is pure Go on every backend, and was already in the module graph as an
   indirect dependency of viper — using it directly adds no new module to the build. It
   has no public recursive watch, even in releases newer than the one pinned here (the
   recursive code path exists but is internal, exercised only by fsnotify's own tests),
   so `internal/watcher` adds one watch per directory itself.

## Two API details the client must get right

- `fromStage` is **synchronous and atomic**: `200` with
  `{catalogId, catalogVersionId, numFiles}`, no `Location`, ~0.35s even for 200 files;
  on a `409` nothing from the stage lands. The `Location` poller is a fallback for large
  stages, not the hot path.
- Presigned URLs come back with an explicit `:443`, and SigV4 signed the host *without*
  it — Go sends the `Host` header exactly as the URL spells it, so the port must be
  stripped before the request or S3 answers `SignatureDoesNotMatch`. No
  `httptest`-based test can catch this, because those always listen on a non-default
  port — it took a run against the live service.

## Non-goals

- Crawling or spidering beyond the target directory tree.
- A mutable pointer object as a lock or CAS token — the API has no compare-and-swap, and
  a mutable head is exactly where lost updates become corruption.
- Persistence beyond the remote manifest — local state is never trusted between runs.

See [Concurrency and garbage collection]({{% relref "docs/concurrency" %}}) for what two
writers racing each other are actually guaranteed.
