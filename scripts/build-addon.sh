#!/usr/bin/env bash
# Builds the app: the web UI (embedded into the binary) and hcs for every
# architecture the app supports, into homeassistant_config_sync/bin/, which is
# what the Dockerfile copies.
#
#   scripts/build-addon.sh                 build in place (release workflow)
#   scripts/build-addon.sh --stage DIR     also copy the app folder to DIR without
#                                          the `image:` line, so a local Supervisor
#                                          builds it instead of pulling from GHCR
#
# SKIP_UI=1 skips the UI build (the binary then serves a "not built" notice).
set -euo pipefail
cd "$(dirname "$0")/.."

stage=""
if [[ ${1:-} == --stage ]]; then
  stage=${2:?usage: scripts/build-addon.sh --stage DIR}
fi

app=homeassistant_config_sync
out=$app/bin
version=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}

if [[ ${SKIP_UI:-} != 1 ]]; then
  mise run ui:build >/dev/null
fi

mkdir -p "$out"
for pair in amd64:amd64 aarch64:arm64; do
  ha_arch=${pair%%:*} go_arch=${pair##*:}
  CGO_ENABLED=0 GOOS=linux GOARCH=$go_arch mise exec -- go build -trimpath \
    -ldflags "-s -w -X main.version=$version" -o "$out/hcs-$ha_arch" ./cmd/hcs
done
ls -la "$out"

if [[ -n $stage ]]; then
  mkdir -p "$stage"
  # Keep the directory itself (it may be bind-mounted) and replace its content.
  find "$stage" -mindepth 1 -delete
  cp -a "$app/." "$stage/"
  grep -v '^image:' "$app/config.yaml" > "$stage/config.yaml"
  echo "staged for a local build in $stage"
fi
