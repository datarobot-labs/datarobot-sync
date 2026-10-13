+++
title = 'Examples'
weight = 5
+++

Runnable companions to [Quick Start]({{% relref "docs/quick-start" %}}) §4, in
[`examples/`](https://github.com/datarobot-labs/datarobot-sync/tree/main/examples).
None of these need credentials written into them — `dr-sync` resolves
`DATAROBOT_ENDPOINT`/`DATAROBOT_API_TOKEN` itself, environment first, then
`~/.config/datarobot/drconfig.yaml` (the file `dr auth login` writes). `DR_SYNC_CATALOG_ID`
is not a credential — it only names which container to sync — so it does appear directly
in a couple of these, as a placeholder to replace.

| | |
|---|---|
| [`Taskfile.yml`](https://github.com/datarobot-labs/datarobot-sync/blob/main/examples/Taskfile.yml) | `sync:push` / `sync:pull` / `sync:watch` / `sync:compact` / `sync:gc` targets to copy into a project |
| [`docker/`](https://github.com/datarobot-labs/datarobot-sync/tree/main/examples/docker) | a resident `watch` container and a periodic-push cron container, both tested live against a real container |
| [`systemd/`](https://github.com/datarobot-labs/datarobot-sync/tree/main/examples/systemd) | the same two shapes as native user-level systemd units (Linux) |
| [`launchd/`](https://github.com/datarobot-labs/datarobot-sync/tree/main/examples/launchd) | the resident-`watch` shape as a per-user LaunchAgent (macOS) |

## Docker

Two shapes, side by side in `docker-compose.yml`:

- **`watch` — resident mirror.** Runs the published `ghcr.io/datarobot-labs/dr-sync` image
  directly with a different argv; no custom image needed. Docker's
  `restart: unless-stopped` is the supervisor.
- **`cron-backup` — periodic snapshot push.** A scheduled `dr-sync upload`, built from
  `Dockerfile.cron`. This one *does* need an image of its own, because the published image
  is built `FROM cgr.dev/chainguard/static` — no shell, no package manager, nothing but the
  binary — and a scheduler needs somewhere to run commands.

```sh
cd examples/docker
docker compose up -d
docker compose logs -f watch
```

{{< callout type="info" >}}
**Why supercronic, not cron.** Traditional cron resets the environment before running a
job, so `docker run -e DATAROBOT_API_TOKEN=...` never reaches it — the classic "cron works
in a container but the job can't see its own credentials" trap. `Dockerfile.cron` runs
[`supercronic`](https://github.com/aptible/supercronic) instead: a normal foreground
process that spawns each job inheriting its own environment, exactly like any other
subprocess. The image itself is `cgr.dev/chainguard/busybox`, not the fully shell-less
`static` the published `dr-sync` image uses, because supercronic execs every crontab line
through `/bin/sh -c`.
{{< /callout >}}

Both examples mount at `/tmp/data`, not `/data` — see
[Installation]({{% relref "docs/installation" %}}) for why — so the exact same compose file
and `docker run` invocations work whether they run on your own infrastructure or inside a
DataRobot execution environment, with nothing to change. Credentials can go through
`environment:` / `docker run -e`, or a mounted read-only `drconfig.yaml`, which keeps the
token out of `docker inspect` entirely; both are wired up in `docker-compose.yml`.

To validate a crontab without waiting for the schedule:

```sh
docker build -f Dockerfile.cron -t dr-sync-cron .
docker run --rm --entrypoint supercronic dr-sync-cron -test /app/crontab
```

## systemd (Linux)

Two independent pairs — pick the one matching what you want, not both:

- **`dr-sync-watch.service`** — a resident `watch`, restarted on failure. The systemd
  equivalent of the Docker `watch` service above.
- **`dr-sync-backup.service` + `.timer`** — a periodic one-shot `upload`, systemd's native
  replacement for a cron line. The service unit does nothing on its own; the timer is what
  triggers it, and is the one you enable.

These are **user** units, so they run as you and inherit `~/.config/datarobot/drconfig.yaml`
exactly the way an interactive shell would — nothing to configure beyond the container id.

```sh
mkdir -p ~/.config/systemd/user
cp dr-sync-watch.service ~/.config/systemd/user/       # edit REPLACE_WITH_YOUR_CONTAINER_ID first
systemctl --user daemon-reload
systemctl --user enable --now dr-sync-watch.service
```

`loginctl enable-linger $USER` keeps user units running after you log out — otherwise
systemd stops them along with your session, which defeats the point of a resident watch.

## launchd (macOS)

`com.datarobot.dr-sync.watch.plist` runs a resident `dr-sync watch` as a per-user
LaunchAgent — the macOS equivalent of the systemd `watch` unit above. There is no launchd
periodic-push equivalent shipped here; `StartCalendarInterval` covers that if you want one,
or just use the Docker or plain-cron shape instead.

```sh
cp com.datarobot.dr-sync.watch.plist ~/Library/LaunchAgents/   # edit the two REPLACE_WITH_* placeholders first
launchctl load ~/Library/LaunchAgents/com.datarobot.dr-sync.watch.plist
```

No `DATAROBOT_ENDPOINT` or `DATAROBOT_API_TOKEN` anywhere in the plist: launchd runs this as
you, so `drconfig.yaml` resolves them exactly as it would in an interactive shell.
