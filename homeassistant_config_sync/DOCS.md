# Git Config Sync

Syncs the folders this app can see (Home Assistant's configuration, other apps' configuration, `share`, `ssl`, `media`, `backup`, `local_apps`) with a git repo, in both directions, using plain git.

- What changes on this box is committed to the `box` branch and reaches `main`.
- What is pushed to `main` is applied to the box. If it touches Home Assistant's configuration, the app runs Home Assistant's config check, tells you the result, and reloads it.
- If both ends changed the same lines, the sync stops and tells you. You merge `box` into `main` like any branch.
- What is synced is decided by the repo's own `.gitignore`. Nothing is forbidden: the defaults only prevent accidents, and you can change them.

## Setup

1. Create a repository (private is fine) on GitHub, GitLab, Gitea or any git host with HTTPS.
2. Create an access token with read and write access to the repository's contents. On GitHub: Settings → Developer settings → Fine-grained tokens, limited to that one repository, with **Contents: Read and write**.
3. In **Configuration**, set `repository` (the HTTPS URL, e.g. `https://github.com/you/home.git`) and `token`.
4. Start the app.

The first sync never touches `main`: it pushes only the `box` branch and waits for you. What `main` holds is always your decision.

- **Empty repository:** `box` holds everything the base `.gitignore` lets through. Review it and create `main` from it, for example with `git push origin origin/box:refs/heads/main` or by creating the branch in the host's web UI. On GitHub the first branch pushed becomes the default one, so set `main` as the default branch afterwards.
- **Repository with content:** `box` goes on top of `main`. Compare `main` with `box` (a PR shows exactly how the box differs) and leave the real state in `main` by merging `box`. To keep `main` as it is, use `git merge -s ours box`.

Syncing resumes by itself once `box` is part of `main`.

## The repository

The repo mirrors the folders: `homeassistant/configuration.yaml` in the repo is `/homeassistant/configuration.yaml` here. With `root: /homeassistant` (or `config`) the repo holds only Home Assistant's configuration, without the prefix. Top-level files such as `README.md` or `.github/` are kept in the repo and restored when the app restarts.

What is synced is decided by the repo's `.gitignore`, with git's exact rules. If the repo has none, the first sync commits a base one that leaves out Home Assistant's `.storage`, `.cloud`, `secrets.yaml`, `ssl/`, databases, logs, caches, what HACS installs (`custom_components/`, `www/community/`), `backup/` and `media/`. Edit it like any file. A path that becomes ignored leaves the repo on the next sync (the file stays on disk; history is not rewritten).

Merges are `git merge main` run on `box`. `merge_options` are passed as `-X` (`ours` = the box wins, `theirs` = main wins), and per-file rules go in the repo's `.gitattributes` (`merge=union`, for example).

## Guards

- **Many deletions** (more than `delete_guard_files`, or at least 5 that are more than `delete_guard_percent`): from the box, only `box` is pushed and you resolve in `main`; from `main`, nothing is applied until you raise the threshold for one sync.
- **A file larger than `max_file_size_mb`** (default 10) stops the sync until you add it to `.gitignore` or raise the limit.
- **Changing `root`** works like a first sync: only `box` is pushed and you resolve in `main`.
- With `root: /`, a top-level path from `main` that collides with the app's own container (for example `data/`) is not written.

## Home Assistant

After applying a change to Home Assistant's configuration the app runs its config check (`check_config`) and reloads it (`reload`). If the check reports errors, it does not reload and tells you; the files stay as they came from `main` and the check is repeated until it passes. Nothing is ever rolled back. Changes to `configuration.yaml`, `packages/` or `custom_components/` need a restart: you are told, the app never restarts anything.

Other apps' files are synced like any other file; the app does not restart or watch other apps. A program that keeps a file in memory (a dashboard, an app's own config) picks up a change from git when it reloads that file.

## Secrets

The secret scanner (gitleaks) looks at the whole repo every sync. By default (`secret_scan.mode: warn`) it reports possible secrets (path, line, rule; never the value) and syncs anyway; `block` stops the sync; `off` turns it off. With `warn` or `off`, a literal secret that is committed stays in git history.

The rules are gitleaks' defaults, or the repo's `.gitleaks.toml`. False positives: add the fingerprint shown in the panel to the repo's `.gitleaksignore`, or end the line with a `gitleaks:allow` comment. Example `.gitleaks.toml` that keeps the defaults and adds a rule for literal passwords in YAML:

```toml
[extend]
useDefault = true

[[rules]]
id = "literal-password"
description = "Literal password instead of !secret"
regex = '''(?i)^\s*\w*password:\s*['"]?([^!\s'"][^\s'"]{3,})'''
secretGroup = 1
keywords = ["password"]
```

## The integration (optional)

The `config_sync` integration shows this app in Home Assistant: the sync state, the last sync, whether something needs you, whether a restart is recommended, the scanner's findings, and a **Sync now** button.

1. In HACS: ⋮ → Custom repositories, add `https://github.com/MaximilianoFelice/homeassistant-config-sync` as an *Integration*, and install **Git Config Sync**.
2. **Restart Home Assistant.** This app announces itself when it starts; Home Assistant only picks up that announcement for an integration it had loaded, so without the restart nothing shows up.
3. Settings → Devices & services: **Git Config Sync** appears under *Discovered*. Press **Add**, then **Submit**. There is nothing to type.

If it does not appear in *Discovered*, add it by hand: Settings → Devices & services → Add integration → **Git Config Sync**. The host, port and API token to enter are in this app's web UI, in the **Home Assistant integration** section (shown only to administrators, through the sidebar).

## Triggering a sync

- The **Sync now** button in the panel, or the `button.git_config_sync_sync_now` entity of the integration (also from automations, with `button.press`).
- Every `sync_interval_minutes` (default 5; 0 = manual only).

## Events and notifications

Every cycle that did something fires the `homeassistant_config_sync` event (`type`: `sync_finished`, `conflict`, `review`, `mass_delete`, `too_large`, `path_collision`, `blocked_secrets`, `secrets_found`, `check_failed`, `error`). When you need to do something you also get one persistent notification (and the `notify_service`, if set), once per problem; it is dismissed when the sync is back to normal.

More details in the [design document](https://github.com/MaximilianoFelice/homeassistant-config-sync/blob/main/docs/design.md).
