import assert from "node:assert/strict";
import { test } from "node:test";
import { changeKind, findingLine, planRows, relativeTime, shortSha, stateInfo } from "../src/format.js";

test("stateInfo maps engine states and prefers syncing", () => {
  assert.equal(stateInfo("conflict").tone, "error");
  assert.equal(stateInfo("idle").label, "In sync");
  assert.equal(stateInfo("conflict", true).key, "syncing");
  assert.equal(stateInfo("something_new").tone, "error");
  assert.equal(stateInfo("").key, "idle");
});

test("relativeTime", () => {
  const now = Date.parse("2026-11-12T10:00:00Z");
  assert.equal(relativeTime("0001-01-01T00:00:00Z", now), "never");
  assert.equal(relativeTime("", now), "never");
  assert.equal(relativeTime("2026-11-12T09:59:50Z", now), "just now");
  assert.equal(relativeTime("2026-11-12T09:57:00Z", now), "3 minutes ago");
  assert.equal(relativeTime("2026-11-12T09:00:00Z", now), "1 hour ago");
  assert.equal(relativeTime("2026-11-10T10:00:00Z", now), "2 days ago");
});

test("shortSha and changeKind", () => {
  assert.equal(shortSha("0123456789abcdef"), "0123456789");
  assert.equal(shortSha(""), "—");
  assert.equal(changeKind("A"), "added");
  assert.equal(changeKind("D"), "deleted");
});

test("planRows classifies ops", () => {
  const rows = planRows({
    ops: [
      { repo_path: "config/a.yaml", old_oid: "1", new_oid: "2" },
      { repo_path: "config/b.yaml", new_oid: "3" },
      { repo_path: "config/c.yaml", old_oid: "4" },
    ],
  });
  assert.deepEqual(
    rows.map((r) => r.kind),
    ["modified", "added", "deleted"],
  );
  assert.deepEqual(planRows(null), []);
});

test("findingLine never includes a value", () => {
  assert.equal(findingLine({ path: "config/a.yaml", line: 3, rule: "ha-literal-secret" }), "config/a.yaml:3 — ha-literal-secret");
});
