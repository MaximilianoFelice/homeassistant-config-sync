#!/bin/sh
# Arranque de la app: inicializa /data (repo bare, deploy key, token de la
# API), muestra la deploy key y levanta la API, el scheduler y stdin.
set -eu

options=/data/options.json

if ! hcs init --config "$options" --state-dir /data; then
  echo "La configuración no es válida. Revisá la pestaña Configuración de la app (por ejemplo, 'repository' tiene que ser git@host:owner/repo.git)."
  exit 1
fi

echo "Deploy key de esta app (cargala en GitHub → Settings → Deploy keys, con Allow write access):"
hcs deploy-key --state-dir /data

exec hcs serve --config "$options" --state-dir /data --listen 0.0.0.0:8099 --stdin --sync-on-start
