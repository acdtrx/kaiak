// The contract tests must catch a broken store, not only pass a good one: run in a child
// process against each deliberately broken store, they fail — exactly the tests that
// name the broken guarantee, each for the reason that guarantee gives, and no others
// (docs/reviews/2026-10-07/AUDIT.md M4, AUDIT-2.md 2M7, AUDIT-3.md 3M5 and 3L3).

import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { test } from "node:test";

const BROKEN: { runner: string; store: string; failing: string[]; reason: RegExp }[] = [
  {
    runner: "./torn-store.ts",
    store: "a store whose snapshot is torn",
    failing: [
      "a snapshot holds exactly the batches its cursors name, whichever process wrote",
      "a snapshot names a batch counted exactly when its windows hold it",
    ],
    reason: /cursor and windows/,
  },
  {
    runner: "./silent-reconnect-store.ts",
    store: "a store whose channel reconnects without a catch-up",
    failing: ["a channel that reconnects announces a catch-up to every subscriber"],
    reason: /no notification within \d+ ms: catch-up 1/,
  },
  {
    runner: "./one-subscriber-catch-up-store.ts",
    store: "a store that announces the catch-up to one subscriber",
    failing: ["a channel that reconnects announces a catch-up to every subscriber"],
    reason: /no notification within \d+ ms: catch-up 1/,
  },
  {
    runner: "./early-announcement-store.ts",
    store: "a store that announces a publish before its write is visible",
    failing: ["a change is heard after its write: a read in the listener sees what it announced"],
    reason: /config missed/,
  },
];

for (const { runner, store, failing, reason } of BROKEN) {
  test(`the contract tests fail ${store}`, () => {
    const path = fileURLToPath(new URL(runner, import.meta.url));
    // A run of its own: without the parent runner's context, which would make the child
    // report to it instead of printing.
    const { NODE_TEST_CONTEXT: _context, ...env } = process.env;
    const run = spawnSync(process.execPath, ["--test", "--test-reporter=tap", path], { encoding: "utf8", env });
    assert.notEqual(run.status, 0, `the contract passed ${store}:\n${run.stdout}`);
    // The failing tests are exactly the named ones (suites that hold them also report
    // "not ok", but "# fail" counts tests only).
    for (const name of failing) assert.match(run.stdout, new RegExp(`not ok \\d+ - ${name}\\n`), name);
    assert.match(run.stdout, new RegExp(`^# fail ${failing.length}$`, "m"), `failing tests other than ${failing.join("; ")}`);
    assert.match(run.stdout, reason, "the failure's reason");
  });
}
