// Pure helpers (no DOM), unit-tested with `node --test`.

/** What each engine state means for the user, and how urgent it is. */
export const STATES = {
  idle: { label: "In sync", tone: "ok", help: "The box and the repo agree." },
  syncing: { label: "Syncing…", tone: "info", help: "A sync cycle is running." },
  conflict: {
    label: "Conflict",
    tone: "error",
    help: "The box and main changed the same lines. The box branch was pushed; merge it into main like any branch and the next sync continues.",
  },
  review: {
    label: "Review needed",
    tone: "warn",
    help: "Only the box branch was pushed. Leave the real state in main by merging box (a PR or locally); syncing resumes after that.",
  },
  mass_delete: {
    label: "Many deletions held back",
    tone: "warn",
    help: "Applying main would delete many files on the box, so nothing was applied. If it is intended, raise the deletion guard for one sync.",
  },
  too_large: {
    label: "File too large",
    tone: "warn",
    help: "Nothing was committed: a file is larger than max_file_size_mb. Add it to .gitignore or raise the limit.",
  },
  path_collision: {
    label: "Path collides with the app",
    tone: "error",
    help: "main adds a top-level path that already exists in the app's container. Nothing was applied; move or rename it in main.",
  },
  blocked_secrets: {
    label: "Possible secret",
    tone: "error",
    help: "Nothing was committed (secret_scan.mode is block). Move the values out of the repo, or add the fingerprints to .gitleaksignore if they are false positives.",
  },
  check_failed: {
    label: "Config check failed",
    tone: "error",
    help: "The changes from main were applied, but Home Assistant's config check reported errors, so it was not reloaded. Fix them in main.",
  },
  error: { label: "Error", tone: "error", help: "The last cycle failed; see the details." },
};

/** Why a review is needed. */
export const REVIEWS = {
  first_sync: "This is the first sync: nothing reaches main until you merge box into it.",
  root_changed: "The root option changed, so the repo layout changed.",
  mass_delete: "The box deleted many files.",
};

export function stateInfo(state, syncing) {
  if (syncing) return { key: "syncing", ...STATES.syncing };
  return { key: state || "idle", ...(STATES[state] ?? STATES.error) };
}

/** Short commit id for display. */
export function shortSha(sha) {
  return sha ? String(sha).slice(0, 10) : "—";
}

/** "3 minutes ago" style relative time; "never" for empty/zero dates. */
export function relativeTime(iso, now = Date.now()) {
  if (!iso || iso.startsWith("0001-")) return "never";
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return "never";
  const s = Math.round((now - t) / 1000);
  if (s < 45) return "just now";
  const units = [
    ["year", 31536000],
    ["month", 2592000],
    ["day", 86400],
    ["hour", 3600],
    ["minute", 60],
  ];
  for (const [name, secs] of units) {
    const n = Math.floor(s / secs);
    if (n >= 1) return `${n} ${name}${n === 1 ? "" : "s"} ago`;
  }
  return "just now";
}

/** Human label for git's status letter (A/M/D/T). */
export function changeKind(status) {
  return { A: "added", M: "modified", D: "deleted", T: "type changed" }[status] ?? status;
}

/** One line per secret finding, never with values (the API has none). */
export function findingLine(f) {
  return `${f.path}:${f.line} — ${f.rule}`;
}
