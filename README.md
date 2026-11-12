# homeassistant-config-sync

Two-way sync between the Home Assistant configuration and a git repo, as a Home Assistant OS app (add-on) called **Git Config Sync**, plus an optional integration that brings its state into Home Assistant.

- What changes on the box is committed to the `box` branch; what arrives on `main` is applied to the box only if it passes the config check, and automations that break after the reload are reverted.
- Conflicts stop the sync and send a notification; nothing is ever silently overwritten.
- SSH with a deploy key only; no tokens. Secrets never leave the box: a secret scanner (gitleaks rules plus Home Assistant ones) blocks the commit.
- Modes: `bidirectional` (default), `push_only` (backup), `pull_only` (the repo is the source of truth).

Status: v0–v3 implemented and tested against a real Supervisor 2026.11 and Core 2026.11.1 in Docker; no release published yet. See [docs/design.md](docs/design.md).

## Install

**The app.** In Home Assistant: Settings → Apps → App store → ⋮ → Repositories, add `https://github.com/MaximilianoFelice/homeassistant-config-sync`, and install **Git Config Sync**. Set `repository` to the SSH URL of your config repo, start the app, and add the public key it shows (in its web UI or log) as a deploy key **with write access** on that repo. The first sync waits for you to review it in the app's UI. Details in [homeassistant_config_sync/DOCS.md](homeassistant_config_sync/DOCS.md).

**The integration (optional).** In HACS: ⋮ → Custom repositories, add the same URL as an *Integration*, install **Git Config Sync**, and restart Home Assistant. The running app announces itself through Supervisor discovery, so the integration shows up under Settings → Devices & services → Discovered; confirm it. It adds:

| Entity | What it shows |
|---|---|
| `sensor.git_config_sync_sync_state` | `idle`, `syncing`, `conflict`, `rejected`, `blocked_secrets`, `bootstrap_review`, `suspicious_snapshot`, `error` |
| `sensor.git_config_sync_last_sync` | Time of the last finished cycle |
| `binary_sensor.git_config_sync_needs_attention` | On when the state needs a person (conflict, rejected, secrets, review, error) |
| `binary_sensor.git_config_sync_restart_recommended` | On when an applied change needs a Home Assistant restart |
| `button.git_config_sync_sync_now` | Runs a cycle now |

It updates as soon as the app finishes a cycle (server-sent events), with a poll every 2 minutes as a fallback.

## Development

Tools are pinned in [mise.toml](mise.toml) (Go, golangci-lint, Node, Python, uv).

```bash
mise install
mise run check             # lint + Go tests with -race + build
mise run ui:build          # Lit web UI, embedded in the binary
mise run integration:test  # integration tests with the Home Assistant test harness
```

Heavier suites, all in Docker and on synthetic fixtures:

```bash
mise run test:e2e          # E2E on testdata/ha-config (real check_config)
mise run test:ha-core      # HA client against a real Core
mise run supervisor:up     # real Supervisor with the local app
mise run test:supervisor   # build, options, startup and a full cycle of the app
scripts/integration-e2e.sh # discovery → config flow → entities → Sync now
mise run supervisor:down
```

CI runs on every push to `main`: lint, Go tests, E2E with the no-op checker, UI build and tests, integration tests, HACS validation and hassfest.

## Releases

A `vX.Y.Z` tag triggers [the release workflow](.github/workflows/release.yml): the full test suite (including the Docker ones and the Supervisor test), multi-arch images (`amd64`, `aarch64`) pushed to `ghcr.io/maximilianofelice/{arch}-homeassistant-config-sync`, and a GitHub release with the `hcs` binaries and checksums.

```bash
scripts/release.sh 0.0.2   # bumps the versions, checks the changelog, commits and tags
git push --follow-tags
```

`scripts/release-verify.sh` (also run by the workflow) checks that the tag, `config.yaml`, `manifest.json` and the changelog agree.

## CLI

The engine is a single binary, `hcs`, which can also run outside Home Assistant:

```bash
hcs init     --config cfg.yaml --state-dir DIR   # bare repo, deploy key, known_hosts, API token
hcs plan     --config cfg.yaml --state-dir DIR   # what a sync would do, without touching anything
hcs sync     --config cfg.yaml --state-dir DIR   # one cycle
hcs serve    --config cfg.yaml --state-dir DIR   # HTTP API, web UI and scheduler
hcs status   --state-dir DIR
hcs deploy-key --state-dir DIR [--rotate]
hcs scan-secrets PATH...
```

License: MIT.
