import assert from "node:assert/strict";
import { EventEmitter, once } from "node:events";
import { readFileSync } from "node:fs";
import path from "node:path";
import { afterEach, describe, test } from "node:test";

import Fastify from "fastify";

import type { Config } from "../config/index.ts";
import { createControlPlane } from "../control-plane/index.ts";
import type { ControlPlane } from "../control-plane/index.ts";
import { configHash } from "../config-publishing/index.ts";
import { validateConfigEvent } from "../messages/index.ts";
import { createMemoryStore } from "../storage/index.ts";
import type { ControlPlaneStore } from "../storage/index.ts";

import { controlProtocolPlugin } from "./index.ts";
import type { ControlProtocolPluginOptions } from "./index.ts";
import { openSseStream } from "./sse-client.ts";
import type { SseItem, SseStream } from "./sse-client.ts";

const TOKEN = "test-token";
const MINIMAL = path.resolve(import.meta.dirname, "../../../../protocol/fixtures/config/valid/minimal.json");

const GATEWAY_HEADERS = {
  authorization: `Bearer ${TOKEN}`,
  "kaiak-protocol": "5",
  "kaiak-instance": "gw-1",
};

// A valid config distinguishable by its context length.
function configNumbered(n: number): Config {
  const config = JSON.parse(readFileSync(MINIMAL, "utf8")) as Config;
  const model = config.models["llama"];
  assert.ok(model, "the minimal fixture has model llama");
  model.metadata.context_length = 1000 + n;
  return config;
}

function newControlPlane(store: ControlPlaneStore = createMemoryStore()): ControlPlane {
  return createControlPlane({
    store,
    token: TOKEN,
    onListenerError: (error) => assert.fail(`listener failed: ${String(error)}`),
  });
}

async function publish(controlPlane: ControlPlane, ...numbers: number[]): Promise<void> {
  for (const n of numbers) {
    const result = await controlPlane.publishConfig(configNumbered(n));
    assert.ok(result.ok, "publish succeeded");
  }
}

interface RunningApp {
  base: string;
  close(): Promise<void>;
}

const running: RunningApp[] = [];
const openStreams: SseStream[] = [];

