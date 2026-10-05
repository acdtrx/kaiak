// Runs the shared config fixtures in protocol/fixtures/config/. The gateway's suite runs
// the same files: a valid fixture passes both, an invalid one fails both, and a semantic
// fixture fails with the rule code its cases.json entry names.

import assert from "node:assert/strict";
import { readdirSync, readFileSync } from "node:fs";
import path from "node:path";
import { describe, test } from "node:test";

import { BACKEND_TYPES, validateConfig } from "./index.ts";

const FIXTURES = path.resolve(import.meta.dirname, "../../../../protocol/fixtures/config");
const VALID_DIR = path.join(FIXTURES, "valid");
const INVALID_DIR = path.join(FIXTURES, "invalid");
const EXAMPLES_DIR = path.resolve(import.meta.dirname, "../../../../examples");
const CASES_FILE = "cases.json";
const CONFIG_SCHEMA = path.resolve(import.meta.dirname, "../../schema/config.schema.json");

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

function readCases(): Map<string, InvalidCase> {
  const raw = readJson(path.join(INVALID_DIR, CASES_FILE));
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

test("BACKEND_TYPES is the schema's backend type enum, in its order", () => {
  const schema = readJson(CONFIG_SCHEMA) as { $defs: { backend: { properties: { type: { enum: unknown } } } } };
  assert.deepEqual(BACKEND_TYPES, schema.$defs.backend.properties.type.enum);
});

describe("valid config fixtures", () => {
  for (const file of fixtureFiles(VALID_DIR)) {
    test(file, () => {
      const result = validateConfig(readJson(path.join(VALID_DIR, file)));
      assert.deepEqual(result.ok ? [] : result.issues, []);
    });
  }
});

// The documented example configs must stay valid; the gateway's suite checks them too.
describe("example configs", () => {
  for (const file of fixtureFiles(EXAMPLES_DIR)) {
    test(file, () => {
      const result = validateConfig(readJson(path.join(EXAMPLES_DIR, file)));
      assert.deepEqual(result.ok ? [] : result.issues, []);
    });
  }
});

describe("invalid config fixtures", () => {
  const cases = readCases();

  test(`every invalid fixture has a ${CASES_FILE} entry and every entry has a fixture`, () => {
    assert.deepEqual([...cases.keys()].sort(), fixtureFiles(INVALID_DIR));
  });

  for (const file of fixtureFiles(INVALID_DIR)) {
    const expected = cases.get(file);
    if (!expected) continue;
    test(`${file}: ${expected.reason}`, () => {
      const result = validateConfig(readJson(path.join(INVALID_DIR, file)));
      assert.equal(result.ok, false, "the document is rejected");
      if (result.ok) return;
      const codes = [...new Set(result.issues.map((issue) => issue.code))];
      // A semantic fixture breaks exactly one rule, so no other code may appear.
      assert.deepEqual(codes, [expected.kind === "semantic" ? expected.code : "schema"]);
    });
  }
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

  test("the effective-limits bound counts each group from its direct parent, reported once at the root", () => {
    // Exactly 50 000: global's, users' own, and 499 children with users' 100 defaults.
    const atBound = (): { groups: Record<string, unknown> } =>
      readJson(path.join(VALID_DIR, "effective-limits-at-bound.json")) as { groups: Record<string, unknown> };
    const exceeded = { code: "effective-limits-exceeded", path: "" };

    // A new child of users takes its 100 defaults.
    const child = atBound();
    child.groups["u-new"] = { parent: "users" };
    assert.deepEqual(issuesOf(child), [exceeded]);

    // A group whose parent has no entry counts its own limits alone.
    const stray = atBound();
    stray.groups["stray"] = { parent: "ghost" };
    assert.deepEqual(issuesOf(stray), [{ code: "group-parent-unknown", path: "/groups/stray/parent" }]);
    (stray.groups["stray"] as { limits?: unknown[] }).limits = [{ type: "requests_per_minute", value: 1 }];
    assert.deepEqual(issuesOf(stray), [{ code: "group-parent-unknown", path: "/groups/stray/parent" }, exceeded]);

    // A group on a cycle still takes its direct parent's defaults.
    const cycle = atBound();
    cycle.groups["loop"] = { parent: "loop", child_defaults: { limits: [{ type: "requests_per_minute", value: 1 }] } };
    assert.deepEqual(issuesOf(cycle), [{ code: "group-cycle", path: "/groups/loop/parent" }, exceeded]);
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
