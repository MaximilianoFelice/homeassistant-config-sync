# homeassistant-config-sync

Two-way sync between the Home Assistant configuration and a git repo, designed to run as a Home Assistant OS app (add-on).

- What changes on the box is committed to the `box` branch; what arrives on `main` is applied to the box only if it passes the config check.
- Conflicts stop the sync and send a notification; nothing is ever silently overwritten.
- SSH with a deploy key only; no tokens. Secrets never leave the box.

Status: **v0** (engine as a CLI, testable locally). See [docs/design.md](docs/design.md) and [docs/plan-v0.md](docs/plan-v0.md).

## Development

```bash
mise install          # Go and golangci-lint pinned in mise.toml
mise run check        # lint + tests with -race + build
```

## End-to-end tests

```bash
mise run test:e2e          # E2E on the synthetic config in testdata/ (real check_config in Docker)
mise run test:ha-core      # HA client against a real Core in Docker
mise run supervisor:up     # real Supervisor in Docker with the local app
mise run test:supervisor   # build, options, startup, and full cycle of the app
```

## CLI

```bash
hcs init     --config cfg.yaml --state-dir DIR   # bare repo, deploy key, known_hosts, API token
hcs plan     --config cfg.yaml --state-dir DIR   # what a sync would do, without touching anything
hcs sync     --config cfg.yaml --state-dir DIR   # one cycle
hcs serve    --config cfg.yaml --state-dir DIR   # HTTP API + scheduler
hcs status   --state-dir DIR
hcs scan-secrets PATH...
```

License: MIT.
