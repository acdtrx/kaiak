// Runs the shared config fixtures in protocol/fixtures/config/. The gateway's suite runs
// the same files: a valid fixture passes both, an invalid one fails both, and a semantic
// fixture fails with the rule code its cases.json entry names.

import assert from "node:assert/strict";
import path from "node:path";
import { describe, test } from "node:test";

import { fixturePath, readJson, testInvalidFixtures, testValidFixtures } from "../test-support/index.ts";

import { BACKEND_TYPES, MODEL_CAPABILITIES, validateConfig } from "./index.ts";

const VALID_DIR = fixturePath("config", "valid");
const INVALID_DIR = fixturePath("config", "invalid");
const EXAMPLES_DIR = path.resolve(import.meta.dirname, "../../../../examples");
const CONFIG_SCHEMA = path.resolve(import.meta.dirname, "../../schema/config.schema.json");

test("BACKEND_TYPES is the schema's backend type enum, in its order", () => {
  const schema = readJson(CONFIG_SCHEMA) as { $defs: { backend: { properties: { type: { enum: unknown } } } } };
  assert.deepEqual(BACKEND_TYPES, schema.$defs.backend.properties.type.enum);
});

test("MODEL_CAPABILITIES is the schema's required metadata capabilities, in its order", () => {
  const schema = readJson(CONFIG_SCHEMA) as {
    $defs: { model: { properties: { metadata: { properties: { capabilities: { required: unknown } } } } } };
  };
  assert.deepEqual(MODEL_CAPABILITIES, schema.$defs.model.properties.metadata.properties.capabilities.required);
});

describe("valid config fixtures", () => {
  testValidFixtures(VALID_DIR, validateConfig);
});

// The documented example configs must stay valid; the gateway's suite checks them too.
describe("example configs", () => {
  testValidFixtures(EXAMPLES_DIR, validateConfig);
});

describe("invalid config fixtures", () => {
  testInvalidFixtures(INVALID_DIR, validateConfig);
});

// The 2026-10-05 review's H1: the gateway's log exporter reads its collector's
// credentials and addresses from OTEL_ variables, so no backend may name one as its
// key — its value would be sent to the backend's URL, which the config author chooses.
test("a backend's api_key_env cannot name a gateway OTEL_ variable", () => {
  const doc = readJson(path.join(VALID_DIR, "minimal.json")) as { backends: { local: Record<string, unknown> } };
  for (const name of ["OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_LOGS_HEADERS", "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"]) {
    doc.backends.local.api_key_env = name;
    assert.equal(validateConfig(doc).ok, false, `accepted a backend referencing ${name}`);
  }
});

describe("group tree rules", () => {
  // minimal.json (its key in group "me") with more groups.
  const withGroups = (groups: Record<string, { parent?: string }>): unknown => {
    const config = readJson(path.join(VALID_DIR, "minimal.json")) as { groups: Record<string, unknown> };
    config.groups = { me: {}, ...groups };
    return config;
  };
  const issuesOf = (doc: unknown): { code: string; path: string }[] => {
    const result = validateConfig(doc);
    return result.ok ? [] : result.issues.map(({ code, path: at }) => ({ code, path: at }));
  };

  test("a group below an unknown parent or a cycle reports nothing of its own", () => {
    const doc = withGroups({
      orphan: { parent: "ghost" },
      "below-orphan": { parent: "orphan" },
      "loop-a": { parent: "loop-b" },
      "loop-b": { parent: "loop-a" },
      "below-loop": { parent: "loop-a" },
    });
    assert.deepEqual(issuesOf(doc), [
      { code: "group-parent-unknown", path: "/groups/orphan/parent" },
      { code: "group-cycle", path: "/groups/loop-a/parent" },
      { code: "group-cycle", path: "/groups/loop-b/parent" },
    ]);
  });

  test("every group below level 8 reports its depth", () => {
    const chain: Record<string, { parent?: string }> = { l1: {} };
    for (let level = 2; level <= 10; level++) chain[`l${level}`] = { parent: `l${level - 1}` };
    assert.deepEqual(issuesOf(withGroups(chain)), [
      { code: "group-depth-exceeded", path: "/groups/l9" },
      { code: "group-depth-exceeded", path: "/groups/l10" },
    ]);
  });

  test("the counter bound counts two per scope and each group's per-minute limits from its direct parent, reported once at the root", () => {
    // Exactly 50 000: global's 2, users' 4 (2 and its own 2 per-minute limits), 12 498
    // children with 4 each (2 and users' 2 per-minute defaults), and spare's 2.
    const atBound = (): { groups: Record<string, unknown> } =>
      readJson(path.join(VALID_DIR, "counters-at-bound.json")) as { groups: Record<string, unknown> };
    const exceeded = { code: "counters-exceeded", path: "" };
    assert.deepEqual(issuesOf(atBound()), []);

    // A new child of users: 2, and users' 2 per-minute defaults.
    const child = atBound();
    child.groups["u-new"] = { parent: "users" };
    assert.deepEqual(issuesOf(child), [exceeded]);

    // A group with no limits still takes its two counters.
    const empty = atBound();
    empty.groups["also-empty"] = {};
    assert.deepEqual(issuesOf(empty), [exceeded]);

    // A group whose parent has no entry counts its own per-minute limits alone.
    const stray = atBound();
    delete stray.groups["spare"];
    stray.groups["stray"] = { parent: "ghost" };
    assert.deepEqual(issuesOf(stray), [{ code: "group-parent-unknown", path: "/groups/stray/parent" }]);
    (stray.groups["stray"] as { limits?: unknown[] }).limits = [{ type: "requests_per_minute", value: 1 }];
    assert.deepEqual(issuesOf(stray), [{ code: "group-parent-unknown", path: "/groups/stray/parent" }, exceeded]);

    // Hour and month limits add no counter: they check the two every scope has.
    const hourly = atBound();
    hourly.groups["spare"] = { limits: [{ type: "tokens_per_hour", value: 1 }, { type: "usd_per_month", value: 1 }] };
    assert.deepEqual(issuesOf(hourly), []);

    // A group on a cycle still takes its direct parent's defaults.
    const cycle = atBound();
    cycle.groups["spare"] = { parent: "spare", child_defaults: { limits: [{ type: "requests_per_minute", value: 1 }] } };
    assert.deepEqual(issuesOf(cycle), [{ code: "group-cycle", path: "/groups/spare/parent" }, exceeded]);
  });

  // 3M2 ([C] C6): groups without limits are counters all the same.
  test("a config of groups without limits past the bound is refused", () => {
    const config = readJson(path.join(VALID_DIR, "minimal.json")) as { groups?: Record<string, unknown> };
    config.groups ??= {};
    for (let i = 0; i < 25_000; i += 1) config.groups[`g${i}`] = {};
    assert.deepEqual(issuesOf(config), [{ code: "counters-exceeded", path: "" }]);
  });
});

