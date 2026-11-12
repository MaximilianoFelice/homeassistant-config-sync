# homeassistant-config-sync: design

> Status: draft for review, 2026-11-11. None of this is published yet.

## 1. Goal

A Home Assistant OS app (add-on) that keeps the box configuration synced **in both directions** with a git repo:

- What changes on the box (edits from the HA UI, the file editor, other apps) ends up committed and pushed.
- What is pushed to `main` from elsewhere (local machine, GitHub) ends up applied on the box, **only if it passes the HA config check**.
- If both ends touch the same thing in incompatible ways, the sync **stops** and reports it. Nobody's work is ever silently overwritten.

It is open source from day one and is distributed as an HA app repository (not through HACS, which does not distribute apps). An optional HACS integration comes later, as a presentation layer.

### Non-goals

- It is not a backup system: it does not version all of `.storage`, databases, or media. HA backups cover that.
- It does not restart Home Assistant on its own (unless explicitly opted in).
- It does not manage secrets: `secrets.yaml` never leaves the box.
- It does not resolve conflicts automatically in v0/v1.

## 2. Glossary

| Term | Meaning |
|---|---|
| **box** | The HA OS instance where the app runs. |
| **live disk** | The directories mounted into the app: `/homeassistant` and `/app_configs`. |
| **managed path** | A file on the live disk that passes its mapping's whitelist. Only these are read, written, or deleted. |
| **`box` branch** | Branch that reflects the managed state of the box. Only the engine writes it. |
| **`main` branch** | Branch for human work. Users write it; the engine only advances it by fast-forward. |
| **cycle** | One complete sync run (manual or scheduled). |
| **engine** | The binary that implements the cycle (a CLI in v0, the app process from v1). |

## 3. Architecture

```
                 GitHub (git@github.com:owner/repo.git, SSH deploy key)
                 ┌──────────────────────────────────────────────┐
                 │  main  ◄── human pushes (local, web)          │
                 │  box   ◄── engine only                        │
                 └───────────────▲──────────────────────────────┘
                                 │ fetch / push --atomic (SSH)
┌──────────────── App homeassistant-config-sync ─────────────────────────┐
│                                                                        │
│  scheduler ─┐                                                          │
│  stdin ─────┼──► engine (cycle) ──► git CLI (bare repo at /data/repo.git)│
│  HTTP API ──┘        │                                                 │
│                      ├──► scan / apply ──► /homeassistant (rw)         │
│                      │                └──► /app_configs (rw)           │
│                      └──► HA Core API (http://supervisor/core/api,     │
│                            SUPERVISOR_TOKEN): check_config, reloads,   │
│                            notifications, events                       │
└────────────────────────────────────────────────────────────────────────┘
```

Core decisions:

- **The repo is bare and lives at `/data/repo.git`.** The live disk is not a git working tree: the engine builds trees with plumbing (`hash-object`, a temporary index via `GIT_INDEX_FILE`, `write-tree`, `commit-tree`, `merge-tree --write-tree`) and writes only managed paths to disk. This way a `.git` never appears inside `/homeassistant`, and a monorepo can map to several directories on the box.
- **The git CLI is used, not a library.** git's 3-way merge (`merge-tree --write-tree`, git ≥ 2.38) is the most delicate part and is not worth reimplementing.

## 4. Mounts and repo layout

### App mounts

```yaml
map:
  - type: homeassistant_config   # → /homeassistant
    read_only: false
  - type: all_app_configs        # → /app_configs
    read_only: false
```

Supervisor 2026.11 renamed "add-ons" to "apps": `all_addon_configs` (which mounted at `/addon_configs`) became a legacy name that logs a warning. `all_app_configs` is used, verified with Supervisor 2026.11.0 in Docker.

`/data` (private to the app) holds the engine state; it does not need to be mapped.

### Monorepo with configurable mapping

Default:

| Repo prefix | Box directory |
|---|---|
| `config/` | `/homeassistant` |
| `app_configs/` | `/app_configs` |

```yaml
mappings:
  - repo: config
    box: /homeassistant
    include: [...]      # see §5
    push_only: [...]
    exclude: [...]
  - repo: app_configs
    box: /app_configs
    include: []         # empty by default: apps must be chosen explicitly
    push_only: []
    exclude: []
```

Rules:

