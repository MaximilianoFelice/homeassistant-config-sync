#!/usr/bin/env bash
# Test de integración del cliente de HA (internal/ha) contra un Home
# Assistant Core real en Docker, versión fijada.
#
# Levanta un Core descartable con una config mínima (sin integraciones de
# red), hace el onboarding con un usuario de prueba de contraseña aleatoria
# (nunca se imprime ni se guarda), saca un access token de corta duración y
# corre `go test -tags integration ./internal/ha/`. Al final borra el
# contenedor y su volumen.
#
# Lo único que no cubre es el proxy del Supervisor: acá el cliente le habla
# directo a Core en /api en vez de a http://supervisor/core/api.
set -euo pipefail
cd "$(dirname "$0")/.."

image=${HA_IMAGE:-ghcr.io/home-assistant/home-assistant:2026.11.1}
name=hcs-ha-core-test
port=${HA_PORT:-18123}
base=http://127.0.0.1:$port

cleanup() { docker rm -f -v "$name" >/dev/null 2>&1 || true; docker volume rm "$name" >/dev/null 2>&1 || true; }
trap cleanup EXIT
cleanup

docker volume create "$name" >/dev/null
docker run --rm -v "$name:/config" --entrypoint sh "$image" -c 'cat > /config/configuration.yaml <<EOF
homeassistant:
  name: hcs-test
  latitude: 0
  longitude: 0
  elevation: 0
  unit_system: metric
  time_zone: UTC
http:
api:
config:
onboarding:
input_boolean:
  hcs_test:
EOF'
docker run -d --name "$name" -p "127.0.0.1:$port:8123" -v "$name:/config" "$image" >/dev/null

printf "esperando a Home Assistant"
for _ in $(seq 1 120); do
  if curl -fs "$base/api/onboarding" >/dev/null 2>&1; then break; fi
  printf "."; sleep 1
done
echo
curl -fs "$base/api/onboarding" >/dev/null || { echo "HA no levantó"; docker logs --tail 30 "$name"; exit 1; }

client_id="$base/"
password=$(head -c 24 /dev/urandom | base64 | tr -d '/+=')
auth_code=$(curl -fs -X POST "$base/api/onboarding/users" -H 'Content-Type: application/json' \
  -d "{\"client_id\": \"$client_id\", \"name\": \"hcs test\", \"username\": \"hcs-test\", \"password\": \"$password\", \"language\": \"es\"}" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["auth_code"])')
unset password
token=$(curl -fs -X POST "$base/auth/token" -d grant_type=authorization_code -d "code=$auth_code" -d "client_id=$client_id" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])')
echo "onboarding hecho; corriendo los tests de integración"

HCS_HA_URL="$base/api" HCS_HA_TOKEN="$token" HCS_HA_CONTAINER="$name" \
  mise exec -- go test -count=1 -tags integration -run Integration -v ./internal/ha/
