# v0 implementation plan: the engine as a CLI

> Companion to [`design.md`](design.md). Status: draft for review, 2026-11-11.

## Scope

v0 is the complete sync-cycle engine (`design.md` §6–§9) packaged as a CLI, `hcs`, that runs locally against:

- a **synthetic sample config** (`testdata/ha-config`) acting as the box, copied into a local sandbox;
- a **local bare remote** (`file://`) instead of GitHub;
- a **pluggable HA checker** instead of the Supervisor.

Out of scope for v0: Dockerfile, the app's `config.yaml`, UI, and the HACS integration. The HTTP API and the scheduler are in (`hcs serve`), because they are small and test just as well locally.

## Language: Go 1.27

Go 1.27.4, pinned in `mise.toml`.

| Criterion | Go | Python | Rust | Bash (what existing apps use) |
|---|---|---|---|---|
| App image (v1/v2) | One static binary; the image is `base` + `git` + `openssh-client`. | Interpreter + venv + dependencies. | Same as Go. | Minimal. |
| Multi-arch (aarch64 + amd64) | `GOARCH=arm64` from amd64, no emulation. | Per-architecture wheels, QEMU in CI. | Cross-compiling is possible, heavier toolchain. | Trivial. |
| Secret scanner | gitleaks **is** Go: it is embedded as a library. | Needs an external binary. | Needs an external binary. | Needs an external binary. |
| HTTP API + scheduler | `net/http` and goroutines from the stdlib. | aiohttp or FastAPI. | axum or tokio, more ceremony. | No. |
| Testability | `testing`, `httptest`, native fuzzing, `-race`, `testscript` for the CLI. | pytest, very good. | Very good. | Poor. |
| Iteration speed | High. | High. | Medium. | High at first, low later. |

Why Go: static binary, cheap multi-arch, and gitleaks as a library. Python would be the natural choice given its closeness to HA (and the v3 integration will be Python), but the app shares no code with the integration: they talk over HTTP. Rust buys nothing here that Go does not provide. Bash is ruled out for testability.

The merge is **not** done with go-git (it does not implement 3-way merge): the git CLI is invoked (≥ 2.38 for `merge-tree --write-tree`; the local machine has 2.53 and Alpine ships recent versions).

### Planned dependencies (minimal)

| Package | Use |
|---|---|
| `github.com/zricethezav/gitleaks/v8` | Secret rules and detector. If its library API turns out to be unstable, plan B is to vendor only its `config/gitleaks.toml` and apply the regexes ourselves. |
| `gopkg.in/yaml.v3` | YAML pre-check. The `Node` API preserves unknown tags (`!secret`, `!include`…). |
| `github.com/bmatcuk/doublestar/v4` | Whitelist globs. |
| `github.com/rogpeppe/go-internal/testscript` | CLI tests with golden files. |

Everything else is stdlib.

## Code structure

```
cmd/hcs/                 main: subcommands and flags
internal/config/         config loading and validation (same schema as the app's options.json)
internal/paths/          repo↔box mappings, whitelist, hard denylist, per-rule direction
internal/scan/           live disk scan → {path: blob, mode}; size, binaries, unstable reads; guards
internal/gitx/           git CLI wrapper: hardened environment, fetch, hash-object, temporary index,
                         write-tree, commit-tree, merge-tree, push --atomic, ls-tree, cat-file
internal/secrets/        gitleaks + HA rules; exception fingerprints
internal/yamlcheck/      YAML/JSON parsing with HA tags
internal/apply/          tree diff → operations; CAS; atomic write; rollback; write-ahead
internal/ha/             Checker/Notifier/Reloader interface; Supervisor impl.; noop and command impls.
internal/engine/         cycle (state machine), lock, state.json, bootstrap, retries
internal/api/            HTTP server (design §12) and scheduler
internal/testkit/        scenario helpers: fake box, bare remote, "dev" clone, HA fake
scripts/                 E2E, check_config in Docker, test Supervisor
testdata/ha-config/      synthetic HA config for the E2E
```

## CLI interface

