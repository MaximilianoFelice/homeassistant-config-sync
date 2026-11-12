#!/bin/sh
# Corre DENTRO del contenedor de la app (lo manda scripts/supervisor-test.sh).
# Prueba lo que solo existe ahí: SUPERVISOR_TOKEN, el proxy del Supervisor
# hacia Core y los mounts /homeassistant y /addon_configs. El remoto es un
# repo local en /data/e2e (sin red ni GitHub).
set -u
fails=0
ok()   { echo "  OK   $*"; }
bad()  { echo "  FAIL $*"; fails=$((fails + 1)); }
state(){ sed -n 's/^estado: \([a-z_]*\).*/\1/p' "$1" | head -1; }

E=/data/e2e
rm -rf "$E" && mkdir -p "$E"
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=e2e GIT_AUTHOR_EMAIL=e2e@example.invalid GIT_COMMITTER_NAME=e2e GIT_COMMITTER_EMAIL=e2e@example.invalid

echo "== entorno de la app"
[ -n "${SUPERVISOR_TOKEN:-}" ] && ok "SUPERVISOR_TOKEN presente" || bad "sin SUPERVISOR_TOKEN"
[ -f /homeassistant/configuration.yaml ] && ok "/homeassistant montado (configuration.yaml)" || bad "/homeassistant sin configuration.yaml"
[ -d /app_configs ] && ok "/app_configs montado" || bad "/app_configs no existe ($(ls -d /addon_configs 2>/dev/null))"
touch /homeassistant/.hcs-rw && rm /homeassistant/.hcs-rw && ok "/homeassistant escribible" || bad "/homeassistant de solo lectura"
echo "  $(hcs version), $(git --version)"

cat > "$E/cfg.yaml" <<EOF
repository: file://$E/origin.git
sync_interval_minutes: 0
mappings:
  - repo: config
    box: /homeassistant
    check: true
    include: ["configuration.yaml", "automations.yaml", "scripts.yaml", "scenes.yaml"]
EOF
git init -q --bare -b main "$E/origin.git"
A="--config $E/cfg.yaml --state-dir $E/state --allow-file-remote"
cp /homeassistant/configuration.yaml "$E/configuration.before"

echo "== init + primer sync (checker: Supervisor)"
hcs init $A > "$E/init.txt" 2>&1 && ok "init" || bad "init: $(cat "$E/init.txt")"
hcs sync $A > "$E/s1.txt" 2>&1
[ "$(state "$E/s1.txt")" = idle ] && ok "primer sync idle" || bad "primer sync: $(cat "$E/s1.txt")"

echo "== cambio válido desde 'main' → check_config por el proxy + reload"
git clone -q "$E/origin.git" "$E/dev"
printf '\n# hcs inapp test\n' >> "$E/dev/config/configuration.yaml"
git -C "$E/dev" commit -qam "inapp: comentario" && git -C "$E/dev" push -q origin main
hcs sync $A > "$E/s2.txt" 2>&1
[ "$(state "$E/s2.txt")" = idle ] && ok "sync idle" || bad "sync: $(cat "$E/s2.txt")"
grep -q 'aplicado (check: true, reload: true)' "$E/s2.txt" && ok "check_config y reload_all por el Supervisor" || bad "no hubo check/reload: $(cat "$E/s2.txt")"
grep -q 'hcs inapp test' /homeassistant/configuration.yaml && ok "cambio aplicado en /homeassistant" || bad "el cambio no llegó"

echo "== integración inválida desde 'main' → rechazo (warning nuevo)"
git -C "$E/dev" pull -q
printf '\ninput_boolean:\n  hcs_x:\n    initial: no_es_un_bool\n' >> "$E/dev/config/configuration.yaml"
git -C "$E/dev" commit -qam "inapp: input_boolean roto" && git -C "$E/dev" push -q origin main
hcs sync $A > "$E/s3.txt" 2>&1
[ "$(state "$E/s3.txt")" = rejected ] && ok "rechazado" || bad "no se rechazó: $(cat "$E/s3.txt")"
grep -q input_boolean "$E/s3.txt" && ok "el rechazo nombra input_boolean" || bad "rechazo sin detalle"
grep -q 'no_es_un_bool' /homeassistant/configuration.yaml && bad "quedó el cambio inválido en /homeassistant" || ok "rollback en /homeassistant"

echo "== revertir → vuelve a idle"
git -C "$E/dev" revert --no-edit HEAD >/dev/null && git -C "$E/dev" push -q origin main
hcs sync $A > "$E/s4.txt" 2>&1
[ "$(state "$E/s4.txt")" = idle ] && ok "idle" || bad "después de revertir: $(cat "$E/s4.txt")"

echo "== limpieza: dejar configuration.yaml como estaba"
cp "$E/configuration.before" /homeassistant/configuration.yaml && rm -rf "$E"

echo
[ "$fails" -eq 0 ] && echo "INAPP OK" || echo "INAPP: $fails fallas"
exit "$fails"
