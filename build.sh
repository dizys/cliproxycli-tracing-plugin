#!/usr/bin/env bash
# Build the plugin as a Linux shared library for the CLIProxyAPI host.
#
# The host loads plugins from its plugins directory and requires a filename
# matching ^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$, optionally with a -vX.Y.Z suffix.
set -euo pipefail

cd "$(dirname "$0")"

VERSION="${VERSION:-0.2.0}"
ARCH="${ARCH:-amd64}"
OUT="cliproxy-tracing-v${VERSION}.so"

# cgo is required: the plugin is a C ABI shared library.
CGO_ENABLED=1 GOOS=linux GOARCH="${ARCH}" \
    go build -trimpath -buildmode=c-shared -ldflags "-s -w -X main.pluginVersion=${VERSION}" -o "${OUT}" .

echo "built ${OUT}"
