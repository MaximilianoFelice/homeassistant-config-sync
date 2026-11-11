#!/usr/bin/env bash
# Trae una copia de la config del box a un directorio local, en modo
# lectura: en el box solo corre `tar -c` hacia stdout, no escribe nada.
#
# Lo sensible y lo pesado se excluye EN EL BOX, así ni siquiera viaja:
# la denylist dura del motor (secrets.yaml, DBs, .storage/auth…, claves,
# logs), los directorios .git (un clon puede tener credenciales en
# .git/config), .cache, backups, tts, deps y todo .storage salvo lovelace*.
#
# Uso: scripts/fetch-box-copy.sh [DESTINO] [HOST_SSH]
#   DESTINO   default .sandbox/e2e/box  (queda con homeassistant/ y addon_configs/)
#   HOST_SSH  host SSH del box (obligatorio; por ejemplo, el add-on de SSH)
set -euo pipefail

dest=${1:-.sandbox/e2e/box}
host=${2:?falta HOST_SSH}

excludes=(
  # denylist dura (internal/paths.HardDeny), en sintaxis de busybox tar
  '*secrets.yaml' '*secrets.yml'
  '*.db' '*.db-shm' '*.db-wal' '*.db-journal' '*.sqlite*'
  '*/.cloud' '*/.ssh'
  '*id_rsa*' '*id_dsa*' '*id_ecdsa*' '*id_ed25519*'
  '*.pem' '*.key' '*.p12' '*.pfx'
  '*.log' '*.log.*'
  # nunca: historia de git (puede tener credenciales) y estado interno de HA
  '*/.git' '*/.storage'
  # pesado e irrelevante para el sync
  'homeassistant/.cache' 'homeassistant/backups' 'homeassistant/tts' 'homeassistant/deps'
)
args=()
for e in "${excludes[@]}"; do
  args+=("--exclude=$e")
done

rm -rf "$dest"
mkdir -p "$dest/homeassistant" "$dest/addon_configs"

# /homeassistant sin .storage, y aparte solo .storage/lovelace*.
# shellcheck disable=SC2029 # los args se expanden acá a propósito
ssh "$host" "cd / && tar -cf - ${args[*]} homeassistant" | tar -xf - -C "$dest"
ssh "$host" 'cd / && ls homeassistant/.storage/lovelace* >/dev/null 2>&1 && tar -cf - homeassistant/.storage/lovelace* || true' | tar -xf - -C "$dest" 2>/dev/null || true

# /app_configs (así se llama en el add-on de SSH) → addon_configs/ (como en la app).
ssh "$host" "cd /app_configs && tar -cf - ${args[*]} ." | tar -xf - -C "$dest/addon_configs"

# Chequeo defensivo: nada de la denylist llegó.
if find "$dest" \( -name 'secrets.yaml' -o -name '*.db' -o -name '.git' -o -name 'auth' -o -name 'core.config_entries' \) | grep -q .; then
  echo "ERROR: llegó algo que no debía:" >&2
  find "$dest" \( -name 'secrets.yaml' -o -name '*.db' -o -name '.git' -o -name 'auth' -o -name 'core.config_entries' \) >&2
  rm -rf "$dest"
  exit 1
fi

echo "copia en $dest: $(find "$dest" -type f | wc -l) archivos, $(du -sh "$dest" | cut -f1)"
