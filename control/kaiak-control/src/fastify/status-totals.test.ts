// POST /v1/status and the totals the stream pushes, over a real listening app.

import assert from "node:assert/strict";
import { EventEmitter, once } from "node:events";
import net from "node:net";
import { performance } from "node:perf_hooks";
import { afterEach, describe, test } from "node:test";

import type { Config } from "../config/index.ts";
import { configHash } from "../config-publishing/index.ts";
import type { ControlPlane, ControlPlaneOptions } from "../control-plane/index.ts";
import type { ExpirySweepRun } from "../gateways/index.ts";
import type { GatewayStatus, Totals, UsageBatch } from "../messages/index.ts";
import { PROTOCOL_VERSION } from "../protocol/index.ts";
import { createMemoryStore } from "../storage/index.ts";
import type { ControlPlaneStore } from "../storage/index.ts";
import {
  TEST_INSTANCE,
  closeAfterTest,
  closeOpened,
  configNumbered,
  countingSubscriptions,
  failOnListenerError,
  fixture,
  gatewayHeaders,
  gatewayStatus,
  nextTotals,
  openStream,
  restorable,
  startApp,
  testCore,
  totalsOf,
  usageBatch,
} from "../test-support/index.ts";
import type { RunningApp, SseStream } from "../test-support/index.ts";

const READY = fixture("messages/status/valid/ready.json") as GatewayStatus;
const NOW = Date.UTC(2026, 9, 7, 12, 30);
const EPOCH = "a".repeat(32);

function newControlPlane(options: Partial<ControlPlaneOptions> = {}): ControlPlane {
  return testCore({ onListenerError: failOnListenerError, ...options });
}

async function publishFixture(controlPlane: ControlPlane, file: string): Promise<void> {
  assert.ok((await controlPlane.publishConfig(fixture(`config/valid/${file}`))).ok);
}

afterEach(closeOpened);

async function postStatus(app: RunningApp, instance: string, body: unknown = { ...READY, instance }): Promise<Response> {
  return fetch(`${app.base}/v1/status`, {
    method: "POST",
    headers: { ...gatewayHeaders(instance), "content-type": "application/json" },
    body: JSON.stringify(body),
  });
}

// The config_hash of the next event, which must be a config event.
async function nextConfigHash(stream: SseStream): Promise<string> {
  const item = await stream.nextEvent();
  assert.ok(item.kind === "event" && item.event === "config", "a config event");
  return (JSON.parse(item.data) as { config_hash: string }).config_hash;
}

// A batch from TEST_INSTANCE in epoch EPOCH: one record of group g at NOW, counting
// tokens input tokens and cost nano-USD.
function batchOf(sequence: number, tokens = 100, cost = 0): UsageBatch {
  return usageBatch({ instance: TEST_INSTANCE, epoch: EPOCH, sequence }, [
    {
      record_id: sequence.toString(16).padStart(32, "0"),
      request_id: `req-${sequence}`,
      gateway_instance: TEST_INSTANCE,
      key_id: "key",
      groups: ["g"],
      model: "m",
      deployment: { backend: "b", model: "m" },
      units: { tokens_in: tokens },
      cost_nano_usd: cost,
      gateway_time: new Date(NOW).toISOString(),
    },
  ]);
}

// The tokens a tokens_per_hour window of group (global when undefined) has used.
const used = (totals: Totals, group?: string): string | undefined =>
  totals.windows.find((window) => window.group === group && window.type === "tokens_per_hour")?.used;

