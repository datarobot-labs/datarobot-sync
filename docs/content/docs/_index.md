+++
title = 'dr-sync'
weight = 1
+++

`dr-sync` is rsync for the [DataRobot Files API](https://docs.datarobot.com): a single
static Go binary that makes a local directory and a DataRobot Files container match,
transferring only the difference, xz-compressed. It is also a `dr` CLI plugin, so
everything below works equally as `dr sync …`.

## What it does

- **Diffs exactly**, not heuristically — the Files API records a SHA-256 for every stored
  object, so `dr-sync` knows precisely what changed.
- **Dedupes by content.** Files are stored under their own digest, so renaming or
  reverting a file transfers zero bytes.
- **Batches uploads into packs.** Each API upload costs about 0.2s even in parallel, so a
  thousand small files go up as a handful of compressed packs instead of a thousand
  requests.
- **Stops early on download.** A pull reads only the packs it needs, and stops reading
  each one as soon as it has the last member it wanted.
- **Keeps mirroring in the background.** `dr-sync watch` is a resident process that keeps
  pushing a directory as it changes, instead of running `upload` in a loop yourself.

## Get started

- [**Quick start**]({{% relref "docs/quick-start" %}}) — from zero to a container that
  stays mirrored automatically.
- [**Installation**]({{% relref "docs/installation" %}}) — as a `dr` plugin, a standalone
  binary, or a container.
- [**Usage**]({{% relref "docs/usage" %}}) — the full command reference: init, upload,
  download, status, ls, inspect, compact, gc.
- [**Watch**]({{% relref "docs/watch" %}}) — the resident mirroring mode, its flags, and
  platform watch-descriptor limits.
- [**Examples**]({{% relref "docs/examples" %}}) — running `watch` and scheduled `upload`
  as a Docker container, a systemd unit, or a launchd agent.
- [**Configuration**]({{% relref "docs/configuration" %}}) — flags, environment
  variables, and the `drconfig.yaml` fallback.
- [**Container layout**]({{% relref "docs/container-layout" %}}) — what actually lands in
  the DataRobot Files container.
- [**Concurrency and garbage collection**]({{% relref "docs/concurrency" %}}) — what two
  writers racing each other are guaranteed, and how space is reclaimed.
- [**Exit codes**]({{% relref "docs/exit-codes" %}}) — what each code means for scripting
  and CI.
- [**Architecture**]({{% relref "docs/architecture" %}}) — the design decisions and the
  verified API behavior they rest on.
- [**Development**]({{% relref "docs/development" %}}) — building, testing, and releasing
  `dr-sync` itself.

## Project layout

| Path | Purpose |
|---|---|
| `cmd/dr-sync/` | `--dr-plugin-manifest` fast path, then cobra; error → exit code |
| `internal/cli/` | cobra command tree, shared persistent flags, exit-code mapping |
| `internal/config/` | endpoint/token/catalog resolution; the only package that uses viper |
| `internal/drapi/` | client, retry, typed errors, `allFiles` iterator, links, stage upload/commit |
| `internal/manifest/` | schema + validation, xz codec, head selection & fork detection |
| `internal/scan/` | walk, parallel SHA-256 pipeline, `--check` modes, NFC + path safety |
| `internal/packer/` | pack planning, deterministic tar+xz+slice+sha build |
| `internal/extract/` | needed-blob planning, slice-chained streaming with early abort |
| `internal/syncer/` | the upload/download phase machines, classify, gc, compact, lease |
| `internal/watcher/` | watch's debounce/coalesce state machine and OS filesystem watching |
