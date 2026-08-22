+++
title = 'Quick Start'
weight = 1
+++

A walkthrough from zero to a container that stays mirrored automatically. Everything below
is copy-pasteable; the sample output is lightly edited from a real run.

See [Installation]({{% relref "docs/installation" %}}) for install options and
[Usage]({{% relref "docs/usage" %}}) for the full command reference.

## 1. Credentials

Nothing to configure if you already ran `dr auth login`. `dr-sync` resolves the endpoint
and token itself, in this order:

1. `--endpoint` (there is no `--token` flag, deliberately — a flag would put it in the
   process table and the shell history)
2. `DATAROBOT_ENDPOINT` / `DATAROBOT_API_TOKEN`
3. a config file, found at `--config` → `$DATAROBOT_CONFIG` →
   `$XDG_CONFIG_HOME/datarobot/drconfig.yaml` → `~/.config/datarobot/drconfig.yaml`

That last file is the same one `dr auth login` writes. If you already use the `dr` CLI,
there is nothing to set up — every example below was tested with **no environment
variables set at all**, credentials coming entirely from that file.

## 2. Create a container and push

```sh
$ dr-sync init --name "dr-sync-quickstart"
create  container 6a88a0e0de792a9d144a69c9
seed    0000000000-e67a202c written
done    container ready
6a88a0e0de792a9d144a69c9

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

`DR_SYNC_CATALOG_ID` is not a credential — it just names which container to sync — so it
is fine to export it for the rest of a shell session or bake it into a Taskfile.

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

Running `dr-sync upload` in a loop yourself pays for process startup, config resolution
and a fresh connection every time. [`dr-sync watch`]({{% relref "docs/watch" %}}) stays
resident and pushes only when the tree actually settles:

```sh
$ dr-sync watch ./my-project --verbose
```

`--delete` defaults **on** for `watch` — unlike `upload`, which stays additive — because
watch exists to mirror the tree, not accrete it.

## 4. Run it unattended

- **A resident mirror** (`watch`) is already a long-running process. It wants a process
  supervisor — Docker's restart policy, systemd, or launchd — not cron.
- **A periodic snapshot push** (`upload` on a timer) is a one-shot command that wants
  exactly cron, or systemd's timer equivalent.

The published image's entrypoint is the binary itself, so a resident watch needs no custom
image. The mount target is `/tmp/data`, not `/data` — a DataRobot execution environment's
container only has `/tmp` writable, and mounting there means the same command works
unmodified whether it runs on your own infrastructure or inside one of those:

```sh
docker run -d --name dr-sync-watch --restart unless-stopped \
  -v "$PWD:/tmp/data" \
  -e DATAROBOT_ENDPOINT -e DATAROBOT_API_TOKEN -e DR_SYNC_CATALOG_ID \
  ghcr.io/datarobot/dr-sync:latest watch /tmp/data --delete
```

For a periodic push instead, [Examples]({{% relref "docs/examples" %}}) has a Docker image
built on [`supercronic`](https://github.com/aptible/supercronic) rather than `cron`/`crond` —
traditional cron resets the environment before running a job, so `docker run -e
DATAROBOT_API_TOKEN=...` never reaches it; supercronic is a normal foreground process that
inherits its own environment like any other subprocess. The same page covers native systemd
units (Linux) and a launchd agent (macOS) for running without a container at all.

Naming the token directly in a crontab line would put it in `ps` output and the crontab
file itself — for cron, let `~/.config/datarobot/drconfig.yaml` carry it instead.

## 5. Checking on it

```sh
dr-sync inspect --packs      # generations, dead bytes, what compact would reclaim
dr-sync ls --generation 3    # what an older push looked like, no download
dr-sync compact ./my-project # reclaim dead bytes from a churny watch session
dr-sync gc                   # tier-1 cleanup only, safe to run from CI on a timer
```

See [Concurrency and garbage collection]({{% relref "docs/concurrency" %}}) for why a
long `watch` session produces packs that `gc` alone cannot reclaim.