describe("price tier rules", () => {
  // minimal.json with prices on its model.
  const withPrices = (prices: { effective_from: string; tiers: number[] }[]): unknown => {
    const config = readJson(path.join(VALID_DIR, "minimal.json")) as { models: { llama: Record<string, unknown> } };
    config.models.llama["prices"] = prices.map(({ effective_from, tiers }) => ({
      effective_from,
      tiers: tiers.map((above) => ({ above_input_tokens: above, usd_per_million: { tokens_in: 1, tokens_out: 2 } })),
    }));
    return config;
  };
  const issuesOf = (doc: unknown): { code: string; path: string }[] => {
    const result = validateConfig(doc);
    return result.ok ? [] : result.issues.map(({ code, path: at }) => ({ code, path: at }));
  };
  const at = (entry: number, tier: number): string => `/models/llama/prices/${entry}/tiers/${tier}/above_input_tokens`;

  test("tiers from 0, each above the one before, pass", () => {
    assert.deepEqual(issuesOf(withPrices([{ effective_from: "2026-01-01", tiers: [0, 32000, 128000] }])), []);
  });

  test("a first tier above 0 is reported at that tier, in any entry", () => {
    const doc = withPrices([
      { effective_from: "2026-01-01", tiers: [0] },
      { effective_from: "2026-02-01", tiers: [1000, 272000] },
    ]);
    assert.deepEqual(issuesOf(doc), [{ code: "price-tier-first-not-zero", path: at(1, 0) }]);
  });

  test("each tier not above the one before is reported at that tier", () => {
    const doc = withPrices([{ effective_from: "2026-01-01", tiers: [0, 200000, 200000, 100000, 272000] }]);
    assert.deepEqual(issuesOf(doc), [
      { code: "price-tiers-not-increasing", path: at(0, 2) },
      { code: "price-tiers-not-increasing", path: at(0, 3) },
    ]);
  });

  test("the tier rules run beside the date rules", () => {
    const doc = withPrices([
      { effective_from: "2026-02-01", tiers: [0] },
      { effective_from: "2026-01-01", tiers: [5, 0] },
      { effective_from: "2026-02-30", tiers: [0, 0] },
    ]);
    assert.deepEqual(issuesOf(doc), [
      { code: "price-tier-first-not-zero", path: at(1, 0) },
      { code: "price-tiers-not-increasing", path: at(1, 1) },
      { code: "price-dates-not-increasing", path: "/models/llama/prices/1/effective_from" },
      { code: "price-tiers-not-increasing", path: at(2, 1) },
      { code: "date-invalid", path: "/models/llama/prices/2/effective_from" },
    ]);
  });
});
