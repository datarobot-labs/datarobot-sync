+++
title = 'Usage'
weight = 3
+++

```sh
# First time
datarobot sync init --name "my project"
export DR_SYNC_CATALOG_ID=<the id it printed>

# Push and pull
datarobot sync upload   ./src
datarobot sync download ./restored

# See what a push would do, change nothing
datarobot sync status ./src

# Make the remote mirror the local tree exactly, dropping paths that are gone
datarobot sync upload ./src --delete

# Make the local tree mirror the remote exactly
datarobot sync download ./src --delete

# Only part of the tree
datarobot sync download ./src --prefix docs/ --prefix src/api/

# Roll back to a retained older push
datarobot sync ls --generation 41
datarobot sync download ./src --generation 41

# What is in there, and what is reclaimable
datarobot sync inspect --packs
datarobot sync compact ./src     # rewrite sparse packs, run from a complete checkout
datarobot sync gc                # delete unreferenced objects, moves no data
```

## Shell completions

Contributed by cobra:

```sh
dr-sync completion bash > /usr/local/etc/bash_completion.d/dr-sync
dr-sync completion zsh  > "${fpath[1]}/_dr-sync"
```

## Exclusions

Gitignore syntax, from `--exclude`, `--exclude-from`, or a `.drsyncignore` in the sync
root:

```
node_modules/
*.log
!important.log
```

## Shared flags

`--catalog-id` (`DR_SYNC_CATALOG_ID`), `--endpoint` (`DATAROBOT_ENDPOINT`), `--config`
(`DATAROBOT_CONFIG`, default `~/.config/datarobot/drconfig.yaml`), `--meta-prefix`
(`.dr-sync/`), `--check auto|full|size|mtime`, `--exclude`/`--include`/`--exclude-from`
(gitignore-style, matched on the relative POSIX path), `--jobs`, `--timeout`,
`--retries`, `-v/--verbose`, `-q/--quiet`, `--output text|json`, `--no-color`,
`--dry-run`, `--yes`. The token comes from `DATAROBOT_API_TOKEN` or the config file
only — no flag, so it stays out of `ps` and shell history.

`upload` adds `--delete`, `--level 0|1|3|6|9` (default 6), `--pack-size`,
`--pack-input-max`, `--big-file-threshold`, `--slice-size`, `--max-stage-bytes`,
`--max-stage-files`, `--keep-generations`, `--gc-grace`, `--no-gc`, `--auto-compact`,
`--compact-threshold`, `--no-lock`, `--force`, `--follow-symlinks`,
`--overwrite error|replace`.

`download` adds `--delete`, `--delete-excluded`, `--prefix` (repeatable),
`--generation`/`--manifest` (pull a retained older generation), `--parallel`,
`--link-duration`, `--no-local-reuse`, `--preserve-special-modes`,
`--allow-unsafe-links`, `--tmpdir` (must share the filesystem with root).

See [Watch]({{% relref "docs/watch" %}}) for the flags specific to the resident
mirroring mode.

## Exit codes

`--output json` writes one JSON object per line to stdout, with human output on
stderr, so it composes in CI. See [Exit codes]({{% relref "docs/exit-codes" %}}) for
what each one means.
