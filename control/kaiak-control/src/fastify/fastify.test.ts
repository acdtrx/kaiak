import assert from "node:assert/strict";
import { EventEmitter, once } from "node:events";
import { readFileSync } from "node:fs";
import path from "node:path";
import { afterEach, describe, test } from "node:test";

import Fastify from "fastify";

import type { Config } from "../config/index.ts";
import { createControlPlane } from "../control-plane/index.ts";
import type { ControlPlane } from "../control-plane/index.ts";
import { validateConfigSnapshot } from "../messages/index.ts";
import { createMemoryStore } from "../storage/index.ts";

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

function newControlPlane(configHistorySize = 100): ControlPlane {
  return createControlPlane({
    store: createMemoryStore(),
    token: TOKEN,
    configHistorySize,
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
  // The control plane's config epoch, which a stream names with its since.
  epoch: string;
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
    epoch: await controlPlane.configEpoch(),
    close: () => app.close(),
  };
  running.push(started);
  return started;
}

async function openStream(app: RunningApp, since: number, epoch = app.epoch): Promise<SseStream> {
  const stream = await openSseStream(`${app.base}/v1/stream?since=${since}&config_epoch=${epoch}`, GATEWAY_HEADERS);
  openStreams.push(stream);
  assert.equal(stream.response.status, 200);
  return stream;
}

// The version a config event carries, after checking the event is a valid snapshot
// whose id is that version.
function configVersionOf(item: SseItem): number {
  assert.equal(item.kind, "event");
  assert.ok(item.kind === "event");
  assert.equal(item.event, "config");
  const validation = validateConfigSnapshot(JSON.parse(item.data));
  assert.ok(validation.ok, "the data is a valid config snapshot");
  assert.equal(item.id, String(validation.message.version));
  return validation.message.version;
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

async function nextVersions(stream: SseStream, count: number): Promise<number[]> {
  const versions: number[] = [];
  for (let n = 0; n < count; n++) versions.push(configVersionOf(await nextConfigOrEnd(stream)));
  return versions;
}

async function assertResyncThenEnd(stream: SseStream): Promise<void> {
  const item = await stream.nextEvent();
  assert.deepEqual(item.kind === "event" && { event: item.event, data: JSON.parse(item.data) }, {
    event: "resync",
    data: {},
  });
  assert.deepEqual(await stream.nextEvent(), { kind: "end" });
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

describe("GET /v1/config", { timeout: 10_000 }, () => {
  test("answers the current version's snapshot, with the protocol header", async () => {
    const controlPlane = newControlPlane();
    await publish(controlPlane, 1, 2);
    const app = await startApp(controlPlane);

    const response = await fetch(`${app.base}/v1/config`, { headers: GATEWAY_HEADERS });
    assert.equal(response.status, 200);
    assert.equal(response.headers.get("kaiak-protocol"), "5");
    const body: unknown = await response.json();
    assert.deepEqual(body, { config_epoch: app.epoch, version: 2, config: configNumbered(2) });
    assert.ok(validateConfigSnapshot(body).ok);
  });

  test("answers 503 config-unavailable before anything is published", async () => {
    const app = await startApp(newControlPlane());
    const response = await fetch(`${app.base}/v1/config`, { headers: GATEWAY_HEADERS });
    assert.equal(response.status, 503);
    assert.equal(response.headers.get("kaiak-protocol"), "5");
    const body = (await response.json()) as { error: string; detail: string };
    assert.equal(body.error, "config-unavailable");
    assert.equal(typeof body.detail, "string");
  });

  test("mounts under a prefix the host chooses", async () => {
    const controlPlane = newControlPlane();
    await publish(controlPlane, 1);
    const app = await startApp(controlPlane, { prefix: "/control/v1" });
    const response = await fetch(`${app.base}/control/v1/config`, { headers: GATEWAY_HEADERS });
    assert.equal(response.status, 200);
    assert.equal((await fetch(`${app.base}/v1/config`, { headers: GATEWAY_HEADERS })).status, 404);
  });
});

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

  for (const endpoint of ["/v1/config", "/v1/stream?since=1"]) {
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

  const epoch = "0123456789abcdef0123456789abcdef";
  for (const since of [
    "",
    "since=",
    "since=abc",
    "since=-1",
    "since=1.5",
    "since=01",
    "since=1&since=2",
    "since=1",
    `config_epoch=${epoch}`,
    "since=1&config_epoch=",
    "since=1&config_epoch=0123456789ABCDEF0123456789ABCDEF",
    `since=1&config_epoch=${epoch}&config_epoch=${epoch}`,
  ]) {
    test(`/v1/stream?${since} answers 400 since-invalid`, async () => {
      const controlPlane = newControlPlane();
      await publish(controlPlane, 1);
      const app = await startApp(controlPlane);
      const response = await fetch(`${app.base}/v1/stream?${since}`, { headers: GATEWAY_HEADERS });
      assert.equal(response.status, 400);
      assert.equal(response.headers.get("kaiak-protocol"), "5");
      const body = (await response.json()) as { error: string; detail: unknown };
      assert.equal(body.error, "since-invalid");
      assert.equal(typeof body.detail, "string");
    });
  }
});

describe("answers outside the routes", { timeout: 10_000 }, () => {
  const cases = [
    { name: "an unknown path", method: "GET", path: "/v1/nothing-here" },
    { name: "a method the protocol does not define", method: "DELETE", path: "/v1/config" },
    { name: "HEAD on the stream", method: "HEAD", path: "/v1/stream?since=1" },
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
      const response = await fetch(`${app.base}/v1/config`, { headers });
      assert.equal(response.status, 400);
      assert.equal(((await response.json()) as { error: string }).error, error);
    }
  });
});

