# Changelog

## 0.0.1

First release.

- Two-way sync between the Home Assistant configuration and a git repo: the box's edits are committed to the `box` branch and merged with `main`; changes from `main` are applied only if they pass the checks.
- Checks before applying: YAML/JSON syntax, Home Assistant's config check (rejecting changes that add warnings, which is how HA reports an integration it would disable), and automations that become unavailable after the reload. A rejected change is rolled back.
- Conflicts stop the sync and notify you; nothing is overwritten silently.
- Secrets never leave the box: a hard denylist (`secrets.yaml`, databases, `.storage`, keys, logs) plus a secret scanner (gitleaks rules and Home Assistant rules) that blocks the commit.
- SSH only, with a deploy key the app generates. No tokens.
- Modes: `bidirectional`, `push_only`, `pull_only`.
- Web UI in the sidebar (ingress): status, preview, conflict, deploy key, sync now.
- Events (`homeassistant_config_sync`) and a persistent notification when something needs you.
