#!/bin/sh
# Runs INSIDE the app container (sent there by scripts/supervisor-test.sh).
# Tests what only exists there: SUPERVISOR_TOKEN, the Supervisor proxy to
# Core, the seven mounts at the container's root, and root / as git's work
# tree. The remote is a local repo in /data/e2e (no network, no git host).
set -u
fails=0
ok()   { echo "  OK   $*"; }
bad()  { echo "  FAIL $*"; fails=$((fails + 1)); }
state(){ sed -n 's/^state: \([a-z_]*\).*/\1/p' "$1" | head -1; }

E=/data/e2e
rm -rf "$E" /.gitignore && mkdir -p "$E"
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=e2e GIT_AUTHOR_EMAIL=e2e@example.invalid GIT_COMMITTER_NAME=e2e GIT_COMMITTER_EMAIL=e2e@example.invalid

echo "== app environment"
[ -n "${SUPERVISOR_TOKEN:-}" ] && ok "SUPERVISOR_TOKEN present" || bad "no SUPERVISOR_TOKEN"
for m in homeassistant app_configs share ssl media backup local_apps; do
  [ -d "/$m" ] && ok "/$m mounted" || bad "/$m missing"
done
[ -f /homeassistant/configuration.yaml ] && ok "/homeassistant has configuration.yaml" || bad "/homeassistant without configuration.yaml"
touch /homeassistant/.hcs-rw && rm /homeassistant/.hcs-rw && ok "/homeassistant writable" || bad "/homeassistant read-only"
echo "  $(hcs version), $(git --version)"

# The app's own folder in local_apps holds the built binaries (>10 MB).
cat > "$E/cfg.yaml" <<EOF
repository: file://$E/origin.git
sync_interval_minutes: 0
max_file_size_mb: 50
EOF
git init -q --bare -b main "$E/origin.git"
A="--config $E/cfg.yaml --state-dir $E/state --allow-file-remote"
cp /homeassistant/configuration.yaml "$E/configuration.before"

echo "== init + first sync with root / (checker: Supervisor)"
hcs init $A > "$E/init.txt" 2>&1 && ok "init" || bad "init: $(cat "$E/init.txt")"
hcs sync $A > "$E/s1.txt" 2>&1
[ "$(state "$E/s1.txt")" = review ] && ok "first sync waits for review" || bad "first sync: $(tail -5 "$E/s1.txt")"
git --git-dir="$E/origin.git" rev-parse -q --verify refs/heads/main >/dev/null && bad "main was created by the app" || ok "only box pushed"
files=$(git --git-dir="$E/origin.git" ls-tree -r --name-only box)
echo "$files" | grep -qx 'homeassistant/configuration.yaml' && ok "homeassistant/configuration.yaml in box" || bad "configuration.yaml not in box"
echo "$files" | grep -qx '.gitignore' && ok "base .gitignore committed" || bad "no .gitignore in box"
echo "$files" | grep -q '^homeassistant/\.storage/' && bad ".storage reached the repo" || ok ".storage left out"
echo "$files" | grep -qE '^(bin|etc|usr|data)/' && bad "the container's own folders reached the repo" || ok "only the mounts and the repo's files"
[ ! -d /homeassistant/.git ] && [ ! -d /.git ] && ok "no .git in the work tree" || bad "a .git appeared"

echo "== the person creates main from box → syncing resumes"
git --git-dir="$E/origin.git" branch main box
hcs sync $A > "$E/s1b.txt" 2>&1
[ "$(state "$E/s1b.txt")" = idle ] && ok "idle after main is created" || bad "after creating main: $(tail -5 "$E/s1b.txt")"

echo "== change from 'main' → check_config through the proxy + reload"
git clone -q "$E/origin.git" "$E/dev"
printf '\n# hcs inapp test\n' >> "$E/dev/homeassistant/configuration.yaml"
git -C "$E/dev" commit -qam "inapp: comment" && git -C "$E/dev" push -q origin main
hcs sync $A > "$E/s2.txt" 2>&1
[ "$(state "$E/s2.txt")" = idle ] && ok "sync idle" || bad "sync: $(tail -5 "$E/s2.txt")"
grep -q 'applied (check: true, reload: true)' "$E/s2.txt" && ok "check_config and reload_all through the Supervisor" || bad "no check/reload: $(tail -5 "$E/s2.txt")"
grep -q 'hcs inapp test' /homeassistant/configuration.yaml && ok "change applied in /homeassistant" || bad "the change did not arrive"

echo "== broken config from 'main' → applied, check fails, no reload, no rollback"
git -C "$E/dev" pull -q
printf '\nbroken: [\n' >> "$E/dev/homeassistant/configuration.yaml"
git -C "$E/dev" commit -qam "inapp: broken" && git -C "$E/dev" push -q origin main
hcs sync $A > "$E/s3.txt" 2>&1
[ "$(state "$E/s3.txt")" = check_failed ] && ok "check_failed" || bad "not check_failed: $(tail -5 "$E/s3.txt")"
grep -q 'reload: false' "$E/s3.txt" && ok "not reloaded" || bad "reloaded with errors"
grep -q 'broken: \[' /homeassistant/configuration.yaml && ok "applied as is (no rollback)" || bad "rolled back"

echo "== revert in 'main' → back to idle"
git -C "$E/dev" revert --no-edit HEAD >/dev/null && git -C "$E/dev" push -q origin main
hcs sync $A > "$E/s4.txt" 2>&1
[ "$(state "$E/s4.txt")" = idle ] && ok "idle" || bad "after reverting: $(tail -5 "$E/s4.txt")"

echo "== cleanup: restore configuration.yaml"
cp "$E/configuration.before" /homeassistant/configuration.yaml && rm -rf "$E" /.gitignore

echo
[ "$fails" -eq 0 ] && echo "INAPP OK" || echo "INAPP: $fails failures"
exit "$fails"
