// Pure helpers (no DOM), unit-tested with `node --test`.

/** What each engine state means for the user, and how urgent it is. */
export const STATES = {
  idle: { label: "In sync", tone: "ok", help: "The box and the repo agree." },
  syncing: { label: "Syncing…", tone: "info", help: "A sync cycle is running." },
  conflict: {
    label: "Conflict",
    tone: "error",
    help: "The box and main changed the same lines. The box branch was pushed; merge it into main and the next sync continues.",
  },
  rejected: {
    label: "Change rejected",
    tone: "error",
    help: "A change from main did not pass the checks and was rolled back. Fix it on main; it is not retried until main changes.",
  },
  blocked_secrets: {
    label: "Possible secret",
    tone: "error",
    help: "Nothing was committed. Move the values to secrets.yaml (use !secret), or allow the finding by fingerprint if it is a false positive.",
  },
  bootstrap_review: {
    label: "Review the first sync",
    tone: "warn",
    help: "The box branch shows how this box differs from main. Merge it into main, or accept it here.",
  },
  suspicious_snapshot: {
    label: "Snapshot stopped",
    tone: "warn",
    help: "The snapshot looked wrong (an empty mount or a mass deletion), so nothing was committed.",
  },
  error: { label: "Error", tone: "error", help: "The last cycle failed; see the details." },
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

/** Human label for a snapshot change kind (A/M/D). */
export function changeKind(kind) {
  return { A: "added", M: "modified", D: "deleted" }[kind] ?? kind;
}

/** Ops of an apply plan as {kind, path} rows. */
export function planRows(plan) {
  return (plan?.ops ?? []).map((op) => ({
    kind: op.new_oid ? (op.old_oid ? "modified" : "added") : "deleted",
    path: op.repo_path,
  }));
}

/** One line per secret finding, never with values (the API has none). */
export function findingLine(f) {
  return `${f.path}:${f.line} — ${f.rule}`;
}
