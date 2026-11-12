# homeassistant-config-sync: design

> Status: implemented in v0.1.0 (2026-11-12). Every decision here was agreed point by point with the maintainer; §15 lists them.

## 1. Goal

A Home Assistant OS app ("Git Config Sync") that keeps the folders it can see in sync with a git repo, in both directions, using plain git:

- What changes on the box (the HA UI, the file editor, other apps) is committed to the `box` branch and reaches `main`.
- What is pushed to `main` is applied to the box. When it touches Home Assistant's configuration, the app asks Home Assistant to check it, tells the user the result, and reloads it.
- When both ends changed the same lines, the sync stops and the user merges `box` into `main` like any branch. Nobody's work is silently overwritten.

Two principles shape everything below:

- **The person owns their instance.** If they want `/ssl` in their private repo, that is their decision and their risk. Guards exist to prevent accidents, never to forbid a deliberate choice. Defaults are suggestions that can be edited.
- **Standard git, not reimplemented git.** What gets synced, how merges resolve and how secrets are scanned are expressed with standard tools' own files and options (`.gitignore`, `.gitattributes`, `-X`, `.gitleaks.toml`), managed from the repo. The engine drives the git CLI; HA-specific logic is limited to checking and reloading Home Assistant.

## 2. Architecture

```
            git host (GitHub, GitLab, Gitea…) over HTTPS + access token
            ┌───────────────────────────────────────────────────┐
            │  main  ◄── people (local clones, PRs, web edits)   │
            │  box   ◄── the app only                           │
            └──────────────────────▲────────────────────────────┘
                                   │ ls-remote / fetch / push --atomic
┌──────────── App container ───────┼──────────────────────────────────────┐
│                                  │                                      │
│  scheduler ─┐                    │                                      │
│  HTTP API ──┼──► engine ──► git CLI: bare repo /data/repo.git,          │
│  (UI, integration)      │         index /data/index,                    │
│                         │         work tree = root (default /)          │
│                         │            ├─ /homeassistant  /app_configs    │
│                         │            ├─ /share  /ssl  /media  /backup   │
│                         │            └─ /local_apps                     │
│                         ├──► gitleaks (secret scanner, in-process)      │
│                         └──► Supervisor proxy (SUPERVISOR_TOKEN):       │
│                              check_config, reload_all, events,          │
│                              notifications, discovery                   │
└─────────────────────────────────────────────────────────────────────────┘
```

The engine is a Go binary (`hcs`) that runs the cycle, serves the HTTP API and the web UI (ingress), and announces itself to the optional `config_sync` integration through Supervisor discovery.

## 3. What the app sees

An app only sees the folders its `map:` asks for; the list is closed (Supervisor `MappingType`). The app maps the same seven as the official Terminal & SSH app, all read-write:

| `map:` | Supervisor folder (`/mnt/data/supervisor/…`) | In the app |
|---|---|---|
| `homeassistant_config` | `homeassistant/` | `/homeassistant` |
| `all_app_configs` | `app_configs/` | `/app_configs` |
| `share` | `share/` | `/share` |
| `ssl` | `ssl/` | `/ssl` |
| `media` | `media/` | `/media` |
| `backup` | `backup/` | `/backup` |
| `local_apps` | `apps/local/` | `/local_apps` |

Not visible to any app, and out of scope: other apps' `/data` (including their options), the Supervisor's own state (installed apps, repositories, network mounts), and HA OS host config (network, boot). A future **root mode** may export some of it through the Supervisor API; nothing here blocks that.

## 4. Root

The container's `/` is a simplified view of `/mnt/data/supervisor`: the mounts sit there as real directories (bind mounts), next to the image's system folders and the app's private `/data`.

