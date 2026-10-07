// Regression tests from the pre-merge review (docs/reviews/2026-10-07/AUDIT.md): races
// between cores over one store, each held at the point the review named by a store
// wrapper that waits until released.

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";

import type { Config } from "../config/index.ts";
import type { GatewayStatus, UsageBatch } from "../messages/index.ts";
import { createMemoryStore } from "../storage/index.ts";
import type { ControlPlaneStore, StoreChangeListener } from "../storage/index.ts";

import { createControlPlane } from "./index.ts";
import type { ControlPlaneOptions } from "./index.ts";

const NOW = Date.UTC(2026, 9, 7, 12, 30);
const fixture = (path: string): unknown =>
  JSON.parse(readFileSync(new URL(`../../../../protocol/fixtures/${path}`, import.meta.url), "utf8"));
const config = (): Config => fixture("config/valid/minimal.json") as Config;
const status = (startedAt: string): GatewayStatus => ({
  ...(fixture("messages/status/valid/ready.json") as GatewayStatus),
  instance: "gw-review",
  started_at: startedAt,
});

function batch(epoch: string, sequence: number, recordId: string, at = NOW): UsageBatch {
  return {
    batch: { instance: "gw-review", epoch: epoch.repeat(32), sequence },
    records: [
      {
        record_id: recordId.repeat(32),
        request_id: `req-${recordId}`,
        gateway_instance: "gw-review",
        key_id: "key",
        groups: ["g"],
        model: "m",
        deployment: { backend: "b", model: "m" },
        units: { tokens_in: 1, tokens_cached: 0, tokens_cache_write: 0, tokens_out: 0, tokens_reasoning: 0 },
        cost_nano_usd: 1,
        estimated: false,
        partial: false,
        gateway_time: new Date(at).toISOString(),
      },
    ],
  };
}

const core = (store: ControlPlaneStore, options: Partial<ControlPlaneOptions> = {}) =>
  createControlPlane({ store, token: "review", clock: () => NOW, ...options });

// A store whose `method` waits, on its first call, until released.
function held<K extends keyof ControlPlaneStore>(store: ControlPlaneStore, method: K) {
  const entered = Promise.withResolvers<void>();
  const release = Promise.withResolvers<void>();
  let first = true;
  const original = store[method] as (...args: unknown[]) => Promise<unknown>;
  const wrapped = {
    ...store,
    [method]: async (...args: unknown[]) => {
      if (first) {
        first = false;
        entered.resolve();
        await release.promise;
      }
      return original.apply(store, args);
    },
  } as ControlPlaneStore;
  return { store: wrapped, entered: entered.promise, release: () => release.resolve() };
}

const settle = () => new Promise<void>((resolve) => setImmediate(resolve));

// H1 ([R] M1, [B] B1): a write stalled on one core, while the gateway's resend was
// counted by another and the gateway moved to a new batch epoch, is a duplicate.
test("a stalled write of an older epoch's batch is not counted again after a new epoch", async () => {
  const store = createMemoryStore();
  const stalled = held(store, "saveCountedBatch");
  const a = core(stalled.store);
  const b = core(store);
  assert.ok((await b.publishConfig(config())).ok);
  const old = batch("a", 1, "1");
  const inProgress = a.acceptUsageBatch("gw-review", old);
  await stalled.entered;
  assert.ok((await b.acceptUsageBatch("gw-review", old)).ok);
  assert.ok((await b.acceptUsageBatch("gw-review", batch("b", 1, "2"))).ok);
  stalled.release();
  const late = await inProgress;
  assert.ok(late.ok);
  assert.equal(late.outcome, "duplicate");
  assert.equal((await store.recentRecords(100)).length, 2, "two batches, two records");
});