describe("GET /v1/stream", { timeout: 10_000 }, () => {
  test("is an uncached event stream carrying the protocol header", async () => {
    const controlPlane = newControlPlane();
    await publish(controlPlane, 1);
    const stream = await openStream(await startApp(controlPlane), 1);
    const headers = stream.response.headers;
    assert.equal(headers.get("content-type"), "text/event-stream; charset=utf-8");
    assert.equal(headers.get("cache-control"), "no-cache");
    assert.equal(headers.get("x-accel-buffering"), "no");
    assert.equal(headers.get("kaiak-protocol"), "5");
    assert.equal(headers.get("content-encoding"), null);
  });

  test("delivers a config published while connected", async () => {
    const controlPlane = newControlPlane();
    await publish(controlPlane, 1);
    const stream = await openStream(await startApp(controlPlane), 1);
    await publish(controlPlane, 2);
    const item = await nextConfigOrEnd(stream);
    assert.equal(configVersionOf(item), 2);
    assert.ok(item.kind === "event");
    assert.deepEqual(JSON.parse(item.data), { config_epoch: await controlPlane.configEpoch(), version: 2, config: configNumbered(2) });
  });

  test("resuming replays exactly the newer versions, oldest first, then goes live", async () => {
    const controlPlane = newControlPlane();
    await publish(controlPlane, 1, 2, 3, 4);
    const stream = await openStream(await startApp(controlPlane), 2);
    assert.deepEqual(await nextVersions(stream, 2), [3, 4]);
    await publish(controlPlane, 5);
    assert.deepEqual(await nextVersions(stream, 1), [5]);
  });

  test("since the current version replays nothing, then goes live", async () => {
    const controlPlane = newControlPlane();
    await publish(controlPlane, 1, 2);
    const stream = await openStream(await startApp(controlPlane), 2);
    await publish(controlPlane, 3);
    assert.deepEqual(await nextVersions(stream, 1), [3]);
  });

  test("since from another epoch sends resync, even at the current version number", async () => {
    // A gateway that followed another store (or this one before it started over) up
    // to version 2: the number matches, the config does not.
    const controlPlane = newControlPlane();
    await publish(controlPlane, 1, 2);
    const app = await startApp(controlPlane);
    await assertResyncThenEnd(await openStream(app, 2, "0123456789abcdef0123456789abcdef"));
  });

  test("since older than the history sends resync and ends the stream", async () => {
    const controlPlane = newControlPlane(2);
    await publish(controlPlane, 1, 2, 3, 4);
    await assertResyncThenEnd(await openStream(await startApp(controlPlane), 1));
  });

  test("since ahead of the current version sends resync and ends the stream", async () => {
    const controlPlane = newControlPlane();
    await publish(controlPlane, 1, 2);
    await assertResyncThenEnd(await openStream(await startApp(controlPlane), 9));
  });

  test("before anything is published, any since gets resync", async () => {
    await assertResyncThenEnd(await openStream(await startApp(newControlPlane()), 0));
  });

  test("an idle stream carries heartbeat comments", async () => {
    const controlPlane = newControlPlane();
    await publish(controlPlane, 1);
    const stream = await openStream(await startApp(controlPlane, { heartbeatIntervalMs: 20 }), 1);
    assert.deepEqual(await nextWithoutTotals(stream), { kind: "comment", text: "heartbeat" });
    assert.deepEqual(await nextWithoutTotals(stream), { kind: "comment", text: "heartbeat" });
  });

  test("a client going away removes its subscription", async () => {
    const core = newControlPlane();
    await publish(core, 1);
    const { controlPlane, activeReaches } = countingSubscriptions(core);
    const app = await startApp(controlPlane, { heartbeatIntervalMs: 20 });
    const stream = await openStream(app, 1);
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
    const streams = await Promise.all(Array.from({ length: 50 }, () => openStream(app, 1)));
    await activeReaches(50);
    await publish(core, 2);
    for (const stream of streams) assert.deepEqual(await nextVersions(stream, 1), [2]);
    for (const stream of streams) stream.close();
    await activeReaches(0);
  });

  test("a version published while the replay is read is delivered exactly once", async () => {
    const core = newControlPlane();
    await publish(core, 1);
    // The publish lands after the stream subscribed and before it reads the replay, so
    // the version reaches it twice: from the subscription and in the replay.
    const controlPlane: ControlPlane = {
      ...core,
      async configsSince(version) {
        await publish(core, 2);
        return core.configsSince(version);
      },
    };
    const stream = await openStream(await startApp(controlPlane), 1);
    assert.deepEqual(await nextVersions(stream, 1), [2]);
    await publish(core, 3);
    assert.deepEqual(await nextVersions(stream, 1), [3]);
  });

  test("a version published after the replay is read, before it is written, is delivered once, in order", async () => {
    const core = newControlPlane();
    await publish(core, 1, 2);
    const controlPlane: ControlPlane = {
      ...core,
      async configsSince(version) {
        const answer = await core.configsSince(version);
        await publish(core, 3);
        return answer;
      },
    };
    const stream = await openStream(await startApp(controlPlane), 1);
    assert.deepEqual(await nextVersions(stream, 2), [2, 3]);
    await publish(core, 4);
    assert.deepEqual(await nextVersions(stream, 1), [4]);
  });

  test("closing the app ends open streams", async () => {
    const core = newControlPlane();
    await publish(core, 1);
    const { controlPlane, activeReaches } = countingSubscriptions(core);
    const app = await startApp(controlPlane);
    const stream = await openStream(app, 1);
    await activeReaches(1);
    await app.close();
    assert.deepEqual(await nextConfigOrEnd(stream), { kind: "end" });
    await activeReaches(0);
  });
});