- **Option `root`, default `/`.** Git's work tree is the root. With `/`, the repo matches the container 1:1: `homeassistant/configuration.yaml` in the repo is `/homeassistant/configuration.yaml` on the box.
- **Narrower roots** let a user start small: `root: /homeassistant` (alias `config`) syncs only Home Assistant's configuration, with no prefix in the repo; any folder inside a mount works the same way. The repo's top-level files (`.gitignore`, `README.md`…) then live inside that folder.
- **With `root: /`**, the app's internal exclude file (`/data/repo.git/info/exclude`, not the user's `.gitignore`) contains `/*`, `!/<mount>/` for each mount, and the repo's own config files (`.gitignore`, `.gitattributes`, `.gitleaks.toml`, `.gitleaksignore`), so git never walks the image's system folders or `/data`.
- **Top-level files with `root: /`** (`README.md`, `.github/`…) are written to the container's `/`. The Supervisor recreates the container on every start, so the app restores the missing ones from the index at the start of each cycle (only once the index holds this box's state; never on a first sync or a root change).
- **Changing `root` on an existing repo** is treated like a first sync (§6).

## 5. What is synced: `.gitignore`

Everything under the root is synced in both directions, except what git ignores.

- **The ignore rules are the repo's own `.gitignore` files**, with git's exact semantics. `git add -A` applies them; the engine does not parse them.
- `.gitignore` is a file like any other: a change pushed to `main` is applied to the box and takes effect from the next snapshot.
- **A tracked path that becomes ignored** leaves the repo on the next snapshot (`git rm --cached`): the file stays on disk and history is not rewritten. A path that stops being ignored is added. Both go through the mass-deletion guard.
- **Symlinks** are stored as links (git mode `120000`: the target path, never its contents). Git refuses to write through a symlink, so a link cannot be used to write outside the root.
- **A folder with its own `.git`** (an app under development in `local_apps/`, a clone in `share/`) is stored by git as a pointer, like a submodule, not its contents.

### Base `.gitignore`

Committed on the first sync when neither the repo nor the box has a `.gitignore`. Its only purpose is preventing accidents (huge files, state rewritten all the time, credentials); it is edited like any file afterwards. With a root other than `/` the app writes the lines that apply to it (`internal/engine/gitignore.go`).

```gitignore
homeassistant/.storage/
homeassistant/.cloud/
secrets.yaml
ssl/
*.db
*.db-*
*.sqlite*
*.log
*.log.*
homeassistant/.cache/
homeassistant/deps/
homeassistant/tts/
__pycache__/
.esphome/
node_modules/
homeassistant/custom_components/
homeassistant/www/community/
backup/
media/
```

`homeassistant/themes/` is deliberately synced: it mixes hand-written themes with HACS ones, and ignoring it would silently stop versioning the former.

## 6. Branches and the cycle

The box is one more clone of the repo. `main` belongs to people; `box` is written only by the app and always says what is on the box, also during a conflict. The app never force-pushes and only moves branches by fast-forward.

```
 0. lock; index in step with the local box branch
 1. ls-remote + fetch main and box
 2. SNAPSHOT   restore lost top-level files; git add -A; drop tracked paths now ignored;
               write-tree; size guard; secret scan; commit on box (parent: box)
 3. REVIEW     first sync / root change / mass deletion from the box: push only box, stop
 4. MERGE      main ancestor of box → box; box ancestor of main → main (fast-forward);
               otherwise git merge-tree --write-tree -X<opts> --attr-source=main
               conflicts → push only box, stop (state conflict)
               clean → merge commit, parents [main, box], "Merge branch 'box'"
 5. GUARDS     mass deletion from main, top-level collisions → push only box, stop
 6. APPLY      git read-tree -m -u box result   (git refuses if a file changed meanwhile:
               the cycle starts over, up to 3 times)
               then, if Home Assistant's config changed: check_config, reload
 7. PUBLISH    box := result; push --atomic result:main result:box
               (rejected because main moved → start over)
```