- Everything in the repo **outside** a mapped prefix (README, `.github/`, docs, `.gitattributes`) belongs to the repo: the engine keeps it as is in its commits and never brings it down to the box.
- A path inside a mapped prefix that does **not** pass the whitelist is ignored in both directions and reported as "unmanaged".
- `* -text` in the repo's `.gitattributes` is recommended so nobody rewrites line endings when cloning on Windows. The engine writes bytes as is.

## 5. Path whitelist

The whitelist lives **on the box side** (app options), never in the repo. If it lived in the repo, anyone with push access could widen the whitelist and pull files out of the box into git: that would escalate from "repo writer" to "box reader".

### Semantics

A path is managed if and only if:

1. it is under a mapping's `box`,
2. it matches some `include` or `push_only` glob (doublestar syntax: `*`, `**`, `?`, `[...]`; relative to the mapping root),
3. it matches no `exclude` glob,
4. it does not match the **hard denylist**,
5. it is not a symlink, socket, or device (symlinks are ignored, never followed),
6. it is smaller than `max_file_size_kb` (default 1024) and does not look binary (NUL byte in the first 8 KiB).

Points 5 and 6 produce notices under "pending/ignored", not errors.

The direction comes from the list that matches:

- `include`: synced in both directions.
- `push_only`: box → repo only. Anything arriving from `main` for that path is ignored and reported as "incoming change not applicable". If a path matches both lists, `push_only` wins.

They are three lists of strings (rather than rules with a direction field) so the Supervisor options schema can validate them.

In addition, every path coming from the repo must be relative and clean: no `..`, `.`, empty components, backslashes, control bytes, or `.git` components. Otherwise it is dropped as `invalid_path` before the whitelist is checked. This is the barrier against a `main` that tries to write outside the mapping.

### Hard denylist (cannot be disabled)

It applies at any depth and is case-insensitive (`Secrets.YAML` is caught too):

```
**/secrets.yaml  **/secrets.yml
**/*.db  **/*.db-shm  **/*.db-wal  **/*.db-journal  **/*.sqlite*
**/.storage/auth*  **/.storage/core.config_entries  **/.storage/http*  **/.storage/cloud
**/.cloud/**  **/.ssh/**
**/id_rsa*  **/id_dsa*  **/id_ecdsa*  **/id_ed25519*  **/*.pem  **/*.key  **/*.p12  **/*.pfx
**/*.log  **/*.log.*
```

The source of truth is `paths.HardDeny` in the code.

### Defaults for the `config/` mapping

```yaml
include:
  - "**/*.yaml"
  - "**/*.yml"
  - "custom_templates/**/*.jinja"
  - "python_scripts/**/*.py"
  - "pyscript/**/*.py"
push_only:
  - ".storage/lovelace*"
exclude:
  - ".cache/**"
  - "deps/**"
  - "tts/**"
  - "backups/**"
  - "custom_components/**" # managed by HACS; opt-in for anyone who wants to version it
  - "esphome/.esphome/**"
  - "**/__pycache__/**"
  - "ip_bans.yaml"
```

Since this is a whitelist, `.storage` stays out except for what is explicitly included (`lovelace*`): it does not need to be excluded. `exclude` always wins over `include`, and the hard denylist wins over both.

**Why `.storage/lovelace*` is `push_only`:** HA keeps dashboards in memory and rewrites those JSON files when they are saved from the UI. Writing them from git while HA is running has no visible effect until a restart, and the next save from the UI overwrites them. Versioning them is still useful (history, diffs). Pulling them down remains a future option with a "requires restart" notice.

### Scan safety guards

- **Empty or missing root:** if a mapping's directory does not exist or has no managed paths while the `box` tree did have some, the cycle aborts (`suspicious_snapshot`). This protects against a broken mount being read as "everything was deleted".
- **Mass deletion:** if a snapshot deletes more than `delete_guard_files` (default 25) managed paths, or at least 5 that also exceed `delete_guard_percent` (default 20 %) of the managed paths, it aborts and asks for confirmation. The minimum of 5 keeps deleting 1 of 4 files in a small config from stopping the sync. The same applies to an incoming change from `main`.
- **Skipping is never deleting:** a skipped file (large, binary, symlink, unstable, unreadable, or an entire unreadable directory) keeps the version it had in `box`.
- **Stat cache:** a file's blob is reused if size, `mtime`, `ctime`, inode, and mode match. A file modified less than 2 s ago does not enter the cache, and since `ctime` changes on any write, restoring `mtime` by hand does not hide an edit.
- **Unstable reads:** each file is hashed and then stat'ed again. If `mtime` or size changed during the read, it is retried (up to 3 times), and if it keeps changing, that path is left for the next cycle.

