# Generic sync model (proposal)

Status: **agreed with the user, 2026-11-12; not implemented yet.** It replaces §4, §5, the modes in §6, §8 and §13 of [design.md](design.md), and ships as a breaking release.

## 1. Why

The current design encodes knowledge about Home Assistant's folders in the engine: invented repo prefixes (`config/`, `app_configs/`), a box-side whitelist with includes per file type, a hard denylist, per-path `push_only`, and global `push_only`/`pull_only` modes. That tailors the tool to one way of using HA and takes decisions away from the person who owns the instance.

The new model is plain git over the folders the app can see:

- **The person owns their instance.** If they want `/ssl` in their private repo, that is their decision and their risk. Guards exist to prevent accidents, never to forbid a deliberate choice. Defaults are suggestions that can be edited.
- **Generic over HA-specific.** What gets synced and how merges resolve are expressed with git's own files and options (`.gitignore`, `.gitattributes`, `-X`), managed from the repo. HA-specific logic is limited to what makes this an HA app: checking and reloading Home Assistant before publishing.

## 2. What the app sees

An app only sees the folders its `map:` asks for. The list is closed (Supervisor `MappingType`). The app maps the same seven as the official Terminal & SSH app, all read-write:

| `map:` | Supervisor folder (`/mnt/data/supervisor/…`) | In the app |
|---|---|---|
| `homeassistant_config` | `homeassistant/` | `/homeassistant` |
| `all_app_configs` | `app_configs/` | `/app_configs` |
| `share` | `share/` | `/share` |
| `ssl` | `ssl/` | `/ssl` |
| `media` | `media/` | `/media` |
| `backup` | `backup/` | `/backup` |
| `local_apps` | `apps/local/` | `/local_apps` |

Not visible to any app, and out of scope for this version: other apps' `/data` (including their options), the Supervisor's own state (installed apps, repositories, network mounts), and HA OS host config (network, boot). A future **root mode** may export some of it through the Supervisor API; nothing in this design blocks that.

## 3. Root

Decided 2026-11-12.

The app container's `/` is a simplified view of `/mnt/data/supervisor`: the seven mounts sit there as real directories (bind mounts, not links), next to the image's own system folders and the app's private `/data`.

- **Option `root`, default `/`.** Git's work tree is the root. With `/`, the repo matches the container 1:1: `homeassistant/configuration.yaml` in the repo is `/homeassistant/configuration.yaml` on the box.
- **Narrower roots** let a user start small: `root: /homeassistant` syncs only Home Assistant's configuration, with no prefix in the repo; `root: /app_configs/<slug>` or any folder inside a mount works the same way. The work tree is that folder and the repo's top-level files (`.gitignore`, `README.md`…) live inside it like any other file.
- **With `root: /`**, an internal exclude in the app's repo (`/data/repo.git/info/exclude`, not the user's `.gitignore`) contains `/*` plus `!/<mount>/` for each mount, so git only ever looks at the mounts and never walks the image's system folders or `/data`.
- **Top-level files with `root: /`** (`.gitignore`, `.gitattributes`, `README.md`, `.github/`) are written to the container's `/`. The Supervisor recreates the container on every start, so the app restores them from `box` at startup. Nobody edits them on the box side; they only arrive from `main`.
- **Guard:** with `root: /`, a top-level path from `main` that collides with a folder of the container (`data/`, `etc/`, `bin/`…) is not written and is reported. Otherwise a `data/` in the repo would overwrite the app's state.
- **Changing `root` on an existing repo** is treated like a first sync: the app remembers the last `root` it used; when it changes, it commits the snapshot with the new layout to `box`, applies nothing from `main`, and waits for confirmation in the first-sync review, saying "root changed from X to Y". Merging into `main` is left to the user.

## 4. What is synced: `.gitignore`

Everything under the root is synced in both directions, except what git ignores.

