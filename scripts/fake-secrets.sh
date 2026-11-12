#!/usr/bin/env bash
# Genera un secrets.yaml falso con todas las claves !secret que usa una
# config, para poder correr check_config sobre una copia del box (que viaja
# sin secrets.yaml). No pisa un secrets.yaml existente.
#
# Los valores son de mentira pero con el tipo que HA suele esperar: números
# para coordenadas y puertos, URLs para *_url, y strings para el resto.
set -euo pipefail

dir=${1:?uso: scripts/fake-secrets.sh DIR_CONFIG}
out=$dir/secrets.yaml
[[ -e $out ]] && { echo "ya existe $out: no lo toco" >&2; exit 0; }

grep -rhoE '!secret[[:space:]]+[A-Za-z0-9_]+' --include='*.yaml' --include='*.yml' "$dir" 2>/dev/null \
  | awk '{print $2}' | sort -u | while read -r k; do
    case $k in
      *latitude*|*lat) echo "$k: 0.0" ;;
      *longitude*|*lon|*lng) echo "$k: 0.0" ;;
      *elevation*|*port*) echo "$k: 1" ;;
      *url*|*uri*) echo "$k: http://example.invalid" ;;
      *) echo "$k: fake-$k" ;;
    esac
  done > "$out"
echo "secrets.yaml falso con $(grep -c . "$out" || true) claves en $out"
