# launchd (macOS)

`com.datarobot.dr-sync.watch.plist` runs a resident `dr-sync watch` as a per-user LaunchAgent
— the macOS equivalent of `examples/systemd/dr-sync-watch.service`. There is no launchd
periodic-push equivalent shipped here; `StartCalendarInterval` covers that if you want one, or
just use `examples/docker/` or plain cron for that shape (see
[`../../docs/quickstart.md`](../../docs/quickstart.md) § 4).

## Install

```sh
cp com.datarobot.dr-sync.watch.plist ~/Library/LaunchAgents/
```

Edit the copy: replace `REPLACE_WITH_THE_DIRECTORY_TO_MIRROR` and
`REPLACE_WITH_YOUR_CONTAINER_ID`, and confirm `/usr/local/bin/dr-sync` matches where dr-sync
actually is (`which dr-sync`).

```sh
launchctl load ~/Library/LaunchAgents/com.datarobot.dr-sync.watch.plist
```

No `DATAROBOT_ENDPOINT` or `DATAROBOT_API_TOKEN` anywhere in the plist: launchd runs this as
you, so `~/.config/datarobot/drconfig.yaml` — the file `dr auth login` writes — resolves them
exactly as it would in an interactive shell. This was verified live against a real container
while writing this example, with the plist carrying nothing but the container id.

## Checking on it

```sh
launchctl list | grep dr-sync
tail -f /tmp/dr-sync-watch.log
```

## Removing it

```sh
launchctl unload ~/Library/LaunchAgents/com.datarobot.dr-sync.watch.plist
rm ~/Library/LaunchAgents/com.datarobot.dr-sync.watch.plist
```