## 6. Sync model

### Branches and invariants

- `box` (local, in `/data/repo.git`) is the truth about **what is on the live disk** for managed paths, except for edits made after the last cycle.
- `origin/box` is the published copy of `box`. The engine only moves it by fast-forward.
- `main` is written by humans. The engine only moves it by fast-forward, to a commit that has already passed the config check and is already applied on the box.
- There is never a `push --force`.
- The engine only modifies managed paths in its commits. The rest of the tree is copied from the parent commit.

### The cycle, step by step

```
 0. lock (/data/lock) and recovery of an interrupted apply (see §7)
 1. fetch origin main box
 2. SNAPSHOT  scan live disk → tree T
              if T ≠ tree(box):
                 scan the changes for secrets (§8) → if findings: blocked_secrets, stop
                 commit C on box (parent = box)
 3. MERGE     B = box, M = origin/main
              M does not exist       → R = B          (bootstrap with empty remote)
              M ancestor of B        → R = B          (only the box changed)
              B ancestor of M        → R = M          (only main changed)
              otherwise              → git merge-tree --write-tree B M
                  with conflicts     → push box, state conflict, notify, stop
                  clean              → R = commit(tree, parents = [M, B])
 4. APPLY     if tree(R) ≠ tree(B): apply diff B→R to disk (§7)
                  check fails        → rollback to B, push box, state rejected, stop
 5. PUBLISH   box := R (local)
              push --atomic origin R:main R:box
                  rejected (main moved) → back to 1 (max. 3 times per cycle)
 6. POST      reloads per policy (§7), notification if needed, event
```

```
main:  A───────M1────────────R          (R = merge of main + box, already applied)
        \                  /
box:     B0──C1──C2───────'              (C1, C2 = box snapshots)
                          after publishing: box = R
```

Details:

- **The merge's first parent is `main`**, so that `git log --first-parent main` tells the human history.
- **Moving `box` locally before pushing** means a rejected push breaks nothing: the disk is already at R, `box` says R, and the next attempt merges R against the new `main`.
- **During a conflict the box keeps being snapshotted** (box changes are never lost) but nothing is applied from `main`.
- **An `R` rejected by the check** is remembered in `state.json` and not retried until `origin/main` changes.
- **`push_only` paths always stay as in `box`.** In every merge case, `R` carries the `box` version of those paths. If `main` had changed or deleted them, that is discarded and reported in `Overridden`. In the fast-forward case this means a new commit on top of `main`. A content, add/add, or modify/delete conflict in which every path is `push_only` is resolved automatically in favor of the box. Structural conflicts (file/directory, renames) always stop the sync, because git leaves auxiliary paths in the tree.
- **Unrelated histories** (someone rewrote `main` from scratch): the merge fails with an explicit error and nothing is applied.

### Conflicts

1. The engine pushes `box` and stays in the `conflict` state with the list of conflicting paths.
2. It notifies (persistent notification in HA + event, see §10).
3. The user resolves it like any merge:
   - locally: `git fetch && git switch main && git merge origin/box`, resolve, `git push`;
   - or on GitHub: a `box → main` PR and the conflict editor.
4. On the next cycle `main` already contains `box` (or nearly) and the merge comes out clean.

There is no automatic strategy (`prefer_main` / `prefer_box`) in v0/v1. It remains a future option.

### Bootstrap

| Situation | Behavior |
|---|---|
| Empty remote | First snapshot as the root commit. `main = box = that commit`. |
| `main` exists, `box` does not | Snapshot with parent `main` (the `main..box` diff shows exactly how the box differs from the repo). `box` is pushed and the engine stays in `bootstrap_review`: it does **not** merge or apply until the user either (a) merges `box` into `main` themselves or (b) confirms with `sync --accept-bootstrap`. Without this stop, the first merge would be a silent fast-forward of the box over `main`. |
| `main` and `box` exist | Normal cycle. |

### Modes

`mode: bidirectional` (default) | `push_only` (never applies from `main`) | `pull_only` (the box follows `main`; useful for a staging box).

- `push_only` only publishes the `box` branch; `main` is left to humans and the box never reads it.
- `pull_only`: the box takes `main`'s tree and **discards its own edits**. They are not lost: the snapshot still commits them to `box` first, and `R` is a commit with parents `[box, main]` and `main`'s tree (`merge.Follow`), so the `box` history keeps every discarded edit. `main` is never written. `push_only` paths still keep the box version. There are no conflicts by construction. The first sync against an existing `main` still stops in `bootstrap_review`, because following `main` replaces the box config.