afterEach(async () => {
  for (const stream of openStreams.splice(0)) stream.close();
  for (const app of running.splice(0)) await app.close();
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
  const started = {
    base: `http://127.0.0.1:${address.port}`,
    close: () => app.close(),
  };
  running.push(started);
  return started;
}

async function openStream(app: RunningApp): Promise<SseStream> {
  const stream = await openSseStream(`${app.base}/v1/stream`, GATEWAY_HEADERS);
  openStreams.push(stream);
  assert.equal(stream.response.status, 200);
  return stream;
}

// Which configNumbered a config event carries, after checking the event is a valid
// config event whose hash is its config's.
function configNumberOf(item: SseItem): number {
  assert.equal(item.kind, "event");
  assert.ok(item.kind === "event");
  assert.equal(item.event, "config");
  assert.equal(item.id, undefined, "config events carry no id: there is nothing to resume from");
  const validation = validateConfigEvent(JSON.parse(item.data));
  assert.ok(validation.ok, "the data is a valid config event");
  const { config, config_hash } = validation.message;
  assert.equal(config_hash, configHash(config));
  return (config.models["llama"]?.metadata.context_length ?? 0) - 1000;
}

// The next event or comment other than totals, which the stream also pushes on connect
// and after each publish (status-totals.test.ts covers them).
async function nextWithoutTotals(stream: SseStream): Promise<SseItem> {
  for (;;) {
    const item = await stream.next();
    if (item.kind !== "event" || item.event !== "totals") return item;
  }
}

async function nextConfigOrEnd(stream: SseStream): Promise<SseItem> {
  for (;;) {
    const item = await nextWithoutTotals(stream);
    if (item.kind !== "comment") return item;
  }
}

async function nextConfigs(stream: SseStream, count: number): Promise<number[]> {
  const numbers: number[] = [];
  for (let n = 0; n < count; n++) numbers.push(configNumberOf(await nextConfigOrEnd(stream)));
  return numbers;
}

// Wraps a core so a test sees how many config subscriptions are active.
function countingSubscriptions(controlPlane: ControlPlane) {
  let active = 0;
  const changes = new EventEmitter();
  const counted: ControlPlane = {
    ...controlPlane,
    onConfigPublished(listener) {
      const unsubscribe = controlPlane.onConfigPublished(listener);
      active++;
      changes.emit("change");
      let subscribed = true;
      return () => {
        unsubscribe();
        if (!subscribed) return;
        subscribed = false;
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

describe("request checks", { timeout: 10_000 }, () => {
  const failures = [
    { name: "no token", headers: { ...GATEWAY_HEADERS, authorization: "" }, status: 401, error: "unauthorized" },
    {
      name: "a wrong token",
      headers: { ...GATEWAY_HEADERS, authorization: "Bearer not-the-token" },
      status: 401,
      error: "unauthorized",
    },
    {
      name: "another protocol version",
      headers: { ...GATEWAY_HEADERS, "kaiak-protocol": "3" },
      status: 400,
      error: "protocol-version-mismatch",
    },
    {
      name: "an invalid instance",
      headers: { ...GATEWAY_HEADERS, "kaiak-instance": "-bad" },
      status: 400,
      error: "instance-invalid",
    },
  ];

  for (const endpoint of ["/v1/stream", "/v1/status"]) {
    for (const failure of failures) {
      test(`${endpoint} with ${failure.name} answers ${failure.status} ${failure.error}`, async () => {
        const controlPlane = newControlPlane();
        await publish(controlPlane, 1);
        const app = await startApp(controlPlane);
        const response = await fetch(`${app.base}${endpoint}`, { headers: failure.headers });
        assert.equal(response.status, failure.status);
        assert.equal(response.headers.get("kaiak-protocol"), "5");
        const text = await response.text();
        assert.ok(!text.includes("not-the-token"), "the presented token is not echoed");
        const body = JSON.parse(text) as { error: string; detail: unknown };
        assert.equal(body.error, failure.error);
        assert.equal(typeof body.detail, "string");
      });
    }
  }
});

describe("answers outside the routes", { timeout: 10_000 }, () => {
  const cases = [
    { name: "an unknown path", method: "GET", path: "/v1/nothing-here" },
    { name: "a method the protocol does not define", method: "DELETE", path: "/v1/stream" },
    { name: "HEAD on the stream", method: "HEAD", path: "/v1/stream" },
    { name: "GET /v1/config: the stream is the one way to get the config", method: "GET", path: "/v1/config" },
  ];
  for (const { name, method, path: requestPath } of cases) {
    test(`${name} answers 404 not-found with the protocol header`, async () => {
      const app = await startApp(newControlPlane());
      const response = await fetch(`${app.base}${requestPath}`, { method, headers: GATEWAY_HEADERS });
      assert.equal(response.status, 404);
      assert.equal(response.headers.get("kaiak-protocol"), "5");
      if (method !== "HEAD") assert.equal(((await response.json()) as { error: string }).error, "not-found");
    });
  }

  test("an unknown path is behind the request checks too", async () => {
    const app = await startApp(newControlPlane());
    const response = await fetch(`${app.base}/v1/nothing-here`, { headers: { ...GATEWAY_HEADERS, authorization: "" } });
    assert.equal(response.status, 401);
    assert.equal(response.headers.get("kaiak-protocol"), "5");
  });

  test("a status body over 64 KiB answers 413 request-invalid with the protocol header", async () => {
    const app = await startApp(newControlPlane());
    const response = await fetch(`${app.base}/v1/status`, {
      method: "POST",
      headers: { ...GATEWAY_HEADERS, "content-type": "application/json" },
      body: JSON.stringify({ padding: "x".repeat(64 * 1024) }),
    });
    assert.equal(response.status, 413);
    assert.equal(response.headers.get("kaiak-protocol"), "5");
    assert.equal(((await response.json()) as { error: string }).error, "request-invalid");
  });

  test("repeated protocol and instance headers arrive joined and are refused", async () => {
    const app = await startApp(newControlPlane());
    for (const [name, error] of [
      ["kaiak-protocol", "protocol-version-mismatch"],
      ["kaiak-instance", "instance-invalid"],
    ] as const) {
      const headers = new Headers(GATEWAY_HEADERS);
      headers.append(name, GATEWAY_HEADERS[name]);
      const response = await fetch(`${app.base}/v1/stream`, { headers });
      assert.equal(response.status, 400);
      assert.equal(((await response.json()) as { error: string }).error, error);
    }
  });
});

describe("GET /v1/stream", { timeout: 10_000 }, () => {
  test("is an uncached event stream carrying the protocol header", async () => {
    const controlPlane = newControlPlane();
    await publish(controlPlane, 1);
    const stream = await openStream(await startApp(controlPlane));
    const headers = stream.response.headers;
    assert.equal(headers.get("content-type"), "text/event-stream; charset=utf-8");
    assert.equal(headers.get("cache-control"), "no-cache");
    assert.equal(headers.get("x-accel-buffering"), "no");
    assert.equal(headers.get("kaiak-protocol"), "5");
    assert.equal(headers.get("content-encoding"), null);
  });

  test("on connect it sends the current config, then the totals", async () => {
    const controlPlane = newControlPlane();
    await publish(controlPlane, 1, 2, 3);
    const stream = await openStream(await startApp(controlPlane));
    const first = await stream.nextEvent();
    assert.equal(configNumberOf(first), 3);
    assert.ok(first.kind === "event");
    assert.deepEqual(JSON.parse(first.data), { config_hash: configHash(configNumbered(3)), config: configNumbered(3) });
    const second = await stream.nextEvent();
    assert.ok(second.kind === "event" && second.event === "totals");
  });

  test("mounts under a prefix the host chooses", async () => {
    const controlPlane = newControlPlane();
    await publish(controlPlane, 1);
    const app = await startApp(controlPlane, { prefix: "/control/v1" });
    const stream = await openSseStream(`${app.base}/control/v1/stream`, GATEWAY_HEADERS);
    openStreams.push(stream);
    assert.equal(stream.response.status, 200);
    assert.deepEqual(await nextConfigs(stream, 1), [1]);
    assert.equal((await fetch(`${app.base}/v1/stream`, { headers: GATEWAY_HEADERS })).status, 404);
  });

  test("before anything is published the stream stays open, then gets the first config and totals", async () => {
    const controlPlane = newControlPlane();
    const stream = await openStream(await startApp(controlPlane, { heartbeatIntervalMs: 20 }));
    assert.deepEqual(await stream.next(), { kind: "comment", text: "heartbeat" });
    await publish(controlPlane, 1);
    assert.deepEqual(await nextConfigs(stream, 1), [1]);
    const totals = await stream.nextEvent();
    assert.ok(totals.kind === "event" && totals.event === "totals");
  });

  test("delivers every config published while connected", async () => {
    const controlPlane = newControlPlane();
    await publish(controlPlane, 1);
    const stream = await openStream(await startApp(controlPlane));
    assert.deepEqual(await nextConfigs(stream, 1), [1]);
    await publish(controlPlane, 2);
    await publish(controlPlane, 3);
    assert.deepEqual(await nextConfigs(stream, 2), [2, 3]);
  });

  test("a store restored to an older config sends it like any other on a new stream", async () => {
    const controlPlane = newControlPlane();
    await publish(controlPlane, 1, 2);
    const app = await startApp(controlPlane);
    assert.deepEqual(await nextConfigs(await openStream(app), 1), [2]);
    // The current config is config 1 again: a new stream gets it, nothing refuses it.
    await publish(controlPlane, 1);
    assert.deepEqual(await nextConfigs(await openStream(app), 1), [1]);
  });

  test("an idle stream carries heartbeat comments", async () => {
    const controlPlane = newControlPlane();
    await publish(controlPlane, 1);
    const stream = await openStream(await startApp(controlPlane, { heartbeatIntervalMs: 20 }));
    assert.deepEqual(await nextConfigs(stream, 1), [1]);
    assert.deepEqual(await nextWithoutTotals(stream), { kind: "comment", text: "heartbeat" });
    assert.deepEqual(await nextWithoutTotals(stream), { kind: "comment", text: "heartbeat" });
  });

  test("a client going away removes its subscription", async () => {
    const core = newControlPlane();
    await publish(core, 1);
    const { controlPlane, activeReaches } = countingSubscriptions(core);
    const app = await startApp(controlPlane, { heartbeatIntervalMs: 20 });
    const stream = await openStream(app);
    await activeReaches(1);
    stream.close();
    await activeReaches(0);
    // Publishing afterwards reaches nobody and fails nothing.
    await publish(core, 2);
  });

  test("many concurrent streams all receive a publish", async () => {
    const core = newControlPlane();
    await publish(core, 1);
    const { controlPlane, activeReaches } = countingSubscriptions(core);
    const app = await startApp(controlPlane);
    const streams = await Promise.all(Array.from({ length: 50 }, () => openStream(app)));
    await activeReaches(50);
    for (const stream of streams) assert.deepEqual(await nextConfigs(stream, 1), [1]);
    await publish(core, 2);
    for (const stream of streams) assert.deepEqual(await nextConfigs(stream, 1), [2]);
    for (const stream of streams) stream.close();
    await activeReaches(0);
  });

  test("a config published while the current config is read is sent once, after it", async () => {
    const core = newControlPlane();
    await publish(core, 1);
    // The publish lands after the stream subscribed and before its read returns: the
    // config reaches it both ways, and is sent once.
    const controlPlane: ControlPlane = {
      ...core,
      async currentConfig() {
        const read = await core.currentConfig();
        await publish(core, 2);
        return read;
      },
    };
    const stream = await openStream(await startApp(controlPlane));
    assert.deepEqual(await nextConfigs(stream, 2), [1, 2]);
    await publish(core, 3);
    assert.deepEqual(await nextConfigs(stream, 1), [3]);
  });

  test("a connect read overtaken by a newer config is not sent after it", async () => {
    const core = newControlPlane();
    await publish(core, 1);
    // The connect read sees config 1, but the newer config 2 reaches the stream first:
    // sending config 1 after it would put the gateway back on a config no longer current.
    let releaseRead = (): void => {};
    const readHeld = new Promise<void>((resolve) => (releaseRead = resolve));
    const controlPlane: ControlPlane = {
      ...core,
      async currentConfig() {
        const read = await core.currentConfig();
        await readHeld;
        return read;
      },
    };
    const app = await startApp(controlPlane);
    const opened = openStream(app);
    await publish(core, 2);
    releaseRead();
    const stream = await opened;
    assert.deepEqual(await nextConfigs(stream, 1), [2]);
    await publish(core, 3);
    assert.deepEqual(await nextConfigs(stream, 1), [3]);
  });

  test("a store going back ends every stream; the gateways reconnect to its current state", async () => {
    const inner = createMemoryStore();
    let back = 0;
    const store: ControlPlaneStore = {
      ...inner,
      async currentConfig() {
        const current = await inner.currentConfig();
        return current && { ...current, sequence: current.sequence - back };
      },
      async totalsSnapshot(current, instance) {
        const snapshot = await inner.totalsSnapshot(current, instance);
        return { ...snapshot, sequence: snapshot.sequence - back };
      },
      subscribe(listener) {
        return inner.subscribe((change) => listener({ ...change, sequence: change.sequence - back }));
      },
    };
    const controlPlane = newControlPlane(store);
    await publish(controlPlane, 1, 2, 3);
    const app = await startApp(controlPlane);
    const streams = [await openStream(app), await openStream(app)];
    for (const stream of streams) assert.deepEqual(await nextConfigs(stream, 1), [3]);

    // Restored to a copy two writes behind: the next write shows it.
    back = 2;
    await publish(controlPlane, 1);
    for (const stream of streams) {
      let item = await nextWithoutTotals(stream);
      // A config event may still go out before the rollback is seen.
      while (item.kind !== "end") item = await nextWithoutTotals(stream);
    }
    assert.deepEqual(await nextConfigs(await openStream(app), 1), [1]);
  });

  test("closing the app ends open streams", async () => {
    const core = newControlPlane();
    await publish(core, 1);
    const { controlPlane, activeReaches } = countingSubscriptions(core);
    const app = await startApp(controlPlane);
    const stream = await openStream(app);
    await activeReaches(1);
    await app.close();
    assert.deepEqual(await nextConfigs(stream, 1), [1]);
    assert.deepEqual(await nextConfigOrEnd(stream), { kind: "end" });
    await activeReaches(0);
  });
});