// H1, the crash variant: the stalled batch was never counted elsewhere, so it is
// counted once, and the newer epoch's batches are still known as counted.
test("a stalled first write of an older epoch's batch is counted once, newer epochs kept", async () => {
  const store = createMemoryStore();
  const stalled = held(store, "saveCountedBatch");
  const a = core(stalled.store);
  const b = core(store);
  assert.ok((await b.publishConfig(config())).ok);
  const inProgress = a.acceptUsageBatch("gw-review", batch("a", 1, "1"));
  await stalled.entered;
  assert.ok((await b.acceptUsageBatch("gw-review", batch("b", 1, "2"))).ok);
  stalled.release();
  assert.ok((await inProgress).ok);
  const resend = await b.acceptUsageBatch("gw-review", batch("b", 1, "2"));
  assert.ok(resend.ok);
  assert.equal(resend.outcome, "duplicate", "the newer epoch's batch is still known as counted");
  assert.equal((await store.recentRecords(100)).length, 2);
});

// M3 ([R] L1, [B] B5): a sweep that read a gateway's record before it was forgotten
// and recreated must not forget the recreated record.
test("a stale forget does not drop a gateway forgotten and joined again meanwhile", async () => {
  const store = createMemoryStore();
  let now = NOW;
  const stalled = held(store, "forgetGateways");
  const a = core(stalled.store, { clock: () => now });
  const b = core(store, { clock: () => now });
  assert.ok((await b.acceptStatus("gw-review", status("2026-10-07T12:00:00.000Z"))).ok);
  now += 3_600_001;
  const sweep = a.expireSilentGateways("review-held");
  await stalled.entered;
  await b.expireSilentGateways("review-other-core");
  assert.ok((await b.acceptStatus("gw-review", status("2026-10-07T13:30:00.000Z"))).ok);
  assert.equal(await b.liveGateways(), 1);
  stalled.release();
  await sweep;
  assert.equal(await b.liveGateways(), 1, "the recreated gateway stays live");
});

// L1 ([R] L2): the old process's last status, written late by one core after another
// core stored the restarted process's status, is older than the stored one: dropped,
// no conflict.
test("a late status from a replaced process does not raise a conflict or win", async () => {
  const store = createMemoryStore();
  let nowA = NOW;
  let nowB = NOW;
  const stalled = held(store, "saveGateway");
  const a = core(stalled.store, { clock: () => nowA });
  const b = core(store, { clock: () => nowB });
  const old = "2026-10-07T11:00:00.000Z";
  const restarted = "2026-10-07T12:29:00.000Z";
  const late = a.acceptStatus("gw-review", status(old));
  await stalled.entered;
  nowB += 1_000;
  assert.ok((await b.acceptStatus("gw-review", status(restarted))).ok);
  nowA += 2_000;
  stalled.release();
  const intake = await late;
  assert.ok(intake.ok);
  assert.equal(intake.conflictStarted, false);
  const [stored] = await b.gateways();
  assert.equal(stored?.status.started_at, restarted, "the newer status stays stored");
  assert.equal(stored?.conflict, undefined);
});

// H3 ([B] B3): a read of the current config that fails after a notification is
// retried; the config reaches this core's listeners.
test("a config read that fails once after a publish is retried and delivered", async () => {
  const store = createMemoryStore();
  let failures = 1;
  const flaky: ControlPlaneStore = {
    ...store,
    async currentConfig() {
      if (failures > 0) {
        failures -= 1;
        throw new Error("temporary database read failure");
      }
      return store.currentConfig();
    },
  };
  const a = core(flaky, { deliveryRetryDelaysMs: [1, 1, 1] });
  const b = core(store);
  const delivered: string[] = [];
  a.onConfigPublished((published) => delivered.push(published.hash));
  const published = await b.publishConfig(config());
  assert.ok(published.ok);
  for (let i = 0; i < 50 && delivered.length === 0; i += 1) await new Promise((resolve) => setTimeout(resolve, 2));
  assert.deepEqual(delivered, [published.published.hash]);
});

// H3: a read that keeps failing is announced, so the host can end the streams that
// would otherwise keep the old config.
test("a config read that keeps failing is announced once the retries run out", async () => {
  const store = createMemoryStore();
  const failure = new Error("database down");
  const failing: ControlPlaneStore = {
    ...store,
    async currentConfig() {
      throw failure;
    },
  };
  const a = core(failing, { deliveryRetryDelaysMs: [1, 1] });
  const b = core(store);
  const announced: unknown[] = [];
  a.onDeliveryFailed((error) => announced.push(error));
  assert.ok((await b.publishConfig(config())).ok);
  for (let i = 0; i < 50 && announced.length === 0; i += 1) await new Promise((resolve) => setTimeout(resolve, 2));
  assert.deepEqual(announced, [failure]);
});