### Other cycle cases (implemented in M6)

- **Someone pushed to `box`:** if `origin/box` is not an ancestor of the local `box`, the engine stops with an error (`box` diverged) and overwrites nothing. `box` belongs to the engine only.
- **Retries:** if the disk changes between the snapshot and the apply (CAS) or `main` advances between the fetch and the push, the cycle starts over, up to 3 times per run.
- **CLI exit codes:** `0` = `idle`; `1` = a state that needs intervention (conflict, rejection, secrets, bootstrap, stopped snapshot); `2` = error.

## 7. Applying to the box, check, rollback, and reloads

### Apply

1. **YAML pre-check** (cheap, no HA): parse every changed `.yaml`/`.yml`, accepting HA tags (`!secret`, `!include`, `!include_dir_*`, `!input`, `!env_var`). A syntax error stops here, before touching the disk. `.json` files are validated the same way.
2. **Write-ahead:** `{"applying": {"from": B, "to": R, "paths": [...]}}` is recorded in `state.json`.
3. **Per-path compare-and-swap:** before writing, every path to be touched must be on disk exactly as in `B` (or not exist, if it does not exist in `B`). If someone edited it since the snapshot, the apply is aborted without writing anything and the cycle goes back to step 1.
4. **Atomic write:** temporary file in the same directory + `rename`. Missing parent directories are created. Deletions remove the file and any directories left empty, only inside the mapping. The executable bit follows git; other permissions are preserved if the file already existed.
5. **Config check:** if anything under the `/homeassistant` mapping changed, `POST /api/config/core/check_config`. There is no generic check for `/app_configs`: only the syntax pre-check.
6. **Rollback** if the check returns `invalid`: the touched paths are rewritten from `B` (the CAS guarantees `B` was the previous state). The write-ahead is cleared.

**`check_config` warnings (`check_warnings: new|any|ignore`, default `new`).** Tested against real HA 2026.11.1: an integration with invalid config (for example, an `input_boolean` with a non-boolean value) is **not** an error for HA, it is a warning. The config comes out "valid", HA starts, and that integration is disabled. The same happens with an automation with a nonexistent trigger, which is disabled and does not even show up as a warning. Therefore:

- `new` (default): a check is run **before** writing to get the baseline warnings, and the change is rejected if it adds any. The comparison ignores line numbers, which shift with any edit. It costs one extra check per apply, but it keeps a push from silently disabling an integration, and a config that already carries warnings does not get stuck.
- `any`: rejects on any warning.
- `ignore`: only looks at errors, like the "Check configuration" button in the UI.
- The REST API is even more permissive than the CLI script: an invalid `http.server_port` is an **error** for `check_config --json`, but through `/api/config/core/check_config` (what the app uses) it is a **warning** with `result: valid`. For the API, only YAML that does not parse is an error. With `ignore` almost everything would get through; that is why the default is `new`.
- Invalid automations are not detected by any of the three; they are handled after the reload (see below).

**Invalid automations (verified against HA 2026.11.1).** `check_config` accepts an automation with a nonexistent trigger; after the reload HA exposes it as an `automation.*` entity in state `unavailable`. So with `reload: auto` the engine reads the unavailable automations before the apply (`GET /api/states`), reloads **before publishing**, and reads them again. If new ones appeared, it undoes the apply (`apply.Undo`), reloads again to get back to `B`, and the state becomes `rejected`, naming the automations. Nothing reaches `main` or `box`. With `reload: none` there is no reload, so nothing can be detected.

**Risk window:** `check_config` validates the live directory, so new files are on disk for a few seconds before the rollback. HA does not reread YAML on its own (except specific integrations that watch files, such as pyscript), so the risk is low. A check in a staging directory would need the HA Core image inside the app, which is too heavy. This is documented and ruled out for v1.

**Recovery after an interruption:** the journal has two phases: `writing` (writing or validating) and `checked` (the disk is at `R` and the check passed; `box` still has to move). On startup:

- in the `checked` phase and with every path as in `R`: `R` is confirmed (`box := R`);
- in any other case it goes back to `B`: an `R` that was never validated is never taken as good, even if it ended up complete on disk. Only paths that are as in `R` are reverted; if someone edited a path after the interruption, it is not overwritten and goes into the next snapshot.

**Other apply safeguards:**