- **The ignore rules are the repo's own `.gitignore` files**, at any level, with git's exact semantics (negations with `!`, anchors, nested files). The engine does not parse them: `git add` applies them (§7).
- `.gitignore` is a file like any other: a change pushed to `main` is applied to the box and takes effect from the next snapshot. The app is configured from the repo.
- **First sync with no `.gitignore` in the repo:** the app commits the base one (§5) so it is visible and editable.
- **A tracked path that becomes ignored** is removed from the repo on the next snapshot (`git rm --cached` semantics): the file stays on disk, history is not rewritten. A path that stops being ignored is added. Both go through the mass-deletion guard (§8).
- **Symlinks** inside the mounts are stored as links (git mode `120000`, the link's target path, never its contents), git's standard behavior (decided 2026-11-12). Git itself refuses to write a path through a symlink, so a link cannot be used to write outside the root. Rule for this and later choices: git's standard behavior first; a different behavior only as an option, if someone turns out to need it.

## 5. Base `.gitignore`

Decided 2026-11-12. `homeassistant/themes/` is deliberately not ignored: it mixes hand-written themes with HACS ones, and ignoring it would silently stop versioning the former; the cost of syncing it is a commit when HACS updates a theme. Committed on the first sync when the repo has none, and edited like any other file afterwards. Its only purpose is preventing accidents: huge files, state that HA or an app rewrites all the time, and credentials. Paths are for `root: /`; with another root the app writes the lines that apply to it.

```gitignore
# State Home Assistant rewrites all the time; includes login tokens
homeassistant/.storage/
# Nabu Casa credentials
homeassistant/.cloud/

# Secrets: remove this line to version them (they stay in history for good)
secrets.yaml
# Certificate private keys
ssl/

# Databases and logs, in any folder (recorder, Zigbee2MQTT, …)
*.db
*.db-*
*.sqlite*
*.log
*.log.*

# Caches and dependencies that are regenerated
homeassistant/.cache/
homeassistant/deps/
homeassistant/tts/
__pycache__/
.esphome/
node_modules/

# Installed by HACS; HACS reinstalls them
homeassistant/custom_components/
homeassistant/www/community/

# Huge files
backup/
media/
```

Documented git behavior: a folder with its own `.git` (an app under development in `local_apps/`, a clone in `share/`) is stored by git as a pointer (like a submodule), not its contents.

## 6. Merging

The cycle keeps its shape (design.md §6: snapshot → merge → apply → reload → publish), with these changes:

- **Branches (decided 2026-11-12, unchanged from today):** the box is one more clone of the repo. `main` belongs to people; `box` is written only by the app and always says what is on the box, also during a conflict. Each cycle commits the snapshot to `box`, merges `main`, applies and checks the result, then pushes it to both `main` and `box` (fast-forward only, never `--force`). On a conflict only `box` is pushed and the user merges it into `main` like any branch. The first-sync and root-change reviews use the same branch.
- **First sync against a `main` with content, and `root` changes (decided 2026-11-12):** the app commits the snapshot on top of `main`, pushes `box`, applies nothing and stops, asking the user to resolve: leave the real state in `main` by merging `box` (editing whatever is needed; to discard the box's version entirely, `git merge -s ours box`), with a PR or locally. The app does not merge on its own and has no "accept" button. It resumes when `box` is an ancestor of `main`; from then on the normal cycle applies `main` to the box, with the usual checks. The snapshot goes on top of `main` (not as an unrelated root commit) so that `main..box` shows exactly how the box differs and a PR can be opened: GitHub does not open PRs between unrelated histories. An empty repo needs no review: the first snapshot becomes `main` and `box`.
- **The merge is `git merge main` run on `box`:** `ours` is the box, `theirs` is `main`.
- **Option `merge_options`:** a list passed to git as `-X` options, unchanged: `ours`, `theirs`, `patience`, `diff-algorithm=histogram`, `ignore-space-change`, and so on. Default: empty.
- **Per-path rules come from the repo's `.gitattributes`**, which git already honors: `merge=union`, `merge=binary`, `-merge`, `merge=text`.
- **Custom merge drivers** (`merge.<name>.driver` in git config) are not supported: they run arbitrary commands inside the app. Git's built-in drivers work.
- **Whatever is still in conflict** fails the merge, as in git: nothing is applied, the state becomes `conflict`, and the user is notified.

Verified with git 2.49.1 (the version in the image): `git merge-tree --write-tree` honors `-X ours`, `-X theirs` and `merge=union` from `.gitattributes`.

**Removed:** `mode` (`push_only`, `pull_only`) and per-path `push_only`.

**Commit messages (decided 2026-11-12):** merges use git's standard message, `Merge branch 'box'`. Snapshots keep today's format (git has no standard for them): `box: <path>` or `box: <n> changes`, and a body listing each path with its status letter, like `git status --short`, cut at 100. Author: the `commit_author` option.

## 7. Git does the work

The engine drives the git CLI over the mounts instead of reimplementing it. The repo stays in `/data/repo.git`; the root is git's work tree (`GIT_WORK_TREE`) with a persistent index in `/data`. No `.git` is written into the mounted folders.

| Step | Git |
|---|---|
| Snapshot | `git add -A` + `git commit` on `box`. Honors `.gitignore`, stores symlinks, and the index is the stat cache. |
| Merge | `git merge-tree --write-tree -X …` (§6). It never touches the live files, so conflict markers never land in the config. |
| Apply | `git read-tree -m -u <result>`: git updates the work tree and refuses if there are uncommitted local changes. |
This removes the engine's own scanner, stat cache, temporary index, file-by-file apply and journal. What stays in the engine is what git does not do: the Home Assistant check and reload, the guards (§8) as checks on what git is about to do, notifications, and the API.

## 7b. Applying to the box: Home Assistant specifics

Decided 2026-11-12. Validating files is not this app's job, and it never rolls back on its own: when something fails, the user finds out and fixes it in `main`.

- **No file validation.** No YAML/JSON syntax check of any file, in any mount.
- **Home Assistant config check, informational:** after applying a change that touches `homeassistant/`, the app asks Home Assistant to check its config (`check_config`) and tells the user what it reports. It never reverts. Option to turn it off.
- **Automatic reload, quality of life:** after applying, the app asks Home Assistant to reload. Option to turn it off. **Not done when the check reported errors:** Home Assistant keeps running what it had in memory, the files stay written as they came from `main`, and the notice says the reload was skipped. The next cycle after the fix in `main` applies it and reloads.
- **No automation rollback:** the check for automations that become unavailable after the reload, and the revert it triggered, are removed.
- **Removed:** the rollback of the box, the `rejected` state, and the memory of a rejected `main` that is not retried.
- **Restart notices:**
  - **Home Assistant (decided 2026-11-12): notice only.** A change to `configuration.yaml`, `packages/` or `custom_components/` sets "restart recommended" (entity, UI, notification). The app never restarts Home Assistant; the `restart` option is removed.
  - **Other apps: nothing (decided 2026-11-12).** The app does not track, notify or restart other apps; what they do with their files is their business.
- **Known limitation, documented:** files a program keeps in memory and rewrites (dashboards in `.storage/lovelace*`, files of other apps) only pick up a change from git when that program reloads them. If someone edits them from its UI first, the UI wins and the next snapshot commits that version.

## 7c. Remote

- **Authentication (decided 2026-11-12): HTTPS with an access token only.** The user gives the repo's HTTPS URL and a token (a GitHub fine-grained token limited to that repo, a GitLab project access token, a Gitea token…), plus an optional username for hosts that need one. Git gets the token through its standard credential mechanism (`GIT_ASKPASS`), never in the URL, the git config or the logs. TLS certificates are verified the standard way.
- **SSH is removed:** no generated key pair, no deploy key, no key rotation, no host-key handling (`ssh_known_hosts`, the hardcoded GitHub keys). The UI's deploy-key panel and the `deploy-key` API endpoints go away.
- **Known trade-offs, documented:** the token lives in the app's options, which the Supervisor stores in plain text and includes in Home Assistant backups. Tokens expire (fine-grained GitHub tokens up to a year): an authentication failure is reported as such ("token rejected or expired"), not as a generic sync error.

## 7d. Triggers

Decided 2026-11-12: the interval (`sync_interval_minutes`, default 5, `0` = manual only), `POST /api/v1/sync`, and the Sync now button (UI and integration; automations use `button.press`). The `stdin` trigger is removed: it duplicated the button.

## 8. Guards

They prevent accidents; none of them forbids a choice. All thresholds are options.

| Guard | Prevents |
|---|---|
| Mass deletion (more than 25 paths, or at least 5 that are more than 20 %; configurable; decided 2026-11-12) | A wrong ignore rule or an emptied `main` deleting half the config. **From the box:** the snapshot is committed and only `box` is pushed; `main` is not touched and the cycle stops. If the deletions are intended, the user merges `box` into `main`. **From `main`:** nothing is applied and the cycle stops; to go ahead the user raises the threshold, lets a cycle run, and lowers it again. No "accept" button. |
| First-sync and root-change review (§6) | The first sync against an existing repo overwriting either side silently. Only `box` is pushed; the user resolves in `main`. |
| Top-level paths that collide with the container (only with `root: /`; decided 2026-11-12) | `main` writing into `/data` (the app's own state) or the image's system folders. A top-level path from `main` that is not a mount and exists in the container is not written and is reported. Paths with `..` or `.git`, and writes through symlinks, are already refused by git itself. |
| Maximum file size (default 10 MB, configurable; decided 2026-11-12) | A huge file entering the history for good (GitHub warns at 50 MB and rejects over 100 MB, and history is never rewritten). Checked on what `git add` staged, before committing. **The cycle stops** and nothing is committed until the file is ignored or the limit raised: skipping it silently would only surface when it is too late. |

The guards are checks on what git is about to do: deletions counted and blob sizes read from `git diff --cached` before committing; the result of `merge-tree` checked before `read-tree`.

**Removed:** the hard denylist, the box-side whitelist, the binary-file detection (git stores binaries fine), ignoring symlinks, and the retry of files that change while read (that was part of the engine's own scanner; a half-written file that gets committed is corrected by the next cycle).

## 9. Secrets

The secret scanner is gitleaks, as a warning, not a gate:

- `secret_scan.mode: warn` (default) | `block` | `off` (issue #2, with the default flipped to `warn`).
- `warn`: findings (path, line, rule; never the value) go to the log, `/api/v1/pending`, the HA event and an entity of the integration; the snapshot is committed.
- The DOCS say plainly that with `warn` or `off`, literal secrets end up in git history.
- **What it scans (decided 2026-11-12): the whole tree of each `box` commit**, that is, everything in the repo, every cycle. Scanning only what each commit adds would let an intermediate change hide an earlier finding. Ignored files are not scanned (they are not in the repo).
- **Findings are a state, not a stream:** the entity and the UI show the current findings; the notification fires only when that set changes, so the same finding does not notify every cycle.
- **False positives (decided 2026-11-12): gitleaks' standard mechanisms**, managed from the repo: a `.gitleaksignore` file with the fingerprints to ignore, and a `gitleaks:allow` comment at the end of a line. The `secret_scan.allow` option is removed.
- **Rules (decided 2026-11-12): gitleaks' standard configuration.** Without a `.gitleaks.toml` in the repo, gitleaks' default rules apply. Anyone who wants to tune them adds their own `.gitleaks.toml` to the repo (gitleaks' standard format, `[extend] useDefault = true` to keep the defaults). The app ships no rules of its own and commits no config file; the DOCS show the old Home Assistant rules (literal passwords instead of `!secret`, `webhook_id`, URLs with credentials) as an example to copy. The YAML-parsing rules and the `webhook_id` option are removed.

## 10. Monitoring

The Supervisor already gives every app a **Logs** tab (the container's output, where `hcs` logs every cycle, conflict and error), state, CPU and memory, restart and watchdog. The app does not duplicate any of that. On top of it:

- **Integration entities** (existing): sync state, last sync, needs attention, restart recommended, Sync now. **New (decided 2026-11-12):** secret-scan warnings (count, with paths and rules as attributes; never values).
- **Ingress UI** (existing): state, preview, conflict, Sync now. **New:** the secret warnings.
- **History lives in the repo host** (decided 2026-11-12): GitHub, GitLab and the like already show commits and diffs; the app does not duplicate them.
- **HA event and persistent notification** (existing) for anything that needs a person.

## 11. App options

| Option | Default |
|---|---|
| `repository` (HTTPS URL), `token`, `username`, `main_branch`, `box_branch` | `""`, `""`, `""`, `main`, `box` |
| `root` | `.` |
| `sync_interval_minutes` | `5` |
| `merge_options` | `[]` |
| `secret_scan.mode` | `warn` |
| `check_config`, `reload` | `true`, `true` |
| `max_file_size_mb` | `10` |
| `delete_guard_files`, `delete_guard_percent` | `25`, `20` |
| `commit_author`, `notify_service` | as today (`Home Assistant <ha@box.local>`, `""`) |

Everything about *what* is synced and *how* it merges lives in the repo (`.gitignore`, `.gitattributes`). The options only say how to reach the repo and how the box behaves.

## 12. Migration and release

- Breaking: the repo layout changes from `config/` to `homeassistant/` (with `root: /`), authentication moves from SSH to an HTTPS token, the `mappings`, `mode`, `push_only`, `restart`, `check_warnings` and `secret_scan.allow` options go away, and the hard denylist no longer applies. Released as `v0.1.0` (decided 2026-11-12: in semver `0.x`, the minor number marks a breaking change; `1.0` waits for real use).
- **No automatic migration** (decided 2026-11-12): there are no users besides the author yet. The DOCS explain how to move an existing repo (`git mv config homeassistant`, add a `.gitignore`).
- Issues: #2 is covered by §9, #3 by §5 (`www/community/` ignored by default, `themes/` synced). The behavior asked in #3's open question becomes §4: an ignored path is removed from the repo, not frozen.

## 13. Open points

None as of 2026-11-12: every point was closed with the user, one by one.
