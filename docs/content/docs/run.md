+++
title = 'Run a command against shared state'
weight = 4
+++

`datarobot sync run` makes a directory a working copy of a container for one command. Use it when
a tool keeps its state in a directory and several people or CI jobs share that state, for example
the Pulumi file backend.

```sh
datarobot sync run ./state -- pulumi up
```

Put `--` before the command so its flags are not read as datarobot-sync flags.

## What it does

1. Takes the container's lease. A second `run` is refused with exit code 4 while the first holds it.
2. Pulls the head into `./state` and mirrors it. Every local file the pull replaces or removes
   moves to `.dr-sync-backup/<timestamp>/` in that directory, so a local edit is never lost.
3. Runs the command in the current directory. `DR_SYNC_ROOT` holds the absolute path of the
   synced directory.
4. Pushes the directory as the successor of the head it pulled, whether or not the command
   succeeded. The state a failed run leaves behind is what the next run starts from.
5. Releases the lease.

datarobot-sync never uploads `.dr-sync-backup/`.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | The command succeeded and the push landed. |
| 7 | The command failed. The push still happened. |
| 4 | Another run holds the lease, or the head moved during the run. |
| 6 | The pull left paths out, or skipped an entry, so the command did not start. |
| 130 | The run was interrupted. The push still followed if the command had started. |

A sync failure wins over a command failure.

If the head moved during the run, which a writer that ignored the lease, a lease that lapsed, or
`--no-lock` can cause, nothing is pushed and the command's changes stay in the directory. Run
`datarobot sync download <path> --backup` to bring in the new head and keep your files under
`.dr-sync-backup/`, then merge by hand. Until you do, the next `run` moves those files aside,
warns, and runs the command on the head.

A pull that skips an entry, such as a symlink whose target is an absolute path, also stops the
run with exit code 6, because the push would drop it from the container.

## Pulumi

The Pulumi file backend keeps state in a directory, so `run` turns it into shared, durable
state. Stack selection and configuration are local to each machine: select the stack inside the
run, and share `Pulumi.<stack>.yaml` through your repository.

```sh
export PULUMI_BACKEND_URL="file://$HOME/pulumi-state"
export PULUMI_CONFIG_PASSPHRASE=<shared passphrase>

datarobot sync run "$HOME/pulumi-state" --exclude-from pulumi.drsyncignore -- \
  sh -c 'pulumi stack select dev && exec pulumi up --yes'
```

Keep Pulumi's lock files, logs, plugin cache, and per-machine settings out of the container with
the patterns below, saved in `pulumi.drsyncignore` in your repository. They match at any depth. If
`PULUMI_HOME` shares the directory with the backend, as in some prebuilt images, the plugin cache
alone is around 100 MB, and every Pulumi command outside `run` writes a log that the next run
would move aside with a warning.
A `.drsyncignore` inside the state directory works too, but `run` reads it before the pull, so a
machine that does not have it yet pushes what it would have excluded.

```text
plugins/
locks/
bin/
templates/
logs/
workspaces/
credentials.json
```

## Interrupts

Ctrl-C reaches the command through the terminal, so the command winds down and writes its state.
The push still follows. A second interrupt anywhere in the run cancels the push. An interrupt
before the command starts ends the run without running it, and nothing is pushed.

SIGTERM goes to datarobot-sync only, which passes it to the command but not to the processes the
command started. Put `exec` before the last command in a shell wrapper, as in the Pulumi example,
so the signal reaches the program that holds the state.

## Limits

- `run` takes the upload flags except `--expect-head`, which it sets itself, and `--compact`.
  `--delete` is on, so a path the command removes leaves the container.
- The pull moves local files that differ from the head into `.dr-sync-backup/` and prints a
  warning that names it.
- `--dry-run` and `--output json` are refused. The first has nothing to preview, and the second
  would mix events into the command's output. Use `status` to preview a push.
- `--timeout` bounds each transfer, not the command.
- A lease needs a process that renews it, so there is no `lock` or `unlock`. For a manual
  session, run a shell under the lease: `datarobot sync run ./state -- "$SHELL"`.
