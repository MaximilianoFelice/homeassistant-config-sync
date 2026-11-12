#!/usr/bin/env bash
# Builds hcs for the app's architectures and puts it in
# homeassistant_config_sync/bin/, which is what the Dockerfile copies.
set -euo pipefail
cd "$(dirname "$0")/.."

out=homeassistant_config_sync/bin
version=$(git describe --always --dirty 2>/dev/null || echo dev)
mkdir -p "$out"
for pair in amd64:amd64 aarch64:arm64; do
  ha_arch=${pair%%:*} go_arch=${pair##*:}
  CGO_ENABLED=0 GOOS=linux GOARCH=$go_arch mise exec -- go build -trimpath \
    -ldflags "-s -w -X main.version=$version" -o "$out/hcs-$ha_arch" ./cmd/hcs
done
ls -la "$out"
