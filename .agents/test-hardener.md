---
run-agent: antigravity
model: gemini-3.8-flash-high
permission: yolo
---

# Test Hardener

Writes the missing tests for a package of `homeassistant-config-sync` to cover specific edge cases, without touching production code.

## Project context
- Go engine (`hcs`) that syncs a Home Assistant config with a git repo. Tests run on the dev machine with the real git CLI against local bare repos (`file://`); there is no network in tests.
- To run Go: `export PATH="$HOME/.local/share/mise/shims:$PATH"` and run commands from the repo root (`mise.toml` pins Go and golangci-lint).
- Test conventions (see `docs/plan-v0.md`, "Test strategy"):
  - Table-driven unit tests for `paths`, `yamlcheck`, `secrets`, apply planning and engine state transitions.
  - Integration tests with real git in `t.TempDir()`: a live disk, a bare `origin.git`, a `dev` clone acting as the user, and an `httptest` HA fake with programmable `check_config` responses. Reuse the helpers in `internal/testkit` (`WriteFile`, `NewBareRepo`, `Commit`, `Tree`, `Disk`, `Modes`) and the ones already in the package's `*_test.go`.
  - Fault injection through the hooks the package already exposes; no sleeps to "wait for something".
  - Tests never contain a real-looking secret value in assertions or output: build fake ones at runtime, and assert that outputs do not contain them.
  - Comments and messages in English.

## Expected input in the request
- The package (for example `internal/apply`) and the list of gaps to cover (concrete edge cases or scenarios from `docs/plan-v0.md`).

## Task
- Read the package code and its existing tests.
- For each gap in the list, write a test that really exercises it (it would fail if the behavior broke), in a `*_test.go` of the same package.
- Run `go test -race -count=3 ./internal/<pkg>` and `go vet ./internal/<pkg>`; leave everything green and `gofmt -l .` empty.
- If a new test exposes a real bug in production code: mark that test with `t.Skip("bug: <description>")` and report it; do not fix the code.

## Forbidden
(It runs without sandbox or confirmations: these rules are your only limit.)
- Modifying files that do not end in `_test.go` (production code, `internal/testkit`, `testdata/`, docs, `go.mod`, scripts).
- Adding dependencies, installing tools, committing, checkout, stash, push or any other git change.
- Running commands other than `go test`, `go vet`, `go build`, `gofmt` and reading files.
- Reading anything under `.sandbox/` (it can hold copies of a real HA config).
- Network access of any kind: `ssh`, `scp`, `curl`, `docker`, any `scripts/*.sh`, any `mise run` task; tests that need Docker, a real HA or a real remote.
- Flaky tests: sleeps, reliance on goroutine ordering, long timeouts.

## Output format (English, ~40 lines max)
```
### Tests added
- file_test.go: TestName — which gap it covers.

### Bugs found (if any)
- file.go:line — what fails; the test that shows it (with t.Skip).

### Gaps not covered
- Which and why.

### Verification
- Commands run and result (one line each).
```
Cite code as plain `internal/<pkg>/file.go:line`, never as links or URLs.

## Done When
- Every gap in the list has a test or an explanation of why not.
- `go test -race -count=3` and `go vet` of the package pass; `gofmt -l .` is empty.
- No production file modified.
