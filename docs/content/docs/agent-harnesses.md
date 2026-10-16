+++
title = 'Agent harnesses'
weight = 5
+++

An always-on agent harness keeps its skills, memories, and sessions in a state directory. In a
container or workload, that directory is ephemeral. A restart, a reschedule, or a new node wipes it,
and the agent comes back with no memory of what it learned.

datarobot-sync closes that gap. It mirrors the state directory to a DataRobot Files container, restores it
on boot, and moves only what changed. [Hermes](https://github.com/datarobot-labs/datarobot-labs-hermes)
runs this way on DataRobot. Any harness that keeps its state under one directory can follow the same
pattern, OpenClaw-style harnesses included.

The coupling is deliberately thin. datarobot-sync knows nothing about the harness, and the harness knows
nothing about datarobot-sync. They meet at one directory and one entrypoint, so either side can change
without the other noticing.

## Sync at three points

The pattern makes one datarobot-sync call at each of three points in the container lifecycle, as the
following table shows.

| Point | Command | Purpose |
|---|---|---|
| Before the agent starts | `dr-sync download "$AGENT_HOME"` | Restore what the last run pushed. |
| While the agent runs | `dr-sync upload "$AGENT_HOME"` on a timer, or `dr-sync watch` | Keep the container current. |
| On SIGTERM | One final `upload`, bounded by a timeout | Capture changes since the last push. |

## Write the entrypoint

The following entrypoint implements all three points. Set `AGENT_HOME` to the harness state
directory and `DR_SYNC_CATALOG_ID` to the container ID from `dr-sync init`. datarobot-sync reads
`DATAROBOT_ENDPOINT` and `DATAROBOT_API_TOKEN` from the environment.

```sh
#!/bin/sh
# AGENT_HOME: the harness's state directory.
# DR_SYNC_CATALOG_ID: the container (create one with `dr-sync init`).
# DATAROBOT_ENDPOINT and DATAROBOT_API_TOKEN are read from the environment.
set -u

push() { flock /tmp/.dr-sync.lock timeout "${1:-0}" dr-sync upload "$AGENT_HOME"; }

# 1. Restore, and fail closed. A failed download must not fall through to an
#    empty start: the first upload would replace the good remote copy.
for attempt in 1 2 3 4 5; do
  dr-sync download "$AGENT_HOME" && break
  [ "$attempt" = 5 ] && { echo "restore failed; not starting empty" >&2; exit 1; }
  sleep $((attempt * 5))
done

# 2. Push while the agent runs. A plain loop is enough without a supervisor.
( while sleep 600; do push 300 || echo "upload failed; will retry" >&2; done ) &
loop=$!

# 3. Run the agent as a child, not with exec, so this shell gets SIGTERM.
my-agent --home "$AGENT_HOME" &
agent=$!
trap 'kill -TERM "$agent"' TERM INT
wait "$agent"
wait "$agent"  # the trap interrupts the first wait; wait for the real exit
kill "$loop" 2>/dev/null

# Final push, bounded to fit the termination grace period (30s here).
push 20
```

A failed download stops the boot. If the agent started empty and then uploaded, its first push would
replace the good remote copy as the head. An empty container, as on a first deploy, counts as a
successful download, so the first boot starts clean and later boots restore.

## Keep a way back

A corrupted memory file is a rollback, not a loss. Nothing in the container is ever overwritten, and
`--keep-generations 144` at one push per ten minutes retains about a day of history. To restore an
earlier state, inspect a retained generation, then download it:

```bash
dr-sync ls --generation 41
dr-sync download "$AGENT_HOME" --generation 41
```

For what a generation is, see [Container layout]({{% relref "docs/container-layout" %}}).

## Avoid these failure modes

Four failure modes recur in harness deployments. Each has a fix that takes one line.

- **Live SQLite databases.** Copying `state.db`, `-wal`, and `-shm` at different instants can yield a
  mismatched set that silently drops recent writes. Take a `sqlite3 .backup` snapshot before each
  upload, and exclude the live files.
- **Two writers.** Two writers to one container are detected, not prevented. See
  [Concurrency]({{% relref "docs/concurrency" %}}). Create a throwaway container with `dr-sync init`
  for local runs instead of pointing a checkout at production.
- **Stale baked-in files.** A download can overwrite what the current image ships. List baked-in
  config, skills, and plugins, plus caches and logs, in `.drsyncignore`.
- **Overlapping pushes.** The timer loop and the shutdown push can contend for the lease. The
  `flock` in the entrypoint serializes them.

A supervisor can act on the result of each call. Each failure class has its own exit code, so an
entrypoint can retry, stop, or alert. See [Exit codes]({{% relref "docs/exit-codes" %}}).
