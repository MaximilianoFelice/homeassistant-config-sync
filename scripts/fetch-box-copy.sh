#!/usr/bin/env bash
# Fetches a copy of the box's config into a local directory, read-only: on
# the box only `tar -c` to stdout runs, nothing is written.
#
# Sensitive and heavy content is excluded ON THE BOX, so it never travels:
# the engine's hard denylist (secrets.yaml, DBs, .storage/auth…, keys,
# logs), .git directories (a clone may have credentials in .git/config),
# .cache, backups, tts, deps and all of .storage except lovelace*.
#
# Usage: scripts/fetch-box-copy.sh [DEST] [SSH_HOST]
#   DEST      default .sandbox/e2e/box  (ends up with homeassistant/ and addon_configs/)
#   SSH_HOST  SSH host of the box (required; for example, the SSH add-on)
set -euo pipefail

dest=${1:-.sandbox/e2e/box}
host=${2:?missing SSH_HOST}

excludes=(
  # hard denylist (internal/paths.HardDeny), in busybox tar syntax
  '*secrets.yaml' '*secrets.yml'
  '*.db' '*.db-shm' '*.db-wal' '*.db-journal' '*.sqlite*'
  '*/.cloud' '*/.ssh'
  '*id_rsa*' '*id_dsa*' '*id_ecdsa*' '*id_ed25519*'
  '*.pem' '*.key' '*.p12' '*.pfx'
  '*.log' '*.log.*'
  # never: git history (may have credentials) and HA's internal state
  '*/.git' '*/.storage'
  # heavy and irrelevant to the sync
  'homeassistant/.cache' 'homeassistant/backups' 'homeassistant/tts' 'homeassistant/deps'
)
args=()
for e in "${excludes[@]}"; do
  args+=("--exclude=$e")
done

rm -rf "$dest"
mkdir -p "$dest/homeassistant" "$dest/addon_configs"

# /homeassistant without .storage, and separately only .storage/lovelace*.
# shellcheck disable=SC2029 # the args are expanded here on purpose
ssh "$host" "cd / && tar -cf - ${args[*]} homeassistant" | tar -xf - -C "$dest"
ssh "$host" 'cd / && ls homeassistant/.storage/lovelace* >/dev/null 2>&1 && tar -cf - homeassistant/.storage/lovelace* || true' | tar -xf - -C "$dest" 2>/dev/null || true

# /app_configs (its name in the SSH add-on) → addon_configs/ (as in the app).
ssh "$host" "cd /app_configs && tar -cf - ${args[*]} ." | tar -xf - -C "$dest/addon_configs"

# Defensive check: nothing from the denylist arrived.
if find "$dest" \( -name 'secrets.yaml' -o -name '*.db' -o -name '.git' -o -name 'auth' -o -name 'core.config_entries' \) | grep -q .; then
  echo "ERROR: something that should not have arrived did:" >&2
  find "$dest" \( -name 'secrets.yaml' -o -name '*.db' -o -name '.git' -o -name 'auth' -o -name 'core.config_entries' \) >&2
  rm -rf "$dest"
  exit 1
fi

echo "copy in $dest: $(find "$dest" -type f | wc -l) files, $(du -sh "$dest" | cut -f1)"