- No component between the mapping root and the file may be a symlink, so nothing is written outside the mapping.
- A directory that becomes a file is only replaced if it is empty after the deletions; if it has unmanaged files, the apply fails and is rolled back.
- Each mapping has `check: bool` (default `true` for `config/`, `false` for `app_configs/`): it defines whether a change there requires `check_config`.

### Reloads and restart

- `reload: auto` (default): if the apply touched `/homeassistant` and the check passed, `homeassistant.reload_all` is called (it reloads everything that is reloadable from YAML).
- `reload: none`: reloads nothing; only notifies.
- The reload happens **before** publishing, so a change that breaks automations can still be undone (see above).
- **Restart is never automatic by default.** If paths that usually require a restart changed (`configuration.yaml`, `packages/**`, `custom_components/**` if someone included them), the state gets `restart_recommended: true` and a notification is sent. With `restart: auto` (explicit opt-in) the engine calls `homeassistant.restart` after publishing and clears the flag.
- In `/app_configs` the engine does not restart apps; it reports which apps had changes. Decided (2026-11-12): apps are never restarted by the engine, to keep the permissions minimal (only `homeassistant_api`, no `hassio_role: manager`).
- **Notifications are not repeated:** a problem that persists across cycles is notified once; the key resets when the sync returns to `idle`.

## 8. Secret scanner

It runs **before committing a snapshot**, on added or modified content. If it finds something, the snapshot is not committed (so the secret never ends up even in the local repo) and the state becomes `blocked_secrets`.

**It only looks at what the box changes.** A secret already in `main` with the same content does not stop the sync: it has already leaked, and blocking protects nothing. To audit what is already in the repo, run `hcs scan-secrets` on a clone. This showed up in the M9 E2E: against an existing repo, ESPHome secrets that were already tracked did not stop the sync; against an empty or clean repo, they did.

Rules:

- gitleaks default rules (provider tokens, private keys, `generic-api-key`), embedded as a library (v8.30.1; the binary stays at ~9 MB for aarch64, stripped). Its logger is silenced so it never writes content.
- HA-specific rules:
  - YAML key matching `password|passwd|token|api_key|apikey|secret|client_secret|access_token|private_key|bearer` with a literal scalar value (not `!secret`, not `!env_var`, not a template) → finding;
  - literal `webhook_id` → warning (anyone with the URL can trigger the automation). Configurable to treat it as a finding;
  - URL with `user:pass@`.
- Findings are reported as `path:line:rule` and **never include the value**, neither in logs nor in the API.
- Exceptions: a list of fingerprints (`sha256(path + rule + normalized line)`) in the app options (`secret_scan.allow`). Editing that line or moving the file invalidates the exception; moving the line within the file does not.
- `hcs scan-secrets PATH...` runs the scanner standalone (useful as a local pre-commit hook). It exits with 1 if there are blocking findings.

## 9. Authentication with the remote

- **SSH with a deploy key only.** Never a PAT, never HTTPS.
- The app generates an ed25519 key pair at `/data/ssh/id_ed25519` (0600) on first start. It shows the public key in the log (v1), at `GET /api/v1/deploy-key`, and in the UI (v2). The user adds it on GitHub as a deploy key **with write access**, limited to that repo.
- Pinned `known_hosts`: GitHub's published host keys are embedded. For other hosts (Gitea, GitLab) there is an `ssh_known_hosts` option. Always with `StrictHostKeyChecking=yes`; there is no trust-on-first-use.
- Hardened git environment on every invocation:
  - `GIT_SSH_COMMAND="ssh -i /data/ssh/id_ed25519 -o IdentitiesOnly=yes -o IdentityAgent=none -o UserKnownHostsFile=/data/ssh/known_hosts -o StrictHostKeyChecking=yes"`
  - `GIT_TERMINAL_PROMPT=0`, `GIT_ASKPASS=/bin/false`, `-c credential.helper=`
  - `-c protocol.allow=never -c protocol.ssh.allow=always` (`file` enabled only in tests)
- Repo URL validation: only `git@host:owner/repo.git` or `ssh://...`. `http(s)://` and any URL with embedded credentials are rejected.
- Rotation: `hcs deploy-key --rotate` or `POST /api/v1/deploy-key/rotate`. It takes the engine lock (never in the middle of a sync), generates a new key, returns the public key, and keeps the old pair as `id_ed25519.old(.pub)`. The app uses the new key right away; the user adds it on GitHub and removes the old one.

