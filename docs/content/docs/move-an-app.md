+++
title = 'Move an app between installs'
weight = 5
+++

A move is a redeploy. Deploy the same repository to the new install with that install's settings. Do not copy the running objects, because IDs, credentials and environments can differ per install. Look up what the app depends on by name on the target install.

This guide covers a Custom Application deployed with Pulumi. Each install keeps the Pulumi state in its own Files container, so a teammate can continue the work from another machine, and a wiped Codespace loses nothing. The commands use [`run`]({{% relref "docs/run" %}}) to take a lease, pull the state, run Pulumi, and push the state back.

## What you need

- The app repository, with the Pulumi program in it.
- An account and an API key on the target install.
- The stack passphrase. Ask your team, and never commit it.
- On a laptop: `pulumi`, the `dr` CLI and the sync plugin. A Codespace has `pulumi` and `dr` already.

## Pick where you run the commands

| Does your laptop reach the target install's API? | Run from |
| --- | --- |
| Yes, for example over VPN | Your laptop |
| No, only a browser reaches it | A Codespace in the target install |

The deploy command is the same in both.

## Set up the machine

Install the tools once per machine. Run the `export` and `STATE_DIR` lines in every new terminal.

### Laptop

[Install the plugin]({{% relref "docs/installation" %}}), then point the shell at the target install. The second line reads the key without showing it.

```sh
export DATAROBOT_ENDPOINT="https://<install-host>/api/v2"
read -rs DATAROBOT_API_TOKEN && export DATAROBOT_API_TOKEN
export PULUMI_BACKEND_URL="file://$HOME/pulumi-state/<install>"
STATE_DIR="$HOME/pulumi-state/<install>/.pulumi"
mkdir -p "$STATE_DIR"
```

On a laptop the plugin command is `datarobot sync`. Use it wherever this page says `dr-sync`.

If the install uses a private certificate authority, get its CA certificate file from the install's administrator and add this line. It covers both `dr-sync` and Pulumi.

```sh
export SSL_CERT_FILE=/path/to/install-ca.pem
```

### Codespace

The Codespace is already signed in to its own install, and its Pulumi backend is preset. Paths on this page use `$STORAGE`, the Codespace's persistent folder, because `~` does not always point there.

```sh
STATE_DIR="${PULUMI_HOME%/}"
STORAGE="$(dirname "$STATE_DIR")"
export DR_SYNC_NO_PRESIGN=1
export PATH="$STORAGE/tools/bin:$PATH"
```

Check that the Codespace reaches the API:

```sh
curl -sS -m 20 -o /dev/null -w '%{http_code}\n' \
  -H "Authorization: Bearer $DATAROBOT_API_TOKEN" "$DATAROBOT_ENDPOINT/version/"
```

It prints `200`. On a timeout, stop and ask the install's administrator, because nothing on this page works without the API.

If the Codespace has internet, download the plugin and clone the app repository:

```sh
mkdir -p "$STORAGE/tools/bin"
curl -sSL "https://github.com/datarobot-labs/datarobot-sync/releases/download/v<version>/dr-sync_<version>_linux_amd64.tar.gz" \
  | tar -xz -C "$STORAGE/tools/bin" dr-sync
git clone <repository-url> "$STORAGE/<app>"
```

If the install has no internet, bring in three files first:

