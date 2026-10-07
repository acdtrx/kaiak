// Regression tests from the third pre-merge review (docs/reviews/2026-10-07/AUDIT-3.md):
// a stream's first totals come from a read issued after it connected, so they never
// lag what another process sent or serve a failing read's last good state; a batch
// that moves only a cursor is pushed; a window a store lost within its window is
// pushed at "0".

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { afterEach, test } from "node:test";

import Fastify from "fastify";

import type { Config } from "../config/index.ts";
import { createControlPlane } from "../control-plane/index.ts";
import type { ControlPlane } from "../control-plane/index.ts";
import type { GatewayStatus, Totals, UsageBatch } from "../messages/index.ts";
import { createMemoryStore } from "../storage/index.ts";
import type { ControlPlaneStore, StoreChangeListener } from "../storage/index.ts";

import { controlProtocolPlugin } from "./index.ts";
import { openSseStream } from "./sse-client.ts";
import type { SseStream } from "./sse-client.ts";

const TOKEN = "round-3";
const INSTANCE = "gw-r3";
const NOW = Date.UTC(2026, 9, 7, 12, 30);
const EPOCH = "a".repeat(32);
const fixture = (path: string): unknown =>
  JSON.parse(readFileSync(new URL(`../../../../protocol/fixtures/${path}`, import.meta.url), "utf8"));
const config = (): Config => fixture("config/valid/minimal.json") as Config;

function batch(sequence: number, tokens = 100): UsageBatch {
  return {
    batch: { instance: INSTANCE, epoch: EPOCH, sequence },
    records: [
      {
        record_id: sequence.toString(16).padStart(32, "0"),
        request_id: `req-${sequence}`,
        gateway_instance: INSTANCE,
        key_id: "key",
        groups: ["g"],
        model: "m",
        deployment: { backend: "b", model: "m" },
        units: { tokens_in: tokens, tokens_cached: 0, tokens_cache_write: 0, tokens_out: 0, tokens_reasoning: 0 },
        cost_nano_usd: 0,
        estimated: false,
        partial: false,
        gateway_time: new Date(NOW).toISOString(),
      },
    ],
  };
}

const core = (store: ControlPlaneStore): ControlPlane => createControlPlane({ store, token: TOKEN, clock: () => NOW });

const closers: (() => Promise<void> | void)[] = [];

afterEach(async () => {
  for (const close of closers.splice(0).reverse()) await close();
});

async function startApp(controlPlane: ControlPlane, totalsPushIntervalMs: number): Promise<string> {
  const app = Fastify();
  await app.register(controlProtocolPlugin, { controlPlane, totalsPushIntervalMs });
  await app.listen({ host: "127.0.0.1", port: 0 });
  closers.push(() => app.close());
  const address = app.server.address();
  assert.ok(address && typeof address === "object", "listening on a TCP port");
  return `http://127.0.0.1:${address.port}`;
}

async function openStream(base: string): Promise<SseStream> {
  const stream = await openSseStream(`${base}/v1/stream`, {
    authorization: `Bearer ${TOKEN}`,
    "kaiak-protocol": "5",
    "kaiak-instance": INSTANCE,
  });
  closers.push(() => stream.close());
  assert.equal(stream.response.status, 200);
  return stream;
}

// The next totals event, past config events and comments.
async function nextTotals(stream: SseStream): Promise<Totals> {
  for (;;) {
    const item = await stream.next();
    if (item.kind === "comment" || (item.kind === "event" && item.event === "config")) continue;
    assert.ok(item.kind === "event" && item.event === "totals", `a totals event, got ${JSON.stringify(item)}`);
    return JSON.parse(item.data) as Totals;
  }
}

const used = (totals: Totals, group?: string): string | undefined =>
  totals.windows.find((window) => window.group === group && window.type === "tokens_per_hour")?.used;

// 3H1 ([R] R3-M1): a gateway that moves to a core whose change channel lags gets that
// core's complete totals; they must include the batch counted just before, which the
// gateway may already have seen counted through another core.
test("a new stream's first totals include every batch counted before it connected", { timeout: 10_000 }, async () => {
  const store = createMemoryStore();
  // Core b hears of every change 300 ms late.
  const lagging: ControlPlaneStore = {
    ...store,
    subscribe(listener) {
      const timers = new Set<ReturnType<typeof setTimeout>>();
      const unsubscribe = store.subscribe((change) => {
        const timer = setTimeout(() => {
          timers.delete(timer);
          listener(change);
        }, 300);
        timers.add(timer);
      });
      return () => {
        unsubscribe();
        for (const timer of timers) clearTimeout(timer);
      };
    },
  };
  const a = core(store);
  const b = core(lagging);
  closers.push(() => b.stop());
  assert.ok((await a.publishConfig(config())).ok);
  const base = await startApp(b, 1);
  const before = await openStream(base);
  assert.deepEqual((await nextTotals(before)).counted_through, []);
  assert.ok((await a.acceptUsageBatch(INSTANCE, batch(1))).ok);
  const after = await openStream(base);
  const first = await nextTotals(after);
  assert.deepEqual(first.counted_through, [{ epoch: EPOCH, sequence: 1 }]);
  assert.equal(used(first, "g"), "100");
});

