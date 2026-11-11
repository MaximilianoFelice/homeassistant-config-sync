#!/usr/bin/env bash
# E2E del motor sobre la config sintética de testdata/ha-config.
#
# Todo pasa en .sandbox/e2e (ignorado por git): el "box" es una copia de la
# config de ejemplo, el "GitHub" es un repo bare local sembrado con una
# versión anterior de esa config (como un repo que ya existía), y "el
# usuario" es un clon local que commitea y pushea a main. Sin red.
#
# El check_config es el real de Home Assistant en Docker (scripts/ha-check.sh).
# Con E2E_CHECKER=noop se saltea (para entornos sin Docker).
#
# Imprime solo paths, conteos y estados.
set -uo pipefail

cd "$(dirname "$0")/.."
S=$PWD/.sandbox/e2e
H=$PWD/bin/hcs
fixture=$PWD/testdata/ha-config
box=$S/box/homeassistant

mise run build >/dev/null || exit 2
rm -rf "$S" && mkdir -p "$S/out"

# --- El box: la config de ejemplo + lo que nunca puede estar en el repo -----
cp -a "$fixture" "$S/box"
rm -f "$S/box/README.md"
scripts/fake-secrets.sh "$box" >/dev/null
printf 'wifi_password: fake\n' > "$box/esphome/secrets.yaml"
head -c 4096 /dev/urandom > "$box/home-assistant_v2.db"
printf 'log de mentira\n' > "$box/home-assistant.log"
printf '{"data": {"users": []}}\n' > "$box/.storage/auth"
mkdir -p "$box/www" && printf 'JPEG\0\0\0binario' > "$box/www/foto.jpg"
# Un ESPHome viejo con la clave escrita a mano (el caso que el escáner frena).
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

# git del "usuario" aislado de cualquier config real.
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=e2e GIT_AUTHOR_EMAIL=e2e@example.invalid GIT_COMMITTER_NAME=e2e GIT_COMMITTER_EMAIL=e2e@example.invalid

