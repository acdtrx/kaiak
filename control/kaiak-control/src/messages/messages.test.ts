// Runs the shared message fixtures in protocol/fixtures/messages/<kind>/. The gateway's
// suite runs the same files: a valid fixture passes both, an invalid one fails both,
// and a semantic fixture fails with the rule code its cases.json entry names.

import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readdirSync } from "node:fs";
import path from "node:path";
import { describe, test } from "node:test";

import { fixtureFiles, fixturePath, readJson, testInvalidFixtures, testValidFixtures } from "../test-support/index.ts";

import {
  validateConfigEvent,
  validateGatewayStatus,
  validateTotals,
  validateUsageAck,
  validateUsageBatch,
  validateUsageRecord,
} from "./index.ts";
import type { MessageValidation } from "./index.ts";

const FIXTURES = fixturePath("messages");

// Each fixture directory and the validator for its message.
const VALIDATORS: Record<string, (doc: unknown) => MessageValidation<unknown>> = {
  "config-event": validateConfigEvent,
  status: validateGatewayStatus,
  totals: validateTotals,
  "usage-ack": validateUsageAck,
  "usage-batch": validateUsageBatch,
  "usage-record": validateUsageRecord,
};

test("every message fixture directory has a validator and every validator has fixtures", () => {
  const dirs = readdirSync(FIXTURES, { withFileTypes: true })
    .filter((entry) => entry.isDirectory())
    .map((entry) => entry.name)
    .sort();
  assert.deepEqual(dirs, Object.keys(VALIDATORS).sort());
});

for (const [kind, validate] of Object.entries(VALIDATORS)) {
  describe(`${kind} fixtures`, () => {
    testValidFixtures(path.join(FIXTURES, kind, "valid"), validate, "valid/");
    testInvalidFixtures(path.join(FIXTURES, kind, "invalid"), validate, "invalid/");
  });
}

test("a totals amount above 2^53 reads exactly as a BigInt", () => {
  const result = validateTotals(readJson(path.join(FIXTURES, "totals", "valid", "windows.json")));
  assert.ok(result.ok);
  const used = result.message.windows.map((window) => BigInt(window.used));
  assert.equal(used[0], 123456789012345678n);
  assert.ok((used[0] ?? 0n) > BigInt(Number.MAX_SAFE_INTEGER));
});

// config_hash is the SHA-256 of the config's JSON text as the control plane sends it
// (CONTROL-PROTOCOL.md, Current config): JSON.stringify of the config, in its members'
// order as written in the fixture.
test("every valid config event's config_hash is the SHA-256 of its config's JSON text", () => {
  const dir = path.join(FIXTURES, "config-event", "valid");
  for (const file of fixtureFiles(dir)) {
    const event = readJson(path.join(dir, file)) as { config_hash: string; config: unknown };
    const hash = createHash("sha256").update(JSON.stringify(event.config)).digest("hex");
    assert.equal(event.config_hash, hash, file);
  }
});
