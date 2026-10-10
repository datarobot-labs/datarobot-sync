# Docker examples

Start with [`../../docs/quickstart.md`](../../docs/quickstart.md) § 4 for the narrative
version. This directory has the runnable pieces:

| File | What it is |
|---|---|
| `docker-compose.yml` | both patterns below, side by side |
| `Dockerfile.cron` + `crontab` | the periodic-push image |

```sh
docker compose up -d
docker compose logs -f watch
```

## Two shapes

**`watch` — resident mirror.** Runs the published `ghcr.io/datarobot-labs/dr-sync` image directly
with a different argv; no custom image needed. Docker's `restart: unless-stopped` is the
supervisor.

**`cron-backup` — periodic snapshot push.** A scheduled `dr-sync upload`, built from
`Dockerfile.cron`. This one *does* need an image of its own, because the published image is
built `FROM cgr.dev/chainguard/static` — no shell, no package manager, nothing but the binary
— and a scheduler needs somewhere to run commands.

## Why supercronic, not cron

`Dockerfile.cron` runs [`supercronic`](https://github.com/aptible/supercronic) rather than
`cron`/`crond`. The reason is not style: traditional cron resets the environment before
running a job (it is designed to run as root and drop privileges per job), which means
`docker run -e DATAROBOT_API_TOKEN=...` never reaches a job cron runs — the classic
"cron works in a container but the job can't see its own credentials" trap.

supercronic is a normal foreground process, not a setuid daemon. It spawns each job the way
any Go program spawns a subprocess — inheriting its own environment unless told otherwise —
so whatever `docker run -e` or `environment:` set is simply there. Verified directly while
building this example:

```sh
$ docker run -e MY_TEST_VAR=hello-from-docker-run ... # crontab: echo "got: $MY_TEST_VAR"
got: hello-from-docker-run
```

The image itself is `cgr.dev/chainguard/busybox`, not the fully shell-less `static` the
published dr-sync image uses: supercronic execs every crontab line through `/bin/sh -c`,
so it needs the one thing `static` deliberately omits. `busybox` is the smallest Wolfi/
Chainguard image that still has it, and both images run as chainguard's nonroot user
(uid 65532, `$HOME=/home/nonroot`) by default — no root anywhere in this picture.

## Credentials

Either works, and both are wired up in `docker-compose.yml`:

- `environment:` / `docker run -e` — `DATAROBOT_ENDPOINT`, `DATAROBOT_API_TOKEN`,
  `DR_SYNC_CATALOG_ID` (see the commented-out alternative at the bottom of the compose file).
- A mounted `drconfig.yaml`, read-only, at `/home/nonroot/.config/datarobot/drconfig.yaml` —
  keeps the token out of `docker inspect` entirely.

`DR_SYNC_CATALOG_ID` is not a credential (it only names the container), so it stays in
`environment:` either way.

## Why `/tmp/data`

Both examples mount at `/tmp/data`, not `/data`: a DataRobot execution environment's
container only has `/tmp` writable. Mounting there means the exact same compose file and
`docker run` invocations work whether they run on your own infrastructure or inside one of
those, with nothing to change.

## Validating a crontab without waiting for the schedule

```sh
docker build -f Dockerfile.cron -t dr-sync-cron .
docker run --rm --entrypoint supercronic dr-sync-cron -test /app/crontab
```
