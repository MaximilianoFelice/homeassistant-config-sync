#!/usr/bin/env bash
# Engine E2E on the synthetic config in testdata/ha-config.
#
# Everything happens in .sandbox/e2e (ignored by git): "fs" plays the app's
# filesystem (the mounts homeassistant/, app_configs/, share/…), "origin.git"
# plays the git host, seeded with an older version of the config (like a repo
# that already existed), and "dev" is a person's clone that commits and
# pushes to main. No network.
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
fs=$S/fs
ha=$fs/homeassistant

mise run build >/dev/null || exit 2
rm -rf "$S" && mkdir -p "$S/out" "$fs/share" "$fs/ssl" "$fs/backup"

# --- The box: the example config + what must not reach the repo by default --
cp -a "$fixture/homeassistant" "$fixture/app_configs" "$fs/"
scripts/fake-secrets.sh "$ha" >/dev/null
head -c 4096 /dev/urandom > "$ha/home-assistant_v2.db"
printf 'fake log\n' > "$ha/home-assistant.log"
printf '{"data": {"users": []}}\n' > "$ha/.storage/auth"
printf 'not a real key\n' > "$fs/ssl/privkey.pem"
head -c 1024 /dev/urandom > "$fs/backup/full.tar"
printf 'shared notes\n' > "$fs/share/notes.txt"

checker=${E2E_CHECKER:-command:$PWD/scripts/ha-check.sh $ha}

# The person's git, isolated from any real config.
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

write_config() { # write_config [extra YAML lines]
  { printf 'repository: file://%s/origin.git\nsync_interval_minutes: 0\n' "$S"; printf '%b' "${1:-}"; } > "$S/config.yaml"
}
write_config
A=(--config "$S/config.yaml" --state-dir "$S/state" --fs-root "$fs" --allow-file-remote --checker "$checker")
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
cp -a "$fixture/homeassistant" "$fixture/app_configs" "$S/seed/"
rm -rf "$S/seed/homeassistant/themes" "$S/seed/homeassistant/.storage" "$S/seed/homeassistant/custom_components"
printf '{}\n' > "$S/seed/homeassistant/customize.yaml"              # the box changed this later
printf 'home_latitude: 0\n' > "$S/seed/homeassistant/secrets.yaml"   # pushed by hand long ago
printf '# home\n' > "$S/seed/README.md"
git -C "$S/seed" add -A && git -C "$S/seed" commit -qm "seed: existing repo"
git -C "$S/seed" push -q "$S/origin.git" main
echo "  main seeded with $(origin ls-tree -r --name-only main | wc -l) files"

echo "== 2. init"
run init init
[[ $(cat "$S/out/init.exit") == 0 ]] && pass "init" || fail "init (see $S/out/init.txt)"

echo "== 3. plan (dry-run)"
run plan plan
echo "  the box would push: $(grep -cE '^  [AMDT] ' "$S/out/plan.txt") changes"

echo "== 4. first sync against the existing repo: only box, on top of main"
run sync1 sync
expect_state review "$S/out/sync1.txt"
grep -q '^review: first_sync' "$S/out/sync1.txt" && pass "review: first_sync" || fail "no first_sync review"
diff=$(origin diff --name-status main box | sort)
for want in $'A\t.gitignore' $'A\thomeassistant/themes/dark.yaml' $'M\thomeassistant/customize.yaml' \
            $'D\thomeassistant/secrets.yaml' $'D\tREADME.md' $'A\tshare/notes.txt'; do
  grep -qxF "$want" <<<"$diff" && pass "main..box: ${want/$'\t'/ }" || fail "main..box lacks '${want/$'\t'/ }'"
done
leaked=$(origin ls-tree -r --name-only box | grep -E 'home-assistant_v2\.db|home-assistant\.log|\.storage/|custom_components|secrets\.yaml|^ssl/|^backup/' || true)
[[ -z $leaked ]] && pass "the base .gitignore kept out .storage, the db, logs, HACS, secrets, ssl and backup" || fail "pushed: $leaked"
[[ ! -e $fs/README.md ]] && pass "nothing applied before the review" || fail "README.md was written before the review"

echo "== 5. the person resolves in main: keep the repo's README, take the rest from box"
git clone -q "$S/origin.git" "$S/dev"
dev fetch -q origin
dev merge -q --no-edit origin/box >/dev/null 2>&1
dev checkout -q HEAD~1 -- README.md && dev commit -qm "e2e: keep the README" && dev push -q origin main
run sync2 sync
expect_state idle "$S/out/sync2.txt"
[[ $(origin rev-parse main) == $(origin rev-parse box) ]] && pass "main == box" || fail "main != box"
[[ $(cat "$fs/README.md" 2>/dev/null) == "# home" ]] && pass "README.md restored at the top level" || fail "README.md missing at the top level"
[[ $(cat "$ha/secrets.yaml") != "home_latitude: 0" ]] && pass "the box's secrets.yaml is intact" || fail "the box's secrets.yaml was overwritten"

echo "== 6. second sync without changes: no-op"
run sync3 sync
expect_state idle "$S/out/sync3.txt"
grep -q '^push:' "$S/out/sync3.txt" && fail "pushed without changes" || pass "no push"

echo "== 7. edit from another machine → reaches the box, checked and reloaded"
dev pull -q
printf '# e2e: edited from another machine\n' >> "$S/dev/homeassistant/scripts.yaml"
dev commit -qam "e2e: scripts" && dev push -q origin main
run sync4 sync
expect_state idle "$S/out/sync4.txt"
grep -q 'edited from another machine' "$ha/scripts.yaml" && pass "scripts.yaml applied on the box" || fail "scripts.yaml did not arrive"
if [[ $checker == command:* ]]; then
  grep -q 'applied (check: true' "$S/out/sync4.txt" && pass "went through check_config" || fail "the check did not run"
