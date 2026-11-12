#!/usr/bin/env bash
# Checks that a release tag matches every place the version lives: the app's
# config.yaml, the integration's manifest.json (when present), and a section
# in the app's CHANGELOG.md. Used by scripts/release.sh and the release
# workflow.
#
#   scripts/release-verify.sh v0.0.1
set -euo pipefail
cd "$(dirname "$0")/.."

tag=${1:?usage: scripts/release-verify.sh vX.Y.Z}
version=${tag#v}
fails=0
check() { if [[ $2 == "$version" ]]; then echo "  OK   $1: $2"; else echo "  FAIL $1: '$2', want '$version'"; fails=$((fails + 1)); fi; }

check "homeassistant_config_sync/config.yaml" "$(sed -n -E 's/^version: "?([^"]*)"?$/\1/p' homeassistant_config_sync/config.yaml)"
manifest=custom_components/config_sync/manifest.json
if [[ -f $manifest ]]; then
  check "$manifest" "$(sed -n -E 's/.*"version": "([^"]*)".*/\1/p' "$manifest")"
fi
if grep -q "^## $version\$" homeassistant_config_sync/CHANGELOG.md; then
  echo "  OK   CHANGELOG.md has '## $version'"
else
  echo "  FAIL CHANGELOG.md has no '## $version' section"; fails=$((fails + 1))
fi
exit $(( fails > 0 ))