1. On a machine with internet, download `dr-sync_<version>_linux_amd64.tar.gz` from the [datarobot-sync releases](https://github.com/datarobot-labs/datarobot-sync/releases) and `pulumi-resource-datarobot-v<version>-linux-amd64.tar.gz` from the [provider releases](https://github.com/datarobot-community/pulumi-datarobot/releases). The provider version must match `pip show pulumi-datarobot` in the Codespace.
2. In the app repository, pack the history into one file:

   ```sh
   git bundle create repo.bundle --all
   ```

3. Put the three files in one folder. In the target install, go to **Registry** > **Files**, select **+**, and upload the folder, so the files form one Files item. Copy the item's ID. The files keep their plain names inside the item. Uploading the files one at a time creates one item per file, and the download step below then needs one ID per file.
4. In the Codespace terminal:

   ```sh
   mkdir -p "$STORAGE/tools/bin" && cd "$STORAGE/tools"
   for f in dr-sync_<version>_linux_amd64.tar.gz pulumi-resource-datarobot-v<version>-linux-amd64.tar.gz repo.bundle; do
     curl -sS -H "Authorization: Bearer $DATAROBOT_API_TOKEN" -o "$f" \
       "$DATAROBOT_ENDPOINT/files/<item-id>/file/?fileName=$f"
   done
   tar -xzf dr-sync_<version>_linux_amd64.tar.gz -C bin dr-sync && chmod +x bin/dr-sync
   pulumi plugin install resource datarobot v<version> --file pulumi-resource-datarobot-v<version>-linux-amd64.tar.gz
   pulumi plugin ls
   git clone repo.bundle "$STORAGE/<app>"
   ```

`pulumi plugin ls` lists `datarobot` with the version you installed. Keep the files in the Files item. You need them again if the Codespace is wiped.

To bring in later changes, upload a new `repo.bundle`, download it the same way, and run `git pull "$STORAGE/tools/repo.bundle"` in the checkout.

To send changes made in the Codespace back, bundle only the new commits, move the bundle to the other machine through the Files item, and pull it there. Git merges the changes, and a conflict stops the pull as usual.

```sh
git bundle create changes.bundle <last-shared-commit>..main
git pull changes.bundle main
```

Run the first line where the changes are, and the second on the machine that receives them. The pull fails with `Repository lacks these prerequisite commits` when that machine does not have `<last-shared-commit>`.

## Create the state container

Do this once per install. Skip it if a teammate already did, and use their ID.

```sh
dr-sync init --name "<app>-state"
```

The output shows the container ID on the `create` line and again in an `export DR_SYNC_CATALOG_ID=<container-id>` line. Save it in the repository as `installs/<install>.env` and commit it. It is not a secret.

```text
DR_SYNC_CATALOG_ID=<container-id>
```

Then:

1. Share the Files item with each teammate as an Editor. Without it they get a `404`.
2. Add `pulumi.drsyncignore` next to `Pulumi.yaml` and commit it. It keeps Pulumi's lock files, logs, plugin cache and per-machine settings out of the container.

```text
plugins/
locks/
bin/
templates/
logs/
workspaces/
credentials.json
```

## Create what the app depends on

Create the execution environment, credentials and any deployment the app calls on the target install. Put their names and IDs in `Pulumi.<install>.yaml` and commit it. Use one stack per install, named after the install.

## Deploy

Run from the directory that holds `Pulumi.yaml`.

```sh
export DR_SYNC_CATALOG_ID=<container-id>
read -rs PULUMI_CONFIG_PASSPHRASE && export PULUMI_CONFIG_PASSPHRASE
```

The first deploy to an install creates the stack:

```sh
dr-sync run "$STATE_DIR" --exclude-from pulumi.drsyncignore -- \
  sh -c 'pulumi stack init <install> && exec pulumi up --yes'
```

Every later deploy, from any machine, selects it:

```sh
dr-sync run "$STATE_DIR" --exclude-from pulumi.drsyncignore -- \
  sh -c 'pulumi stack select <install> && exec pulumi up --yes'
```

After the first deploy, commit `Pulumi.<install>.yaml` again. Pulumi adds an `encryptionsalt` line to it, and a teammate needs that line to read any secret settings.

If the first-deploy command says `already exists`, the stack is there. Use the later-deploy command.

If the later-deploy command says `no stack named`, stop and check `DR_SYNC_CATALOG_ID` against `installs/<install>.env`. A deploy against a new or wrong container starts from empty state and creates a duplicate app.

To see the plan without changing anything, replace `pulumi up --yes` with `pulumi preview --diff`.

Check the result with `echo $?`. `0` means the deploy worked and the state is saved.

## Continue a teammate's work

Set up your machine, then run the later-deploy command. The first run pulls the state. You need the same passphrase and the committed `Pulumi.<install>.yaml`.

## When it fails

Several rows say the state may be only on this machine. In that case, save your state first: copy the whole state folder, including `.dr-sync-backup/`, to a folder outside it.

```sh
cp -a "$STATE_DIR" "$STATE_DIR.saved"
```

The next run replaces the state folder with the head, and `.dr-sync-backup/` does not exist until it does, so this copy is the only safe record of what Pulumi created. Then ask for help.

| You see | What it means | Do this |
| --- | --- | --- |
| Exit code `4`, `another dr-sync run holds this container` | A teammate is deploying, or a killed run holds the lease. Nothing ran. The message names who holds it and until when | Wait, then run the command again. After a killed run, the hold lasts up to 30 minutes. Wait it out, or add `--force` right after `"$STATE_DIR"` once you confirm the holder named in the message is not still deploying |
| Exit code `4`, `the container advanced` or `another writer claimed` | Pulumi ran, and a teammate pushed first. Your run's state is only in the state folder | Do not run again yet, because the deploy's resources would be missing from the state you continue with. Save your state first, then ask for help |
| Exit code `7` | Pulumi failed. The state is still saved | Read the Pulumi error, fix it, run again |
| Exit code `6` | The pull did not finish, so nothing ran | Run again. If it repeats, ask for help before you deploy |
| Any other exit code, such as `3` (API or sign-in failure) | Unknown. If Pulumi started and the push is not confirmed, the state may be only on this machine | Do not run again. Save your state first, then ask for help |
| Exit code `130`, or `255` from `datarobot sync` | You pressed Ctrl-C. With `dr-sync`, Pulumi stops and the state is pushed. With `datarobot sync`, `dr` ends the plugin 5 seconds after Ctrl-C, so the push or the lease release can be cut short | Read the last lines of the output. `interrupted; the tree was pushed as it stood` means the state is saved: run the command again. Any other message, or exit `255`, means the state may be only on this machine. Do not run again, because Pulumi would run on a state that lacks it and create duplicate resources. Save your state first, then ask for help. If a later run exits `4`, a killed run still holds the lease: add `--force` only after you confirm nobody else is deploying. If Pulumi reports a lock, see the next rows. Avoid Ctrl-C during a deploy |
| A warning that names `.dr-sync-backup/` | The state on this machine differed from the head, so the head replaced it. Either a teammate deployed since your last run, or your last run did not push (exit `4`, `130`, `255` or a kill) | If your last run here pushed, nothing: delete the folder when you like. Otherwise keep it, because it can hold the only record of what that deploy created. Do not deploy again until someone has merged it back |
| `the stack is currently locked` | A killed run left a lock on this machine | Run `pulumi cancel --stack <install>`, then the deploy command again |
| `Application name must be unique` | An earlier deploy died while it created the app. The app exists, but the saved state does not know it | Do not retry, because each retry leaves one more app named `<source name> <timestamp>`. Delete the app with the clashing name and those extra apps in the UI, then deploy again |
| The Codespace or the state folder is gone | Local files only. The state is in Files | Codespace: set up the machine again. Then run the later-deploy command |
| The app fails a few minutes after create, on an install with no internet | The build cannot download Python packages | See the next section |

## App dependencies on an install with no internet

The app build runs `pip install`, and it fails when the install reaches no package index. Ship the packages inside the app source.

1. On a machine with internet, resolve the full package list for Linux. Match the Python version of the app's base environment.

   ```sh
   uv pip compile requirements.in --python-platform x86_64-manylinux_2_28 --python-version 3.12 -o requirements.lock
   ```

2. Unpack the Linux wheels into `vendor/` in the app source.

   ```sh
   python -m pip install --target app/vendor --no-deps --only-binary=:all: \
     --platform manylinux2014_x86_64 --platform manylinux_2_28_x86_64 \
     --python-version 3.12 --implementation cp --abi cp312 \
     -r requirements.lock
   ```

   Pass both `--platform` values. Some packages, such as recent `numpy`, publish `manylinux_2_28` wheels only, and the step stops with `No matching distribution found` when the second value is missing.

3. Add this line to `start-app.sh`, before the app starts:

   ```sh
   export PYTHONPATH="$PWD/vendor:$PYTHONPATH"
   ```

4. Remove `requirements.txt` from the app source, so the build skips `pip install`.

If the install has a package mirror, try `requirements.txt` with a throwaway app first. On a mirror that uses a private certificate authority, the build can stop within a minute with `CERTIFICATE_VERIFY_FAILED` on the package index, because the build does not trust that authority. In that case, vendor the packages as above, or ask the install's administrator to trust the authority for builds.

## Workloads

Deploy a Workload with `dr workload up` from a laptop that reaches the install. Use a separate checkout of the repository per install, because `up` writes that install's `workloadId` into `.datarobot.yaml`. A shared checkout creates a duplicate workload.

From a Codespace, `dr workload up` works only when the Codespace reaches the install's public address. Check it first:

```sh
curl -sS -m 20 -o /dev/null -w '%{http_code}\n' \
  -H "Authorization: Bearer $DATAROBOT_API_TOKEN" \
  "https://<install-host>/api/v2/workloads/?limit=1"
```

- `200`: point the CLI at the public address, then run `dr workload up`.

  ```sh
  export DATAROBOT_ENDPOINT="https://<install-host>/api/v2"
  ```

- A timeout: the Codespace cannot reach the Workload routes. Deploy from a laptop.

## Limits

- This is a field tool, not a supported state service. When the install reaches an S3 or Azure Blob bucket, use Pulumi's own `s3://` or `azblob://` backend.
- State belongs to one install. Never copy a state container or a stack to another install.
- The state does not hold `Pulumi.<install>.yaml`. Deploying with different local settings reverts a teammate's change, so commit the file.
- An Editor can delete files in the state container. A sync's own deletions leave older generations intact, but older generations share the same pack files. Deleting a pack in Files can break every generation that uses it, so tell Editors to leave `.dr-sync/` alone.
- If a deploy dies while it creates an app, that app is missing from the saved state, and `pulumi refresh` does not find it. Follow the `Application name must be unique` row above.
- `datarobot sync` runs through `dr`, which ends a plugin 5 seconds after Ctrl-C. A deploy interrupted that way can leave the lease held for up to 30 minutes. The standalone `dr-sync` has no such limit.