fails=0
pass() { echo "  OK   $*"; }
fail() { echo "  FAIL $*"; fails=$((fails + 1)); }
expect_state() { # expect_state ESTADO ARCHIVO_DE_SALIDA
  local got
  got=$(sed -n 's/^estado: \([a-z_]*\).*/\1/p' "$2" | head -1)
  if [[ $got == "$1" ]]; then pass "estado $1"; else fail "estado $got, quería $1 (ver $2)"; fi
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
run() { # run NOMBRE [flags…] → deja la salida en $S/out/NOMBRE.txt
  local name=$1; shift
  "$H" "$@" "${A[@]}" > "$S/out/$name.txt" 2>&1
  echo $? > "$S/out/$name.exit"
}
dev() { git -C "$S/dev" "$@"; }
origin() { git --git-dir="$S/origin.git" "$@"; }

echo "== 1. un repo que ya existía, con una versión anterior de la config"
git init -q --bare -b main "$S/origin.git"
git init -q -b main "$S/seed"
mkdir -p "$S/seed/config" "$S/seed/app_configs"
cp -a "$fixture/homeassistant/." "$S/seed/config/"
cp -a "$fixture/app_configs/." "$S/seed/app_configs/"
rm -rf "$S/seed/config/themes" "$S/seed/config/.storage" "$S/seed/config/custom_components"
printf '{}\n' > "$S/seed/config/customize.yaml"            # el box cambió esto después
cp "$box/esphome/sensor-legacy.yaml" "$S/seed/config/esphome/"  # un secreto que ya se había filtrado
printf 'home_latitude: 0\n' > "$S/seed/config/secrets.yaml"    # y un secrets.yaml que nunca debió subir
printf '# casa\n' > "$S/seed/README.md"
git -C "$S/seed" add -A && git -C "$S/seed" commit -qm "seed: repo existente"
git -C "$S/seed" push -q "$S/origin.git" main
echo "  main sembrado con $(origin ls-tree -r --name-only main | wc -l) archivos"

echo "== 2. init"
run init init
[[ $(cat "$S/out/init.exit") == 0 ]] && pass "init" || fail "init (ver $S/out/init.txt)"

echo "== 3. plan (dry-run)"
run plan plan
echo "  el box subiría: $(grep -cE '^  [AMD] ' "$S/out/plan.txt") cambios"
grep -q 'salteado: config/www/foto.jpg' "$S/out/plan.txt" || true

echo "== 4. primer sync contra el repo existente: bootstrap con revisión"
# Los secretos que ya están en main sin cambios no frenan: el escáner mira
# solo lo que el box cambia (ver paso 13).
run sync1 sync
expect_state bootstrap_review "$S/out/sync1.txt"
if grep -q 'secreto:' "$S/out/sync1.txt"; then fail "bloqueó por secretos que ya estaban en main"; else pass "sin bloqueo por lo que ya estaba en main"; fi
diff=$(origin diff --name-status main box | sort)
want=$(printf 'A\tconfig/.storage/lovelace\nA\tconfig/themes/oscuro.yaml\nM\tconfig/customize.yaml\n' | sort)
[[ $diff == "$want" ]] && pass "box difiere de main exactamente en lovelace, el theme y customize" || fail "diff inesperado:\n$diff"
leaked=$(origin ls-tree -r --name-only box | grep -E 'home-assistant_v2\.db|home-assistant\.log|\.storage/auth|www/foto\.jpg|custom_components|esphome/secrets\.yaml' || true)
[[ -z $leaked ]] && pass "nada de la denylist, binarios ni HACS en box" || fail "se subió: $leaked"

echo "== 5. aceptar el bootstrap"
run sync2 sync --accept-bootstrap
expect_state idle "$S/out/sync2.txt"
[[ $(origin rev-parse main) == $(origin rev-parse box) ]] && pass "main == box" || fail "main != box"
origin cat-file -e main:config/secrets.yaml 2>/dev/null && pass "el secrets.yaml que ya estaba en main se conserva, pero nunca baja al box" || true
[[ $(cat "$box/secrets.yaml") != "home_latitude: 0" ]] && pass "secrets.yaml del box intacto" || fail "se pisó secrets.yaml del box"

echo "== 6. segundo sync sin cambios: no-op"
run sync3 sync
expect_state idle "$S/out/sync3.txt"
grep -q '^push:' "$S/out/sync3.txt" && fail "pusheó sin cambios" || pass "sin push"

git clone -q "$S/origin.git" "$S/dev"

echo "== 7. edición desde otra máquina → llega al box"
printf '# e2e: editado desde otra máquina\n' >> "$S/dev/config/scripts.yaml"
dev commit -qam "e2e: scripts" && dev push -q origin main
run sync4 sync
expect_state idle "$S/out/sync4.txt"
grep -q 'editado desde otra máquina' "$box/scripts.yaml" && pass "scripts.yaml aplicado en el box" || fail "scripts.yaml no llegó"
grep -q 'aplicado (check: true' "$S/out/sync4.txt" && pass "pasó por check_config" || fail "no corrió el check"

echo "== 8. edición en el box → llega a main"
printf '# e2e: editado en la UI\n' >> "$box/customize.yaml"
run sync5 sync
expect_state idle "$S/out/sync5.txt"
origin show main:config/customize.yaml | grep -q 'editado en la UI' && pass "customize.yaml en main" || fail "customize.yaml no llegó a main"

echo "== 9. conflicto y resolución"
dev pull -q
printf '# e2e: línea desde otra máquina\n' >> "$S/dev/config/packages/luces.yaml"
dev commit -qam "e2e: luces" && dev push -q origin main
printf '# e2e: línea desde la UI\n' >> "$box/packages/luces.yaml"
run sync6 sync
expect_state conflict "$S/out/sync6.txt"
grep -q 'conflicto: config/packages/luces.yaml' "$S/out/sync6.txt" && pass "conflicto en packages/luces.yaml" || fail "conflicto mal reportado"
grep -q 'desde la UI' "$box/packages/luces.yaml" && ! grep -q 'desde otra máquina' "$box/packages/luces.yaml" && pass "el box no se tocó" || fail "el box se tocó en un conflicto"
dev fetch -q origin
dev merge -q origin/box -m "e2e: merge box" >/dev/null 2>&1
grep -v -E '^(<<<<<<<|=======|>>>>>>>)' "$S/dev/config/packages/luces.yaml" > "$S/out/luces.tmp" && mv "$S/out/luces.tmp" "$S/dev/config/packages/luces.yaml"
dev add config/packages/luces.yaml && dev commit -qm "e2e: resuelvo luces (me quedo con las dos líneas)" && dev push -q origin main
run sync7 sync
expect_state idle "$S/out/sync7.txt"
grep -q 'desde otra máquina' "$box/packages/luces.yaml" && grep -q 'desde la UI' "$box/packages/luces.yaml" && pass "resolución aplicada en el box" || fail "la resolución no llegó"

rejected() { # rejected NOMBRE ARCHIVO_DEL_REPO CONTENIDO_A_AGREGAR TEXTO_ESPERADO
  local name=$1 path=$2 add=$3 want=$4 rel=${2#config/}
  dev pull -q
  cp "$box/$rel" "$S/out/$name.before"
  printf '%b' "$add" >> "$S/dev/$path"
  dev commit -qam "e2e: $name" && dev push -q origin main
  run "$name" sync
  expect_state rejected "$S/out/$name.txt"
  grep -q -- "$want" "$S/out/$name.txt" && pass "el rechazo explica: $want" || fail "el rechazo no explica qué pasó"
  cmp -s "$box/$rel" "$S/out/$name.before" && pass "$rel intacto (rollback)" || fail "$rel cambió"
  dev revert --no-edit HEAD >/dev/null && dev push -q origin main
  run "$name-revert" sync
  expect_state idle "$S/out/$name-revert.txt"
}

echo "== 10. YAML roto desde main → rechazado por el pre-check"
rejected yaml-roto config/packages/clima.yaml 'roto: [\n' 'clima.yaml'

if [[ $checker == command:* ]]; then
  echo "== 11. integración inválida (warning de check_config) → rechazada"
  rejected integracion config/configuration.yaml '\ninput_boolean:\n  e2e_x:\n    initial: no_es_un_bool\n' 'input_boolean'
  echo "== 12. error de config de core (http) → rechazado"
  rejected http config/configuration.yaml '\nhttp:\n  server_port: no_es_numero\n' "'http'"
else
  echo "== 11-12. salteados: E2E_CHECKER=$checker"
fi

echo "== estado final"
run status status
sed 's/^/  /' "$S/out/status.txt"
[[ $(origin rev-parse main) == $(origin rev-parse box) ]] && pass "main == box" || fail "main != box"

echo "== 13. el mismo box contra un repo vacío: el secreto de ESPHome frena todo"
rm -rf "$S/state" "$S/origin.git"
git init -q --bare -b main "$S/origin.git"
run init2 init
run sync8 sync
expect_state blocked_secrets "$S/out/sync8.txt"
grep -q 'secreto: config/esphome/sensor-legacy.yaml' "$S/out/sync8.txt" && pass "señala sensor-legacy.yaml" || fail "no señaló el archivo"
grep -qF "$legacy_pw" "$S/out/sync8.txt" && fail "la salida incluye el valor del secreto" || pass "la salida no incluye el valor"
[[ -z $(origin for-each-ref) ]] && pass "no se pusheó nada" || fail "se pusheó algo con secretos"
git --git-dir="$S/state/repo.git" for-each-ref --format='%(refname)' | grep -q heads/box && fail "box local tiene commits con secretos" || pass "ni siquiera hay commits locales"

echo
if (( fails == 0 )); then echo "E2E OK"; else echo "E2E: $fails fallas (salidas en $S/out)"; fi
exit $(( fails > 0 ))