## 10. Home Assistant API via `SUPERVISOR_TOKEN`

With `homeassistant_api: true` the app receives `SUPERVISOR_TOKEN` and talks to Core through the Supervisor proxy (`http://supervisor/core/api/...`, header `Authorization: Bearer $SUPERVISOR_TOKEN`). No long-lived token is needed.

| Use | Call |
|---|---|
| Config check | `POST /api/config/core/check_config` → `{"result": "valid" \| "invalid", "errors": ...}` |
| Reload | `POST /api/services/homeassistant/reload_all` |
| Notify | `POST /api/services/persistent_notification/create` with a fixed `notification_id` (updated instead of stacking). Optional: `notify_service` (e.g. `notify.mobile_app_x`). |
| Events | `POST /api/events/homeassistant_config_sync` with `{"type": "sync_finished" \| "conflict" \| "rejected" \| "blocked_secrets" \| "error", ...}`. Usable in automations from v1, without waiting for the integration. |

**Verified (2026-11-11)** with Supervisor 2026.11.0 and Core 2026.11.1 in Docker (`scripts/supervisor-dev.sh` + `scripts/supervisor-test.sh`): from the app, with its `SUPERVISOR_TOKEN` and `homeassistant_api: true`, `check_config`, `reload_all`, the event, and the notification work through the proxy. `hassio_api` is not needed.

Implemented behavior (M7, `internal/ha/supervisor.go`):

- **Every cycle that did something** fires the `homeassistant_config_sync` event with `type`, `title`, `message`, and extra data (`state`, `paths`, `findings`), which cannot overwrite the first three.
- **States that need intervention** also create or update a single persistent notification (`notification_id: homeassistant_config_sync`) and, if `notify_service` is set, send it there too.
- **A sync that returns to normal** dismisses that notification (`persistent_notification.dismiss`). A cycle with no changes sends nothing.
- **Timeouts:** 120 s for `check_config` (it is slow on a Raspberry Pi) and 30 s for everything else. If one step fails, the others are still attempted.
- **No error includes the token.** Responses are read with a 1 MiB limit.
- **The CLI picks automatically:** with `SUPERVISOR_TOKEN` in the environment (inside the app) it uses the Supervisor for check, reload, and notifications. Locally it uses `--checker noop|command:'…'` and does not reload or notify.

## 11. Triggers

- **Scheduled:** `sync_interval_minutes` (default 5; `0` = manual only). If a cycle is still running, the tick is skipped. So that a short interval does not load the box, the scan only re-hashes files whose `mtime`, size, or inode changed since the previous cycle (stat cache in `/data/state.json`), and the fetch is a single SSH round-trip.
- **Manual:**
  - v1: `stdin: true`, so the `hassio.addon_stdin` action with `{"input": "sync"}` triggers a cycle from an automation or a dashboard button, without a dedicated UI;
  - API: `POST /api/v1/sync`;
  - v2: button in the ingress UI; v3: `button` entity from the integration.
- It never triggers an HA restart (§7).

## 12. HTTP API

Designed for the v2 UI and the v3 HACS integration. It listens on an internal port (8099), not exposed to the host.

| Method and route | Response |
|---|---|
| `GET /api/v1/status` | `state` (`idle`, `syncing`, `conflict`, `rejected`, `blocked_secrets`, `bootstrap_review`, `suspicious_snapshot`, `error`), `last_sync`, `last_result`, `box_head`, `main_head`, `ahead`/`behind`, `pending_local`, `pending_incoming`, `restart_recommended`, `last_error`. |
| `GET /api/v1/pending` | Uncommitted local changes (dry scan), unapplied incoming changes from `origin/main`, ignored paths with their reason. |
| `GET /api/v1/conflict` | Conflicting paths, `box` and `main` commits, and the GitHub compare link if the remote is GitHub. |
| `POST /api/v1/sync` | Optional body `{"dry_run": bool, "accept_bootstrap": bool}`. `202 {"job_id"}` or `409` if a cycle is already running. |
| `GET /api/v1/jobs/{id}` | Progress and result of a cycle. |
| `GET /api/v1/deploy-key` | Public key. |
| `POST /api/v1/deploy-key/rotate` | Rotation (§9). |
| `GET /api/v1/events` (v2) | Server-Sent Events with the same types as the HA event. |

Authentication:

