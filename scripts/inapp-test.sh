#!/bin/sh
# Runs INSIDE the app container (sent there by scripts/supervisor-test.sh).
# Tests what only exists there: SUPERVISOR_TOKEN, the Supervisor proxy to
# Core and the /homeassistant and /addon_configs mounts. The remote is a
# local repo in /data/e2e (no network, no GitHub).
set -u
fails=0
ok()   { echo "  OK   $*"; }
bad()  { echo "  FAIL $*"; fails=$((fails + 1)); }
state(){ sed -n 's/^state: \([a-z_]*\).*/\1/p' "$1" | head -1; }

E=/data/e2e
rm -rf "$E" && mkdir -p "$E"
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=e2e GIT_AUTHOR_EMAIL=e2e@example.invalid GIT_COMMITTER_NAME=e2e GIT_COMMITTER_EMAIL=e2e@example.invalid

echo "== app environment"
[ -n "${SUPERVISOR_TOKEN:-}" ] && ok "SUPERVISOR_TOKEN present" || bad "no SUPERVISOR_TOKEN"
[ -f /homeassistant/configuration.yaml ] && ok "/homeassistant mounted (configuration.yaml)" || bad "/homeassistant without configuration.yaml"
[ -d /app_configs ] && ok "/app_configs mounted" || bad "/app_configs does not exist ($(ls -d /addon_configs 2>/dev/null))"
touch /homeassistant/.hcs-rw && rm /homeassistant/.hcs-rw && ok "/homeassistant writable" || bad "/homeassistant read-only"
echo "  $(hcs version), $(git --version)"

cat > "$E/cfg.yaml" <<EOF
repository: file://$E/origin.git
sync_interval_minutes: 0
mappings:
  - repo: config
    box: /homeassistant
    check: true
    include: ["configuration.yaml", "automations.yaml", "scripts.yaml", "scenes.yaml"]
EOF
git init -q --bare -b main "$E/origin.git"
A="--config $E/cfg.yaml --state-dir $E/state --allow-file-remote"
cp /homeassistant/configuration.yaml "$E/configuration.before"

echo "== init + first sync (checker: Supervisor)"
hcs init $A > "$E/init.txt" 2>&1 && ok "init" || bad "init: $(cat "$E/init.txt")"
hcs sync $A > "$E/s1.txt" 2>&1
[ "$(state "$E/s1.txt")" = idle ] && ok "first sync idle" || bad "first sync: $(cat "$E/s1.txt")"

echo "== valid change from 'main' → check_config through the proxy + reload"
git clone -q "$E/origin.git" "$E/dev"
printf '\n# hcs inapp test\n' >> "$E/dev/config/configuration.yaml"
git -C "$E/dev" commit -qam "inapp: comment" && git -C "$E/dev" push -q origin main
hcs sync $A > "$E/s2.txt" 2>&1
[ "$(state "$E/s2.txt")" = idle ] && ok "sync idle" || bad "sync: $(cat "$E/s2.txt")"
grep -q 'applied (check: true, reload: true)' "$E/s2.txt" && ok "check_config and reload_all through the Supervisor" || bad "no check/reload: $(cat "$E/s2.txt")"
grep -q 'hcs inapp test' /homeassistant/configuration.yaml && ok "change applied in /homeassistant" || bad "the change did not arrive"

echo "== invalid integration from 'main' → rejected (new warning)"
git -C "$E/dev" pull -q
printf '\ninput_boolean:\n  hcs_x:\n    initial: not_a_bool\n' >> "$E/dev/config/configuration.yaml"
git -C "$E/dev" commit -qam "inapp: broken input_boolean" && git -C "$E/dev" push -q origin main
hcs sync $A > "$E/s3.txt" 2>&1
[ "$(state "$E/s3.txt")" = rejected ] && ok "rejected" || bad "not rejected: $(cat "$E/s3.txt")"
grep -q input_boolean "$E/s3.txt" && ok "the rejection names input_boolean" || bad "rejection without details"
grep -q 'not_a_bool' /homeassistant/configuration.yaml && bad "the invalid change stayed in /homeassistant" || ok "rollback in /homeassistant"

echo "== revert → back to idle"
git -C "$E/dev" revert --no-edit HEAD >/dev/null && git -C "$E/dev" push -q origin main
hcs sync $A > "$E/s4.txt" 2>&1
[ "$(state "$E/s4.txt")" = idle ] && ok "idle" || bad "after reverting: $(cat "$E/s4.txt")"

echo "== cleanup: restore configuration.yaml"
cp "$E/configuration.before" /homeassistant/configuration.yaml && rm -rf "$E"

echo
[ "$fails" -eq 0 ] && echo "INAPP OK" || echo "INAPP: $fails failures"
exit "$fails"