fi

echo "== 8. edit on the box → reaches main"
printf '# e2e: edited in the UI\n' >> "$ha/customize.yaml"
run sync5 sync
expect_state idle "$S/out/sync5.txt"
origin show main:homeassistant/customize.yaml | grep -q 'edited in the UI' && pass "customize.yaml in main" || fail "customize.yaml did not reach main"

echo "== 9. conflict and resolution"
dev pull -q
printf '# e2e: line from another machine\n' >> "$S/dev/homeassistant/packages/lights.yaml"
dev commit -qam "e2e: lights" && dev push -q origin main
printf '# e2e: line from the UI\n' >> "$ha/packages/lights.yaml"
run sync6 sync
expect_state conflict "$S/out/sync6.txt"
grep -q 'conflict: homeassistant/packages/lights.yaml' "$S/out/sync6.txt" && pass "conflict in packages/lights.yaml" || fail "conflict reported incorrectly"
grep -q 'from the UI' "$ha/packages/lights.yaml" && ! grep -q 'from another machine' "$ha/packages/lights.yaml" && pass "the box was not touched" || fail "the box was touched during a conflict"
dev fetch -q origin
dev merge -q origin/box -m "e2e: merge box" >/dev/null 2>&1
grep -v -E '^(<<<<<<<|=======|>>>>>>>)' "$S/dev/homeassistant/packages/lights.yaml" > "$S/out/lights.tmp" && mv "$S/out/lights.tmp" "$S/dev/homeassistant/packages/lights.yaml"
dev add homeassistant/packages/lights.yaml && dev commit -qm "e2e: resolve lights (keep both lines)" && dev push -q origin main
run sync7 sync
expect_state idle "$S/out/sync7.txt"
grep -q 'from another machine' "$ha/packages/lights.yaml" && grep -q 'from the UI' "$ha/packages/lights.yaml" && pass "resolution applied on the box" || fail "the resolution did not arrive"

if [[ $checker == command:* ]]; then
  echo "== 10. broken config from main → applied, check fails, no reload, no rollback"
  dev pull -q
  printf '\nbroken: [\n' >> "$S/dev/homeassistant/configuration.yaml"
  dev commit -qam "e2e: broken" && dev push -q origin main
  run sync8 sync
  expect_state check_failed "$S/out/sync8.txt"
  grep -q 'broken: \[' "$ha/configuration.yaml" && pass "applied as is (no rollback)" || fail "the change was rolled back"
  grep -q 'reload: false' "$S/out/sync8.txt" && pass "not reloaded" || fail "reloaded with errors"
  dev revert --no-edit HEAD >/dev/null && dev push -q origin main
  run sync9 sync
  expect_state idle "$S/out/sync9.txt"
else
  echo "== 10. skipped: E2E_CHECKER=$checker"
fi

echo "== 11. .gitignore from main: share/ leaves the repo, stays on disk"
dev pull -q
printf 'share/\n' >> "$S/dev/.gitignore"
dev commit -qam "e2e: ignore share" && dev push -q origin main
run sync10 sync
run sync11 sync
origin cat-file -e main:share/notes.txt 2>/dev/null && fail "share/notes.txt still in main" || pass "share/notes.txt left the repo"
[[ -f $fs/share/notes.txt ]] && pass "share/notes.txt stays on disk" || fail "share/notes.txt was deleted from disk"

echo "== 12. a possible secret: warned (default), committed, never printed"
token="ghp_$(head -c 64 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 36)"
printf 'e2e_token: %s\n' "$token" > "$ha/packages/leak.yaml"
run sync12 sync
expect_state idle "$S/out/sync12.txt"
grep -q 'secret: homeassistant/packages/leak.yaml:1' "$S/out/sync12.txt" && pass "warned about packages/leak.yaml" || fail "no warning"
grep -qF "$token" "$S/out/sync12.txt" && fail "the output includes the value" || pass "the output does not include the value"
origin cat-file -e main:homeassistant/packages/leak.yaml 2>/dev/null && pass "committed (warn mode)" || fail "not committed in warn mode"
rm "$ha/packages/leak.yaml"
run sync13 sync

echo "== 13. a file above max_file_size_mb stops the cycle"
head -c $((11 << 20)) /dev/zero > "$fs/app_configs/local_example/huge.bin"
run sync14 sync
expect_state too_large "$S/out/sync14.txt"
origin cat-file -e box:app_configs/local_example/huge.bin 2>/dev/null && fail "huge.bin was committed" || pass "nothing committed"
rm "$fs/app_configs/local_example/huge.bin"
run sync15 sync
expect_state idle "$S/out/sync15.txt"

echo "== final state"
run status status
sed 's/^/  /' "$S/out/status.txt"
[[ $(origin rev-parse main) == $(origin rev-parse box) ]] && pass "main == box" || fail "main != box"

echo "== 14. secret_scan.mode: block against an empty repo: nothing is pushed"
rm -rf "$S/state" "$S/origin.git" "$fs/.gitignore" "$fs/README.md"
git init -q --bare -b main "$S/origin.git"
printf 'e2e_token: %s\n' "$token" > "$ha/packages/leak.yaml"
write_config 'secret_scan:\n  mode: block\n'
run init2 init
run sync16 sync
expect_state blocked_secrets "$S/out/sync16.txt"
grep -qF "$token" "$S/out/sync16.txt" && fail "the output includes the value" || pass "the output does not include the value"
[[ -z $(origin for-each-ref) ]] && pass "nothing was pushed" || fail "something was pushed"

echo
if (( fails == 0 )); then echo "E2E OK"; else echo "E2E: $fails failures (outputs in $S/out)"; fi
exit $(( fails > 0 ))
