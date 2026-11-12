#!/usr/bin/env bash
# A real Home Assistant Supervisor, in Docker, to test the app without
# touching the box. Uses the official add-on devcontainer (Docker in Docker,
# privileged container) and mounts a staged copy of homeassistant_config_sync/
# (.sandbox/addon-local, without the `image:` line, so the Supervisor builds
# it locally) as a local app.
#
#   scripts/supervisor-dev.sh up      starts everything (slow the first time)
#   scripts/supervisor-dev.sh ha …    runs the Supervisor's `ha` CLI
#   scripts/supervisor-dev.sh logs    Supervisor log
#   scripts/supervisor-dev.sh down    removes the container and its volumes
#
# Core ends up at http://127.0.0.1:7123 (localhost only). Supervisor 2026.11
# runs Core on port 80 of its host network, so that is what 7123 maps to.
set -euo pipefail
cd "$(dirname "$0")/.."

name=hcs-supervisor
image=${DEVCONTAINER_IMAGE:-ghcr.io/home-assistant/devcontainer:addons}
addon=$PWD/.sandbox/addon-local

in_dc() { docker exec "$name" "$@"; }

# Runs the Supervisor's `ha` CLI, retrying while the Supervisor is busy with
# another job of the same group (for instance, still installing Core when
# Core already reports a version): "Another job is running for job group …".
ha_retry() {
  local out
  for _ in $(seq 1 120); do
    if out=$(in_dc docker exec hassio_cli ha "$@" 2>&1); then
      printf '%s\n' "$out"; return 0
    fi
    grep -q 'Another job is running' <<<"$out" || { printf '%s\n' "$out" >&2; return 1; }
    sleep 5
  done
  printf '%s\n' "$out" >&2; return 1
}

case ${1:-} in
  up)
    if docker container inspect "$name" >/dev/null 2>&1; then
      echo "already running"; exit 0
    fi
    [[ -x $addon/bin/hcs-amd64 ]] || scripts/build-addon.sh --stage "$addon"
    docker volume create "$name-docker" >/dev/null
    docker volume create "$name-share" >/dev/null
    docker run -d --name "$name" --privileged \
      -v "$name-docker:/var/lib/docker" \
      -v "$name-share:/mnt/supervisor" \
      -v "$addon:/mnt/supervisor/addons/local/homeassistant_config_sync" \
      -p 127.0.0.1:7123:80 \
      --entrypoint sleep "$image" infinity >/dev/null
    # The devcontainer uses the Supervisor's dev channel, which may expect a
    # different layout than the one this image prepares. Use the stable
    # channel, the same one the box runs (or the one requested with CHANNEL=).
    channel=${CHANNEL:-stable}
    # Also, Supervisor 2026.11 expects the host's /run/supervisor mounted at
    # /run/os inside its container (from there it binds /run/supervisor/core
    # and /run/supervisor/supervisor/core into Core). This devcontainer image
    # predates that and does not mount it.
    in_dc bash -c "sed 's#/dev.json#/$channel.json#' /etc/supervisor_scripts/common > /tmp/common_$channel &&
      sed -e 's#/etc/supervisor_scripts/common#/tmp/common_$channel#' \
          -e 's#-v /run/udev:/run/udev:ro#-v /run/udev:/run/udev:ro -v /run/supervisor:/run/os:rw#' \
          /usr/bin/supervisor_run > /tmp/supervisor_run_$channel &&
      chmod +x /tmp/supervisor_run_$channel && sudo mkdir -p /run/supervisor/core /run/supervisor/supervisor/core"
    # supervisor_run does `stty sane` and aborts without a terminal: `script`
    # gives it a pseudo-terminal and puts everything in the log.
    in_dc bash -c "nohup script -qfc /tmp/supervisor_run_$channel /tmp/supervisor.log > /dev/null 2>&1 &"
    printf "waiting for the Supervisor and Core"
    for _ in $(seq 1 180); do
      # "landingpage" is the placeholder while the Supervisor installs Core.
      if in_dc docker exec hassio_cli ha core info --raw-json 2>/dev/null | grep '"version"' | grep -vq landingpage; then
        # The dev channel installs a Core dev build; pin the stable channel
        # and the Core version under test (HA_VERSION, default the one the
        # project targets). This also guards against a job condition that
        # blocks installs in Docker-in-Docker.
        ha_retry jobs options --ignore-conditions healthy >/dev/null
        ha_retry supervisor options --channel stable >/dev/null
        echo; echo "pinning Core to ${HA_VERSION:-2026.11.1}"
        ha_retry core update --version "${HA_VERSION:-2026.11.1}" --no-progress >/dev/null
        in_dc docker exec hassio_cli ha core info | grep -E '^(version|machine|arch):' || true
        exit 0
      fi
      printf "."; sleep 5
    done
    echo; echo "did not come up in 15 minutes; check: scripts/supervisor-dev.sh logs" >&2; exit 1
    ;;
  ha)
    shift; in_dc docker exec hassio_cli ha "$@"
    ;;
  restart)
    # Restarts only the Supervisor (keeps the images already pulled).
    # Kill the previous runner first: otherwise, when its Supervisor goes
    # down it would shut down the inner Docker.
    channel=${CHANNEL:-stable}
    # ('[s]' so pkill does not kill itself: its command line also contains
    # the pattern.)
    in_dc bash -c "sudo pkill -f '[s]upervisor_run_' ; sudo pkill -x script ; true"
    in_dc docker rm -f hassio_supervisor >/dev/null 2>&1 || true
    in_dc bash -c "nohup script -qfc /tmp/supervisor_run_$channel /tmp/supervisor.log > /dev/null 2>&1 &"
    echo "Supervisor restarted; check: scripts/supervisor-dev.sh logs"
    ;;
  logs)
    in_dc tail -n "${2:-60}" /tmp/supervisor.log
    ;;
  down)
    docker rm -f "$name" >/dev/null 2>&1 || true
    docker volume rm "$name-docker" "$name-share" >/dev/null 2>&1 || true
    echo "removed"
    ;;
  *)
    sed -n '2,12p' "$0"; exit 2
    ;;
esac
