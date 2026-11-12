#!/usr/bin/env bash
# Builds the app images for every architecture, as the Supervisor expects them
# (ghcr.io/<owner>/{arch}-homeassistant-config-sync:<version>, the `image:` in
# config.yaml). Run scripts/build-addon.sh first: the images copy its binaries.
#
#   scripts/release-images.sh 0.0.1           build and load locally (needs QEMU for aarch64)
#   scripts/release-images.sh 0.0.1 --push    build and push (release workflow)
#
# ARCHS="amd64" limits the architectures (local smoke tests).
set -euo pipefail
cd "$(dirname "$0")/.."

version=${1:?usage: scripts/release-images.sh X.Y.Z [--push]}
mode=--load
[[ ${2:-} == --push ]] && mode=--push

image=$(sed -n -E 's/^image: *"?([^"]*)"?$/\1/p' homeassistant_config_sync/config.yaml)
[[ -n $image ]] || { echo "config.yaml has no image:" >&2; exit 1; }
source=https://github.com/MaximilianoFelice/homeassistant-config-sync

for arch in ${ARCHS:-amd64 aarch64}; do
  case $arch in
    amd64) platform=linux/amd64 ;;
    aarch64) platform=linux/arm64 ;;
    *) echo "unknown arch $arch" >&2; exit 2 ;;
  esac
  [[ -x homeassistant_config_sync/bin/hcs-$arch ]] || { echo "missing homeassistant_config_sync/bin/hcs-$arch (run scripts/build-addon.sh)" >&2; exit 1; }
  ref=${image//\{arch\}/$arch}
  echo "== $ref:$version ($platform)"
  docker buildx build "$mode" \
    --platform "$platform" \
    --build-arg "BUILD_ARCH=$arch" \
    --label "org.opencontainers.image.source=$source" \
    --label "org.opencontainers.image.version=$version" \
    --label "org.opencontainers.image.licenses=MIT" \
    --label "io.hass.type=addon" \
    --label "io.hass.version=$version" \
    --label "io.hass.arch=$arch" \
    --tag "$ref:$version" \
    homeassistant_config_sync
done