// M2 ([B] B4): a totals read that started before an hour boundary and returned after a
// batch counted past it lists the window the batch was counted in.
test("a totals read across an hour boundary lists the windows current after the read", async () => {
  const store = createMemoryStore();
  const boundary = Date.UTC(2026, 9, 7, 13);
  let now = boundary - 1;
  const stalled = held(store, "totalsSnapshot");
  const a = core(stalled.store, { clock: () => now });
  const b = core(store, { clock: () => now });
  const doc = config();
  doc.global.limits = [{ type: "tokens_per_hour", value: 10 }];
  assert.ok((await b.publishConfig(doc)).ok);
  const pending = a.totals("gw-review");
  await stalled.entered;
  now = boundary + 1;
  assert.ok((await b.acceptUsageBatch("gw-review", batch("a", 1, "1", now))).ok);
  stalled.release();
  const late = await pending;
  assert.deepEqual(late, await b.totals("gw-review"));
  assert.equal(late?.windows.length, 1, "the batch's window is listed");
});

// M1 ([R] M2, [K] M2): a store whose change channel reconnected announces a catch-up;
// a config published while the channel was down then reaches the listeners.
test("a catch-up from the store delivers what was missed while its channel was down", async () => {
  const store = createMemoryStore();
  let channelUp = true;
  const subscribers: StoreChangeListener[] = [];
  const reconnecting: ControlPlaneStore = {
    ...store,
    subscribe(listener) {
      subscribers.push(listener);
      return store.subscribe((change) => {
        if (channelUp) listener(change);
      });
    },
  };
  const a = core(reconnecting);
  const b = core(store);
  const configs: string[] = [];
  let totalsHeard = 0;
  let liveChanges = 0;
  a.onConfigPublished((published) => configs.push(published.hash));
  a.onTotalsChanged(() => (totalsHeard += 1));
  a.onGatewaysChanged((change) => (liveChanges += change.liveChanged ? 1 : 0));
  channelUp = false;
  const published = await b.publishConfig(config());
  assert.ok(published.ok);
  await settle();
  assert.deepEqual(configs, []);
  channelUp = true;
  const sequence = (await store.totalsSnapshot({ hourStart: 0, monthStart: 0 })).sequence;
  for (const listener of subscribers) listener({ type: "catch-up", sequence });
  for (let i = 0; i < 50 && configs.length === 0; i += 1) await settle();
  assert.deepEqual(configs, [published.published.hash]);
  assert.ok(totalsHeard >= 1, "totals listeners hear of a possible change");
  assert.ok(liveChanges >= 1, "gateway listeners hear of a possible live-set change");
});

// L4 ([R] L6): stop releases the core's store subscription; start takes it again and
// catches up on what changed meanwhile.
test("stop releases the store subscription; start takes it again and catches up", async () => {
  const store = createMemoryStore();
  let subscribed = 0;
  const counting: ControlPlaneStore = {
    ...store,
    subscribe(listener) {
      subscribed += 1;
      const unsubscribe = store.subscribe(listener);
      return () => {
        subscribed -= 1;
        unsubscribe();
      };
    },
  };
  const a = core(counting);
  const b = core(store);
  const configs: string[] = [];
  a.onConfigPublished((published) => configs.push(published.hash));
  await a.start();
  assert.equal(subscribed, 1);
  await a.stop();
  assert.equal(subscribed, 0, "a stopped core holds no store subscription");
  const published = await b.publishConfig(config());
  assert.ok(published.ok);
  await a.start();
  assert.equal(subscribed, 1);
  for (let i = 0; i < 50 && configs.length === 0; i += 1) await settle();
  assert.deepEqual(configs, [published.published.hash]);
  await a.stop();
});
