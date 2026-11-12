#!/usr/bin/env bash
# Engine E2E on the synthetic config in testdata/ha-config.
#
# Everything happens in .sandbox/e2e (ignored by git): the "box" is a copy of
# the example config, "GitHub" is a local bare repo seeded with an older
# version of that config (like a repo that already existed), and "the user"
# is a local clone that commits and pushes to main. No network.
#
# check_config is Home Assistant's real one, in Docker (scripts/ha-check.sh).
# With E2E_CHECKER=noop it is skipped (for environments without Docker).
#
# Prints only paths, counts and states.
set -uo pipefail

cd "$(dirname "$0")/.."
S=$PWD/.sandbox/e2e
H=$PWD/bin/hcs
fixture=$PWD/testdata/ha-config
box=$S/box/homeassistant

mise run build >/dev/null || exit 2
rm -rf "$S" && mkdir -p "$S/out"

# --- The box: the example config + what must never be in the repo -----------
cp -a "$fixture" "$S/box"
rm -f "$S/box/README.md"
scripts/fake-secrets.sh "$box" >/dev/null
printf 'wifi_password: fake\n' > "$box/esphome/secrets.yaml"
head -c 4096 /dev/urandom > "$box/home-assistant_v2.db"
printf 'fake log\n' > "$box/home-assistant.log"
printf '{"data": {"users": []}}\n' > "$box/.storage/auth"
mkdir -p "$box/www" && printf 'JPEG\0\0\0binary' > "$box/www/photo.jpg"
# An old ESPHome node with the key written by hand (the case the scanner stops).
legacy_key=$(head -c 32 /dev/urandom | base64)
legacy_pw=$(head -c 12 /dev/urandom | base64 | tr -d '/+=')
cat > "$box/esphome/sensor-legacy.yaml" <<EOF
esphome:
  name: sensor-legacy
api:
  encryption:
    key: "$legacy_key"
ota:
  - platform: esphome
    password: "$legacy_pw"
EOF

checker=${E2E_CHECKER:-command:$PWD/scripts/ha-check.sh $box}

# The "user's" git, isolated from any real config.
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=e2e GIT_AUTHOR_EMAIL=e2e@example.invalid GIT_COMMITTER_NAME=e2e GIT_COMMITTER_EMAIL=e2e@example.invalid

fails=0
pass() { echo "  OK   $*"; }
fail() { echo "  FAIL $*"; fails=$((fails + 1)); }
expect_state() { # expect_state STATE OUTPUT_FILE
  local got
  got=$(sed -n 's/^state: \([a-z_]*\).*/\1/p' "$2" | head -1)
  if [[ $got == "$1" ]]; then pass "state $1"; else fail "state $got, want $1 (see $2)"; fi
}

cat > "$S/config.yaml" <<EOF
repository: file://$S/origin.git
sync_interval_minutes: 0
mappings:
  - repo: config
    box: box/homeassistant
    check: true
    include: ["**/*.yaml", "**/*.yml", "custom_templates/**/*.jinja", "python_scripts/**/*.py", "pyscript/**/*.py"]
    push_only: [".storage/lovelace*"]
    exclude: [".cache/**", "deps/**", "tts/**", "backups/**", "custom_components/**", "esphome/.esphome/**", "**/__pycache__/**", "ip_bans.yaml"]
  - repo: app_configs
    box: box/app_configs
    include: ["local_example/**/*.yaml", "local_example/**/*.py"]
EOF
A=(--config "$S/config.yaml" --state-dir "$S/state" --allow-file-remote --checker "$checker")
run() { # run NAME [flags…] → leaves the output in $S/out/NAME.txt
  local name=$1; shift
  "$H" "$@" "${A[@]}" > "$S/out/$name.txt" 2>&1
  echo $? > "$S/out/$name.exit"
}
dev() { git -C "$S/dev" "$@"; }
origin() { git --git-dir="$S/origin.git" "$@"; }

