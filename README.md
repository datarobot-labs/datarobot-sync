<p align="center">
  <a href="https://github.com/datarobot/golang-datarobot-filesync">
    <img src="https://af.datarobot.com/img/datarobot_logo.avif" width="600px" alt="DataRobot Logo"/>
  </a>
</p>
<h2 align="center">dr-sync: DataRobot rsync</h2>
<p align="center">
  <a href="https://datarobot.github.io/golang-datarobot-filesync/">
    <img src="https://img.shields.io/badge/documentation-a?label=Docs&labelColor=30373D&color=5B8FF9" alt="Documentation">
  </a>
</p>

rsync for the DataRobot Files API. A single static Go binary that makes a local directory and
a DataRobot Files container match, transferring only the difference, xz-compressed.

It is also a `dr` CLI plugin, so everything below works equally as `dr sync …`. Full
documentation: [Docs Site](https://datarobot.github.io/golang-datarobot-filesync/).

```sh
dr-sync init                          # create a container, print its id
dr-sync upload   ./my-project         # push, sending only what changed
dr-sync download ./my-project         # pull, fetching only what is missing
```

## Requirements

Works against DataRobot 11.4 and later. The Files API's `allFiles` listing endpoint gained a
`recursive` parameter in 11.7; against an older instance dr-sync detects the resulting 400 and
falls back to the equivalent request automatically, no configuration needed.

## Why it is fast

- **Nothing unchanged is re-sent.** The Files API records a SHA-256 for every stored object, so
  the diff is exact rather than heuristic.
- **A rename or a revert transfers nothing.** Content is stored under its own digest, so moving
  a 5 GB file is a manifest edit.
- **No-op runs do not re-read the tree.** The remote manifest records size, mode and mtime per
  path, which is the same baseline rsync gets from its destination.
- **Files are batched, not sent one by one.** Each upload request costs about 0.2 s even in
  parallel, so a thousand small files go up as a handful of compressed packs.
- **A pull reads only the packs it needs**, and stops reading each one as soon as it has the
  last member it wanted.

## Install

As a `dr` plugin, from a release or a local archive:

```sh
dr plugin install sync --url https://github.com/datarobot/golang-datarobot-filesync/releases/latest/download/sync-<version>.tar.xz
dr plugin install sync --file dist/sync-<version>.tar.xz    # from `task plugin:package`
dr sync --help
```

That installs under `$XDG_CONFIG_HOME/datarobot/plugins/sync/`. For development, a
`dr-`-prefixed binary anywhere on `PATH` is discovered just as well — which is what
`task install:local` does.

As a standalone binary — download the archive for your platform from
[releases](https://github.com/datarobot/golang-datarobot-filesync/releases), or build it:

```sh
task install:local     # builds into ~/.local/bin/dr-sync
```

Building from source needs Go as pinned in `.tool-versions`; with asdf, run `asdf reshim` after
first checkout so the `go` shim exists.

As a container. The mount target is `/tmp/data`, not `/data`: a DataRobot execution
environment's container only has `/tmp` writable, and mounting there means this works
unmodified whether it runs on your own infra or inside one of those.

```sh
docker run --rm -v "$PWD:/tmp/data" \
  -e DATAROBOT_ENDPOINT -e DATAROBOT_API_TOKEN -e DR_SYNC_CATALOG_ID \
  ghcr.io/datarobot/dr-sync:latest upload /tmp/data
```

See [`docs/quickstart.md`](docs/quickstart.md) for a full walkthrough, including running
`watch` as a resident container/systemd/launchd service and `upload` on a cron schedule.

## Configuration

| What | Flag | Environment |
|---|---|---|
| Container to sync | `--catalog-id` | `DR_SYNC_CATALOG_ID` |
| API endpoint | `--endpoint` | `DATAROBOT_ENDPOINT` |
| API token | *(none, deliberately)* | `DATAROBOT_API_TOKEN` |
| Config file | `--config` | `DATAROBOT_CONFIG` |

Precedence is flag, then environment, then config file. Under the `dr` CLI the endpoint and
token arrive already validated; standalone, dr-sync falls back to
`~/.config/datarobot/drconfig.yaml` — the same file `dr auth login` writes. Because the flag
names double as config keys, that file can also carry dr-sync's own settings:

```yaml
endpoint: https://app.datarobot.com/api/v2
catalog-id: 68a1f0c9d2e3b4a5c6d7e8f9
```

There is no `--token` flag on purpose: a flag would put the token in the process table and the
shell history.

## Usage

```sh
# First time
dr-sync init --name "my project"
export DR_SYNC_CATALOG_ID=<the id it printed>

# Push and pull
dr-sync upload   ./src
dr-sync download ./restored

# See what a push would do, change nothing
dr-sync status ./src

# Make the remote mirror the local tree exactly, dropping paths that are gone
dr-sync upload ./src --delete

# Make the local tree mirror the remote exactly
dr-sync download ./src --delete

# Only part of the tree
dr-sync download ./src --prefix docs/ --prefix src/api/

# Roll back to a retained older push
dr-sync ls --generation 41
dr-sync download ./src --generation 41

# What is in there, and what is reclaimable
dr-sync inspect --packs
dr-sync compact ./src     # rewrite sparse packs, run from a complete checkout
dr-sync gc                # delete unreferenced objects, moves no data
```

Shell completions come from cobra:

```sh
dr-sync completion bash > /usr/local/etc/bash_completion.d/dr-sync
dr-sync completion zsh  > "${fpath[1]}/_dr-sync"
```

Exclusions use gitignore syntax, from `--exclude`, `--exclude-from`, or a `.drsyncignore` in
the sync root:

```
node_modules/
*.log
!important.log
```

## Watch

`dr-sync watch <path>` is a resident process that keeps pushing a directory as it changes,
instead of running `upload` in a loop yourself:

```sh
dr-sync watch ./src
```

It shares every `upload` flag, but `--delete` defaults **on** — watch mirrors the local tree
rather than only adding to it — and `--timeout` bounds one push, not the whole process. A
filesystem event only ever triggers a rescan; it is never trusted on its own, so a missed
event costs latency, not correctness.

Flags worth knowing:

| Flag | Default | |
|---|---|---|
| `--debounce` | `2s` | wait for the tree to go quiet before pushing |
| `--min-interval` | `15s` | floor between the start of two pushes |
| `--poll-interval` | `60s` | rescan on a timer regardless of events (`0` disables it) |
| `--poll-only` | off | skip the OS watcher entirely — for network filesystems |
| `--gc-interval` | `10m` | how often unreferenced objects are swept (`0` disables it) |
| `--writer-ttl` | `5m` | how long this process advertises itself as active |

`watch` uses your OS's native filesystem notifications (inotify on Linux, kqueue on
macOS/BSD, ReadDirectoryChangesW on Windows), one watch per directory. Each one costs a
watch descriptor: a very large tree, or several `watch` processes at once, can hit a
platform limit.

- **Linux (inotify):** raise `fs.inotify.max_user_watches` (and `max_user_instances` if
  running several watchers), e.g. `sudo sysctl fs.inotify.max_user_watches=524288`.
- **macOS/BSD (kqueue):** kqueue needs one open file descriptor per watched directory, so
  raise the process's open-file limit, e.g. `ulimit -n 65536`.

If a limit is hit mid-run, `watch` warns and falls back to polling for the rest of that
directory tree rather than exiting — pass `--poll-only` to skip straight to that and avoid
the warning.

## What the container looks like

dr-sync does not store your files at their own paths. Everything lives under a reserved
prefix as immutable objects:

```
.dr-sync/manifests/0000000042-7f3a9c2b.json.xz   the file tree, one per push
.dr-sync/packs/p_9f2a1c8d3e4f5061.tar.xz         contents, named after their own digest
.dr-sync/locks/<client>-<n>.lock                 advisory claims held during a push
.dr-sync/writers/<client>-<n>.json               resident `watch` processes announcing themselves
```

That is a deliberate trade. DataRobot's intake cannot unpack xz, so genuine LZMA2 compression
means dr-sync owns the layout on both sides. The container is not browsable as files; in
exchange the transfer is compressed, deduplicated, and resumable. `dr-sync ls` shows the tree
without downloading anything.

Nothing in the container is ever overwritten, and the current manifest is simply the
highest-numbered one — which is what makes an interrupted push invisible rather than
corrupting.

## Concurrency

Two clients pushing to one container is guarded, not prevented: the API has no
compare-and-swap. dr-sync takes an advisory lease, revalidates immediately before committing,
and reports a fork afterwards if one happened. A genuine simultaneous push means one side's
update is not in the head — detected, never silently corrupted, and healed by re-running.
`--force` overrides the lease when the holder is known to be gone.

A resident `watch` process additionally announces itself in `.dr-sync/writers/`, so a peer
can tell it is active. If the container moves while `watch` is running, it holds back
`--delete` for that cycle and warns instead of racing another writer's paths; `--force`
overrides that too.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | success, including "nothing to do" |
| 1 | unexpected failure |
| 2 | usage, config, or precondition error |
| 3 | API or authentication failure |
| 4 | conflict: a lease is held, the head moved, or a fork was found |
| 5 | integrity failure: checksum mismatch or a missing pack |
| 6 | partial success: some files failed, the rest are complete |
| 130 | interrupted |

`--output json` writes one JSON object per line to stdout, with human output on stderr, so it
composes in CI.

## Development

```sh
task lint            # go mod tidy, gofumpt, vet, golangci-lint per GOOS, goreleaser check
task test            # unit tests, race detector on
task test:integration  # against a real instance; creates and deletes its own containers
task build           # ./dist/dr-sync
task install:local   # ~/.local/bin/dr-sync, so `dr sync …` runs your working tree
task plugin:package  # the dr plugin .tar.xz
task release:snapshot  # full release locally, publishes nothing
```

[SPEC.md](SPEC.md) is the source of truth for the on-container layout, the manifest format,
and the guarantees. Read it before changing sync semantics.

## Licence

Released under the Apache License 2.0; see [LICENSE.txt](LICENSE.txt). See [CONTRIBUTING.md](CONTRIBUTING.md) to contribute.
