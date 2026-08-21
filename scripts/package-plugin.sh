#!/bin/sh
# Assemble the `dr` plugin archive from the cross-compiled binaries in dist/.
#
# manifest.json, the two wrappers, LICENSE.txt (required by the packager), and versions.yaml
# sit at the archive root; the platform binaries go under bin/. They can't sit flat at the
# root: the CLI's plugin-script scanner treats any extension-less dr-prefixed file there as a
# candidate script, so it would pick a wrong-platform binary directly instead of the wrapper.
# The executable bit must already be set in the tar because the CLI does not chmod on install.
set -eu

if ! command -v dr >/dev/null 2>&1; then
    echo "✗ the dr CLI is not on PATH" >&2
    echo "  install: curl -fsSL https://cli.datarobot.com/install | sh" >&2
    exit 1
fi

BINARIES="
dist/dr-sync-linux-amd64
dist/dr-sync-linux-arm64
dist/dr-sync-linux-riscv64
dist/dr-sync-darwin-amd64
dist/dr-sync-darwin-arm64
dist/dr-sync-windows-amd64.exe
dist/dr-sync-windows-arm64.exe
"

for binary in $BINARIES; do
    if [ ! -f "$binary" ]; then
        echo "✗ $binary is missing; run 'task build:dist' first" >&2
        exit 1
    fi
done

STAGING=$(mktemp -d 2>/dev/null || mktemp -d -t dr-sync)
trap 'rm -rf "$STAGING"' EXIT

cp cmd/dr-sync/manifest.json "$STAGING/manifest.json"
cp LICENSE.txt "$STAGING/LICENSE.txt"
cp versions.yaml "$STAGING/versions.yaml"
cp dr-sync.sh "$STAGING/dr-sync.sh"
cp dr-sync.ps1 "$STAGING/dr-sync.ps1"
chmod +x "$STAGING/dr-sync.sh"

mkdir -p "$STAGING/bin"
for binary in $BINARIES; do
    cp "$binary" "$STAGING/bin/"
    chmod +x "$STAGING/bin/$(basename "$binary")"
done

mkdir -p dist
dr self plugin package "$STAGING" -o dist/ --index-output dist/index-fragment.json
