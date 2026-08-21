# Quickstart

A walkthrough from zero to a container that stays mirrored automatically, with runnable
examples for a local dev loop, a resident background service, and a container/cron setup for
unattended environments. Everything below is copy-pasteable; the sample output is lightly
edited from a real run.

See [`../README.md`](../README.md) for install options and the full flag reference, and
[`../SPEC.md`](../SPEC.md) for how the on-container layout and guarantees work.

## 1. Credentials — nothing to configure if you already ran `dr auth login`

dr-sync resolves the endpoint and token itself, in this order:

1. `--endpoint` (there is no `--token` flag, deliberately — a flag would put it in the
   process table and the shell history)
2. `DATAROBOT_ENDPOINT` / `DATAROBOT_API_TOKEN`
3. a config file, found at: `--config` → `$DATAROBOT_CONFIG` → `$XDG_CONFIG_HOME/datarobot/drconfig.yaml`
   → `~/.config/datarobot/drconfig.yaml`

That last file is the same one `dr auth login` writes. If you use the `dr` CLI already, there
is nothing to set up — every example below was tested with **no environment variables set at
all**, credentials coming entirely from that file:

```sh
env -i HOME="$HOME" PATH="$PATH" dr-sync init --name "my-project"
```

For a CI job or a container, set the three environment variables instead — see
[§4](#4-run-it-unattended) below.

## 2. Create a container and push

```sh
$ dr-sync init --name "dr-sync-quickstart"
create  container 6a88a0e0de792a9d144a69c9
seed    0000000000-e67a202c written
done    container ready
6a88a0e0de792a9d144a69c9

export DR_SYNC_CATALOG_ID=6a88a0e0de792a9d144a69c9
dr-sync upload <path>
```

The last two lines above are `init` suggesting the next step, not something you typed —
`DR_SYNC_CATALOG_ID` is not a credential, just the container's id, so it is fine to export it
for the rest of the shell session, put it in a project's `.envrc`, or bake it into a Taskfile
(see [§3](#3-a-local-dev-loop)). Taking the suggestion:

```sh
$ export DR_SYNC_CATALOG_ID=6a88a0e0de792a9d144a69c9
$ dr-sync upload ./my-project
remote  empty container; this will be generation 0
scan    142 entries  hashed 138 (4.2 MiB)  reused 0
plan    new 138  changed 0  unchanged 0  remote-only 0
plan    upload 138 blobs, 4.2 MiB
pack    3 packs  4.2 MiB in  1.1 MiB out  (3.8x)
commit  0000000000-e67a202c committed
done    0000000000-e67a202c  1.1 MiB uploaded (3.8x from 4.2 MiB)
```

Pull it back somewhere else, and confirm it round-trips exactly:

```sh
$ dr-sync download ./restored
remote  generation 0  138 files  4.2 MiB  3 packs
scan    up-to-date 0  stale/missing 138  extraneous 0
plan    138 files from 3 packs (~1.1 MiB to transfer)
fetch   1.1 MiB transferred from 3 packs
apply   138 files  9 dirs  0 symlinks
done    0000000000-e67a202c  138 written  1.1 MiB transferred

$ diff -r ./my-project ./restored -x .dr-sync
$ echo $?
0
```

## 3. A local dev loop

Running `dr-sync upload` in a loop yourself pays for process startup, config resolution and a
fresh connection every time. `dr-sync watch` stays resident and pushes only when the tree
actually settles:

```sh
$ dr-sync watch ./my-project --verbose
```

`--delete` defaults **on** for `watch` — unlike `upload`, which stays additive — because watch
exists to mirror the tree, not accrete it. Everything is still just a hint to itself: every
trigger, whatever caused it, re-walks and re-hashes the tree exactly like `upload` does, so a
missed filesystem event only costs latency. From a real run, editing one file and then writing
five more in quick succession:

```json
{"event":"watch","message":"change (2 events)"}
{"event":"plan","message":"new 0  changed 1  unchanged 3  remote-only 0"}
{"event":"upload","generation":3, ...}
...
{"event":"watch","message":"change (1 events)"}
{"event":"plan","message":"new 1  changed 0  unchanged 4  remote-only 0"}
{"event":"upload","generation":4, ...}
```

Five rapid writes to the same file became **one** new generation, not five — that is
`--debounce` (2 s default) coalescing the burst, and `--min-interval` (15 s default) is the
floor that keeps a noisy tree from minting a generation per keystroke. Deleting a file locally
drops it from the container on the next cycle, same as `upload --delete`:

```
plan    new 0  changed 0  unchanged 3  remote-only 1 (1 dropped)
done    0000000005-c931b1f7  0 B uploaded
```

`Ctrl-C` shuts down cleanly: the in-flight cycle finishes, one last garbage-collection sweep
runs, and this process's entry in the container's writer registry
(`.dr-sync/writers/`, ahead of the eventual multi-writer FUSE mode) is removed — nothing is
left dangling for the next run to trip over.

[`examples/Taskfile.yml`](../examples/Taskfile.yml) wraps this into `task sync:push`,
`sync:pull`, `sync:watch` and `sync:status` targets you can drop into a project.

## 4. Run it unattended

Two different shapes, and they want different tools:

- **A resident mirror** (`watch`) is already a long-running process. It wants a process
  supervisor — Docker's restart policy, systemd, or launchd — not cron.
- **A periodic snapshot push** (`upload` on a timer) is a one-shot command that wants exactly
  cron, or systemd's timer equivalent.

### As a container

The published image's entrypoint is the binary itself, so a resident watch needs no custom
image. The mount target is `/tmp/data`, not `/data` — a DataRobot execution environment's
container only has `/tmp` writable, and mounting there means the same command works
unmodified whether it runs on your own infra or inside one of those:

```sh
docker run -d --name dr-sync-watch --restart unless-stopped \
  -v "$PWD:/tmp/data" \
  -e DATAROBOT_ENDPOINT -e DATAROBOT_API_TOKEN -e DR_SYNC_CATALOG_ID \
  ghcr.io/datarobot/dr-sync:latest watch /tmp/data --delete
```

For a periodic push, [`examples/docker/`](../examples/docker/) builds a small
`chainguard/busybox` + [`supercronic`](https://github.com/aptible/supercronic) image on top
of the same published binary — supercronic rather than cron/crond because it is a normal
foreground process that does not reset the environment before running a job, which is what
lets `docker run -e` reach a scheduled job at all. Both patterns, side by side, tested live
against a real container as part of writing this doc:

```sh
cd examples/docker
docker compose up -d
```

### As a local service, no container

If dr-sync is already installed on the machine that should stay synced (`task install:local`,
or a release binary on `PATH`), running it as a systemd service or a macOS LaunchAgent needs no
credentials in the unit file at all — it inherits the same `~/.config/datarobot/drconfig.yaml`
fallback described in [§1](#1-credentials--nothing-to-configure-if-you-already-ran-dr-auth-login):

```sh
# Linux, resident watch:
cp examples/systemd/dr-sync-watch.service ~/.config/systemd/user/
systemctl --user enable --now dr-sync-watch.service

# Linux, periodic push instead:
cp examples/systemd/dr-sync-backup.{service,timer} ~/.config/systemd/user/
systemctl --user enable --now dr-sync-backup.timer

# macOS, resident watch:
cp examples/launchd/com.datarobot.dr-sync.watch.plist ~/Library/LaunchAgents/
launchctl load ~/Library/LaunchAgents/com.datarobot.dr-sync.watch.plist
```

See [`examples/systemd/README.md`](../examples/systemd/README.md) and
[`examples/launchd/README.md`](../examples/launchd/README.md) for the paths you need to edit
first (the directory to sync and the container id).

Plain cron works too, and has the identical environment gotcha the Docker cron image works
around — a user crontab does not inherit your shell's exports either:

```cron
# m h dom mon dow  command
*/15 * * * *  DR_SYNC_CATALOG_ID=6a88a0e0de792a9d144a69c9 /usr/local/bin/dr-sync upload --delete /path/to/project >> /var/log/dr-sync.log 2>&1
```

Naming the token in a crontab line the same way would put it in `ps` output and the crontab
file itself — for cron, let `~/.config/datarobot/drconfig.yaml` carry it instead (cron jobs
run as the user who owns the crontab, so `$HOME` resolves the same way it does interactively).

## 5. Checking on it

```sh
dr-sync inspect --packs      # generations, dead bytes, what compact would reclaim
dr-sync ls --generation 3    # what an older push looked like, no download
dr-sync compact ./my-project # reclaim dead bytes from a churny watch session
dr-sync gc                   # tier-1 cleanup only, safe to run from CI on a timer
```

A long `watch` session produces many small, mostly-live packs that `gc` cannot reclaim (see
[`../SPEC.md`](../SPEC.md) § Garbage and compaction) — that is what the periodic
`dr-sync compact` hint in `watch`'s own output is for.
