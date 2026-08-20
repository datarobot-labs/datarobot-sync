#!/bin/sh
# Wrapper shipped inside the `dr` plugin archive. The archive carries one binary per
# platform, so this picks the right one and execs it with the arguments untouched.
#
# `dr self plugin package` runs this with --dr-plugin-manifest during validation, so the
# exec'd binary must answer that flag — it does, before anything else.
set -eu

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

case "$(uname -s)-$(uname -m)" in
    Linux-x86_64|Linux-amd64)   BINARY="dr-sync-linux-amd64" ;;
    Linux-aarch64|Linux-arm64)  BINARY="dr-sync-linux-arm64" ;;
    Linux-riscv64)              BINARY="dr-sync-linux-riscv64" ;;
    Darwin-x86_64)              BINARY="dr-sync-darwin-amd64" ;;
    Darwin-arm64)               BINARY="dr-sync-darwin-arm64" ;;
    *)
        echo "dr-sync: unsupported platform: $(uname -s)-$(uname -m)" >&2
        echo "supported: Linux (x86_64, aarch64, riscv64), macOS (x86_64, arm64)" >&2
        exit 1
        ;;
esac

exec "$SCRIPT_DIR/$BINARY" "$@"
