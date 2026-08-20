#!/bin/sh
# Assert the plugin manifest is internally consistent.
#
# `dr self plugin package` execs the wrapper script with --dr-plugin-manifest and refuses to
# package unless the JSON it prints matches manifest.json exactly on name, version,
# description and authentication. Catch a mismatch here rather than at release time.
set -eu

MANIFEST="cmd/dr-sync/manifest.json"

if [ ! -f "$MANIFEST" ]; then
    echo "✗ $MANIFEST is missing" >&2
    exit 1
fi

if ! command -v python3 >/dev/null 2>&1; then
    echo "⚠️  python3 not found, skipping manifest check" >&2
    exit 0
fi

# The binary must print byte-identical JSON, since it embeds this very file.
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

go build -o "$tmp/dr-sync" ./cmd/dr-sync
"$tmp/dr-sync" --dr-plugin-manifest > "$tmp/printed.json"

if ! cmp -s "$MANIFEST" "$tmp/printed.json"; then
    echo "✗ --dr-plugin-manifest output differs from $MANIFEST" >&2
    diff -u "$MANIFEST" "$tmp/printed.json" >&2 || true
    exit 1
fi

python3 - "$MANIFEST" <<'PY'
import json
import re
import subprocess
import sys

path = sys.argv[1]
with open(path, encoding="utf-8") as handle:
    manifest = json.load(handle)

problems = []

# The `dr` CLI rejects unknown keys outright, so an unexpected field is a hard failure.
allowed = {"name", "version", "description", "authentication", "minCLIVersion", "scripts"}
for key in manifest:
    if key not in allowed:
        problems.append(f"unknown key {key!r}: the dr CLI rejects manifests with extra fields")

for key in ("name", "version", "description"):
    if not manifest.get(key):
        problems.append(f"{key} must be a non-empty string")

# The command registered is `dr <name>`, so naming it "dr-sync" would give `dr dr-sync`.
if manifest.get("name") != "sync":
    problems.append(f'name must be "sync" so the command is `dr sync`, got {manifest.get("name")!r}')

version = manifest.get("version", "")
if not re.fullmatch(r"\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?", version):
    problems.append(f"version {version!r} is not semver; the plugin registry parses it as semver")

# A tagged commit must carry the matching manifest version, or `dr plugin install` would
# resolve a version the archive does not contain.
tag = subprocess.run(
    ["git", "describe", "--exact-match", "--tags", "HEAD"],
    capture_output=True, text=True, check=False,
).stdout.strip()

if tag and tag.lstrip("v") != version:
    problems.append(f"HEAD is tagged {tag} but the manifest says {version}")

for problem in problems:
    print(f"✗ {problem}", file=sys.stderr)

sys.exit(1 if problems else 0)
PY

echo "✅ plugin manifest consistent"