- **The merge's first parent is `main`**, so `git log --first-parent main` tells the human history.
- **First sync (always, even against an empty repo) and root changes:** only `box` is pushed, nothing is applied, and the state is `review`. When `main` exists the snapshot goes on top of it (not an unrelated root) so that `main..box` shows exactly how the box differs and a PR can be opened; the user leaves the real state in `main` by merging `box` (a PR or locally; `git merge -s ours box` keeps `main` as it is). When the repo is empty the user creates `main` from `box`. The app never creates `main` by itself: what it holds is always the user's decision. The app resumes when `box` is an ancestor of `main`.
- **Merging is `git merge main` on `box`:** `ours` is the box, `theirs` is `main`. `merge_options` are passed as `-X` unchanged; per-path rules come from `main`'s `.gitattributes` (`merge=union`, `-merge`…). Custom merge drivers are not supported: they would run arbitrary commands inside the app.
- **Commit messages:** merges use git's standard message; snapshots are `box: <path>` or `box: <n> changes`, with each path and its status letter in the body (cut at 100). Author: `commit_author`; committer: the engine.

## 7. Git does the work

| Step | Git |
|---|---|
| Snapshot | `git add -A`, `git ls-files -ci --exclude-per-directory=.gitignore` + `git rm --cached`, `git write-tree`, `git commit-tree` |
| Diff | `git diff-tree -r --raw` between trees |
| Merge | `git merge-tree --write-tree -X…` with `--attr-source` (needs git ≥ 2.42) |
| Apply | `git read-tree -m -u` (two-tree merge of index and work tree) |
| Restore | `git ls-files --deleted` + `git checkout-index` |

Git runs isolated: no system or global config, no hooks, no credential helpers, only the HTTPS protocol (`file://` only in tests), `core.autocrlf=false`. The repo is bare in `/data/repo.git`; the work tree and the index are given through `GIT_WORK_TREE` and `GIT_INDEX_FILE`, so no `.git` is ever written into the mounted folders. The index is persistent and doubles as git's stat cache. A dry run (`/api/v1/pending`, `hcs plan`) works on a copy of the index and moves no ref.

## 8. Home Assistant

Validating files is not this app's job, and it never rolls back on its own: when something fails, the user finds out and fixes it in `main`.

