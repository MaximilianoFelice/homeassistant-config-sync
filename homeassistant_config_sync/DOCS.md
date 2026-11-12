# Git Config Sync

Syncs the Home Assistant configuration with a git repo in both directions.

- What changes in Home Assistant (UI, file editor) is committed to the `box` branch and reaches `main`.
- What is pushed to `main` is applied only if it passes the checks. If it does not, the change is rolled back and you get a notification.
- If both ends touched the same thing, the sync stops and notifies you. You resolve the merge as usual (on your computer or on GitHub).
- `secrets.yaml`, databases, `.storage` (except dashboards), logs and keys never leave Home Assistant.

## Setup

1. Create a repository (private is fine) on GitHub or any git server reachable over SSH.
2. In **Configuration**, set `repository`, e.g. `git@github.com:you/ha-config.git`.
3. Start the app and open **Git Config Sync** in the sidebar. It shows the app's deploy key (`ssh-ed25519 …`); it is also in the **Log**.
4. On GitHub: your repository → Settings → Deploy keys → Add deploy key. Paste it and check **Allow write access**.
5. Done. If the repository already had content, the first sync pushes the `box` branch and waits for you to review it: merge it into `main`, or press **Accept first sync** in the panel.

The repository layout is `config/` for `/homeassistant` and `app_configs/` for other apps' configuration (opt-in, see `mappings`). Anything else in the repository (README, CI, docs) is left alone.

## What gets synced

By default, from the Home Assistant configuration: every `*.yaml`/`*.yml`, `custom_templates/**/*.jinja`, `python_scripts/**/*.py`, `pyscript/**/*.py`, and the dashboards in `.storage/lovelace*` (upload only). `custom_components/` is left out (HACS manages it). Change it in `mappings`.

Never synced, whatever `mappings` say: `secrets.yaml`, databases, `.storage` auth and config entries, `.cloud`, `.ssh`, private keys and certificates, and logs. Files larger than `max_file_size_kb`, binaries and symlinks are skipped and reported.

## Checks before applying a change from main

1. YAML/JSON syntax.
2. Home Assistant's config check. Home Assistant reports an integration with an invalid config as a *warning* (it starts without that integration), so by default (`check_warnings: new`) a change that adds warnings is rejected too.
3. After the reload, automations that became unavailable (an invalid automation passes the config check). If any appear, the change is undone and the configuration reloaded again.

A rejected change is not retried until `main` changes again.

## Modes

- `bidirectional` (default): both ways.
- `push_only`: only publishes the `box` branch; never applies `main`.
- `pull_only`: this Home Assistant follows `main`; its own edits are discarded (they stay in the `box` branch history). Never writes `main`.

## Restarts

Changes that usually need a restart (`configuration.yaml`, `packages/`) are applied and reloaded, and you are told a restart is recommended. With `restart: auto` the app restarts Home Assistant after such a change.

## Triggering a sync

- The **Sync now** button in the panel.
- From an automation: action `hassio.addon_stdin` with `addon: <this app's slug>` and `input: sync`.
- Every `sync_interval_minutes` (default 5; 0 = manual only).

## Events and notifications

Every cycle that did something fires the `homeassistant_config_sync` event (`type`: `sync_finished`, `conflict`, `rejected`, `blocked_secrets`, `bootstrap_review`, `suspicious_snapshot`, `error`). When you need to do something you also get one persistent notification (and the `notify_service`, if set), once per problem, and it is dismissed when the sync is back to normal.

## Rotating the deploy key

Press **Generate a new key** in the panel, add the new key on GitHub and remove the old one.

More details in the [design document](https://github.com/MaximilianoFelice/homeassistant-config-sync/blob/main/docs/design.md).
