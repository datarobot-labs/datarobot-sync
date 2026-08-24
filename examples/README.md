# Examples

Runnable companions to [`../docs/quickstart.md`](../docs/quickstart.md). Read that first —
each of these is referenced from there with the context for when you'd reach for it.

| | |
|---|---|
| [`Taskfile.yml`](Taskfile.yml) | `sync:push` / `sync:pull` / `sync:watch` / `sync:compact` / `sync:gc` targets to copy into a project |
| [`docker/`](docker/) | a resident `watch` container and a periodic-push cron container, both tested live against a real container |
| [`systemd/`](systemd/) | the same two shapes as native user-level systemd units (Linux) |
| [`launchd/`](launchd/) | the resident-`watch` shape as a per-user LaunchAgent (macOS) |

None of these need credentials written into them. dr-sync resolves
`DATAROBOT_ENDPOINT`/`DATAROBOT_API_TOKEN` itself — environment first, then
`~/.config/datarobot/drconfig.yaml` (the file `dr auth login` writes) — so every example here
either passes them through from its own environment or, for the two local-service examples,
inherits that file directly by running as your own user. `DR_SYNC_CATALOG_ID` is not a
credential — it only names which container to sync — so it does appear directly in a couple
of these, as a placeholder to replace.
