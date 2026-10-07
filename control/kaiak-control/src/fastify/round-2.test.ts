// Regression tests from the second pre-merge review (docs/reviews/2026-10-07/AUDIT-2.md):
// each finding that survived the round-2 decisions, and the order rule (each stream
// sends what was read last, by when the read was issued), held at the point the
// review named.

import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { afterEach, test } from "node:test";

import Fastify from "fastify";

import type { Config } from "../config/index.ts";
import { createControlPlane } from "../control-plane/index.ts";
import type { ControlPlane, ControlPlaneOptions, ListenerEvent } from "../control-plane/index.ts";
import type { GatewayStatus, Totals, UsageBatch } from "../messages/index.ts";
import { createMemoryStore } from "../storage/index.ts";
import type { ControlPlaneStore, StoreChangeListener } from "../storage/index.ts";

import { controlProtocolPlugin } from "./index.ts";
import type { ControlProtocolPluginOptions } from "./index.ts";
import { openSseStream } from "./sse-client.ts";
import type { SseItem, SseStream } from "./sse-client.ts";

const TOKEN = "round-2";
const INSTANCE = "gw-r2";
const NOW = Date.UTC(2026, 9, 7, 12, 30);
const fixture = (path: string): unknown =>
  JSON.parse(readFileSync(new URL(`../../../../protocol/fixtures/${path}`, import.meta.url), "utf8"));

// A valid config distinguishable by its context length.
function configNumbered(n: number): Config {
  const config = fixture("config/valid/minimal.json") as Config;
  const model = config.models["llama"];
  assert.ok(model, "the minimal fixture has model llama");
  model.metadata.context_length = 1000 + n;
  return config;
}

const status = (startedAt: string): GatewayStatus => ({
  ...(fixture("messages/status/valid/ready.json") as GatewayStatus),
  instance: INSTANCE,
  started_at: startedAt,
});

function batch(epoch: string, sequence: number, cost = 100): UsageBatch {
  return {
    batch: { instance: INSTANCE, epoch: epoch.repeat(32), sequence },
    records: [
      {
        record_id: `${epoch}${sequence}`.padEnd(32, "0"),
        request_id: `req-${epoch}${sequence}`,
        gateway_instance: INSTANCE,
        key_id: "key",
        groups: ["g"],
        model: "m",
        deployment: { backend: "b", model: "m" },
        units: { tokens_in: 100, tokens_cached: 0, tokens_cache_write: 0, tokens_out: 0, tokens_reasoning: 0 },
        cost_nano_usd: cost,
        estimated: false,
        partial: false,
        gateway_time: new Date(NOW).toISOString(),
      },
    ],
  };
}

const core = (store: ControlPlaneStore, options: Partial<ControlPlaneOptions> = {}): ControlPlane =>
  createControlPlane({ store, token: TOKEN, clock: () => NOW, ...options });

const settle = () => new Promise<void>((resolve) => setImmediate(resolve));

const closers: (() => Promise<void> | void)[] = [];

afterEach(async () => {
  for (const close of closers.splice(0).reverse()) await close();
});

