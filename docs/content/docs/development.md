+++
title = 'Development'
weight = 11
+++

```sh
task lint              # go mod tidy, gofumpt, vet, golangci-lint per GOOS, goreleaser check
task test              # unit tests, race detector on
task test:integration  # against a real instance; creates and deletes its own containers
task build              # ./dist/dr-sync
task install:local      # ~/.local/bin/dr-sync, so `dr sync …` runs your working tree
task plugin:package     # the dr plugin .tar.xz
task release:snapshot   # full release locally, publishes nothing
```

Building from source needs Go as pinned in `.tool-versions`, managed with asdf. After
changing that file, run `asdf reshim` — a version in `.tool-versions` does not create
the shim on its own.

[SPEC.md](https://github.com/datarobot-labs/datarobot-sync/blob/main/SPEC.md) is
the source of truth for sync semantics, the on-container layout, and the guarantees.
Read it before changing anything that touches the manifest schema, the `.dr-sync/`
object layout, the upload/download phase machines, garbage collection, or exit codes —
those are spec changes, and `SPEC.md` gets updated in the same commit.

## This site

```sh
task docs:serve   # serve the documentation site locally with live reload
task docs:build   # build the documentation site into docs/public
```

Built with [Hugo](https://gohugo.io/) and the
[Hextra](https://github.com/imfing/hextra) theme via Hugo Modules, and published to
GitHub Pages on every push to `main` that touches `docs/`.