- **Config check, informational** (`check_config`, default on): after applying a change under Home Assistant's config folder, the app runs `POST /api/config/core/check_config` and reports errors and warnings.
- **Reload, quality of life** (`reload`, default on): `homeassistant.reload_all` after applying. **Skipped when the check reported errors:** Home Assistant keeps running what it had in memory, the files stay as they came from `main`, and the state is `check_failed`. The check is repeated every cycle until it passes (the fix may come from `main` or be made on the box), and the skipped reload runs then.
- **Restart:** a change to `configuration.yaml`, `packages/` or `custom_components/` sets "restart recommended" (it reflects the last change applied to Home Assistant's config). The app never restarts anything.
- **Other apps:** their files are synced like any other file. The app does not track, notify or restart other apps.
- **Known limitation:** a program that keeps a file in memory and rewrites it (dashboards in `.storage/lovelace*`, other apps' own configs) picks up a change from git when it reloads that file; if someone edits it from its UI first, the UI wins and the next snapshot commits that version.

Everything goes through the Supervisor proxy (`http://supervisor/core/api`, `SUPERVISOR_TOKEN`, `homeassistant_api: true`): no long-lived token. `hassio_api: true` (default role) is only for discovery. Timeouts: 120 s for `check_config`, 30 s for the rest.

## 9. Remote

- **HTTPS with an access token only:** the repo's HTTPS URL plus a token (a GitHub fine-grained token limited to that repo, a GitLab project access token, a Gitea token…) and an optional username. Git gets the token through `GIT_ASKPASS`: a small script in `/data` that answers from its environment, so the token is never in the URL, the git config, argv, the logs or on disk outside the app's options. TLS is verified the standard way.
- **Known trade-offs, documented:** the token lives in the app's options, which the Supervisor stores in plain text and includes in Home Assistant backups. Tokens expire; an authentication failure is reported as "the remote rejected the token", not as a generic error.

## 10. Guards

They prevent accidents; none forbids a choice. Thresholds are options.

| Guard | What happens |
|---|---|
| Mass deletion (more than `delete_guard_files` = 25 paths, or at least 5 that are more than `delete_guard_percent` = 20 %) | **From the box:** the snapshot is committed, only `box` is pushed, state `review`; the user merges `box` into `main` (or restores the files). **From `main`:** nothing is applied, state `mass_delete`; to go ahead, raise the threshold for one sync. |
| First sync and root change | §6: only `box` is pushed; the user resolves in `main`. |
| Top-level collision (only `root: /`) | A top-level path from `main` that is not a mount, not already in the repo, and exists in the container (`data/`, `etc/`…): nothing is applied, state `path_collision`. Paths with `..` or `.git` are refused by git itself. |
| File size (`max_file_size_mb`, default 10) | A larger file staged by `git add`: nothing is committed, state `too_large`, until it is ignored or the limit raised. Silently skipping it would only surface when it is too late; GitHub rejects files over 100 MB and history is never rewritten. |

## 11. Secrets

The scanner is gitleaks, linked into the binary.

- `secret_scan.mode`: `warn` (default) reports and commits; `block` stops the cycle before committing (state `blocked_secrets`); `off`.
- **It scans the whole tree of each `box` commit** every cycle (so an intermediate change cannot hide an earlier finding), with a per-blob cache. Ignored files are not in the repo and are not scanned; binary files (git's heuristic: a NUL byte in the first 8000 bytes) are synced but not scanned, as with gitleaks' CLI.
- **Rules:** gitleaks' defaults, or the repo's `.gitleaks.toml` (`[extend] useDefault = true` keeps the defaults; `extend.path` is not supported). The app applies `useDefault` itself because gitleaks' loader counts extensions in a package variable that never resets.
- **False positives:** the repo's `.gitleaksignore` (fingerprints `path:rule:line`, also accepted with a leading commit) and `gitleaks:allow` comments.
- **Findings never carry the value**: path, line, rule and fingerprint only. They are a state (entity, UI, `status`); the notification fires only when the set changes.

## 12. Monitoring and triggers

- The Supervisor already gives the app a **Logs** tab, state, CPU, memory and watchdog; the app does not duplicate them.
- **Ingress UI** (Lit): state, what needs attention, the secret findings, the config check's warnings, a dry-run preview, and Sync now.
- **`config_sync` integration** (HACS): sync state (enum), last sync, needs attention, restart recommended, possible secrets (count + paths, rules and fingerprints as attributes), and a Sync now button. It is set up from the app's discovery and refreshes on the app's server-sent events, with a 2-minute poll as fallback.
- **Event** `homeassistant_config_sync` on every cycle that did something (`type`: `sync_finished`, `conflict`, `review`, `mass_delete`, `too_large`, `path_collision`, `blocked_secrets`, `secrets_found`, `check_failed`, `error`), plus one persistent notification (and `notify_service`) per problem, dismissed when the sync is back to normal.
- **History lives in the repo host**: the app does not duplicate commits or diffs.
- **Triggers:** `sync_interval_minutes` (default 5; 0 = manual), `POST /api/v1/sync`, and the Sync now button (automations use `button.press`).

## 13. HTTP API

Port 8099, used through ingress (requests from the Supervisor's ingress proxy, `172.30.32.2`, need no token) and by the integration (`Authorization: Bearer <token>`, the token in `/data/api_token`, handed over through discovery).

| Route | Response |
|---|---|
| `GET /api/v1/status` | The persisted state (`state`, `review`, `last_sync`, `box_head`, `main_head`, `conflict_paths`, `deleted`, `too_large`, `collisions`, `check_errors`, `check_warnings`, `findings`, `restart_recommended`, `last_error`), plus `syncing` and the running job. |
| `GET /api/v1/pending` | A dry run: `outgoing` (box → repo) and `incoming` (main → box) changes, and the state it would end in. `409` while a cycle runs. |
| `GET /api/v1/conflict` | Conflicting paths, heads and, for GitHub, the compare link. |
| `POST /api/v1/sync` | Optional `{"dry_run": bool}`. `202 {"job_id"}`, or `409` if a cycle is running. |
| `GET /api/v1/jobs/{id}` | A job and its report. |
| `GET /api/v1/events` | Server-sent events: `hello`, then a `job` event when a cycle starts and ends, keepalive every 25 s. |

One cycle at a time (a file lock in `/data`); a shutdown waits for the running cycle; cycles are capped at 15 minutes.

## 14. Threat model (summary)

| Threat | Mitigation |
|---|---|
| Whoever can push to `main` changes the box | By design: writing to `main` is administering these folders (and with `custom_components/` synced, running code in Home Assistant). Protect `main` (branch protection, PRs). |
| Whoever can push to `main` widens what leaves the box (editing `.gitignore`) | Accepted: the repo is the user's, and they control who writes to it. |
| Secrets committed by mistake | Base `.gitignore`, secret scanner (warn by default, block on request), findings never printed with values. |
| A broken mount, a wrong ignore rule or an emptied `main` deletes a lot | Mass-deletion guard in both directions; first-sync review. |
| `main` writes outside the root or into the app | Git refuses `..`, `.git` and writing through symlinks; the top-level collision guard protects `/data` and the image. |
| Token theft | Fine-grained token scoped to one repo; never in URLs, config, argv or logs; no long-lived Home Assistant token. |
| Network attacker | HTTPS with standard TLS verification; no other protocol allowed. |

## 15. Decisions

All agreed with the maintainer on 2026-11-12, one by one, replacing the v0.0.x design (box-side whitelist with includes per file type, hard denylist, SSH deploy keys, `push_only`/`pull_only`, automatic rollback):

1. Only what the app can see (the seven mounts); a root mode is future work.
2. Work tree at the container's `/`, `root` option defaulting to `/`, narrower roots allowed.
3. A root change goes through the first-sync review.
4. What is synced is the repo's `.gitignore`; the base one above, with `themes/` synced.
5. Files above the size limit stop the cycle (10 MB, configurable).
6. Symlinks stored as links. General rule: git's standard behavior first, anything else only as an option if someone needs it.
7. Two branches, `box` and `main`; the app pushes the applied result to both.
8. First sync, with or without content in the repo: only `box` is pushed and the user resolves in `main` (or creates it from `box`); no accept button.
9. No file validation, no rollback; `check_config` informs and the reload is skipped on errors; both can be turned off.
10. Home Assistant: restart notice only. Other apps: nothing.
11. HTTPS with an access token only; SSH removed.
12. Guards: mass deletion (box side → review, main side → raise the threshold), top-level collisions, size. No empty-mount guard.
13. Secret scanner: gitleaks with its standard configuration and ignores, `warn` by default, whole tree every cycle.
14. Triggers: interval, API, button; no stdin.
15. Merge commits use git's standard message.
16. Monitoring: a secrets entity; history stays in the repo host.
17. Released as `v0.1.0`, without automatic migration.

## 16. What exists and why it falls short

| Project | What it does well | Why it falls short |
|---|---|---|
| **Git pull** (official app) | Simple and maintained. | Pull only, overwrites the box, no merge. |
| **HA Git Sync** | Syncs both ways. | No public code, only `/config`, authenticates against HA with a long-lived admin token. |
| **HomeAssistantVersionControl** | Change history, easy to install. | Push only; the token goes in the HTTPS URL. |

What this project adds: real two-way sync with git's merge that stops on conflicts, every folder an app can see, plain git semantics configured from the repo, Home Assistant's check and reload, and open source.
