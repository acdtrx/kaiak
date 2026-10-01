// The in-memory store's contract points the core relies on for exactly-once counting
// and for one process per store (docs/specs/CONTROL-PROTOCOL.md, Usage intake and
// Control-plane processes).

import assert from "node:assert/strict";
import { test } from "node:test";

import type { BatchId } from "../messages/index.ts";

import { createMemoryStore } from "./index.ts";
import type { CountedBatch, StoredConfig } from "./index.ts";

const EPOCH = "a".repeat(32);

function counted(sequence: number, countedAt = 0): CountedBatch {
  return { batch: { instance: "gw-1", epoch: EPOCH, sequence }, countedAt, additions: [], records: [] };
}

const batchId = (sequence: number): BatchId => ({ instance: "gw-1", epoch: EPOCH, sequence });

test("a counted batch is written only against the expected last batch", async () => {
  const store = createMemoryStore();
  assert.deepEqual(await store.saveCountedBatch(counted(1), undefined, 10), { saved: true });
  // A second writer that decided against "no batch yet" finds batch 1 instead.
  assert.deepEqual(await store.saveCountedBatch(counted(1), undefined, 10), { saved: false, last: batchId(1) });
  assert.deepEqual(await store.saveCountedBatch(counted(2), batchId(3), 10), { saved: false, last: batchId(1) });
  assert.deepEqual(await store.lastBatch("gw-1"), batchId(1));
  assert.deepEqual(await store.saveCountedBatch(counted(2), batchId(1), 10), { saved: true });
  assert.deepEqual(await store.lastBatch("gw-1"), batchId(2));
});

test("the lease is held by one holder until released or expired", async () => {
  const store = createMemoryStore();
  assert.deepEqual(await store.acquireLease("a", 0, 100), { ok: true });
  assert.deepEqual(await store.acquireLease("b", 50, 150), { ok: false, lease: { holder: "a", expiresAt: 100 } });
  // Renewal by the holder extends it.
  assert.deepEqual(await store.acquireLease("a", 90, 190), { ok: true });
  assert.deepEqual(await store.acquireLease("b", 150, 250), { ok: false, lease: { holder: "a", expiresAt: 190 } });
  // Expired: another holder takes it.
  assert.deepEqual(await store.acquireLease("b", 190, 290), { ok: true });
  // Only the holder releases it.
  await store.releaseLease("a");
  assert.equal((await store.acquireLease("a", 200, 300)).ok, false);
  await store.releaseLease("b");
  assert.deepEqual(await store.acquireLease("a", 200, 300), { ok: true });
});

test("forgetting a gateway keeps its last counted batch; the cursor retention drops it", async () => {
  const store = createMemoryStore();
  await store.saveCountedBatch(counted(1, 100), undefined, 10);
  await store.saveCountedBatch({ ...counted(1, 200), batch: { ...batchId(1), instance: "gw-2" } }, undefined, 10);
  await store.deleteGateways(["gw-1"]);
  assert.deepEqual(await store.lastBatch("gw-1"), batchId(1));
  // Counted before 150: gw-1's cursor goes, gw-2's (counted at 200) stays.
  assert.deepEqual(await store.dropBatchCursorsCountedBefore(150), ["gw-1"]);
  assert.equal(await store.lastBatch("gw-1"), undefined);
  assert.deepEqual(await store.lastBatch("gw-2"), { ...batchId(1), instance: "gw-2" });
  // A newer batch moves the cursor's time on.
  await store.saveCountedBatch(counted(1, 300), undefined, 10);
  assert.deepEqual(await store.dropBatchCursorsCountedBefore(250), ["gw-2"]);
  assert.deepEqual(await store.lastBatch("gw-1"), batchId(1));
});

test("stored configs share no state with what callers hold, read or written", async () => {
  const store = createMemoryStore();
  const written = { version: 1, publishedAt: 0, config: { global: { max_n: 8 } } } as unknown as StoredConfig;
  await store.saveConfig(written, 10);
  (written.config as unknown as { global: { max_n: number } }).global.max_n = 1;

  const latest = await store.latestConfig();
  (latest?.config as unknown as { global: { max_n: number } }).global.max_n = 2;
  const [after] = await store.configsAfter(0);
  (after?.config as unknown as { global: { max_n: number } }).global.max_n = 3;

  const stored = (await store.latestConfig())?.config as unknown as { global: { max_n: number } };
  assert.equal(stored.global.max_n, 8);
});

test("every store has its own config epoch", async () => {
  const [a, b] = [createMemoryStore(), createMemoryStore()];
  assert.match(await a.configEpoch(), /^[0-9a-f]{32}$/);
  assert.equal(await a.configEpoch(), await a.configEpoch());
  assert.notEqual(await a.configEpoch(), await b.configEpoch());
});
