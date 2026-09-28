import assert from "node:assert/strict";
import path from "node:path";
import { test } from "node:test";

import { checkBoundaries } from "./check-boundaries.ts";

const fixture = (name: string): string => path.join(import.meta.dirname, "fixtures", "boundaries", name, "src");

test("entry-only imports without cycles pass", () => {
  assert.deepEqual(checkBoundaries([fixture("clean")]), []);
});

test("an import past a subsystem's entry file fails", () => {
  const violations = checkBoundaries([fixture("deep-import")]);
  assert.equal(violations.length, 1);
  assert.equal(violations[0]?.code, "deep-import");
  assert.match(violations[0]?.message ?? "", /billing\/rates\.ts/);
});

test("an import cycle fails, type-only edges included", () => {
  const violations = checkBoundaries([fixture("cycle")]);
  assert.equal(violations.length, 1);
  assert.equal(violations[0]?.code, "cycle");
  assert.match(violations[0]?.message ?? "", /billing\/index\.ts/);
  assert.match(violations[0]?.message ?? "", /ledger\/index\.ts/);
});
