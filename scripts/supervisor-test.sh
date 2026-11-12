#!/usr/bin/env bash
# Tests the app on the development Supervisor (scripts/supervisor-dev.sh up):
# build and install, options schema validation, startup, logs, and a full
# cycle from inside the container (scripts/inapp-test.sh).
set -euo pipefail
cd "$(dirname "$0")/.."

slug=local_homeassistant_config_sync
dc() { docker exec -i hcs-supervisor "$@"; }
api() { # api METHOD PATH [JSON]
  dc docker exec -i -e M="$1" -e P="$2" -e B="${3:-}" hassio_cli sh -c \
    'curl -s -X "$M" -H "Authorization: Bearer $SUPERVISOR_TOKEN" -H "Content-Type: application/json" ${B:+-d "$B"} "http://supervisor$P"'
}
json() { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }

scripts/build-addon.sh --stage "$PWD/.sandbox/addon-local" >/dev/null
scripts/supervisor-dev.sh ha jobs options --ignore-conditions healthy >/dev/null
scripts/supervisor-dev.sh ha store reload >/dev/null

echo "== build and install (from scratch, with the default options)"
if [[ $(api GET "/addons/$slug/info" | json 'd["data"]["version"]') != None ]]; then
  scripts/supervisor-dev.sh ha apps uninstall "$slug" --no-progress >/dev/null
fi
scripts/supervisor-dev.sh ha apps install "$slug" --no-progress

echo "== options (schema)"
opts=$(api GET "/addons/$slug/info" | python3 -c '
import json,sys
o=json.load(sys.stdin)["data"]["options"]
o["repository"]="https://git.invalid/test/does-not-exist.git"
o["token"]="not-a-real-token"
print(json.dumps({"options": o}))')
api POST "/addons/$slug/options" "$opts" | json 'd["result"]'
api POST "/addons/$slug/options/validate" | json '"valid" if d["data"]["valid"] else d'

echo "== startup"
api POST "/addons/$slug/restart" >/dev/null || api POST "/addons/$slug/start" >/dev/null
sleep 10
echo "  state: $(api GET "/addons/$slug/info" | json 'd["data"]["state"]')"
logs=$(api GET "/addons/$slug/logs" | sed -E 's/\x1b\[[0-9;]*m//g')
echo "$logs" | tail -8 | sed 's/^/  log: /'
echo "$logs" | grep -q 'not-a-real-token' && echo "  FAIL the token appears in the log" || echo "  OK   the token is not in the log"

echo "== web UI and API on the internal network"
info=$(api GET "/addons/$slug/info")
echo "  ingress: $(echo "$info" | json 'd["data"]["ingress"]'), ingress_url: $(echo "$info" | json 'd["data"]["ingress_url"]')"
host=$(echo "$info" | json 'd["data"]["hostname"]')
ui=$(dc docker exec hassio_cli curl -s -o /dev/null -w '%{http_code}' "http://$host:8099/")
apicode=$(dc docker exec hassio_cli curl -s -o /dev/null -w '%{http_code}' "http://$host:8099/api/v1/status")
[[ $ui == 200 ]] && echo "  OK   UI served at http://$host:8099/" || echo "  FAIL UI returned $ui"
[[ $apicode == 401 ]] && echo "  OK   API requires a token outside ingress" || echo "  FAIL API returned $apicode without a token"

echo "== full cycle from inside the container"
container=$(dc docker ps --format '{{.Names}}' | grep homeassistant_config_sync | head -1)
dc docker exec -i "$container" sh -s < scripts/inapp-test.sh
