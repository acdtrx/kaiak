// What a usage record counts toward each counted limit type, against the shared
// fixtures in protocol/fixtures/usage/: the gateway's limiter counts the same.

import assert from "node:assert/strict";
import path from "node:path";
import { test } from "node:test";

import type { LimitType } from "../config/index.ts";
import { COUNTED_TYPES, validateUsageRecord } from "../messages/index.ts";
import type { UsageRecord } from "../messages/index.ts";
import { fixtureFiles, fixturePath, readJson, usageRecord } from "../test-support/index.ts";

import { batchAdditions } from "./aggregate.ts";
import { currentWindows } from "./windows.ts";

// A record's units and cost, and what it counts toward each token and cost limit type
// as a decimal string; the per-minute types are the gateway's alone.
interface CountedFixture {
  reason: string;
  units: UsageRecord["units"];
  cost_nano_usd: number;
  expected: Partial<Record<LimitType, string>>;
}

test("a record counts toward each counted type what the gateway's limits count (protocol/fixtures/usage)", () => {
  const dir = fixturePath("usage");
  for (const file of fixtureFiles(dir)) {
    const { reason, units, cost_nano_usd, expected } = readJson(path.join(dir, file)) as CountedFixture;
    const record = usageRecord({ units, cost_nano_usd });
    assert.ok(validateUsageRecord(record).ok, `${file}: a valid record`);
    // Global's window of each type: every record counts toward global.
    const additions = batchAdditions([record], currentWindows(Date.parse(record.gateway_time)));
    for (const type of COUNTED_TYPES) {
      const want = expected[type];
      assert.ok(want !== undefined, `${file}: no expected amount for ${type}`);
      const counted = additions.find((addition) => addition.group === undefined && addition.type === type);
      assert.equal(counted?.used ?? 0n, BigInt(want), `${file}, ${type}: ${reason}`);
    }
  }
});