// 3H1 ([C] C1, [R] R3-M2): while a core's totals reads fail, a new stream must not get
// the last good read as complete totals; it gets none until a read works.
test("while totals reads fail a new stream gets no totals; the first read that works sends them complete", { timeout: 10_000 }, async () => {
  const store = createMemoryStore();
  let failing = false;
  const flaky: ControlPlaneStore = {
    ...store,
    async totalsSnapshot(current) {
      if (failing) throw new Error("totals query unavailable");
      return store.totalsSnapshot(current);
    },
  };
  const controlPlane = core(flaky);
  assert.ok((await controlPlane.publishConfig(config())).ok);
  const base = await startApp(controlPlane, 20);
  const before = await openStream(base);
  assert.deepEqual((await nextTotals(before)).windows, []);
  failing = true;
  assert.ok((await controlPlane.acceptUsageBatch(INSTANCE, batch(1))).ok);
  const after = await openStream(base);
  const pending = nextTotals(after);
  const early = await Promise.race([
    pending.then((totals) => totals),
    new Promise<"none">((resolve) => setTimeout(() => resolve("none"), 300)),
  ]);
  assert.equal(early, "none", `totals sent while every read fails: ${JSON.stringify(early)}`);
  failing = false;
  const first = await pending;
  assert.equal(used(first, "g"), "100");
  assert.deepEqual(first.counted_through, [{ epoch: EPOCH, sequence: 1 }]);
});

// 3H1 (decision 29): a batch that counts nothing still moves its cursor, and the
// gateway needs that push to stop waiting for it.
test("a counted batch that changes no window is pushed for its cursor", { timeout: 10_000 }, async () => {
  const controlPlane = core(createMemoryStore());
  assert.ok((await controlPlane.publishConfig(config())).ok);
  const stream = await openStream(await startApp(controlPlane, 1));
  assert.deepEqual(await nextTotals(stream), { live_gateways: 0, counted_through: [], windows: [] });
  assert.ok((await controlPlane.acceptUsageBatch(INSTANCE, batch(1, 0))).ok);
  assert.deepEqual(await nextTotals(stream), {
    live_gateways: 0,
    counted_through: [{ epoch: EPOCH, sequence: 1 }],
    windows: [],
  });
});

// 3L5: a window the store no longer holds within its own window (a store restored to
// less) is pushed at "0", not left at its old value on the gateway.
test("a window a restored store lacks within its window is pushed at 0", { timeout: 10_000 }, async () => {
  const live = createMemoryStore();
  const backup = createMemoryStore();
  let active = live;
  const store = new Proxy({} as ControlPlaneStore, {
    get(_, name: keyof ControlPlaneStore) {
      if (name === "subscribe") {
        return (listener: StoreChangeListener) => {
          const fromLive = live.subscribe((change) => active === live && listener(change));
          const fromBackup = backup.subscribe((change) => active === backup && listener(change));
          return () => {
            fromLive();
            fromBackup();
          };
        };
      }
      return (...args: unknown[]) => (active[name] as (...a: unknown[]) => unknown).apply(active, args);
    },
  });
  const controlPlane = core(store);
  assert.ok((await controlPlane.publishConfig(config())).ok);
  const backupCore = core(backup);
  assert.ok((await backupCore.publishConfig(config())).ok);
  await backupCore.stop();
  const stream = await openStream(await startApp(controlPlane, 1));
  await nextTotals(stream);
  assert.ok((await controlPlane.acceptUsageBatch(INSTANCE, batch(1))).ok);
  assert.equal(used(await nextTotals(stream), "g"), "100");
  active = backup;
  // A status changes the live set: the next read is of the restored store.
  const status = { ...(fixture("messages/status/valid/ready.json") as GatewayStatus), instance: INSTANCE };
  assert.ok((await controlPlane.acceptStatus(INSTANCE, status)).ok);
  const restored = await nextTotals(stream);
  assert.equal(used(restored, "g"), "0");
  assert.equal(used(restored), "0");
  assert.deepEqual(restored.counted_through, []);
});