async function startApp(
  controlPlane: ControlPlane,
  options: Omit<ControlProtocolPluginOptions, "controlPlane"> = {},
  onResponse?: (raw: NodeJS.WritableStream) => void,
): Promise<string> {
  const app = Fastify();
  if (onResponse) {
    app.addHook("onRequest", async (_request, reply) => {
      onResponse(reply.raw);
    });
  }
  await app.register(controlProtocolPlugin, { controlPlane, ...options });
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

function numberOf(item: SseItem): number {
  assert.ok(item.kind === "event" && item.event === "config", `a config event, got ${JSON.stringify(item)}`);
  const { config } = JSON.parse(item.data) as { config: Config };
  return (config.models["llama"]?.metadata.context_length ?? 0) - 1000;
}

// The next config event's number, past totals events and comments.
async function nextConfig(stream: SseStream): Promise<number> {
  for (;;) {
    const item = await stream.next();
    if (item.kind === "comment" || (item.kind === "event" && item.event === "totals")) continue;
    return numberOf(item);
  }
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

// A store that can be switched to a backup, as a store restored from one reads; only
// the active store's changes are announced.
function restorable(live: ControlPlaneStore, backup: ControlPlaneStore) {
  let active = live;
  const listeners: StoreChangeListener[] = [];
  const store = new Proxy({} as ControlPlaneStore, {
    get(_, name: keyof ControlPlaneStore) {
      if (name === "subscribe") {
        return (listener: StoreChangeListener) => {
          listeners.push(listener);
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
  return { store, restore: () => (active = backup), listeners };
}

// 2H2 ([B] B2): a stream ended while this core delivers configs writes nothing more —
// here the plugin ends every stream when one read fails, and the next delivery runs in
// the same turn, before the socket's close event.
test("a stream ended during config delivery writes nothing into the ended response", { timeout: 10_000 }, async () => {
  const base = createMemoryStore();
  let failNext = false;
  const flaky: ControlPlaneStore = {
    ...base,
    async currentConfig() {
      if (failNext) {
        failNext = false;
        throw new Error("database read failure");
      }
      return base.currentConfig();
    },
  };
  const reader = core(flaky, { deliveryRetryDelaysMs: [] });
  const writer = core(base);
  assert.ok((await writer.publishConfig(configNumbered(1))).ok);
  const errors: string[] = [];
  const url = await startApp(reader, {}, (raw) => {
    raw.on("error", (error: Error & { code?: string }) => errors.push(error.code ?? error.message));
  });
  const stream = await openStream(url);
  assert.equal(await nextConfig(stream), 1);

  // Two publishes at once: the first delivery read fails and ends the stream, the
  // second reads config 3 straight after.
  failNext = true;
  const results = await Promise.all([writer.publishConfig(configNumbered(2)), writer.publishConfig(configNumbered(3))]);
  assert.ok(results.every((result) => result.ok));
  let item = await stream.next();
  while (item.kind !== "end") item = await stream.next();
  await settle();
  assert.deepEqual(errors, [], "nothing was written after the end");
});

// 2H3 ([G] M1, [B] B1): totals list every window with usage, whatever the current
// config limits, so a gateway still running an older config — it rejected the new
// one — keeps the spend of a limit the new one dropped.
test("totals keep listing a window the current config no longer limits", async () => {
  const cp = core(createMemoryStore());
  const running = configNumbered(1);
  running.global.limits = [{ type: "usd_per_month", value: 1 }];
  assert.ok((await cp.publishConfig(running)).ok);
  assert.ok((await cp.acceptUsageBatch(INSTANCE, batch("a", 1))).ok);
  const dropped = configNumbered(2);
  dropped.global.limits = [];
  assert.ok((await cp.publishConfig(dropped)).ok);
  const totals = await cp.totals(INSTANCE);
  assert.equal(totals.windows.find((window) => window.group === undefined && window.type === "usd_per_month")?.used, "100");
});

// 2M2 ([B] B3, [R] L-R2): a gateway process died with its batch write stalled, and its
// replacement (same instance, new epoch) got B/1 counted before the stalled A/1
// committed. counted_through names both epochs, so the new process sees B/1 covered.
test("counted_through covers the acknowledged epoch after an older epoch's late write", async () => {
  const store = createMemoryStore();
  const entered = Promise.withResolvers<void>();
  const release = Promise.withResolvers<void>();
  const stalled = core({
    ...store,
    async saveCountedBatch(...args) {
      entered.resolve();
      await release.promise;
      return store.saveCountedBatch(...args);
    },
  });
  const replacement = core(store);
  const old = stalled.acceptUsageBatch(INSTANCE, batch("a", 1));
  await entered.promise;
  assert.ok((await replacement.acceptUsageBatch(INSTANCE, batch("b", 1))).ok);
  release.resolve();
  assert.ok((await old).ok);
  const totals = await replacement.totals(INSTANCE);
  assert.equal(totals.windows.find((window) => window.group === undefined && window.type === "tokens_per_hour")?.used, "200");
  assert.deepEqual(
    [...totals.counted_through].sort((x, y) => (x.epoch < y.epoch ? -1 : 1)),
    [
      { epoch: "a".repeat(32), sequence: 1 },
      { epoch: "b".repeat(32), sequence: 1 },
    ],
  );
});

// 2M5 ([B] B7): an older status whose first record read is delayed past a newer
// status's write is dropped, not written over it.
test("a delayed first status read does not overwrite a newer status", async () => {
  const store = createMemoryStore();
  const entered = Promise.withResolvers<void>();
  const release = Promise.withResolvers<void>();
  const delayed = core({
    ...store,
    async gateway(instance) {
      entered.resolve();
      await release.promise;
      return store.gateway(instance);
    },
  });
  const newer = core(store, { clock: () => NOW + 1000 });
  const pending = delayed.acceptStatus(INSTANCE, status("2026-10-07T10:00:00Z"));
  await entered.promise;
  assert.ok((await newer.acceptStatus(INSTANCE, status("2026-10-07T12:00:00Z"))).ok);
  release.resolve();
  assert.ok((await pending).ok);
  assert.equal((await store.gateway(INSTANCE))?.status.started_at, "2026-10-07T12:00:00Z");
});

// 2M1 ([R] M-R1): after a restore the core heard nothing of, a stream that connected
// meanwhile runs the restored config; the app publishing its current config again
// reaches it, since each stream skips only what it sent itself.
test("republishing after a restore reaches a stream that connected to the restored config", { timeout: 10_000 }, async () => {
  const backup = createMemoryStore();
  assert.ok((await core(backup).publishConfig(configNumbered(1))).ok);
  const { store, restore } = restorable(createMemoryStore(), backup);
  const cp = core(store);
  assert.ok((await cp.publishConfig(configNumbered(1))).ok);
  assert.ok((await cp.publishConfig(configNumbered(2))).ok);
  const url = await startApp(cp);
  const before = await openStream(url);
  assert.equal(await nextConfig(before), 2);

  restore();
  const after = await openStream(url);
  assert.equal(await nextConfig(after), 1);
  assert.ok((await cp.publishConfig(configNumbered(2))).ok);
  assert.equal(await nextConfig(after), 2);
});

// The order rule: a delivery read issued before a stream's connect read, completing
// after the connect read was sent, is not sent on that stream — it would put the
// gateway back on an older config. A stream open before both gets each in order.
test("a delivery read issued before a stream's connect read is not sent after it", { timeout: 10_000 }, async () => {
  const base = createMemoryStore();
  const entered = Promise.withResolvers<void>();
  const release = Promise.withResolvers<void>();
  let holdNext = false;
  const holding: ControlPlaneStore = {
    ...base,
    async currentConfig() {
      const entry = await base.currentConfig();
      if (holdNext) {
        holdNext = false;
        entered.resolve();
        await release.promise;
      }
      return entry;
    },
  };
  const reader = core(holding);
  const writer = core(base);
  assert.ok((await writer.publishConfig(configNumbered(1))).ok);
  const url = await startApp(reader);
  const early = await openStream(url);
  assert.equal(await nextConfig(early), 1);

  // The reader's delivery read for config 2 reads it, then waits; config 3 follows.
  holdNext = true;
  assert.ok((await writer.publishConfig(configNumbered(2))).ok);
  await entered.promise;
  assert.ok((await writer.publishConfig(configNumbered(3))).ok);
  const late = await openStream(url);
  assert.equal(await nextConfig(late), 3);
  release.resolve();

  assert.deepEqual([await nextConfig(early), await nextConfig(early)], [2, 3]);
  assert.ok((await writer.publishConfig(configNumbered(4))).ok);
  assert.equal(await nextConfig(late), 4, "config 2 never followed config 3");
});

// The order rule for totals: reads run one at a time, so a read that completes late
// never lands after a later one.
test("a slow totals read is never applied after a later one", { timeout: 10_000 }, async () => {
  const base = createMemoryStore();
  const entered = Promise.withResolvers<void>();
  const release = Promise.withResolvers<void>();
  let holdNext = false;
  const holding: ControlPlaneStore = {
    ...base,
    async totalsSnapshot(...args) {
      const snapshot = await base.totalsSnapshot(...args);
      if (holdNext) {
        holdNext = false;
        entered.resolve();
        await release.promise;
      }
      return snapshot;
    },
  };
  const cp = core(holding);
  assert.ok((await cp.publishConfig(configNumbered(1))).ok);
  const url = await startApp(cp, { totalsPushIntervalMs: 1 });
  const stream = await openStream(url);
  await nextTotals(stream);

  holdNext = true;
  assert.ok((await cp.acceptUsageBatch(INSTANCE, batch("a", 1))).ok);
  await entered.promise;
  assert.ok((await cp.acceptUsageBatch(INSTANCE, batch("a", 2))).ok);
  // Time for a read issued after the held one to complete first, if one could.
  await new Promise((resolve) => setTimeout(resolve, 50));
  release.resolve();
  const globalTokens = (totals: Totals) =>
    totals.windows.find((window) => window.group === undefined && window.type === "tokens_per_hour")?.used;
  let used = 0;
  while (used < 200) {
    const next = globalTokens(await nextTotals(stream));
    if (next === undefined) continue;
    assert.ok(Number(next) >= used, `never older: ${next} after ${used}`);
    used = Number(next);
  }
  // Nothing older follows the newest.
  assert.ok((await cp.acceptStatus(INSTANCE, status("2026-10-07T12:00:00Z"))).ok);
  const last = await nextTotals(stream);
  assert.equal(last.live_gateways, 1);
  assert.equal(globalTokens(last), undefined, "no window changed after the newest");
});

// A catch-up re-reads the current config: a stream already running it gets nothing, and
// stays open.
test("a catch-up sends a stream nothing it already runs, and the stream stays open", { timeout: 10_000 }, async () => {
  const base = createMemoryStore();
  const listeners: StoreChangeListener[] = [];
  const store: ControlPlaneStore = {
    ...base,
    subscribe(listener) {
      listeners.push(listener);
      return base.subscribe(listener);
    },
  };
  const cp = core(store);
  assert.ok((await cp.publishConfig(configNumbered(1))).ok);
  assert.ok((await cp.acceptUsageBatch(INSTANCE, batch("a", 1))).ok);
  const url = await startApp(cp, { heartbeatIntervalMs: 20 });
  const stream = await openStream(url);
  assert.equal(await nextConfig(stream), 1);
  for (const listener of listeners) listener({ type: "catch-up" });
  assert.ok((await cp.publishConfig(configNumbered(2))).ok);
  assert.equal(await nextConfig(stream), 2, "the next config is the next published one");
});

// 2L3 ([R] L-R4): a host listener throwing when a config read failed goes to
// onListenerError, and later deliveries go on.
test("a throwing delivery-failed listener does not stop later deliveries", async () => {
  const base = createMemoryStore();
  let failNext = false;
  const flaky: ControlPlaneStore = {
    ...base,
    async currentConfig() {
      if (failNext) {
        failNext = false;
        throw new Error("database read failure");
      }
      return base.currentConfig();
    },
  };
  const reported: ListenerEvent["type"][] = [];
  const reader = core(flaky, { deliveryRetryDelaysMs: [], onListenerError: (_error, event) => reported.push(event.type) });
  const writer = core(base);
  reader.onDeliveryFailed(() => {
    throw new Error("broken host listener");
  });
  const heard: string[] = [];
  reader.onConfigPublished((published) => heard.push(published.hash));
  failNext = true;
  assert.ok((await writer.publishConfig(configNumbered(1))).ok);
  for (let i = 0; i < 50 && reported.length === 0; i += 1) await settle();
  assert.deepEqual(reported, ["delivery-failed"]);
  const next = await writer.publishConfig(configNumbered(2));
  assert.ok(next.ok);
  for (let i = 0; i < 50 && heard.length === 0; i += 1) await settle();
  assert.deepEqual(heard, [next.published.hash]);
});

// 2L1 ([B] B8): the stream sends the stored config text as it is, so config_hash is the
// hash of what goes out whatever the text's spacing or member order.
test("a config event carries the stored text as it is, its hash the hash of that text", { timeout: 10_000 }, async () => {
  const store = createMemoryStore();
  const config = configNumbered(1);
  const reordered = Object.fromEntries(Object.entries(config).reverse());
  const text = JSON.stringify(reordered, null, 1).replaceAll("\n", " ");
  const hash = createHash("sha256").update(text).digest("hex");
  assert.ok((await store.publishConfig({ text, hash, publishedAt: NOW }, undefined)).saved);
  const url = await startApp(core(store));
  const stream = await openStream(url);
  const item = await stream.nextEvent();
  assert.ok(item.kind === "event" && item.event === "config");
  assert.equal(item.data, `{"config_hash":"${hash}","config":${text}}`);
});
