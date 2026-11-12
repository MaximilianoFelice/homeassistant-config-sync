#!/usr/bin/env bash
# check_config real de Home Assistant sobre un directorio de config, con la
# misma imagen que corre el box. Imprime el JSON de `check_config --json`
# (errores y warnings), que el CommandChecker de hcs sabe leer. Sirve como
#   hcs sync --checker "command:scripts/ha-check.sh DIR"
#
# Corre con el uid del usuario para no dejar archivos de root en el sandbox.
set -euo pipefail

dir=$(realpath "${1:?uso: scripts/ha-check.sh DIR_CONFIG}")
image=${HA_IMAGE:-ghcr.io/home-assistant/home-assistant:2026.11.1}

exec docker run --rm --network none \
  --user "$(id -u):$(id -g)" -e HOME=/tmp \
  -v "$dir:/config" \
  "$image" python -m homeassistant --script check_config -c /config --json
