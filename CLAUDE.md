# CLAUDE.md

Project Name: dr-sync

## What this is

`dr-sync` — rsync for the DataRobot Files API. A single static Go binary that makes a local
directory and a DataRobot Files container match, transferring only the difference,
xz-compressed. It is also a `dr` CLI plugin (`dr sync upload ./dir`).

## Read SPEC.md first

**[SPEC.md](SPEC.md) is the source of truth** for sync semantics, the on-container layout,
the manifest schema, and the guarantees the tool makes. Read it before changing anything
that touches:

- the manifest schema or `formatVersion`
- the `.dr-sync/` object layout (manifest naming, pack naming, slicing)
- the upload or download phase machines
- garbage collection, compaction, or the lease protocol
- exit codes

Changes to those are spec changes: update `SPEC.md` in the same commit.

## Invariants — do not break these

These are load-bearing. Breaking one is a data-loss or corruption bug, not a style choice.

1. Nothing in the container is ever overwritten. Manifests are generation-named, packs are
   content-addressed. The `fromStage` `overwrite` value is `error`, not `replace`.
2. A pack is uploaded before any manifest referencing it, and deleted only after the last
   manifest referencing it is gone.
3. A manifest is deleted only after a newer one is verified to be the maximum.
4. One `fromStage` commit per run wherever possible; when split, the manifest commits last.
5. Everything streams. No `io.ReadAll` and no `bytes.Buffer` on file content, ever — a
   single file may be larger than RAM.
6. The `Authorization` header never reaches a presigned S3 URL (S3 returns 403 when a
   presigned signature and an auth header both appear).
7. No persistent local state. Local truth is re-derived by walking and hashing every run;
   scratch files are disposable and correctness never depends on them surviving.
8. Extraction never writes outside the sync root, never writes under the meta prefix, and
   strips setuid/setgid/sticky unless explicitly asked not to.

## Commands

```sh
task lint            # go mod tidy -diff, gofumpt check, go vet, golangci-lint per GOOS,
                     # goreleaser check, plugin manifest version check
task test            # go test -race -shuffle=on ./...
task build           # build ./dist/dr-sync for this platform
task install:local   # build into .dr/plugins/dr-sync so `dr sync ...` picks it up
task plugin:package  # build all platforms and produce the dr plugin .tar.xz
task release:snapshot
```

Run `task lint` after every change, before considering the work done.

## API notes that bite

- The endpoint already includes `/api/v2`. Every path has a **trailing slash** and that is
  load-bearing.
- Wire JSON is camelCase in both directions. `null` fields are omitted, not sent as null.
- `useArchiveContents` is `"True"`/`"False"` (Python-capitalized) in multipart form bodies,
  while `recursive` is `"true"`/`"false"` in query strings. Do not unify them.
- `DELETE files/{id}/allFiles/` takes a **JSON request body**.
- `POST files/{id}/downloads/` returns **303** to a presigned URL; `POST files/{id}/links/`
  returns them in batch and fails the whole batch if any single name is missing.
- `PATCH catalogItems/{id}/` **clears `tags`** if you omit the field. Always send it.
- Async write ops return a `Location` status URL: poll with redirects disabled, 307 → follow,
  303 → done, 200 → read `status`.
- We never set `useArchiveContents=True`: DataRobot's intake cannot unpack xz.

## Style

Follow the global instructions in `~/.claude/CLAUDE.md`: guard clauses over nesting, early
exits, no defensive programming, no bare catch-alls, imports at the top, minimal code inside
error-handling blocks.
