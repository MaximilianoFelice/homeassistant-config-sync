# External agents (Gemini via Antigravity)

Definitions live in `.agents/`, for the **sub-agents** skill (plugin `runner@sub-agents-skills`, enabled in `.claude/settings.json`). They run on Google Antigravity (`agy`), so they use Antigravity quota, not the Claude plan. They are a **second look from another model**: Claude implements and reviews; Gemini reviews independently, attacks the trust boundaries, critiques specs, and hardens tests.

| Agent | Model | Permission | What for | When |
|---|---|---|---|---|
| `gemini-reviewer` | Gemini 4 Argon High | `yolo` | Independent review of a commit range (lost edits, partial applies, path and secret leaks, races) | Once per milestone, in parallel with the final review |
| `threat-auditor` | Gemini 4 Argon High | `read-only` | Attack the trust boundaries of whole packages (a hostile `main`, box content, remote, or API client) | Before each release, or after touching `paths`, `apply`, `gitx`, `secrets` or `api` |
| `spec-critic` | Gemini 4 Argon High | `read-only` | Attack a spec or plan before implementing it | Once per spec, before the plan |
| `test-hardener` | Gemini 3.8 Flash High | `yolo` | Write the missing tests (only `*_test.go`) | When closing a milestone, with the gaps listed |

From Claude Code: "use the `gemini-reviewer` agent to review `BASE..HEAD`, focusing on …". The agents do not see the conversation: the request must carry everything (range, paths inside the repo, focus). Findings from `threat-auditor` come with the test that would prove them, ready to hand to `test-hardener`.

## How they run
- `agy` without interaction rejects any shell command it has no explicit permission for and ends with an empty answer (the runner still reports `success`). The runner's sandbox (`read-only`/`safe-edit` = `--mode plan|accept-edits --sandbox`) also hides everything outside the working directory, including the Go toolchain under `~/.local/share/mise`.
- So agents that **run commands** (git, `go test`) use `yolo` (`--dangerously-skip-permissions`: no sandbox and no confirmations; the "Forbidden" rules in each definition are the only limit). Agents that **only read** use `read-only` and must not run commands.
- Inputs must be **inside the repo** (a `read-only` agent cannot see files outside it).

## What we learned trying them (2026-11-11)
- `threat-auditor` on attacker 1 over `paths` + `apply` (~5 min) found real gaps: incoming deletions from `main` skip the mass-deletion guard, and symlink entries (`120000`) from `main` are written as regular files. Worth running before every release.
- Without a format rule it cites code as `file:///` links; every definition now asks for plain `file.go:line`.
- In Claude Code's auto mode, the classifier blocks launching a `yolo` agent (`--dangerously-skip-permissions`). `read-only` agents run fine. To run `gemini-reviewer` or `test-hardener` from Claude, allow it with a permission rule or run it yourself.

## Common rules (in every definition)
Never read `.sandbox/` (it can hold copies of a real HA config) or print a `secrets.yaml`. No network: no `ssh`, `curl`, `docker`, remote git, `scripts/*.sh` or `mise run` tasks. No commits or git changes, no new dependencies. Output in English and bounded.
