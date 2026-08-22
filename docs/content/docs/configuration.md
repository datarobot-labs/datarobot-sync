+++
title = 'Configuration'
weight = 6
+++

| What | Flag | Environment |
|---|---|---|
| Container to sync | `--catalog-id` | `DR_SYNC_CATALOG_ID` |
| API endpoint | `--endpoint` | `DATAROBOT_ENDPOINT` |
| API token | *(none, deliberately)* | `DATAROBOT_API_TOKEN` |
| Config file | `--config` | `DATAROBOT_CONFIG` |

Precedence is flag, then environment, then config file. Under the `dr` CLI the endpoint
and token arrive already validated; standalone, `dr-sync` falls back to
`~/.config/datarobot/drconfig.yaml` — the same file `dr auth login` writes. Because the
flag names double as config keys, that file can also carry `dr-sync`'s own settings:

```yaml
endpoint: https://app.datarobot.com/api/v2
catalog-id: 68a1f0c9d2e3b4a5c6d7e8f9
```

There is no `--token` flag on purpose: a flag would put the token in the process table
and the shell history.

{{< callout type="info" >}}
`viper` owns this flag → environment → config-file precedence, and lives behind
`internal/config` only — reading it directly from a command would reintroduce exactly
the precedence bugs a single owner exists to prevent.
{{< /callout >}}