- Ingress requests come from the Supervisor proxy (`172.30.32.2`) and are accepted.
- Everything else needs `Authorization: Bearer <token>`, with the token generated at `/data/api_token`. In v3 the app hands it to the integration via Supervisor discovery (`discovery:` in `config.yaml` + `hassio_api`).

Implemented in M8 (`internal/api`, `hcs serve`):

- All endpoints in the table except `events` (SSE), which came in v2: a `hello` event on connect, a `job` event (with the job as data) when a cycle starts and when it ends, and a keepalive every 25 s.
- **`plan` / `pending` are exact:** the dry-run builds the snapshot commit (objects only, after the secret scanner passed) without moving `box`, so the merge it reports includes the uncommitted box changes, conflicts included.
- **One cycle at a time:** if one is already running, `POST /sync` and `GET /pending` respond `409` with the current job. The scheduler skips the tick.
- **`GET /pending`** is a synchronous dry run.
- **A shutdown (SIGTERM)** stops accepting requests and waits for the current cycle to finish. Cycles are never cancelled halfway; they are capped at 15 min.
- **`hcs serve`** accepts `--listen` (default `127.0.0.1:8099`; the app uses `0.0.0.0:8099`), `--stdin` (`hassio.addon_stdin` commands: `"sync"`), and `--sync-on-start`.
- **Token:** `hcs init` generates `api_token`; init never rotates it. It is compared in constant time.

### The `config_sync` integration (v3)

`custom_components/config_sync`, installable from HACS as a custom repository.

- **Setup:** `hcs serve --discovery` publishes `config_sync` to the Supervisor (`POST /discovery`, allowed by `discovery: [config_sync]` and `hassio_api`) with the app's hostname, port and API token. The integration's `hassio` step creates a single entry (or updates the host and token of the existing one). Outside HA OS it can be added by hand with host, port and token. A `401` starts reauth.
- **Updates:** a `DataUpdateCoordinator` reads `GET /status` every 2 minutes, and a background task listens to `GET /events` and refreshes on every job event (reconnecting with a 5/10/30/60 s backoff).
- **Entities** (one device per app): sync state (`enum` sensor), last sync (timestamp), needs attention (`problem` binary sensor), restart recommended, and a "Sync now" button (`POST /sync`; a `409` means a cycle is already running).
- **Left out for now:** pending-changes sensors and device triggers. The `homeassistant_config_sync` event the app already fires on the HA bus covers automations.
- **Tests:** the Home Assistant test harness (`pytest-homeassistant-custom-component`) for the flow, the entities, the button and reauth, plus the API client against a real HTTP server; `scripts/integration-e2e.sh` runs discovery → flow → entities → button on the development Supervisor.

## 13. App configuration (draft `config.yaml`)

```yaml
name: Git Config Sync
slug: homeassistant_config_sync
version: 0.1.0
arch: [aarch64, amd64]
startup: application
boot: auto
init: false
homeassistant_api: true
stdin: true
map:
  - type: homeassistant_config
    read_only: false
  - type: all_app_configs
    read_only: false
options:
  repository: ""                  # git@github.com:owner/repo.git
  main_branch: main
  box_branch: box
  mode: bidirectional
  sync_interval_minutes: 5
  reload: auto
  restart: never
  commit_author: "Home Assistant <ha@box.local>"
  max_file_size_kb: 1024
  delete_guard_percent: 20
  delete_guard_files: 25
  notify_service: ""
  mappings: [ ...defaults from §4 and §5... ]
  secret_scan:
    webhook_id: warn
    allow: []
schema: { ... }                   # Supervisor validation
```

The v0 CLI reads exactly the same schema from a file (`--config`), so v1 does not have a separate configuration.

### State in `/data`

```
/data/options.json          written by the Supervisor
/data/repo.git/             bare repo (refs: box, origin/main, origin/box)
/data/ssh/                  id_ed25519, id_ed25519.pub, known_hosts
/data/state.json            last result, rejected R, apply write-ahead, counters
/data/api_token
/data/lock
```

## 14. Threat model (summary)

| Threat | Mitigation |
|---|---|
| Secrets leaving the box | Hard denylist, box-side whitelist, scanner before commit. |
| Someone with push access to the repo runs code on the box | Only managed paths are written; `custom_components` and `python_scripts` are left out or opt-in. It must be documented that **writing to `main` is equivalent to administering the HA config**: protect `main` (branch protection, PRs). |
| Someone with push access to the repo widens what leaves the box | The whitelist does not live in the repo. |
| A broken mount or an emptied `main` deletes everything | Empty-root and mass-deletion guards in both directions. |
| Credential theft | One deploy key scoped to one repo, no agent or helpers; no long-lived HA tokens. |
| MITM over SSH | Pinned `known_hosts`, no TOFU. |