echo "== 1. a repo that already existed, with an older version of the config"
git init -q --bare -b main "$S/origin.git"
git init -q -b main "$S/seed"
mkdir -p "$S/seed/config" "$S/seed/app_configs"
cp -a "$fixture/homeassistant/." "$S/seed/config/"
cp -a "$fixture/app_configs/." "$S/seed/app_configs/"
rm -rf "$S/seed/config/themes" "$S/seed/config/.storage" "$S/seed/config/custom_components"
printf '{}\n' > "$S/seed/config/customize.yaml"            # the box changed this later
cp "$box/esphome/sensor-legacy.yaml" "$S/seed/config/esphome/"  # a secret that had already leaked
printf 'home_latitude: 0\n' > "$S/seed/config/secrets.yaml"    # and a secrets.yaml that should never have been pushed
printf '# home\n' > "$S/seed/README.md"
git -C "$S/seed" add -A && git -C "$S/seed" commit -qm "seed: existing repo"
git -C "$S/seed" push -q "$S/origin.git" main
echo "  main seeded with $(origin ls-tree -r --name-only main | wc -l) files"

echo "== 2. init"
run init init
[[ $(cat "$S/out/init.exit") == 0 ]] && pass "init" || fail "init (see $S/out/init.txt)"

echo "== 3. plan (dry-run)"
run plan plan
echo "  the box would push: $(grep -cE '^  [AMD] ' "$S/out/plan.txt") changes"
grep -q 'skipped: config/www/photo.jpg' "$S/out/plan.txt" || true

echo "== 4. first sync against the existing repo: bootstrap with review"
# Secrets already in main and unchanged do not block: the scanner only looks
# at what the box changes (see step 13).
run sync1 sync
expect_state bootstrap_review "$S/out/sync1.txt"
if grep -q 'secret:' "$S/out/sync1.txt"; then fail "blocked on secrets that were already in main"; else pass "no block for what was already in main"; fi
diff=$(origin diff --name-status main box | sort)
want=$(printf 'A\tconfig/.storage/lovelace\nA\tconfig/themes/dark.yaml\nM\tconfig/customize.yaml\n' | sort)
[[ $diff == "$want" ]] && pass "box differs from main exactly in lovelace, the theme and customize" || fail "unexpected diff:\n$diff"
leaked=$(origin ls-tree -r --name-only box | grep -E 'home-assistant_v2\.db|home-assistant\.log|\.storage/auth|www/photo\.jpg|custom_components|esphome/secrets\.yaml' || true)
[[ -z $leaked ]] && pass "nothing from the denylist, no binaries and no HACS in box" || fail "pushed: $leaked"

echo "== 5. accept the bootstrap"
run sync2 sync --accept-bootstrap
expect_state idle "$S/out/sync2.txt"
[[ $(origin rev-parse main) == $(origin rev-parse box) ]] && pass "main == box" || fail "main != box"
origin cat-file -e main:config/secrets.yaml 2>/dev/null && pass "the secrets.yaml already in main is kept, but never goes down to the box" || true
[[ $(cat "$box/secrets.yaml") != "home_latitude: 0" ]] && pass "the box's secrets.yaml is intact" || fail "the box's secrets.yaml was overwritten"

echo "== 6. second sync without changes: no-op"
run sync3 sync
expect_state idle "$S/out/sync3.txt"
grep -q '^push:' "$S/out/sync3.txt" && fail "pushed without changes" || pass "no push"

git clone -q "$S/origin.git" "$S/dev"

echo "== 7. edit from another machine → reaches the box"
printf '# e2e: edited from another machine\n' >> "$S/dev/config/scripts.yaml"
dev commit -qam "e2e: scripts" && dev push -q origin main
run sync4 sync
expect_state idle "$S/out/sync4.txt"
grep -q 'edited from another machine' "$box/scripts.yaml" && pass "scripts.yaml applied on the box" || fail "scripts.yaml did not arrive"
grep -q 'applied (check: true' "$S/out/sync4.txt" && pass "went through check_config" || fail "the check did not run"

echo "== 8. edit on the box → reaches main"
printf '# e2e: edited in the UI\n' >> "$box/customize.yaml"
run sync5 sync
expect_state idle "$S/out/sync5.txt"
origin show main:config/customize.yaml | grep -q 'edited in the UI' && pass "customize.yaml in main" || fail "customize.yaml did not reach main"