```
hcs init        --config cfg.yaml --state-dir DIR    creates the bare repo, deploy key, and known_hosts
hcs status      [--json]
hcs plan                                             what a sync would do: snapshot, merge, and apply, dry
hcs sync        [--dry-run] [--accept-bootstrap]
hcs serve       [--listen 127.0.0.1:8099]            API + scheduler
hcs deploy-key  [--rotate]
hcs scan-secrets PATH...                             the standalone scanner, useful for pre-commit hooks

Global flags:
  --config FILE      YAML/JSON with the options.json schema
  --state-dir DIR    default /data (locally: .sandbox/state)
  --checker KIND     supervisor | noop | command:'<cmd>'   (default supervisor if SUPERVISOR_TOKEN exists, otherwise noop)
  --allow-file-remote  enables file:// (v0 and tests only)
```

`--checker command:'…'` makes it possible, if Docker or Podman is available locally, to validate the copy with the same HA version:
`docker run --rm -v $DIR:/config ghcr.io/home-assistant/home-assistant:2026.11 python -m homeassistant --script check_config -c /config`.
It is optional: v0 does not depend on having containers.

## Milestones

Each milestone ends with green tests and its own commit.

| # | Milestone | Content | Main tests |
|---|---|---|---|
| M0 | Scaffold | `go.mod`, `cmd/hcs` with empty subcommands, mise tasks (`test`, `lint`), golangci-lint pinned in `mise.toml`. | `go vet`, lint, `hcs --help`. |
| M1 | Config and paths | Schema, defaults, URL validation (SSH only), mappings, whitelist, denylist, direction. | Matching tables (including `.storage/lovelace*`, `esphome/secrets.yaml`, `custom_components`); matcher fuzzing. |
| M2 | gitx + snapshot | Hardened git environment, bare repo, temporary index, commit on `box` that preserves unmanaged content, guards (empty root, mass deletion, size, binaries, symlinks, unstable reads), stat cache to avoid re-hashing what did not change (the default interval is 5 min). | Idempotent snapshot (2 runs = 1 commit); repo README preserved; guards; the cache does not hide an edit that keeps the size. |
| M3 | Merge | Ancestor/fast-forward cases, `merge-tree`, conflicts with a path list, parent order. | Merge scenarios (see catalog). |
| M4 | Apply | B→R diff filtered by whitelist and direction, YAML/JSON pre-check, write-ahead, CAS, atomic write, deletions, rollback, recovery on startup. | Fault injection between steps; concurrent edit during the apply. |
| M5 | Secrets | Embedded gitleaks + HA rules; blocking before the commit; fingerprints. | Corpus of positives and negatives; no value in logs or output. |
| M6 | Full cycle | State machine, lock, `state.json`, bootstrap (empty remote / `bootstrap_review`), `push --atomic`, retry on rejection, remembered rejected R. | Full scenario catalog + model-based test. |
| M7 | HA client | Supervisor impl. (check_config, reload_all, persistent_notification, events) against `httptest`; `command` impl. | Exact requests, headers, `invalid` handling, timeouts. |
| M8 | API and scheduler | `hcs serve`, §12 endpoints, jobs, `409` if a cycle is running, token auth. | `httptest` + concurrency with `-race`. |
| M9 | E2E | `scripts/e2e-sandbox.sh` on the synthetic config in `testdata/ha-config`, with a local bare remote seeded as a pre-existing repo. | End-to-end scenario with exact assertions. |

## Test strategy

### 1. Unit (tables)

`paths`, `yamlcheck`, `secrets`, building `apply` operations, `engine` state transitions. No I/O or git.

### 2. Integration with real git (the bulk)

Each test sets up in a `t.TempDir()`:

- `box/homeassistant`, `box/app_configs`: the live disk;
- `origin.git`: the bare remote (`file://`, with `--allow-file-remote`);
- `dev/`: a clone that acts as the local user (commits and pushes to `main`, resolves conflicts);
- an **HA fake** (`httptest`) with programmable `check_config` responses and a record of the calls.

A small DSL in `internal/testkit` keeps the scenarios readable:

```go
s := testkit.New(t)
s.Box.Write("config/automations.yaml", "...")
s.Dev.Write("config/scripts.yaml", "...").Commit("edit").Push()
s.Sync().ExpectState(engine.Idle)
s.ExpectBoxFile("config/scripts.yaml", "...")
s.ExpectRemote("main").Contains(s.Remote("box"))
```

### 3. Scenario catalog (v0 done criterion)

Bootstrap