## 15. Roadmap

| Version | Scope | Done when |
|---|---|---|
| **v0** | Engine as a CLI (`hcs`), testable locally against a synthetic config and a local bare remote. Pluggable HA checker (fake / noop / command). | The scenario catalog in `docs/plan-v0.md` is green; E2E on the synthetic config is green. |
| **v1** | Local app in the box's `/addons` (multi-stage Dockerfile: Go build → base image with `git` and `openssh-client`), no UI. Triggers: schedule + `stdin`. Notifications and events in HA. | One week of real sync on a Raspberry Pi without intervention, with at least one conflict resolved end to end. |
| **v2** | Ingress UI (state, pending, conflict, deploy key, "sync now"). Multi-arch builds (aarch64 + amd64) in CI, images on GHCR, repo publishable as an app repository (`repository.yaml`). Optional app restart via `hassio_api`. | Installable from the repo URL on a clean HA. |
| **v3** | HACS integration: sensors, button, events, and discovery via Supervisor. | Published on HACS (default or custom repo). |

Status (2026-11-12): v0–v3 are implemented and pass every suite locally, including the real Supervisor 2026.11.0 with Core 2026.11.1 in Docker. What is left are the "done when" items that need the outside world: the first release (`v0.0.1`, which publishes the images), a week of real use on a Raspberry Pi, and installing from the repo URL and HACS on a clean HA.

## 16. What exists today and why it falls short

| Project | What it does well | Why it falls short |
|---|---|---|
| **Git pull** (official app) | Simple and maintained. | Pull only, overwrites the box, no merge. |
| **HA Git Sync** (a4ng3i) | Good design, the closest to this one: sync in both directions. | No public code (cannot be audited or contributed to), only `/config` (no `/app_configs`), and it authenticates against HA with a long-lived admin token. |
| **HomeAssistantVersionControl** (saihgupr) | Change history, easy to install. | Push only (applies nothing from the repo), and remote auth is a token in the HTTPS URL. |

What this project adds: real two-way sync with a 3-way merge that stops on conflicts, apply gated on the config check, a `config/` + `app_configs/` monorepo, SSH deploy key as the only auth, zero long-lived HA tokens, and open source from day one.

> Source of the comparison: a survey from November 2026. It should be revalidated before publishing v2.

## 17. Decisions made

### For v1–v3 (2026-11-12)

- **Language:** everything in English (docs, UI, CLI, logs, notifications, comments).
- **Display name:** "Git Config Sync". The app slug stays `homeassistant_config_sync`.
- **Integration domain (v3):** `config_sync`.
- **Releases:** a `vX.Y.Z` tag (starting at `v0.0.1`) triggers CI/CD: full tests (including the Docker tests), multi-arch build, images on GHCR, and a GitHub release. The repo is public.
- **CI on push:** lint, tests with `-race`, and E2E with the noop checker. The heavy parts run only on release (or by hand).
- **Invalid automations after the reload:** if new automations show up as `unavailable`, the files are reverted, HA is reloaded again, and the state becomes `rejected`.
- **`app_configs` apps:** not restarted; the engine reports which ones should be restarted (minimal permissions).
- **`pull_only`:** will be implemented. The box discards its local edits in favor of `main`; they stay saved in the `box` branch.
- **Ingress UI:** Lit with a build step (node via mise).

### For v0 (2026-11-11)

1. **License:** MIT. The repo starts private and goes public once there is code.
2. **Names:** slug `homeassistant_config_sync`, binary `hcs`.
3. **Defaults:** `sync_interval_minutes: 5`, `mode: bidirectional`.
4. **`custom_components/**`:** opt-in, off by default. Whoever includes it accepts that writing to `main` means running code in HA (§14).
5. **A single repo** for the engine, the app, and the integration:

   ```
   repository.yaml                      app repository (v2)
   homeassistant_config_sync/           app folder: config.yaml, Dockerfile, DOCS.md (v1)
   custom_components/config_sync/       integration (v3)
   hacs.json                            (v3)
   cmd/hcs/, internal/, go.mod          the engine (v0)
   docs/
   ```

   The Supervisor only looks at `repository.yaml` and folders with a `config.yaml`; HACS only looks at `custom_components/` and `hacs.json`. The Go module at the root bothers neither.
