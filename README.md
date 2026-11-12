# homeassistant-config-sync

Two-way sync between your Home Assistant folders and a git repo, using plain git, as a Home Assistant OS app called **Git Config Sync**, plus an optional integration that brings its state into Home Assistant.

- The repo mirrors the folders the app can see (`homeassistant/`, `app_configs/`, `share/`, `ssl/`, `media/`, `backup/`, `local_apps/`), or just one of them with the `root` option.
- What changes on the box is committed to the `box` branch and reaches `main`; what is pushed to `main` is applied to the box, and Home Assistant's config is checked and reloaded.
- What is synced is the repo's own `.gitignore`; merges follow git's rules (`-X` options, `.gitattributes`). Conflicts stop the sync and you merge `box` into `main` like any branch.
- Any git host over HTTPS with an access token. A secret scanner (gitleaks) warns about possible secrets, never printing them.

Status: v0.1.0. See [docs/design.md](docs/design.md) and the [changelog](homeassistant_config_sync/CHANGELOG.md).

## Install

**The app.** In Home Assistant: Settings → Apps → App store → ⋮ → Repositories, add `https://github.com/MaximilianoFelice/homeassistant-config-sync`, and install **Git Config Sync**. In its Configuration, set `repository` (your repo's HTTPS URL) and `token` (an access token with read and write access to the repo's contents; on GitHub, a fine-grained token limited to that repo). Start it. If the repo already has content, the first sync pushes only the `box` branch and waits for you to merge it into `main`. Details in [homeassistant_config_sync/DOCS.md](homeassistant_config_sync/DOCS.md).

**The integration (optional).** In HACS: ⋮ → Custom repositories, add the same URL as an *Integration*, install **Git Config Sync**, and restart Home Assistant. The running app announces itself through Supervisor discovery, so the integration shows up under Settings → Devices & services → Discovered; confirm it. It adds:

| Entity | What it shows |
|---|---|
| `sensor.git_config_sync_sync_state` | `idle`, `syncing`, `conflict`, `review`, `mass_delete`, `too_large`, `path_collision`, `blocked_secrets`, `check_failed`, `error` |
| `sensor.git_config_sync_last_sync` | Time of the last finished cycle |
| `sensor.git_config_sync_possible_secrets` | How many possible secrets the scanner sees in the repo (paths, rules and fingerprints as attributes; never the values) |
| `binary_sensor.git_config_sync_needs_attention` | On when the state needs a person (anything but `idle` and `syncing`) |
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
mise run test:supervisor   # install, options, startup and a full cycle inside the app (root /)
scripts/integration-e2e.sh # discovery → config flow → entities → Sync now
mise run supervisor:down
```

CI runs on every push to `main`: lint, Go tests, E2E with the no-op checker, UI build and tests, integration tests, HACS validation and hassfest.

## Releases

A `vX.Y.Z` tag triggers [the release workflow](.github/workflows/release.yml): the full test suite (including the Docker ones and the Supervisor test), multi-arch images (`amd64`, `aarch64`) pushed to `ghcr.io/maximilianofelice/{arch}-homeassistant-config-sync`, and a GitHub release with the `hcs` binaries and checksums.

```bash
scripts/release.sh 0.1.1   # bumps the versions, checks the changelog, commits and tags
git push --follow-tags
```

`scripts/release-verify.sh` (also run by the workflow) checks that the tag, `config.yaml`, `manifest.json` and the changelog agree.

## CLI

The engine is a single binary, `hcs`, which can also run outside Home Assistant:

```bash
hcs init     --config cfg.yaml --state-dir DIR   # bare repo, askpass program, API token
hcs plan     --config cfg.yaml --state-dir DIR   # what a sync would do, without writing anything
hcs sync     --config cfg.yaml --state-dir DIR   # one cycle
hcs serve    --config cfg.yaml --state-dir DIR   # HTTP API, web UI and scheduler
hcs status   --state-dir DIR
hcs scan-secrets DIR                             # gitleaks with DIR's .gitleaks.toml / .gitleaksignore
```

`--fs-root DIR` makes the root resolve under `DIR` instead of `/` (tests and local sandboxes).

License: MIT.