describe("POST /v1/status", { timeout: 10_000 }, () => {
  test("a valid status answers 204 and is readable from the core", async () => {
    const controlPlane = newControlPlane({ clock: () => 1_000 });
    await publishFixture(controlPlane, "minimal.json");
    const app = await startApp(controlPlane);
    const response = await postStatus(app, "gw-1");
    assert.equal(response.status, 204);
    assert.equal(response.headers.get("kaiak-protocol"), String(PROTOCOL_VERSION));
    assert.equal(await response.text(), "");
    assert.deepEqual(await controlPlane.gateways(), [
      { instance: "gw-1", status: { ...READY, instance: "gw-1" }, receivedAt: 1_000, live: true },
    ]);
    assert.equal(await controlPlane.liveGateways(), 1);
  });

  test("a status before any config is accepted", async () => {
    const controlPlane = newControlPlane();
    const app = await startApp(controlPlane);
    const starting = { ...READY, state: "starting", applied_config_hash: null };
    assert.equal((await postStatus(app, "gw-1", starting)).status, 204);
    assert.equal(await controlPlane.liveGateways(), 1);
  });

  const failures = [
    { name: "a schema violation", body: fixture("messages/status/invalid/state-unknown.json"), error: "status-invalid" },
    {
      name: "a message rule violation",
      body: fixture("messages/status/invalid/started-at-not-a-day.json"),
      error: "timestamp-invalid",
    },
    { name: "another instance than the header", body: { ...READY, instance: "gw-2" }, error: "instance-mismatch" },
  ];
  for (const failure of failures) {
    test(`${failure.name} answers 400 ${failure.error}`, async () => {
      const controlPlane = newControlPlane();
      const app = await startApp(controlPlane);
      const response = await postStatus(app, "gw-1", failure.body);
      assert.equal(response.status, 400);
      const body = (await response.json()) as { error: string; detail: unknown };
      assert.equal(body.error, failure.error);
      assert.equal(typeof body.detail, "string");
      assert.deepEqual(await controlPlane.gateways(), []);
    });
  }

  test("a status without the token is refused before intake", async () => {
    const controlPlane = newControlPlane();
    const app = await startApp(controlPlane);
    const response = await fetch(`${app.base}/v1/status`, {
      method: "POST",
      headers: { ...gatewayHeaders("gw-1"), authorization: "Bearer wrong", "content-type": "application/json" },
      body: JSON.stringify({ ...READY, instance: "gw-1" }),
    });
    assert.equal(response.status, 401);
    assert.deepEqual(await controlPlane.gateways(), []);
  });

  test("the plugin starts the core with the app — its expiry sweep — and stops it on close", async () => {
    const sweeps = new EventEmitter();
    const store = createMemoryStore();
    const core = newControlPlane({ store, expirySweepIntervalMs: 5, onExpirySweep: (run) => sweeps.emit("run", run) });
    let started = 0;
    let stopped = 0;
    const controlPlane: ControlPlane = {
      ...core,
      async start() {
        started++;
        await core.start();
      },
      async stop() {
        stopped++;
        await core.stop();
      },
    };
    const app = await startApp(controlPlane);
    const [run] = (await once(sweeps, "run")) as [ExpirySweepRun];
    assert.equal(run.trigger, "schedule");
    assert.deepEqual({ started, stopped }, { started: 1, stopped: 0 });
    await app.close();
    assert.deepEqual({ started, stopped }, { started: 1, stopped: 1 });
  });

  test("two apps over one store: a stream on one hears the other's publishes, batches and gateways", async () => {
    const store = createMemoryStore();
    const clock = () => Date.UTC(2026, 8, 24, 10, 30);
    const [coreA, coreB] = [newControlPlane({ store, clock }), newControlPlane({ store, clock })];
    await publishFixture(coreA, "minimal.json");
    const [appA, appB] = [
      await startApp(coreA, { totalsPushIntervalMs: 10 }),
      await startApp(coreB, { totalsPushIntervalMs: 10 }),
    ];
    const stream = await openStream(appA.base);
    assert.equal(await nextConfigHash(stream), configHash(fixture("config/valid/minimal.json") as Config));
    await nextTotals(stream);

    // A publish through B: A's stream gets the config (a publish changes no totals).
    await publishFixture(coreB, "full.json");
    assert.equal(await nextConfigHash(stream), configHash(fixture("config/valid/full.json") as Config));

    // A batch posted to B: A's stream pushes the totals it produced.
    const response = await fetch(`${appB.base}/v1/usage`, {
      method: "POST",
      headers: { ...gatewayHeaders("gw-1"), "content-type": "application/json" },
      body: JSON.stringify(fixture("messages/usage-batch/valid/mixed-groups.json")),
    });
    assert.equal(response.status, 200);
    const pushed = await nextTotals(stream);
    assert.deepEqual(pushed.counted_through, [{ epoch: "5d41402abc4b2a76b9719d911017c592", sequence: 18 }]);
    // The first totals listed no window, so the changes are every window.
    assert.deepEqual(pushed, await coreA.totals("gw-1"));

    // A status posted to B moves the live count A's stream reports.
    assert.equal((await postStatus(appB, "gw-2")).status, 204);
    assert.equal((await nextTotals(stream)).live_gateways, 1);
  });
});

