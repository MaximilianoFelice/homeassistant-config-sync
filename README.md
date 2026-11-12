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

## Pruebas de punta a punta

```bash
mise run test:e2e          # E2E sobre la config sintética de testdata/ (check_config real en Docker)
mise run test:ha-core      # cliente de HA contra un Core real en Docker
mise run supervisor:up     # Supervisor real en Docker con la app local
mise run test:supervisor   # build, opciones, arranque y ciclo completo de la app
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
