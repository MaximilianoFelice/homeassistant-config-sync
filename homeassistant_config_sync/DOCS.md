# Git Config Sync

Syncs the Home Assistant configuration with a git repo in both directions.

- What changes in Home Assistant (UI, file editor) is committed to the `box` branch and reaches `main`.
- What is pushed to `main` is applied only if it passes the config check. If it does not, the change is rolled back and you get a notification.
- If both ends touched the same thing, the sync stops and notifies you. You resolve the merge as usual (on your computer or on GitHub).
- `secrets.yaml`, databases, `.storage` (except dashboards), and keys never leave the box.

## Setup

1. Create a (private) repo on GitHub.
2. In **Configuration**, set `repository: git@github.com:you/your-repo.git`.
3. Start the app. The deploy key (`ssh-ed25519 …`) shows up in the **Log**.
4. On GitHub: your repo → Settings → Deploy keys → Add deploy key. Paste it and check **Allow write access**.
5. Done. If the repo already had content, the first sync pushes the `box` branch and waits for you to review it and merge it into `main`.

## Triggering a sync manually

Action `hassio.addon_stdin` with `addon: local_homeassistant_config_sync` and `input: sync`.

## Status

The app fires the `homeassistant_config_sync` event and, when you need to do something, leaves a persistent notification.

More details in the [design](https://github.com/MaximilianoFelice/homeassistant-config-sync/blob/main/docs/design.md).
