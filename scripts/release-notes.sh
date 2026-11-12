#!/usr/bin/env bash
# Prints the CHANGELOG section of a version (the body of the GitHub release).
#
#   scripts/release-notes.sh 0.0.1
set -euo pipefail
cd "$(dirname "$0")/.."
version=${1:?usage: scripts/release-notes.sh X.Y.Z}
awk -v v="## $version" '
  $0 == v { on = 1; next }
  on && /^## / { exit }
  on { print }
' homeassistant_config_sync/CHANGELOG.md | sed -e '/./,$!d'
