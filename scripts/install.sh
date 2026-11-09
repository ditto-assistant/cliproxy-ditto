#!/usr/bin/env bash
# Build the plugin to match a CLIProxyAPI binary and install it next to it.
#
#   scripts/install.sh [path/to/cli-proxy-api]
#
# Defaults to the EasyCLIProxyAPI core on macOS. A Go c-shared plugin must be
# built with the host's Go release and architecture; the script reads both
# from the binary.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="${1:-$HOME/Library/Application Support/com.cpa.gui/cpa-core/cli-proxy-api}"
[ -x "$BIN" ] || { echo "no CLIProxyAPI binary at $BIN; pass its path" >&2; exit 1; }

info="$(go version -m "$BIN")"
toolchain="$(head -1 <<<"$info" | awk '{print $2}')"
goos="$(awk '$2 ~ /^GOOS=/ {sub("GOOS=","",$2); print $2}' <<<"$info")"
goarch="$(awk '$2 ~ /^GOARCH=/ {sub("GOARCH=","",$2); print $2}' <<<"$info")"
goos="${goos:-$(go env GOOS)}"
[ -n "$toolchain" ] && [ -n "$goarch" ] || { echo "could not read the Go build info of $BIN" >&2; exit 1; }

# An x86_64 CLIProxy on Apple Silicon runs under Rosetta, where a second Go
# runtime in the process crashes on its first call (CLIProxy's own example
# plugins included). Install the Apple Silicon build instead.
if [ "$goos" = darwin ] && [ "$goarch" = amd64 ] && [ "$(sysctl -n hw.optional.arm64 2>/dev/null)" = 1 ]; then
  echo "$BIN is an x86_64 build running under Rosetta on Apple Silicon." >&2
  echo "Native plugins cannot load there. Install the Apple Silicon (aarch64) build of" >&2
  echo "EasyCLIProxyAPI / CLIProxyAPI, then run this again." >&2
  exit 2
fi

ext=so; [ "$goos" = darwin ] && ext=dylib
dest="$(dirname "$BIN")/plugins/$goos/$goarch"
mkdir -p "$dest"
echo "building with $toolchain for $goos/$goarch"
(cd "$ROOT" && GOTOOLCHAIN="$toolchain" GOOS="$goos" GOARCH="$goarch" GOAMD64=v1 CGO_ENABLED=1 \
  go build -trimpath -buildmode=c-shared -o "$dest/ditto.$ext" .)
rm -f "$dest/ditto.h"
echo "installed $dest/ditto.$ext"
echo "next: scripts/connect.sh <endpoint>, then enable plugins.configs.ditto in CLIProxy's config.yaml"