describe("totals on the stream", { timeout: 20_000 }, () => {
  test("a stream gets totals right after the current config", async () => {
    const controlPlane = newControlPlane();
    await publishFixture(controlPlane, "minimal.json");
    await publishFixture(controlPlane, "full.json");
    const stream = await openStream((await startApp(controlPlane)).base);
    assert.equal(await nextConfigHash(stream), configHash(fixture("config/valid/full.json") as Config));
    assert.deepEqual(await nextTotals(stream), { live_gateways: 0, counted_through: [], windows: [] });
  });

  test("a counted batch pushes the totals it produced", async () => {
    const controlPlane = newControlPlane({ clock: () => Date.UTC(2026, 8, 24, 10, 30) });
    await publishFixture(controlPlane, "full.json");
    const app = await startApp(controlPlane, { totalsPushIntervalMs: 10 });
    const stream = await openStream(app.base);
    assert.deepEqual((await nextTotals(stream)).windows, []);
    const response = await fetch(`${app.base}/v1/usage`, {
      method: "POST",
      headers: { ...gatewayHeaders("gw-1"), "content-type": "application/json" },
      body: JSON.stringify(fixture("messages/usage-batch/valid/mixed-groups.json")),
    });
    assert.equal(response.status, 200);
    // The ack names the batch only: the totals come on the stream.
    assert.deepEqual(Object.keys((await response.json()) as object), ["batch"]);
    const pushed = await nextTotals(stream);
    assert.ok(pushed.windows.length > 0);
    assert.deepEqual(pushed, await controlPlane.totals("gw-1"));
    // The stream is gw-1's, and the batch was gw-1's: the push counts it.
    assert.deepEqual(pushed.counted_through, [{ epoch: "5d41402abc4b2a76b9719d911017c592", sequence: 18 }]);
  });

  test("the first totals are complete; later ones list only the windows that changed", async () => {
    const controlPlane = newControlPlane({ clock: () => Date.UTC(2026, 8, 24, 10, 30) });
    const app = await startApp(controlPlane, { totalsPushIntervalMs: 10 });
    const batchOf = (sequence: number, groups: string[]): unknown => {
      const batch = fixture("messages/usage-batch/valid/mixed-groups.json") as {
        batch: { sequence: number };
        records: { record_id: string; groups: string[] }[];
      };
      batch.batch.sequence = sequence;
      batch.records = batch.records.slice(0, 1).map((record) => ({ ...record, record_id: `${sequence}`.padStart(32, "0"), groups }));
      return batch;
    };
    const post = async (doc: unknown): Promise<void> => {
      const response = await fetch(`${app.base}/v1/usage`, {
        method: "POST",
        headers: { ...gatewayHeaders("gw-1"), "content-type": "application/json" },
        body: JSON.stringify(doc),
      });
      assert.equal(response.status, 200);
    };
    await publishFixture(controlPlane, "minimal.json");
    await post(batchOf(100, ["users", "carol"]));
    const stream = await openStream(app.base);
    const first = await nextTotals(stream);
    assert.deepEqual(first, await controlPlane.totals("gw-1"), "the first totals are complete");
    assert.ok(first.windows.some((window) => window.group === "carol"));

    // A batch for bob changes global's, users' and bob's windows, never carol's.
    await post(batchOf(101, ["users", "bob"]));
    const changes = await nextTotals(stream);
    const groups = [...new Set(changes.windows.map((window) => window.group ?? "global"))].sort();
    assert.deepEqual(groups, ["bob", "global", "users"]);
    assert.deepEqual(changes.counted_through, [{ epoch: "5d41402abc4b2a76b9719d911017c592", sequence: 101 }]);
  });

  test("a publish pushes no totals, and the windows stay whatever the config", async () => {
    const controlPlane = newControlPlane({ clock: () => Date.UTC(2026, 8, 24, 10, 30) });
    await publishFixture(controlPlane, "full.json");
    const app = await startApp(controlPlane, { totalsPushIntervalMs: 10 });
    const stream = await openStream(app.base);
    await nextTotals(stream);
    const response = await fetch(`${app.base}/v1/usage`, {
      method: "POST",
      headers: { ...gatewayHeaders("gw-1"), "content-type": "application/json" },
      body: JSON.stringify(fixture("messages/usage-batch/valid/mixed-groups.json")),
    });
    assert.equal(response.status, 200);
    assert.ok((await nextTotals(stream)).windows.length > 0);
    // A config without limits: the config goes out, and no totals with it. The next
    // totals (a gateway joining) list no window: none changed.
    await publishFixture(controlPlane, "minimal.json");
    assert.equal(await nextConfigHash(stream), configHash(fixture("config/valid/minimal.json") as Config));
    assert.equal((await postStatus(app, "gw-2")).status, 204);
    const next = totalsOf(await stream.nextEvent());
    assert.equal(next.live_gateways, 1);
    assert.deepEqual(next.windows, []);
    assert.ok((await controlPlane.totals("gw-1")).windows.length > 0, "the windows are still counted");
  });

  test("gateways joining and leaving push the live count", async () => {
    let now = Date.UTC(2026, 8, 24, 10, 0);
    const controlPlane = newControlPlane({ clock: () => now });
    await publishFixture(controlPlane, "minimal.json");
    const app = await startApp(controlPlane, { totalsPushIntervalMs: 10 });
    const stream = await openStream(app.base);
    assert.equal((await nextTotals(stream)).live_gateways, 0);

    assert.equal((await postStatus(app, "gw-1")).status, 204);
    assert.equal((await nextTotals(stream)).live_gateways, 1);

    now += 30_000;
    const run = await controlPlane.expireSilentGateways("manual");
    assert.deepEqual(run.expired, ["gw-1"]);
    assert.equal((await nextTotals(stream)).live_gateways, 0);
  });

  test("pushes are coalesced: a leading push, then one trailing push carrying the latest", async () => {
    const interval = 300;
    const controlPlane = newControlPlane();
    await publishFixture(controlPlane, "minimal.json");
    const app = await startApp(controlPlane, { totalsPushIntervalMs: interval });
    const stream = await openStream(app.base);
    assert.equal((await nextTotals(stream)).live_gateways, 0);
    const leadingAt = performance.now();

    // Three joins within the interval of the connect push: one trailing push.
    for (const instance of ["gw-1", "gw-2", "gw-3"]) assert.equal((await postStatus(app, instance)).status, 204);
    assert.equal((await nextTotals(stream)).live_gateways, 3);
    const trailingAt = performance.now();
    assert.ok(trailingAt - leadingAt >= interval - 50, `the trailing push waited for the interval (${trailingAt - leadingAt} ms)`);

    // The next change's push is the next totals event: nothing else was queued.
    assert.equal((await postStatus(app, "gw-4")).status, 204);
    assert.equal((await nextTotals(stream)).live_gateways, 4);
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
    const cp = testCore({ store: holding, clock: () => NOW });
    assert.ok((await cp.publishConfig(configNumbered(1))).ok);
    const app = await startApp(cp, { totalsPushIntervalMs: 1 });
    const stream = await openStream(app.base);
    await nextTotals(stream);

    holdNext = true;
    assert.ok((await cp.acceptUsageBatch(TEST_INSTANCE, batchOf(1, 100, 100))).ok);
    await entered.promise;
    assert.ok((await cp.acceptUsageBatch(TEST_INSTANCE, batchOf(2, 100, 100))).ok);
    // Time for a read issued after the held one to complete first, if one could.
    await new Promise((resolve) => setTimeout(resolve, 50));
    release.resolve();
    let usedTokens = 0;
    while (usedTokens < 200) {
      const next = used(await nextTotals(stream));
      if (next === undefined) continue;
      assert.ok(Number(next) >= usedTokens, `never older: ${next} after ${usedTokens}`);
      usedTokens = Number(next);
    }
    // Nothing older follows the newest.
    assert.ok((await cp.acceptStatus(TEST_INSTANCE, gatewayStatus(TEST_INSTANCE, { started_at: "2026-10-07T12:00:00Z" }))).ok);
    const last = await nextTotals(stream);
    assert.equal(last.live_gateways, 1);
    assert.equal(used(last), undefined, "no window changed after the newest");
  });

  // A gateway that moves to a core whose change channel lags gets that core's complete
  // totals; they must include the batch counted just before, which the gateway may
  // already have seen counted through another core.
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
    const a = testCore({ store, clock: () => NOW });
    const b = testCore({ store: lagging, clock: () => NOW });
    closeAfterTest(() => b.stop());
    await publishFixture(a, "minimal.json");
    const app = await startApp(b, { totalsPushIntervalMs: 1 });
    const before = await openStream(app.base);
    assert.deepEqual((await nextTotals(before)).counted_through, []);
    assert.ok((await a.acceptUsageBatch(TEST_INSTANCE, batchOf(1))).ok);
    const after = await openStream(app.base);
    const first = await nextTotals(after);
    assert.deepEqual(first.counted_through, [{ epoch: EPOCH, sequence: 1 }]);
    assert.equal(used(first, "g"), "100");
  });

  // While a core's totals reads fail, a new stream must not get the last good read as
  // complete totals; it gets none until a read works.
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
    const controlPlane = testCore({ store: flaky, clock: () => NOW });
    await publishFixture(controlPlane, "minimal.json");
    const app = await startApp(controlPlane, { totalsPushIntervalMs: 20 });
    const before = await openStream(app.base);
    assert.deepEqual((await nextTotals(before)).windows, []);
    failing = true;
    assert.ok((await controlPlane.acceptUsageBatch(TEST_INSTANCE, batchOf(1))).ok);
    const after = await openStream(app.base);
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

  // A batch that counts nothing still moves its cursor, and the gateway needs that push
  // to stop waiting for it.
  test("a counted batch that changes no window is pushed for its cursor", { timeout: 10_000 }, async () => {
    const controlPlane = testCore({ clock: () => NOW });
    await publishFixture(controlPlane, "minimal.json");
    const stream = await openStream((await startApp(controlPlane, { totalsPushIntervalMs: 1 })).base);
    assert.deepEqual(await nextTotals(stream), { live_gateways: 0, counted_through: [], windows: [] });
    assert.ok((await controlPlane.acceptUsageBatch(TEST_INSTANCE, batchOf(1, 0))).ok);
    assert.deepEqual(await nextTotals(stream), {
      live_gateways: 0,
      counted_through: [{ epoch: EPOCH, sequence: 1 }],
      windows: [],
    });
  });

  // A window the store no longer holds within its own window (a store restored to less)
  // is pushed at "0", not left at its old value on the gateway.
  test("a window a restored store lacks within its window is pushed at 0", { timeout: 10_000 }, async () => {
    const backup = createMemoryStore();
    const { store, restore } = restorable(createMemoryStore(), backup);
    const controlPlane = testCore({ store, clock: () => NOW });
    await publishFixture(controlPlane, "minimal.json");
    const backupCore = testCore({ store: backup, clock: () => NOW });
    await publishFixture(backupCore, "minimal.json");
    await backupCore.stop();
    const stream = await openStream((await startApp(controlPlane, { totalsPushIntervalMs: 1 })).base);
    await nextTotals(stream);
    assert.ok((await controlPlane.acceptUsageBatch(TEST_INSTANCE, batchOf(1))).ok);
    assert.equal(used(await nextTotals(stream), "g"), "100");
    restore();
    // A status changes the live set: the next read is of the restored store.
    assert.ok((await controlPlane.acceptStatus(TEST_INSTANCE, gatewayStatus(TEST_INSTANCE))).ok);
    const restored = await nextTotals(stream);
    assert.equal(used(restored, "g"), "0");
    assert.equal(used(restored), "0");
    assert.deepEqual(restored.counted_through, []);
  });
});