echo "== 9. conflict and resolution"
dev pull -q
printf '# e2e: line from another machine\n' >> "$S/dev/config/packages/lights.yaml"
dev commit -qam "e2e: lights" && dev push -q origin main
printf '# e2e: line from the UI\n' >> "$box/packages/lights.yaml"
run sync6 sync
expect_state conflict "$S/out/sync6.txt"
grep -q 'conflict: config/packages/lights.yaml' "$S/out/sync6.txt" && pass "conflict in packages/lights.yaml" || fail "conflict reported incorrectly"
grep -q 'from the UI' "$box/packages/lights.yaml" && ! grep -q 'from another machine' "$box/packages/lights.yaml" && pass "the box was not touched" || fail "the box was touched during a conflict"
dev fetch -q origin
dev merge -q origin/box -m "e2e: merge box" >/dev/null 2>&1
grep -v -E '^(<<<<<<<|=======|>>>>>>>)' "$S/dev/config/packages/lights.yaml" > "$S/out/lights.tmp" && mv "$S/out/lights.tmp" "$S/dev/config/packages/lights.yaml"
dev add config/packages/lights.yaml && dev commit -qm "e2e: resolve lights (keep both lines)" && dev push -q origin main
run sync7 sync
expect_state idle "$S/out/sync7.txt"
grep -q 'from another machine' "$box/packages/lights.yaml" && grep -q 'from the UI' "$box/packages/lights.yaml" && pass "resolution applied on the box" || fail "the resolution did not arrive"

rejected() { # rejected NAME REPO_FILE CONTENT_TO_APPEND EXPECTED_TEXT
  local name=$1 path=$2 add=$3 want=$4 rel=${2#config/}
  dev pull -q
  cp "$box/$rel" "$S/out/$name.before"
  printf '%b' "$add" >> "$S/dev/$path"
  dev commit -qam "e2e: $name" && dev push -q origin main
  run "$name" sync
  expect_state rejected "$S/out/$name.txt"
  grep -q -- "$want" "$S/out/$name.txt" && pass "the rejection explains: $want" || fail "the rejection does not explain what happened"
  cmp -s "$box/$rel" "$S/out/$name.before" && pass "$rel intact (rollback)" || fail "$rel changed"
  dev revert --no-edit HEAD >/dev/null && dev push -q origin main
  run "$name-revert" sync
  expect_state idle "$S/out/$name-revert.txt"
}

echo "== 10. broken YAML from main → rejected by the pre-check"
rejected broken-yaml config/packages/climate.yaml 'broken: [\n' 'climate.yaml'

if [[ $checker == command:* ]]; then
  echo "== 11. invalid integration (check_config warning) → rejected"
  rejected integration config/configuration.yaml '\ninput_boolean:\n  e2e_x:\n    initial: not_a_bool\n' 'input_boolean'
  echo "== 12. core config error (http) → rejected"
  rejected http config/configuration.yaml '\nhttp:\n  server_port: not_a_number\n' "'http'"
else
  echo "== 11-12. skipped: E2E_CHECKER=$checker"
fi

echo "== final state"
run status status
sed 's/^/  /' "$S/out/status.txt"
[[ $(origin rev-parse main) == $(origin rev-parse box) ]] && pass "main == box" || fail "main != box"

echo "== 13. the same box against an empty repo: the ESPHome secret stops everything"
rm -rf "$S/state" "$S/origin.git"
git init -q --bare -b main "$S/origin.git"
run init2 init
run sync8 sync
expect_state blocked_secrets "$S/out/sync8.txt"
grep -q 'secret: config/esphome/sensor-legacy.yaml' "$S/out/sync8.txt" && pass "flags sensor-legacy.yaml" || fail "did not flag the file"
grep -qF "$legacy_pw" "$S/out/sync8.txt" && fail "the output includes the secret value" || pass "the output does not include the value"
[[ -z $(origin for-each-ref) ]] && pass "nothing was pushed" || fail "something with secrets was pushed"
git --git-dir="$S/state/repo.git" for-each-ref --format='%(refname)' | grep -q heads/box && fail "local box has commits with secrets" || pass "not even local commits"

echo
if (( fails == 0 )); then echo "E2E OK"; else echo "E2E: $fails failures (outputs in $S/out)"; fi
exit $(( fails > 0 ))
