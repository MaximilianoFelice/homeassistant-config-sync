#!/usr/bin/env bash
# Prueba la app en el Supervisor de desarrollo (scripts/supervisor-dev.sh up):
# build e instalación, validación del esquema de opciones, arranque, logs, y
# un ciclo completo desde adentro del contenedor (scripts/inapp-test.sh).
set -euo pipefail
cd "$(dirname "$0")/.."

slug=local_homeassistant_config_sync
dc() { docker exec -i hcs-supervisor "$@"; }
api() { # api METHOD PATH [JSON]
  dc docker exec -i -e M="$1" -e P="$2" -e B="${3:-}" hassio_cli sh -c \
    'curl -s -X "$M" -H "Authorization: Bearer $SUPERVISOR_TOKEN" -H "Content-Type: application/json" ${B:+-d "$B"} "http://supervisor$P"'
}
json() { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }

scripts/build-addon.sh >/dev/null
scripts/supervisor-dev.sh ha jobs options --ignore-conditions healthy >/dev/null
scripts/supervisor-dev.sh ha store reload >/dev/null

echo "== build e instalación"
if [[ $(api GET "/addons/$slug/info" | json 'd["data"]["version"]') == None ]]; then
  scripts/supervisor-dev.sh ha apps install "$slug" --no-progress
else
  scripts/supervisor-dev.sh ha apps rebuild "$slug" --no-progress
fi

echo "== opciones (esquema anidado)"
opts=$(api GET "/addons/$slug/info" | python3 -c '
import json,sys
o=json.load(sys.stdin)["data"]["options"]
o["repository"]="git@github.invalid:prueba/no-existe.git"
print(json.dumps({"options": o}))')
api POST "/addons/$slug/options" "$opts" | json 'd["result"]'
api POST "/addons/$slug/options/validate" | json '"valid" if d["data"]["valid"] else d'

echo "== arranque"
api POST "/addons/$slug/restart" >/dev/null || api POST "/addons/$slug/start" >/dev/null
sleep 10
echo "  estado: $(api GET "/addons/$slug/info" | json 'd["data"]["state"]')"
api GET "/addons/$slug/logs" | sed -E 's/(ssh-ed25519 .{10}).*/\1…/; s/\x1b\[[0-9;]*m//g' | tail -8 | sed 's/^/  log: /'

echo "== ciclo completo desde adentro del contenedor"
container=$(dc docker ps --format '{{.Names}}' | grep homeassistant_config_sync | head -1)
dc docker exec -i "$container" sh -s < scripts/inapp-test.sh
