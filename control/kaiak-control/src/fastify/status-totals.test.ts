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
  return { authorization: `Bearer ${TOKEN}`, "kaiak-protocol": "4", "kaiak-instance": instance };
}

const CONTROL_PLANE = "c".repeat(32);

function newControlPlane(options: Partial<ControlPlaneOptions> = {}): ControlPlane {
  return createControlPlane({
    store: createMemoryStore(),
    token: TOKEN,
    controlPlaneId: CONTROL_PLANE,
    onListenerError: (error) => assert.fail(`listener failed: ${String(error)}`),
    ...options,
  });
}

async function publishFixture(controlPlane: ControlPlane, file: string): Promise<void> {
  assert.ok((await controlPlane.publishConfig(readFixture(`config/valid/${file}`))).ok);
}

interface RunningApp {
  base: string;
  // The control plane's config epoch, which a stream names with its since.
  epoch: string;
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
  return { base: `http://127.0.0.1:${address.port}`, epoch: await controlPlane.configEpoch(), port: address.port, close };
}

async function openStream(app: RunningApp, since: number): Promise<SseStream> {
  const stream = await openSseStream(`${app.base}/v1/stream?since=${since}&config_epoch=${app.epoch}`, headersFor("gw-1"));
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
async function nextTotals(stream: SseStream): Promise<Totals> {
  const item = await stream.nextEvent();
  return totalsOf(item);
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
    assert.equal(response.headers.get("kaiak-protocol"), "4");
    assert.equal(await response.text(), "");
    assert.deepEqual(await controlPlane.gateways(), [
      { instance: "gw-1", status: { ...READY, instance: "gw-1" }, receivedAt: 1_000, live: true },
    ]);
    assert.equal(await controlPlane.liveGateways(), 1);
  });

  test("a status before any config is accepted", async () => {
    const controlPlane = newControlPlane();
    const app = await startApp(controlPlane);
    const starting = { ...READY, state: "starting", applied_config_version: null, applied_config_epoch: null };
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

  test("the plugin starts the core with the app — lease and expiry sweep — and stops it on close", async () => {
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
    const held = await store.acquireLease("someone-else", Date.now(), Date.now() + 1000);
    assert.ok(!held.ok && held.lease.holder === CONTROL_PLANE, "the core holds the store's lease");
    await app.close();
    assert.deepEqual({ started, stopped }, { started: 1, stopped: 1 });
    assert.deepEqual(await store.acquireLease("someone-else", Date.now(), Date.now() + 1000), { ok: true });
  });

  test("an app whose store another core holds fails to start, saying why", async () => {
    const store = createMemoryStore();
    await startApp(newControlPlane({ store, controlPlaneId: "1".repeat(32) }));
    const second = Fastify();
    await second.register(controlProtocolPlugin, { controlPlane: newControlPlane({ store, controlPlaneId: "2".repeat(32) }) });
    await assert.rejects(async () => second.ready(), {
      code: "store-lease-held",
      message: /another control-plane process holds this store \(holder 1{32}, .*run one control-plane process per store/,
    });
    await second.close();
  });
});

describe("totals on the stream", { timeout: 20_000 }, () => {
  test("a stream gets totals right after the replay", async () => {
    const controlPlane = newControlPlane();
    await publishFixture(controlPlane, "minimal.json");
    await publishFixture(controlPlane, "full.json");
    const stream = await openStream(await startApp(controlPlane), 1);
    const config = await stream.nextEvent();
    assert.ok(config.kind === "event" && config.event === "config" && config.id === "2");
    // Two publishes: the revision is at 2.
    assert.deepEqual(await nextTotals(stream), {
      revision: { control_plane: CONTROL_PLANE, sequence: 2 },
      config_epoch: await controlPlane.configEpoch(),
      config_version: 2,
      live_gateways: 0,
      counted_through: null,
      windows: [],
    });
  });

  test("with nothing published a stream gets resync and no totals", async () => {
    const stream = await openStream(await startApp(newControlPlane()), 0);
    const item = await stream.nextEvent();
    assert.ok(item.kind === "event" && item.event === "resync");
    assert.deepEqual(await stream.nextEvent(), { kind: "end" });
  });

  test("a counted batch pushes the totals it produced", async () => {
    const controlPlane = newControlPlane({ clock: () => Date.UTC(2026, 8, 24, 10, 30) });
    await publishFixture(controlPlane, "full.json");
    const app = await startApp(controlPlane, { totalsPushIntervalMs: 10 });
    const stream = await openStream(app, 1);
    assert.deepEqual((await nextTotals(stream)).windows, []);
    const response = await fetch(`${app.base}/v1/usage`, {
      method: "POST",
      headers: { ...headersFor("gw-1"), "content-type": "application/json" },
      body: JSON.stringify(readFixture("messages/usage-batch/valid/mixed-groups.json")),
    });
    assert.equal(response.status, 200);
    const pushed = await nextTotals(stream);
    assert.ok(pushed.windows.length > 0);
    assert.deepEqual(pushed, await controlPlane.totals("gw-1"));
    // The stream is gw-1's, and the batch was gw-1's: the push counts it.
    assert.deepEqual(pushed.counted_through, { epoch: "5d41402abc4b2a76b9719d911017c592", sequence: 18 });
  });

  test("a publish is followed by totals under the new version", async () => {
    const controlPlane = newControlPlane();
    await publishFixture(controlPlane, "minimal.json");
    const stream = await openStream(await startApp(controlPlane, { totalsPushIntervalMs: 10 }), 1);
    assert.equal((await nextTotals(stream)).config_version, 1);
    await publishFixture(controlPlane, "full.json");
    const config = await stream.nextEvent();
    assert.ok(config.kind === "event" && config.event === "config");
    assert.equal((await nextTotals(stream)).config_version, 2);
  });

  test("gateways joining and leaving push the live count", async () => {
    let now = Date.UTC(2026, 8, 24, 10, 0);
    const controlPlane = newControlPlane({ clock: () => now });
    await publishFixture(controlPlane, "minimal.json");
    const app = await startApp(controlPlane, { totalsPushIntervalMs: 10 });
    const stream = await openStream(app, 1);
    assert.equal((await nextTotals(stream)).live_gateways, 0);

    assert.equal((await postStatus(app, "gw-1")).status, 204);
    const joined = await nextTotals(stream);
    assert.equal(joined.live_gateways, 1);
    assert.equal(joined.revision.sequence, 2, "the join moved the revision on");

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
    const stream = await openStream(app, 1);
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
    model.defaults = { padding: "x".repeat(1024 * 1024) };
    return config;
  }

  const LARGE_CONFIGS = 12;

  async function publishLarge(controlPlane: ControlPlane): Promise<void> {
    for (let n = 0; n < LARGE_CONFIGS; n++) assert.ok((await controlPlane.publishConfig(largeConfig(n))).ok);
  }

  // A stream client on a plain socket, paused from the start. HTTP/1.0, so the body is
  // the event stream as written, with no chunk framing.
  async function openPausedStream(app: RunningApp, since: number) {
    const socket = net.connect(app.port, "127.0.0.1");
    socket.pause();
    closers.push(() => {
      socket.destroy();
    });
    await once(socket, "connect");
    const headers = Object.entries(headersFor("gw-1"))
      .map(([name, value]) => `${name}: ${value}\r\n`)
      .join("");
    socket.write(`GET /v1/stream?since=${since}&config_epoch=${app.epoch} HTTP/1.0\r\nhost: 127.0.0.1\r\n${headers}\r\n`);
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

  // The live-gateway counts of the totals events after the config event with this id.
  function liveCountsAfter(text: string, configId: number): number[] {
    const start = text.indexOf(`id: ${configId}\n`);
    if (start === -1) return [];
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
    await openPausedStream(app, 1);
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
    const client = await openPausedStream(app, 1);
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
