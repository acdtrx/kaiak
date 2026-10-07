// Runs the raw-byte fixtures in protocol/fixtures/duplicate-members/: documents that
// name an object member twice. The gateway refuses each one (duplicate-member) before
// any decoder reads it. kaiak-control does not detect repeats: JSON.parse keeps the
// last occurrence, and everything kaiak-control sends is written by JSON.stringify, which
// cannot repeat a member. This suite checks what kaiak-control's reading of each fixture
// is — valid — so a repeat is each fixture's only defect, and the gateway's refusal
// is the only difference between the halves.

import assert from "node:assert/strict";
import path from "node:path";
import { test } from "node:test";

import { validateConfig } from "../config/index.ts";
import { CASES_FILE, fixtureFiles, fixturePath, readJson } from "../test-support/index.ts";

import { validateConfigEvent, validateTotals, validateUsageAck } from "./index.ts";

const FIXTURES = fixturePath("duplicate-members");

const VALIDATORS: Record<string, (doc: unknown) => { ok: true } | { ok: false; issues: unknown[] }> = {
  config: validateConfig,
  "config-event": validateConfigEvent,
  totals: validateTotals,
  "usage-ack": validateUsageAck,
};

const cases = readJson(path.join(FIXTURES, CASES_FILE)) as Record<string, { kind: string; path: string; reason: string }>;
const files = fixtureFiles(FIXTURES);

test(`every duplicate-member fixture has a ${CASES_FILE} entry and every entry has a fixture`, () => {
  assert.deepEqual(Object.keys(cases).sort(), files);
});

for (const file of files) {
  const entry = cases[file];
  if (!entry) continue;
  test(`${file}: ${entry.reason} — JSON.parse keeps the last occurrence, which is valid`, () => {
    const validate = VALIDATORS[entry.kind];
    assert.ok(validate, `a validator for kind ${entry.kind}`);
    const result = validate(readJson(path.join(FIXTURES, file)));
    assert.deepEqual(result.ok ? [] : result.issues, []);
  });
}
