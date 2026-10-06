// Runs the shared message fixtures in protocol/fixtures/messages/<kind>/. The gateway's
// suite runs the same files: a valid fixture passes both, an invalid one fails both,
// and a semantic fixture fails with the rule code its cases.json entry names.

import assert from "node:assert/strict";
import { readdirSync, readFileSync } from "node:fs";
import path from "node:path";
import { describe, test } from "node:test";

import {
  validateConfigEvent,
  validateGatewayStatus,
  validateTotals,
  validateUsageAck,
  validateUsageBatch,
  validateUsageRecord,
} from "./index.ts";
import type { MessageValidation } from "./index.ts";

const FIXTURES = path.resolve(import.meta.dirname, "../../../../protocol/fixtures/messages");
const CASES_FILE = "cases.json";

// Each fixture directory and the validator for its message.
const VALIDATORS: Record<string, (doc: unknown) => MessageValidation<unknown>> = {
  "config-event": validateConfigEvent,
  status: validateGatewayStatus,
  totals: validateTotals,
  "usage-ack": validateUsageAck,
  "usage-batch": validateUsageBatch,
  "usage-record": validateUsageRecord,
};

interface InvalidCase {
  kind: "schema" | "semantic";
  code?: string;
  reason: string;
}

function readJson(file: string): unknown {
  return JSON.parse(readFileSync(file, "utf8"));
}

function fixtureFiles(dir: string): string[] {
  return readdirSync(dir)
    .filter((name) => name.endsWith(".json") && name !== CASES_FILE)
    .sort();
}

function readCases(dir: string): Map<string, InvalidCase> {
  const raw = readJson(path.join(dir, CASES_FILE));
  assert.ok(typeof raw === "object" && raw !== null && !Array.isArray(raw), `${CASES_FILE} is an object`);
  const cases = new Map<string, InvalidCase>();
  for (const [file, entry] of Object.entries(raw)) {
    assert.ok(typeof entry === "object" && entry !== null, `${file}: entry is an object`);
    const { kind, code, reason } = entry as Record<string, unknown>;
    assert.ok(kind === "schema" || kind === "semantic", `${file}: kind is schema or semantic`);
    assert.equal(typeof reason, "string", `${file}: reason is a string`);
    if (kind === "semantic") assert.equal(typeof code, "string", `${file}: semantic case names its code`);
    if (kind === "schema") assert.equal(code, undefined, `${file}: schema case names no code`);
    cases.set(file, { kind, reason: String(reason), ...(typeof code === "string" ? { code } : {}) });
  }
  return cases;
}

test("every message fixture directory has a validator and every validator has fixtures", () => {
  const dirs = readdirSync(FIXTURES, { withFileTypes: true })
    .filter((entry) => entry.isDirectory())
    .map((entry) => entry.name)
    .sort();
  assert.deepEqual(dirs, Object.keys(VALIDATORS).sort());
});

for (const [kind, validate] of Object.entries(VALIDATORS)) {
  describe(`${kind} fixtures`, () => {
    const validDir = path.join(FIXTURES, kind, "valid");
    const invalidDir = path.join(FIXTURES, kind, "invalid");
    const cases = readCases(invalidDir);

    for (const file of fixtureFiles(validDir)) {
      test(`valid/${file}`, () => {
        const result = validate(readJson(path.join(validDir, file)));
        assert.deepEqual(result.ok ? [] : result.issues, []);
      });
    }

    test(`every invalid fixture has a ${CASES_FILE} entry and every entry has a fixture`, () => {
      assert.deepEqual([...cases.keys()].sort(), fixtureFiles(invalidDir));
    });

    for (const file of fixtureFiles(invalidDir)) {
      const expected = cases.get(file);
      if (!expected) continue;
      test(`invalid/${file}: ${expected.reason}`, () => {
        const result = validate(readJson(path.join(invalidDir, file)));
        assert.equal(result.ok, false, "the message is rejected");
        if (result.ok) return;
        const codes = [...new Set(result.issues.map((issue) => issue.code))];
        // A semantic fixture breaks exactly one rule, so no other code may appear.
        assert.deepEqual(codes, [expected.kind === "semantic" ? expected.code : "schema"]);
      });
    }
  });
}

test("a totals amount above 2^53 reads exactly as a BigInt", () => {
  const result = validateTotals(readJson(path.join(FIXTURES, "totals", "valid", "windows.json")));
  assert.ok(result.ok);
  const used = result.message.windows.map((window) => BigInt(window.used));
  assert.equal(used[0], 123456789012345678n);
  assert.ok((used[0] ?? 0n) > BigInt(Number.MAX_SAFE_INTEGER));
});
