+++
title = 'Watch'
weight = 4
+++

`datarobot sync watch <path>` is a resident process that keeps pushing a directory as it
changes, instead of running `upload` in a loop yourself:

```sh
datarobot sync watch ./src
```

It shares every `upload` flag, but `--delete` defaults **on** — watch mirrors the local
tree rather than only adding to it — and `--timeout` bounds one push, not the whole
process. A filesystem event only ever triggers a rescan; it is never trusted on its own,
so a missed event costs latency, not correctness.

## Flags worth knowing

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
watch descriptor: a large tree, or several `watch` processes at once, can hit a
platform limit.

- **Linux (inotify):** raise `fs.inotify.max_user_watches` (and `max_user_instances` if
  running several watchers), e.g. `sudo sysctl fs.inotify.max_user_watches=524288`.
- **macOS/BSD (kqueue):** kqueue needs one open file descriptor per watched directory, so
  raise the process's open-file limit, e.g. `ulimit -n 65536`.

If a limit is hit mid-run, `watch` warns and falls back to polling for the rest of that
directory tree rather than exiting — pass `--poll-only` to skip straight to that and
avoid the warning.

{{< callout type="info" >}}
A watch-descriptor limit does not surface as anything that names itself: inotify
exhaustion is `ENOSPC` ("no space left on device"), kqueue exhaustion is `EMFILE` ("too
many open files"), because kqueue needs one fd per watched path. `internal/watcher`
checks for both with `errors.Is` and degrades to polling rather than dying.
{{< /callout >}}

## The resident-writer registry

Each `watch` process announces itself under `.dr-sync/writers/` in the container, so a
peer can tell it is active. There is no mutable pointer file — nothing in the container
is ever overwritten — so each resident writer instead owns a sequence-named object,
refreshed the same way a lease is: write the next sequence, delete the previous one. The
registry is the union of whatever is live.

If the container moves while `watch` is running — the head advanced, or a fork was
detected — `watch` holds back `--delete` for that cycle and warns, rather than deleting
through it. `--force` overrides that too.

## What `watch` does not do

GC does not run per push. Tier-1 GC is the expensive tail of a commit, and a daemon
pushing every few seconds would pay that cost far more often than it needs to, for less
benefit — many small pushes produce many *mostly*-live packs, which tier-1's
`liveRatio == 0` rule cannot reclaim regardless of how often it runs. Instead `watch`
runs one garbage-collection sweep on a timer (`--gc-interval`) and once more on clean
shutdown. See [Concurrency and garbage collection]({{% relref "docs/concurrency" %}}) for
why a long watch session still wants an occasional `datarobot sync compact`.
