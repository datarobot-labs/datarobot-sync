+++
title = 'Installation'
weight = 2
+++

## Requirements

Works against DataRobot 11.4 and later. The Files API's `allFiles` listing endpoint gained a
`recursive` parameter in 11.7; against an older instance datarobot-sync detects the resulting 400 and
falls back to the equivalent request automatically, no configuration needed.

## As a `dr` plugin

Install it into the [DataRobot CLI](https://cli.datarobot.com) from a release or a local archive:

```sh
dr plugin install sync --url https://github.com/datarobot-labs/datarobot-sync/releases/latest/download/sync-<version>.tar.xz
dr plugin install sync --file dist/sync-<version>.tar.xz    # from `task plugin:package`
datarobot sync --help
```

That installs under `$XDG_CONFIG_HOME/datarobot/plugins/sync/`. For development, a
`dr-`-prefixed binary anywhere on `PATH` is discovered just as well — which is what
`task install:local` does.

## As a standalone binary

Download the archive for your platform from
[releases](https://github.com/datarobot-labs/datarobot-sync/releases), or build it:

```sh
task install:local     # builds into ~/.local/bin/dr-sync
```

Building from source needs Go as pinned in `.tool-versions`; with asdf, run
`asdf reshim` after first checkout so the `go` shim exists.

## As a container

The mount target is `/tmp/data`, not `/data`: a DataRobot execution environment's container
only has `/tmp` writable, and mounting there means this works unmodified whether it runs on
your own infra or inside one of those.

```sh
docker run --rm -v "$PWD:/tmp/data" \
  -e DATAROBOT_ENDPOINT -e DATAROBOT_API_TOKEN -e DR_SYNC_CATALOG_ID \
  ghcr.io/datarobot-labs/dr-sync:latest upload /tmp/data
```

See [Examples]({{% relref "docs/examples" %}}) for running `watch` as a resident
container/systemd/launchd service, and `upload` on a cron schedule.
