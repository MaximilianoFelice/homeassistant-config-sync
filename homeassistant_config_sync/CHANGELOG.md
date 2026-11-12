# Changelog

## 0.1.0

A redesign around plain git. **Breaking:** after updating, open the app's Configuration tab and reset the options (the old ones no longer validate), and move an existing repo to the new layout (`git mv config homeassistant`).

- The repo mirrors the folders the app can see: `homeassistant/`, `app_configs/`, `share/`, `ssl/`, `media/`, `backup/`, `local_apps/` (the same as the Terminal & SSH app). The `root` option narrows it, e.g. `root: /homeassistant` (or `config`) for Home Assistant's configuration only.
- What is synced is decided by the repo's own `.gitignore`, with git's rules. Without one, the first sync commits a base `.gitignore` that leaves out `.storage`, `.cloud`, `secrets.yaml`, `ssl/`, databases, logs, caches, what HACS installs, `backup/` and `media/`. A path that becomes ignored leaves the repo (the file stays on disk).
- Git does the work: `git add -A`, `git merge-tree` and `git read-tree -u` over the mounted folders, with a bare repo and a persistent index in `/data`. `merge_options` are passed as `-X` (`ours`, `theirs`…); per-file rules come from the repo's `.gitattributes`.
- Authentication is an HTTPS access token (`token`), for any git host. SSH, the deploy key and its rotation are gone.
- The first sync against a repo with content pushes only the `box` branch and waits for you to merge it into `main`; there is no "accept" button. Changing `root` works the same way.
- Home Assistant's config check only informs: changes from `main` are applied as they are, and the reload is skipped (and the check repeated) while the check reports errors. Nothing is rolled back. The `restart`, `check_warnings` and `mode` options are gone; a needed restart is only recommended.
- Guards: many deletions (from the box: review; from `main`: held back until you raise the threshold), files above `max_file_size_mb` (default 10) stop the sync, and with `root: /` a path from `main` that collides with the app's own folders is not written.
- The secret scanner is gitleaks with its standard configuration: it warns by default (`secret_scan.mode: warn | block | off`), scans the whole repo every sync, and takes the repo's `.gitleaks.toml` and `.gitleaksignore`. The `webhook_id` and `allow` options are gone.
- The `stdin` trigger is gone: use the integration's Sync now button (`button.press`).
- The integration adds a "Possible secrets" sensor, and its states follow the new ones (`review`, `mass_delete`, `too_large`, `path_collision`, `check_failed`).

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
- Supervisor discovery for the optional `config_sync` integration (HACS): sync state, last sync, needs attention and restart recommended entities, and a Sync now button.
