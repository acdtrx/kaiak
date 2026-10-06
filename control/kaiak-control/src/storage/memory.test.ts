// What the in-memory store does beyond the store contract (src/store-contract): every
// store is a new one, and it holds its callers to the version order.

import assert from "node:assert/strict";
import { test } from "node:test";

import type { Config } from "../config/index.ts";

import { createMemoryStore } from "./index.ts";
import type { StoredConfig } from "./index.ts";

const entry = (version: number): StoredConfig => ({ version, publishedAt: 0, config: {} as unknown as Config });

test("every store has its own config epoch", async () => {
  const [a, b] = [createMemoryStore(), createMemoryStore()];
  assert.match(await a.configEpoch(), /^[0-9a-f]{32}$/);
  assert.notEqual(await a.configEpoch(), await b.configEpoch());
});

test("a publish whose version does not follow the expected one is a caller fault", async () => {
  const store = createMemoryStore();
  await assert.rejects(store.publishConfig({ entry: entry(2), carried: [] }, { version: undefined, sequence: 0 }, 10), {
    code: "config-version-out-of-order",
  });
  assert.equal(await store.latestConfig(), undefined);
});