- [ ] Empty remote → root commit, `main == box`.
- [ ] Existing `main` without `box` → `bootstrap_review`; nothing is applied; `--accept-bootstrap` unblocks it; so does a manual merge of `box` into `main`.

Normal flow

- [ ] Only the box changed → `main` advances by fast-forward.
- [ ] Only `main` changed → it is applied, `check_config` and `reload_all` are called, and `box` advances.
- [ ] Changes to different files on both sides → clean merge, first parent `main`.
- [ ] Same file, different hunks → clean merge.
- [ ] Two consecutive `sync` runs with no changes → no new commit (idempotence).
- [ ] Deletions in both directions.
- [ ] File ↔ directory (a path that changes type).
- [ ] Executable bit preserved; symlinks ignored and reported.

Conflicts

- [ ] Same hunk on both sides → `conflict`, `box` pushed, nothing applied, notification.
- [ ] During a conflict, new edits on the box keep reaching `box`.
- [ ] The user resolves in `dev` and pushes → the next cycle comes out clean.

Security and guards

- [ ] Invalid `check_config` → exact rollback, `rejected`, that R is not retried until `main` changes.
- [ ] Broken YAML from `main` → stopped at the pre-check, without touching the disk.
- [ ] Edit on the box between the snapshot and the apply → the CAS aborts and the retry includes it.
- [ ] `main` advanced between the fetch and the push → push rejected, retry, nothing lost.
- [ ] Empty or missing mount → `suspicious_snapshot`, nothing committed.
- [ ] Mass deletion (box or `main`) → stops and asks for confirmation.
- [ ] Literal secret in a new YAML → `blocked_secrets`, **no** commit (not even local), the value does not appear in any output.
- [ ] Secret with an allowed fingerprint → passes; if the line is edited → blocks again.
- [ ] `secrets.yaml`, `*.db`, or `.storage/auth` present → never get in, even when forcing the whitelist.
- [ ] Large or binary file → ignored and reported.
- [ ] Path from `main` outside the mappings (README) → not brought down to the box and preserved in snapshots.
- [ ] Incoming change in `.storage/lovelace*` (`push`) → not applied and reported.
- [ ] `https://` URL or URL with credentials → rejected in `init`.

Robustness

- [ ] Interruption (injected fault) at each apply step → the next startup leaves the disk at B or at R, never mixed.
- [ ] Two concurrent `sync` runs → the second gets "busy".

### 4. Model-based test

A random sequence (with a recorded seed) of operations: edit, create, or delete on the box; commit and push in `dev`; resolve a conflict; `sync`. After each `sync` the invariants are checked:

- in the `idle` state, the managed disk matches the `box` tree, and `box == main` on the remote;
- no content written on the box or in `dev` is lost: every version is reachable from `box` or from `main`;
- there was never a non-fast-forward push;
- an unmanaged path was never written.

### 5. CLI tests

`testscript` with golden files for `status`, `plan`, and the error messages (including that `scan-secrets` output has no values).

### 6. Hygiene

`go test -race ./...`, `go vet`, golangci-lint; fuzzing of `paths` and `yamlcheck` in the `fuzz` task (outside the normal loop). Coverage target: ≥ 80 % in `engine`, `apply`, and `paths`. CI on GitHub Actions once the remote repo exists.

## Local sandbox

```
.sandbox/e2e/              (ignored by git; rebuilt on every run)
  box/homeassistant/       the sample config + what cannot be in the repo
  box/app_configs/         config for a sample app
  origin.git/              local bare remote, seeded as a pre-existing repo
  dev/                     the "user" clone that pushes to main from another machine
  state/                   the engine's /data
  config.yaml              test config with mappings to box/...
```

- The sample config is synthetic. At run time a fake `secrets.yaml`, a DB, a log, a binary, `.storage/auth`, and an ESPHome YAML with a hand-written key (randomly generated) are added to it, to prove that none of it travels and that the scanner stops the sync.
- No network, no SSH agent, and nobody's keys are used: the engine forces `IdentityAgent=none` and the remote is `file://`.
- `scripts/fetch-box-copy.sh` remains as an optional tool to fetch a read-only copy of your own box (SSH host required). The E2E does not use it.

## E2E result (M9)

`mise run test:e2e` runs these steps with exact assertions:

| Step | What it verifies |
|---|---|
| bootstrap against an existing repo | `bootstrap_review`; `box` differs from `main` exactly in `.storage/lovelace`, a theme, and `customize.yaml`; nothing from the denylist, no binaries, and no HACS content in `box`; a secret that was already in `main` does not stop the sync. |
| accept bootstrap, no-op | `main == box`; the `secrets.yaml` that was in `main` is never brought down to the box; second sync without a push. |
| another machine → box, box → main | They arrive, with a real `check_config`. |
| conflict + resolution | The box is not touched during the conflict; the resolution arrives. |
| broken YAML, invalid integration, invalid `http` | `rejected` (pre-check, new warning, error) with rollback; reverting unblocks. |
| the same box against an empty repo | `blocked_secrets` pointing at the ESPHome YAML; the output does not include the value; nothing pushed, and no local commits. |

### Core level in Docker (after M9)

With the `ghcr.io/home-assistant/home-assistant:2026.11.1` image (pinned version):

- **`mise run test:e2e`:** the E2E uses the real `check_config` (`scripts/ha-check.sh` + `scripts/fake-secrets.sh`). It adds two cases: a change that leaves an integration invalid and another with an invalid `http`. Both are rejected with rollback.
- **`mise run test:ha-core`:** starts a disposable Core with a minimal config, runs onboarding with a test user, and runs `internal/ha` against the real API (`check_config`, `reload_all`, event, persistent notification, nonexistent `notify_service`, invalid token).
- **Finding:** for the REST API almost everything is a warning (see `design.md` §7), which justifies `check_warnings: new`.

### Supervisor level in Docker

`scripts/supervisor-dev.sh up` starts a real Supervisor with the official add-on devcontainer and mounts `homeassistant_config_sync/` as a local app. `scripts/supervisor-test.sh` builds it, installs it, validates the options, starts it, and runs `scripts/inapp-test.sh` inside the container.

To make the devcontainer work with the 2026.11 Supervisor it needed three changes:

- use the **stable** channel (dev expects a different structure);
- mount the host's `/run/supervisor` at **`/run/os`**;
- run `supervisor_run` with a pseudo-terminal (it calls `stty`).

In addition, the environment is `unhealthy` (`docker_gateway_unprotected`): this is ignored with `ha jobs options --ignore-conditions healthy`, only in this disposable environment. Core is then downgraded to 2026.11.1.

Result (`INAPP OK`):

- the nested options schema (`mappings`, `secret_scan`) validates;
- the Supervisor builds the app from `alpine:3.22` (it does not fill in `BUILD_FROM` without `build.yaml`);
- the app starts, shows the deploy key, and brings up the API; the initial sync against a nonexistent repo fails cleanly;
- from inside: `SUPERVISOR_TOKEN`, `/homeassistant`, and `/app_configs` are mounted and writable;
- a full cycle against a local remote applies a change with real `check_config` and `reload_all` through the proxy, rejects an invalid `input_boolean` with rollback, and returns to `idle`.
- `POST /addons/<slug>/options` **replaces** the options (it does not merge them).
- `all_addon_configs` became a legacy name: `all_app_configs` → `/app_configs` is used.

Not covered yet: SSH against GitHub with the deploy key, and aarch64 (the test Supervisor is amd64).

## Risks and things to validate early

| Risk | When it is validated |
|---|---|
| gitleaks library API unstable or heavy in the binary | Resolved in M5: `detect.Detect(Fragment)` is enough and the aarch64 binary stays at ~9 MB. Cost: ~60 indirect dependencies. Plan B if it becomes a problem: vendor only its rules. |
| `check_config` via the Supervisor proxy with `SUPERVISOR_TOKEN` | Before v1, with a minimal app in `/addons`. |
| `merge-tree --write-tree` and parseable conflict output | M3 (uses `-z --name-only`). |
| `check_config` time on a Raspberry Pi | v1 (configurable timeout, default 120 s). |
| Whether the Supervisor options schema accepts `mappings` (a list of objects containing lists of strings) | Resolved: it accepts it (Supervisor 2026.11.0, see "Supervisor level in Docker"). |
| `check_config` via the Supervisor proxy with `SUPERVISOR_TOKEN` | Resolved: works with `homeassistant_api: true`. |
