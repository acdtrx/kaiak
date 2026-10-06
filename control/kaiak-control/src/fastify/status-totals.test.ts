// POST /v1/status and the totals the stream pushes, over a real listening app.

import assert from "node:assert/strict";
import { EventEmitter, once } from "node:events";
import { readFileSync } from "node:fs";
import net from "node:net";
import path from "node:path";
import { performance } from "node:perf_hooks";
import { afterEach, describe, test } from "node:test";

import Fastify from "fastify";

import type { Config } from "../config/index.ts";
import { configHash } from "../config-publishing/index.ts";
import { createControlPlane } from "../control-plane/index.ts";
import type { ControlPlane, ControlPlaneOptions } from "../control-plane/index.ts";
import type { ExpirySweepRun } from "../gateways/index.ts";
import { validateTotals } from "../messages/index.ts";
import type { GatewayStatus, Totals } from "../messages/index.ts";
import { createMemoryStore } from "../storage/index.ts";

import { controlProtocolPlugin } from "./index.ts";
import type { ControlProtocolPluginOptions } from "./index.ts";
import { openSseStream } from "./sse-client.ts";
import type { SseItem, SseStream } from "./sse-client.ts";

const TOKEN = "test-token";
const FIXTURES = path.resolve(import.meta.dirname, "../../../../protocol/fixtures");

function readFixture(relative: string): unknown {
  return JSON.parse(readFileSync(path.join(FIXTURES, relative), "utf8"));
}

const READY = readFixture("messages/status/valid/ready.json") as GatewayStatus;

function headersFor(instance: string): Record<string, string> {
  return { authorization: `Bearer ${TOKEN}`, "kaiak-protocol": "5", "kaiak-instance": instance };
}

function newControlPlane(options: Partial<ControlPlaneOptions> = {}): ControlPlane {
  return createControlPlane({
    store: createMemoryStore(),
    token: TOKEN,
    onListenerError: (error) => assert.fail(`listener failed: ${String(error)}`),
    ...options,
  });
}

async function publishFixture(controlPlane: ControlPlane, file: string): Promise<void> {
  assert.ok((await controlPlane.publishConfig(readFixture(`config/valid/${file}`))).ok);
}

interface RunningApp {
  base: string;
  port: number;
  close(): Promise<void>;
}

const closers: (() => Promise<void> | void)[] = [];

afterEach(async () => {
  for (const close of closers.splice(0).reverse()) await close();
});

async function startApp(
  controlPlane: ControlPlane,
  options: Omit<ControlProtocolPluginOptions, "controlPlane"> = {},
): Promise<RunningApp> {
  const app = Fastify();
  await app.register(controlProtocolPlugin, { controlPlane, ...options });
  await app.listen({ host: "127.0.0.1", port: 0 });
  const address = app.server.address();
  assert.ok(address && typeof address === "object", "listening on a TCP port");
  let closed = false;
  const close = async (): Promise<void> => {
    if (closed) return;
    closed = true;
    await app.close();
  };
  closers.push(close);
  return { base: `http://127.0.0.1:${address.port}`, port: address.port, close };
}

async function openStream(app: RunningApp): Promise<SseStream> {
  const stream = await openSseStream(`${app.base}/v1/stream`, headersFor("gw-1"));
  closers.push(() => stream.close());
  assert.equal(stream.response.status, 200);
  return stream;
}

async function postStatus(app: RunningApp, instance: string, body: unknown = { ...READY, instance }): Promise<Response> {
  return fetch(`${app.base}/v1/status`, {
    method: "POST",
    headers: { ...headersFor(instance), "content-type": "application/json" },
    body: JSON.stringify(body),
  });
}

// The next totals event's message, after checking it is valid and carries no id.
// A config event before it (the stream's first, or a publish's) is skipped.
async function nextTotals(stream: SseStream): Promise<Totals> {
  let item = await stream.nextEvent();
  while (item.kind === "event" && item.event === "config") item = await stream.nextEvent();
  return totalsOf(item);
}

// The config_hash of the next event, which must be a config event.
async function nextConfigHash(stream: SseStream): Promise<string> {
  const item = await stream.nextEvent();
  assert.ok(item.kind === "event" && item.event === "config", "a config event");
  return (JSON.parse(item.data) as { config_hash: string }).config_hash;
}

function totalsOf(item: SseItem): Totals {
  assert.ok(item.kind === "event", `an event, got ${item.kind}`);
  assert.equal(item.event, "totals");
  assert.equal(item.id, undefined, "totals events carry no id");
  const validation = validateTotals(JSON.parse(item.data));
  assert.ok(validation.ok, "the data is valid totals");
  return validation.message;
}

// Wraps a core so a test sees how many streams hold a totals subscription.
function countingStreams(controlPlane: ControlPlane) {
  let active = 0;
  const changes = new EventEmitter();
  const counted: ControlPlane = {
    ...controlPlane,
    onTotalsChanged(listener) {
      const unsubscribe = controlPlane.onTotalsChanged(listener);
      active++;
      changes.emit("change");
      return () => {
        unsubscribe();
        active--;
        changes.emit("change");
      };
    },
  };
  const activeReaches = async (count: number): Promise<void> => {
    while (active !== count) await once(changes, "change");
  };
  return { controlPlane: counted, activeReaches };
}

