// Resolution against the shared resolution fixtures (protocol/fixtures/config/resolved/):
// the gateway's suite holds its config snapshot to the same files.

import assert from "node:assert/strict";
import { readdirSync, readFileSync } from "node:fs";
import path from "node:path";
import { describe, test } from "node:test";

import { validateConfig } from "./index.ts";
import { limitCovers, limitIdentity, mergeLimits, resolveScopes } from "./limits.ts";
import type { Config, Limit } from "./types.ts";

const RESOLVED = path.resolve(import.meta.dirname, "../../../../protocol/fixtures/config/resolved");

interface ResolvedGroup {
  path: string[];
  allowed_models: string[] | "all";
  limits: Limit[];
}

interface ResolutionFixture {
  reason: string;
  config: unknown;
  expected: { groups: Record<string, ResolvedGroup> };
}

function readFixture(file: string): ResolutionFixture {
  const raw: unknown = JSON.parse(readFileSync(path.join(RESOLVED, file), "utf8"));
  assert.ok(typeof raw === "object" && raw !== null && "config" in raw && "expected" in raw && "reason" in raw);
  return raw as ResolutionFixture;
}

describe("resolution fixtures", () => {
  const files = readdirSync(RESOLVED)
    .filter((name) => name.endsWith(".json"))
    .sort();

  test("there are resolution fixtures", () => {
    assert.ok(files.length > 0);
  });

  for (const file of files) {
    const fixture = readFixture(file);
    test(`${file}: ${fixture.reason}`, () => {
      const validation = validateConfig(fixture.config);
      assert.deepEqual(validation.ok ? [] : validation.issues, [], "the fixture's config is valid");
      if (!validation.ok) return;
      const scopes = resolveScopes(validation.config);
      assert.deepEqual(scopes[0], { path: [], limits: validation.config.global.limits ?? [] }, "global comes first");
      const resolved: Record<string, ResolvedGroup> = {};
      for (const scope of scopes.slice(1)) {
        assert.ok(scope.group !== undefined, "every scope after global names its group");
        resolved[scope.group] = {
          path: scope.path,
          allowed_models: scope.allowed_models ?? "all",
          limits: scope.limits,
        };
      }
      assert.deepEqual(resolved, fixture.expected.groups);
    });
  }
});

test("a group's own limit with a new model set follows the defaults", () => {
  const defaults = [{ type: "tokens_per_hour" as const, value: 10 }];
  const own = [{ type: "tokens_per_hour" as const, value: 99, models: ["a"] }];
  assert.deepEqual(mergeLimits(defaults, own), [defaults[0], own[0]]);
});

test("resolving a config whose parents do not reach a top-level group fails loudly", () => {
  const config = {
    format_version: 2,
    global: {},
    backends: {},
    models: {},
    groups: { a: { parent: "b" }, b: { parent: "a" } },
    keys: {},
  } satisfies Config;
  assert.throws(() => resolveScopes(config), { code: "config-invalid" });
});

test("limit identity ignores model order; coverage follows the model set", () => {
  assert.equal(
    limitIdentity({ type: "usd_per_month", models: ["b", "a"] }),
    limitIdentity({ type: "usd_per_month", models: ["a", "b"] }),
  );
  assert.notEqual(limitIdentity({ type: "usd_per_month" }), limitIdentity({ type: "usd_per_month", models: ["a"] }));
  assert.ok(limitCovers({ type: "usd_per_month", value: 1 }, "anything"));
  assert.ok(limitCovers({ type: "usd_per_month", value: 1, models: ["a"] }, "a"));
  assert.ok(!limitCovers({ type: "usd_per_month", value: 1, models: ["a"] }, "b"));
});
