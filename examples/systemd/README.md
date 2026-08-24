# systemd units

Two independent pairs — pick the one matching what you want, not both:

- **`dr-sync-watch.service`** — a resident `watch`, restarted on failure. The systemd
  equivalent of the Docker `watch` service in [`../docker/`](../docker/).
- **`dr-sync-backup.service` + `.timer`** — a periodic one-shot `upload`, systemd's native
  replacement for a cron line. The service unit does nothing on its own; the timer is what
  triggers it, and is the one you enable.

These are **user** units (`%h` — the user's home directory — only expands correctly at that
scope), so they run as you, not as root, and inherit `~/.config/datarobot/drconfig.yaml`
exactly the way an interactive shell would — nothing to configure beyond the container id.

## Install

```sh
mkdir -p ~/.config/systemd/user
cp dr-sync-watch.service ~/.config/systemd/user/
# — or —
cp dr-sync-backup.service dr-sync-backup.timer ~/.config/systemd/user/
```

Edit the copy: replace `REPLACE_WITH_YOUR_CONTAINER_ID` and, if `my-project` is not literally
`~/my-project`, the `ExecStart` path.

```sh
systemctl --user daemon-reload
systemctl --user enable --now dr-sync-watch.service
# — or —
systemctl --user enable --now dr-sync-backup.timer
```

`loginctl enable-linger $USER` keeps user units running after you log out — otherwise
systemd stops them along with your session, which defeats the point of a resident watch.

## Checking on it

```sh
systemctl --user status dr-sync-watch.service
journalctl --user -u dr-sync-watch.service -f
systemctl --user list-timers dr-sync-backup.timer   # next/last run for the periodic push
```