describe("POST /v1/status", { timeout: 10_000 }, () => {
  test("a valid status answers 204 and is readable from the core", async () => {
    const controlPlane = newControlPlane({ clock: () => 1_000 });
    await publishFixture(controlPlane, "minimal.json");
    const app = await startApp(controlPlane);
    const response = await postStatus(app, "gw-1");
    assert.equal(response.status, 204);
    assert.equal(response.headers.get("kaiak-protocol"), "5");
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
    { name: "a schema violation", body: readFixture("messages/status/invalid/state-unknown.json"), error: "status-invalid" },
    {
      name: "a message rule violation",
      body: readFixture("messages/status/invalid/started-at-not-a-day.json"),
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
      headers: { ...headersFor("gw-1"), authorization: "Bearer wrong", "content-type": "application/json" },
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
    const stream = await openStream(appA);
    assert.equal(await nextConfigHash(stream), configHash(readFixture("config/valid/minimal.json") as Config));
    await nextTotals(stream);

    // A publish through B: A's stream gets the config, then the totals.
    await publishFixture(coreB, "full.json");
    assert.equal(await nextConfigHash(stream), configHash(readFixture("config/valid/full.json") as Config));
    await nextTotals(stream);

    // A batch posted to B: A's stream pushes the totals it produced.
    const response = await fetch(`${appB.base}/v1/usage`, {
      method: "POST",
      headers: { ...headersFor("gw-1"), "content-type": "application/json" },
      body: JSON.stringify(readFixture("messages/usage-batch/valid/mixed-groups.json")),
    });
    assert.equal(response.status, 200);
    const pushed = await nextTotals(stream);
    assert.deepEqual(pushed.counted_through, { epoch: "5d41402abc4b2a76b9719d911017c592", sequence: 18 });
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
    const stream = await openStream(await startApp(controlPlane));
    assert.equal(await nextConfigHash(stream), configHash(readFixture("config/valid/full.json") as Config));
    assert.deepEqual(await nextTotals(stream), { live_gateways: 0, counted_through: null, windows: [] });
  });

  test("a counted batch pushes the totals it produced", async () => {
    const controlPlane = newControlPlane({ clock: () => Date.UTC(2026, 8, 24, 10, 30) });
    await publishFixture(controlPlane, "full.json");
    const app = await startApp(controlPlane, { totalsPushIntervalMs: 10 });
    const stream = await openStream(app);
    assert.deepEqual((await nextTotals(stream)).windows, []);
    const response = await fetch(`${app.base}/v1/usage`, {
      method: "POST",
      headers: { ...headersFor("gw-1"), "content-type": "application/json" },
      body: JSON.stringify(readFixture("messages/usage-batch/valid/mixed-groups.json")),
    });
    assert.equal(response.status, 200);
    // The ack names the batch only: the totals come on the stream.
    assert.deepEqual(Object.keys((await response.json()) as object), ["batch"]);
    const pushed = await nextTotals(stream);
    assert.ok(pushed.windows.length > 0);
    assert.deepEqual(pushed, await controlPlane.totals("gw-1"));
    // The stream is gw-1's, and the batch was gw-1's: the push counts it.
    assert.deepEqual(pushed.counted_through, { epoch: "5d41402abc4b2a76b9719d911017c592", sequence: 18 });
  });

  test("a publish is followed by totals listing the new config's limits", async () => {
    const controlPlane = newControlPlane({ clock: () => Date.UTC(2026, 8, 24, 10, 30) });
    await publishFixture(controlPlane, "full.json");
    const app = await startApp(controlPlane, { totalsPushIntervalMs: 10 });
    const stream = await openStream(app);
    await nextTotals(stream);
    const response = await fetch(`${app.base}/v1/usage`, {
      method: "POST",
      headers: { ...headersFor("gw-1"), "content-type": "application/json" },
      body: JSON.stringify(readFixture("messages/usage-batch/valid/mixed-groups.json")),
    });
    assert.equal(response.status, 200);
    assert.ok((await nextTotals(stream)).windows.length > 0);
    // A config without limits: the counted usage stays counted, and nothing is listed.
    await publishFixture(controlPlane, "minimal.json");
    assert.equal(await nextConfigHash(stream), configHash(readFixture("config/valid/minimal.json") as Config));
    assert.deepEqual((await nextTotals(stream)).windows, []);
  });

  test("gateways joining and leaving push the live count", async () => {
    let now = Date.UTC(2026, 8, 24, 10, 0);
    const controlPlane = newControlPlane({ clock: () => now });
    await publishFixture(controlPlane, "minimal.json");
    const app = await startApp(controlPlane, { totalsPushIntervalMs: 10 });
    const stream = await openStream(app);
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
    const stream = await openStream(app);
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
});

describe("slow readers", { timeout: 30_000 }, () => {
  // A config about 1 MiB large: a few of them fill the socket buffers of a client that
  // does not read.
  function largeConfig(n: number): Config {
    const config = readFixture("config/valid/minimal.json") as Config;
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
    closers.push(() => {
      socket.destroy();
    });
    await once(socket, "connect");
    const headers = Object.entries(headersFor("gw-1"))
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
    const { controlPlane, activeReaches } = countingStreams(core);
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
    const { controlPlane, activeReaches } = countingStreams(core);
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
