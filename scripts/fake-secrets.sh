#!/usr/bin/env bash
# Generates a fake secrets.yaml with every !secret key a config uses, so
# check_config can run on a copy of the box (which travels without
# secrets.yaml). It does not overwrite an existing secrets.yaml.
#
# The values are fake but have the type HA usually expects: numbers for
# coordinates and ports, URLs for *_url, and strings for the rest.
set -euo pipefail

dir=${1:?usage: scripts/fake-secrets.sh CONFIG_DIR}
out=$dir/secrets.yaml
[[ -e $out ]] && { echo "$out already exists: leaving it alone" >&2; exit 0; }

grep -rhoE '!secret[[:space:]]+[A-Za-z0-9_]+' --include='*.yaml' --include='*.yml' "$dir" 2>/dev/null \
  | awk '{print $2}' | sort -u | while read -r k; do
    case $k in
      *latitude*|*lat) echo "$k: 0.0" ;;
      *longitude*|*lon|*lng) echo "$k: 0.0" ;;
      *elevation*|*port*) echo "$k: 1" ;;
      *url*|*uri*) echo "$k: http://example.invalid" ;;
      *) echo "$k: fake-$k" ;;
    esac
  done > "$out"
echo "fake secrets.yaml with $(grep -c . "$out" || true) keys in $out"
