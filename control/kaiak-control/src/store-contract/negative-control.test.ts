// The contract tests must catch a broken store, not only pass a good one ([B] B6 in
// docs/reviews/2026-10-07/AUDIT.md): run against a store that reads the batch cursor
// outside the totals snapshot, they fail.

import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { test } from "node:test";

test("the contract tests fail a store whose snapshot is torn", () => {
  const runner = fileURLToPath(new URL("./torn-store.ts", import.meta.url));
  // A run of its own: without the parent runner's context, which would make the child
  // report to it instead of printing.
  const { NODE_TEST_CONTEXT: _context, ...env } = process.env;
  const run = spawnSync(process.execPath, ["--test", "--test-reporter=tap", runner], { encoding: "utf8", env });
  assert.notEqual(run.status, 0, `the contract passed a torn store:\n${run.stdout}`);
  assert.match(run.stdout, /not ok \d+ - a snapshot names a batch counted exactly when its windows hold it/);
});
