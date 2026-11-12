#!/usr/bin/env bash
# Supervisor de Home Assistant real, en Docker, para probar la app sin tocar
# el box. Usa el devcontainer oficial de add-ons (Docker adentro de Docker,
# contenedor privilegiado) y monta homeassistant_config_sync/ como app local.
#
#   scripts/supervisor-dev.sh up      levanta todo (la primera vez tarda)
#   scripts/supervisor-dev.sh ha …    corre el CLI `ha` del Supervisor
#   scripts/supervisor-dev.sh logs    log del Supervisor
#   scripts/supervisor-dev.sh down    borra el contenedor y sus volúmenes
#
# Core queda en http://127.0.0.1:7123 (solo localhost).
set -euo pipefail
cd "$(dirname "$0")/.."

name=hcs-supervisor
image=${DEVCONTAINER_IMAGE:-ghcr.io/home-assistant/devcontainer:addons}
addon=$PWD/homeassistant_config_sync

in_dc() { docker exec "$name" "$@"; }

case ${1:-} in
  up)
    if docker container inspect "$name" >/dev/null 2>&1; then
      echo "ya está corriendo"; exit 0
    fi
    [[ -x $addon/bin/hcs-amd64 ]] || scripts/build-addon.sh
    docker volume create "$name-docker" >/dev/null
    docker volume create "$name-share" >/dev/null
    docker run -d --name "$name" --privileged \
      -v "$name-docker:/var/lib/docker" \
      -v "$name-share:/mnt/supervisor" \
      -v "$addon:/mnt/supervisor/addons/local/homeassistant_config_sync" \
      -p 127.0.0.1:7123:8123 \
      --entrypoint sleep "$image" infinity >/dev/null
    # El devcontainer usa el canal dev del Supervisor, que puede esperar una
    # estructura distinta de la que prepara esta imagen. Se usa el canal
    # estable, el mismo que corre el box (o el que se pida con CHANNEL=).
    channel=${CHANNEL:-stable}
    # Además, el Supervisor 2026.11 espera el /run/supervisor del host montado
    # en /run/os dentro de su contenedor (de ahí bindea /run/supervisor/core y
    # /run/supervisor/supervisor/core a Core). Esta imagen del devcontainer es
    # anterior a eso y no lo monta.
    in_dc bash -c "sed 's#/dev.json#/$channel.json#' /etc/supervisor_scripts/common > /tmp/common_$channel &&
      sed -e 's#/etc/supervisor_scripts/common#/tmp/common_$channel#' \
          -e 's#-v /run/udev:/run/udev:ro#-v /run/udev:/run/udev:ro -v /run/supervisor:/run/os:rw#' \
          /usr/bin/supervisor_run > /tmp/supervisor_run_$channel &&
      chmod +x /tmp/supervisor_run_$channel && sudo mkdir -p /run/supervisor/core /run/supervisor/supervisor/core"
    # supervisor_run hace `stty sane` y aborta sin una terminal: `script` le
    # da una pseudo-terminal y deja todo en el log.
    in_dc bash -c "nohup script -qfc /tmp/supervisor_run_$channel /tmp/supervisor.log > /dev/null 2>&1 &"
    printf "esperando al Supervisor y a Core"
    for _ in $(seq 1 180); do
      # "landingpage" es el placeholder mientras Supervisor instala Core.
      if in_dc docker exec hassio_cli ha core info --raw-json 2>/dev/null | grep '"version"' | grep -vq landingpage; then
        echo; in_dc docker exec hassio_cli ha core info | grep -E '^(version|machine|arch):' || true
        exit 0
      fi
      printf "."; sleep 5
    done
    echo; echo "no levantó en 15 minutos; mirá: scripts/supervisor-dev.sh logs" >&2; exit 1
    ;;
  ha)
    shift; in_dc docker exec hassio_cli ha "$@"
    ;;
  restart)
    # Reinicia solo el Supervisor (conserva las imágenes ya bajadas).
    # Primero se mata el runner anterior: si no, al caerse su Supervisor
    # apagaría el Docker de adentro.
    channel=${CHANNEL:-stable}
    # ('[s]' para que pkill no se mate a sí mismo: su línea de comando
    # también contiene el patrón.)
    in_dc bash -c "sudo pkill -f '[s]upervisor_run_' ; sudo pkill -x script ; true"
    in_dc docker rm -f hassio_supervisor >/dev/null 2>&1 || true
    in_dc bash -c "nohup script -qfc /tmp/supervisor_run_$channel /tmp/supervisor.log > /dev/null 2>&1 &"
    echo "Supervisor reiniciado; mirá: scripts/supervisor-dev.sh logs"
    ;;
  logs)
    in_dc tail -n "${2:-60}" /tmp/supervisor.log
    ;;
  down)
    docker rm -f "$name" >/dev/null 2>&1 || true
    docker volume rm "$name-docker" "$name-share" >/dev/null 2>&1 || true
    echo "borrado"
    ;;
  *)
    sed -n '2,12p' "$0"; exit 2
    ;;
esac
