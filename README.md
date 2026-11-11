# homeassistant-config-sync

Sync bidireccional entre la configuración de Home Assistant y un repo git, pensado para correr como app (add-on) de Home Assistant OS.

- Lo que cambia en el box se commitea en la rama `box`; lo que llega a `main` se aplica al box solo si pasa el check de config.
- Los conflictos frenan el sync y avisan; nunca se pisa nada en silencio.
- Solo SSH con una deploy key; nada de tokens. Los secretos no salen del box.

Estado: **v0** (motor como CLI, testeable en local). Ver [docs/design.md](docs/design.md) y [docs/plan-v0.md](docs/plan-v0.md).

## Desarrollo

```bash
mise install          # Go y golangci-lint fijados en mise.toml
mise run check        # lint + tests con -race + build
```

## Probar el v0 contra una copia del box

```bash
scripts/fetch-box-copy.sh   # copia de solo lectura por `ssh <host>`, sin secretos ni .git
scripts/e2e-sandbox.sh      # escenario completo en .sandbox/e2e, sin tocar el box ni GitHub
```

## CLI

```bash
hcs init     --config cfg.yaml --state-dir DIR   # repo bare, deploy key, known_hosts, token de la API
hcs plan     --config cfg.yaml --state-dir DIR   # qué haría un sync, sin tocar nada
hcs sync     --config cfg.yaml --state-dir DIR   # un ciclo
hcs serve    --config cfg.yaml --state-dir DIR   # API HTTP + scheduler
hcs status   --state-dir DIR
hcs scan-secrets PATH...
```

Licencia: MIT.
