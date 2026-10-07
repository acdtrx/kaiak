// The contract tests must catch a broken store, not only pass a good one: run in a child
// process against each deliberately broken store, they fail, at the test that names the
// broken guarantee (docs/reviews/2026-10-07/AUDIT.md M4, AUDIT-2.md 2M7).

import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { test } from "node:test";

const BROKEN: { runner: string; store: string; failing: string }[] = [
  {
    runner: "./torn-store.ts",
    store: "a store whose snapshot is torn",
    failing: "a snapshot names a batch counted exactly when its windows hold it",
  },
  {
    runner: "./silent-reconnect-store.ts",
    store: "a store whose channel reconnects without a catch-up",
    failing: "a channel that reconnects announces a catch-up to every subscriber",
  },
  {
    runner: "./one-subscriber-catch-up-store.ts",
    store: "a store that announces the catch-up to one subscriber",
    failing: "a channel that reconnects announces a catch-up to every subscriber",
  },
];

for (const { runner, store, failing } of BROKEN) {
  test(`the contract tests fail ${store}`, () => {
    const path = fileURLToPath(new URL(runner, import.meta.url));
    // A run of its own: without the parent runner's context, which would make the child
    // report to it instead of printing.
    const { NODE_TEST_CONTEXT: _context, ...env } = process.env;
    const run = spawnSync(process.execPath, ["--test", "--test-reporter=tap", path], { encoding: "utf8", env });
    assert.notEqual(run.status, 0, `the contract passed ${store}:\n${run.stdout}`);
    assert.match(run.stdout, new RegExp(`not ok \\d+ - ${failing}`));
  });
}