describe("slow readers", { timeout: 30_000 }, () => {
  // A config about 1 MiB large: a few of them fill the socket buffers of a client that
  // does not read.
  function largeConfig(n: number): Config {
    const config = fixture("config/valid/minimal.json") as Config;
    const model = config.models["llama"];
    assert.ok(model, "the minimal fixture has model llama");
    model.metadata.context_length = 1000 + n;
    // Padding: 256 more top-level groups, each with 16 labels of 256 characters.
    const labels = Object.fromEntries(Array.from({ length: 16 }, (_, i) => [`pad${i}`, "x".repeat(256)]));
    for (let i = 0; i < 256; i++) (config.groups ??= {})[`pad-${i}`] = { labels };
    return config;
  }

  const LARGE_CONFIGS = 12;

  async function publishLarge(controlPlane: ControlPlane): Promise<void> {
    for (let n = 0; n < LARGE_CONFIGS; n++) assert.ok((await controlPlane.publishConfig(largeConfig(n))).ok);
  }

  // A stream client on a plain socket, paused from the start. HTTP/1.0, so the body is
  // the event stream as written, with no chunk framing.
  async function openPausedStream(app: RunningApp) {
    const socket = net.connect(app.port, "127.0.0.1");
    socket.pause();
    closeAfterTest(() => {
      socket.destroy();
    });
    await once(socket, "connect");
    const headers = Object.entries(gatewayHeaders("gw-1"))
      .map(([name, value]) => `${name}: ${value}\r\n`)
      .join("");
    socket.write(`GET /v1/stream HTTP/1.0\r\nhost: 127.0.0.1\r\n${headers}\r\n`);
    return {
      // Starts reading; resolves with the text so far once `done` holds for it.
      async readUntil(done: (text: string) => boolean): Promise<string> {
        let text = "";
        const decoder = new TextDecoder();
        socket.resume();
        for await (const chunk of socket) {
          text += decoder.decode(chunk as Buffer, { stream: true });
          if (done(text)) return text;
        }
        assert.fail("the stream ended first");
      },
    };
  }

  // The live-gateway counts of the totals events after the stream's nth config event
  // (counted from 1).
  function liveCountsAfter(text: string, configCount: number): number[] {
    let start = -1;
    for (let n = 0; n < configCount; n++) {
      start = text.indexOf("event: config\n", start + 1);
      if (start === -1) return [];
    }
    return text
      .slice(start)
      .split("\n\n")
      .slice(0, -1)
      .map((block) => block.split("\n"))
      .filter((lines) => lines.includes("event: totals"))
      .map((lines) => {
        const data = lines.find((line) => line.startsWith("data: "));
        assert.ok(data, "a totals event carries data");
        return (JSON.parse(data.slice("data: ".length)) as Totals).live_gateways;
      });
  }

  test("a stream whose gateway stops reading is ended after the stall timeout", async () => {
    const core = newControlPlane();
    await publishFixture(core, "minimal.json");
    const { controlPlane, activeReaches } = countingSubscriptions(core);
    const app = await startApp(controlPlane, { stalledStreamTimeoutMs: 200 });
    await openPausedStream(app);
    await activeReaches(1);
    await publishLarge(core);
    // The server ends the stream on its own; nothing on the client side moved.
    await activeReaches(0);
  });

  test("totals held back by a slow reader are sent once, carrying the latest", async () => {
    const core = newControlPlane();
    await publishFixture(core, "minimal.json");
    const { controlPlane, activeReaches } = countingSubscriptions(core);
    const app = await startApp(controlPlane, { totalsPushIntervalMs: 10 });
    const client = await openPausedStream(app);
    await activeReaches(1);
    await publishLarge(core);
    // Every join asks for a push while the socket still holds the configs.
    for (const instance of ["gw-1", "gw-2", "gw-3"]) assert.equal((await postStatus(app, instance)).status, 204);
    const lastConfig = LARGE_CONFIGS + 1;
    const text = await client.readUntil((soFar) => liveCountsAfter(soFar, lastConfig).length > 0);
    // Without the hold, the first join's totals (1) would wait behind the configs.
    assert.deepEqual(liveCountsAfter(text, lastConfig), [3]);
  });
});
