#!/usr/bin/env bash
# End-to-end test of the config_sync integration on the development
# Supervisor (scripts/supervisor-dev.sh up + scripts/supervisor-test.sh, so the
# app is installed and running): installs the integration into that Core,
# checks that the app's Supervisor discovery creates the config flow, confirms
# it, and checks the entities and the Sync now button.
#
# It onboards the test Core with a throwaway user whose random password is
# never printed or stored. Core is reached from inside the devcontainer
# (Supervisor 2026.11 runs it on port 80 of the host network).
set -euo pipefail
cd "$(dirname "$0")/.."

dc() { docker exec -i hcs-supervisor "$@"; }
curl() { dc curl "$@"; }
base=http://127.0.0.1:80
fails=0
pass() { echo "  OK   $*"; }
fail() { echo "  FAIL $*"; fails=$((fails + 1)); }

echo "== install the integration into the test Core and restart it"
# Fresh onboarding on every run (the test user's password is never kept):
# drop the test Core's users, onboarding state and earlier config_sync entry.
dc sh -c 'cd /mnt/supervisor/homeassistant/.storage 2>/dev/null && rm -f onboarding auth auth_provider.homeassistant core.config_entries || true'
dc sh -c 'rm -rf /mnt/supervisor/homeassistant/custom_components/config_sync && mkdir -p /mnt/supervisor/homeassistant/custom_components'
cp custom_components/config_sync/strings.json custom_components/config_sync/translations/en.json
tar -C custom_components -cf - --exclude=__pycache__ config_sync | dc tar -C /mnt/supervisor/homeassistant/custom_components -xf -
scripts/supervisor-dev.sh ha core restart --no-progress >/dev/null
for _ in $(seq 1 90); do curl -fs "$base/api/onboarding" >/dev/null 2>&1 && break; sleep 2; done

echo "== onboarding / login"
client_id="$base/"
password=$(head -c 24 /dev/urandom | base64 | tr -d '/+=')
if code=$(curl -fs -X POST "$base/api/onboarding/users" -H 'Content-Type: application/json' \
  -d "{\"client_id\": \"$client_id\", \"name\": \"hcs test\", \"username\": \"hcs-test\", \"password\": \"$password\", \"language\": \"en\"}" 2>/dev/null); then
  auth_code=$(echo "$code" | python3 -c 'import json,sys; print(json.load(sys.stdin)["auth_code"])')
else
  echo "  the test Core is already onboarded: run scripts/supervisor-dev.sh down/up for a fresh one" >&2
  exit 2
fi
unset password
token=$(curl -fs -X POST "$base/auth/token" -d grant_type=authorization_code -d "code=$auth_code" -d "client_id=$client_id" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])')
H=(-H "Authorization: Bearer $token" -H 'Content-Type: application/json')

echo "== discovery creates the flow"
flow_id=""
flows=""
for _ in $(seq 1 30); do
  flows=$(dc docker exec -i -e HA_TOKEN="$token" homeassistant python3 - < scripts/ha-flows.py 2>&1 || true)
  flow_id=$(echo "$flows" | python3 -c '
import json,sys
try:
    f=[x for x in json.load(sys.stdin) if x.get("handler")=="config_sync"]
except ValueError:
    f=[]
print(f[0]["flow_id"] if f else "")')
  [[ -n $flow_id ]] && break
  sleep 2
done
[[ -n $flow_id ]] && pass "config_sync flow from discovery" || { fail "no config_sync flow; flows: ${flows:0:300}"; exit 1; }
result=$(curl -fs -X POST "${H[@]}" "$base/api/config/config_entries/flow/$flow_id" -d '{}')
[[ $(echo "$result" | python3 -c 'import json,sys; print(json.load(sys.stdin)["type"])') == create_entry ]] \
  && pass "entry created" || fail "flow result: $result"

echo "== entities"
sleep 3
states=$(curl -fs "${H[@]}" "$base/api/states")
for e in sensor.git_config_sync_sync_state sensor.git_config_sync_last_sync binary_sensor.git_config_sync_needs_attention button.git_config_sync_sync_now; do
  v=$(echo "$states" | python3 -c "import json,sys; s=[x for x in json.load(sys.stdin) if x['entity_id']=='$e']; print(s[0]['state'] if s else '')")
  [[ -n $v ]] && pass "$e = $v" || fail "$e missing"
done

echo "== Sync now button reaches the app"
before=$(scripts/supervisor-dev.sh ha apps logs local_homeassistant_config_sync 2>/dev/null | grep -c 'sync .* (api)' || true)
curl -fs -X POST "${H[@]}" "$base/api/services/button/press" -d '{"entity_id": "button.git_config_sync_sync_now"}' >/dev/null
sleep 5
after=$(scripts/supervisor-dev.sh ha apps logs local_homeassistant_config_sync 2>/dev/null | grep -c 'sync .* (api)' || true)
(( after > before )) && pass "the app ran a sync triggered by the button" || fail "no API-triggered sync in the app log"

echo
if (( fails == 0 )); then echo "INTEGRATION E2E OK"; else echo "INTEGRATION E2E: $fails failures"; fi
exit $(( fails > 0 ))
