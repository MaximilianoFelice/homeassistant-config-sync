#!/usr/bin/env bash
# Home Assistant's real check_config on a config directory, with the same
# image the box runs. Prints the JSON from `check_config --json` (errors and
# warnings), which hcs's CommandChecker can read. Use it as
#   hcs sync --checker "command:scripts/ha-check.sh DIR"
#
# Runs with the user's uid so it leaves no root-owned files in the sandbox.
set -euo pipefail

dir=$(realpath "${1:?usage: scripts/ha-check.sh CONFIG_DIR}")
image=${HA_IMAGE:-ghcr.io/home-assistant/home-assistant:2026.11.1}

exec docker run --rm --network none \
  --user "$(id -u):$(id -g)" -e HOME=/tmp \
  -v "$dir:/config" \
  "$image" python -m homeassistant --script check_config -c /config --json
