import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { afterEach, describe, test } from "node:test";

import type { ControlPlane } from "../control-plane/index.ts";
import { PROTOCOL_VERSION } from "../protocol/index.ts";
import { createMemoryStore } from "../storage/index.ts";
import type { ControlPlaneStore, StoreChangeListener } from "../storage/index.ts";
import {
  TEST_INSTANCE,
  closeAfterTest,
  closeOpened,
  configHash,
  configNumberOf,
  configNumbered,
  countingSubscriptions,
  failOnListenerError,
  gatewayHeaders,
  nextConfig,
  nextConfigOrEnd,
  openSseStream,
  openStream,
  restorable,
  startApp,
  testCore,
  usageBatch,
} from "../test-support/index.ts";
import type { SseItem, SseStream } from "../test-support/index.ts";

const GATEWAY_HEADERS = gatewayHeaders();
const NOW = Date.UTC(2026, 9, 7, 12, 30);

function newControlPlane(store: ControlPlaneStore = createMemoryStore()): ControlPlane {
  return testCore({ store, onListenerError: failOnListenerError });
}

// A core over store whose clock reads NOW.
function coreAtNow(store: ControlPlaneStore, options: Parameters<typeof testCore>[0] = {}): ControlPlane {
  return testCore({ store, clock: () => NOW, ...options });
}

async function publish(controlPlane: ControlPlane, ...numbers: number[]): Promise<void> {
  for (const n of numbers) {
    const result = await controlPlane.publishConfig(configNumbered(n));
    assert.ok(result.ok, "publish succeeded");
  }
}

afterEach(closeOpened);

// The next event or comment other than totals, which the stream also pushes on connect
// (status-totals.test.ts covers them).
async function nextWithoutTotals(stream: SseStream): Promise<SseItem> {
  for (;;) {
    const item = await stream.next();
    if (item.kind !== "event" || item.event !== "totals") return item;
  }
}

async function nextConfigs(stream: SseStream, count: number): Promise<number[]> {
  const numbers: number[] = [];
  for (let n = 0; n < count; n++) numbers.push(await nextConfig(stream));
  return numbers;
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
        assert.equal(response.headers.get("kaiak-protocol"), String(PROTOCOL_VERSION));
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
      assert.equal(response.headers.get("kaiak-protocol"), String(PROTOCOL_VERSION));
      if (method !== "HEAD") assert.equal(((await response.json()) as { error: string }).error, "not-found");
    });
  }

  test("an unknown path is behind the request checks too", async () => {
    const app = await startApp(newControlPlane());
    const response = await fetch(`${app.base}/v1/nothing-here`, { headers: { ...GATEWAY_HEADERS, authorization: "" } });
    assert.equal(response.status, 401);
    assert.equal(response.headers.get("kaiak-protocol"), String(PROTOCOL_VERSION));
  });

  test("a status body over 64 KiB answers 413 request-invalid with the protocol header", async () => {
    const app = await startApp(newControlPlane());
    const response = await fetch(`${app.base}/v1/status`, {
      method: "POST",
      headers: { ...GATEWAY_HEADERS, "content-type": "application/json" },
      body: JSON.stringify({ padding: "x".repeat(64 * 1024) }),
    });
    assert.equal(response.status, 413);
    assert.equal(response.headers.get("kaiak-protocol"), String(PROTOCOL_VERSION));
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
    const stream = await openStream((await startApp(controlPlane)).base);
    const headers = stream.response.headers;
    assert.equal(headers.get("content-type"), "text/event-stream; charset=utf-8");
    assert.equal(headers.get("cache-control"), "no-cache");
    assert.equal(headers.get("x-accel-buffering"), "no");
    assert.equal(headers.get("kaiak-protocol"), String(PROTOCOL_VERSION));
    assert.equal(headers.get("content-encoding"), null);
  });

  test("on connect it sends the current config, then the totals", async () => {
    const controlPlane = newControlPlane();
    await publish(controlPlane, 1, 2, 3);
    const stream = await openStream((await startApp(controlPlane)).base);
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
    closeAfterTest(() => stream.close());
    assert.equal(stream.response.status, 200);
    assert.deepEqual(await nextConfigs(stream, 1), [1]);
    assert.equal((await fetch(`${app.base}/v1/stream`, { headers: GATEWAY_HEADERS })).status, 404);
  });

  test("before anything is published the stream stays open, then gets the first config and totals", async () => {
    const controlPlane = newControlPlane();
    const stream = await openStream((await startApp(controlPlane, { heartbeatIntervalMs: 20 })).base);
    assert.deepEqual(await stream.next(), { kind: "comment", text: "heartbeat" });
    await publish(controlPlane, 1);
    assert.deepEqual(await nextConfigs(stream, 1), [1]);
    const totals = await stream.nextEvent();
    assert.ok(totals.kind === "event" && totals.event === "totals");
  });

  test("delivers every config published while connected", async () => {
    const controlPlane = newControlPlane();
    await publish(controlPlane, 1);
    const stream = await openStream((await startApp(controlPlane)).base);
    assert.deepEqual(await nextConfigs(stream, 1), [1]);
    await publish(controlPlane, 2);
    await publish(controlPlane, 3);
    assert.deepEqual(await nextConfigs(stream, 2), [2, 3]);
  });

  test("a store restored to an older config sends it like any other on a new stream", async () => {
    const controlPlane = newControlPlane();
    await publish(controlPlane, 1, 2);
    const app = await startApp(controlPlane);
    assert.deepEqual(await nextConfigs(await openStream(app.base), 1), [2]);
    // The current config is config 1 again: a new stream gets it, nothing refuses it.
    await publish(controlPlane, 1);
    assert.deepEqual(await nextConfigs(await openStream(app.base), 1), [1]);
  });

  test("an idle stream carries heartbeat comments", async () => {
    const controlPlane = newControlPlane();
    await publish(controlPlane, 1);
    const stream = await openStream((await startApp(controlPlane, { heartbeatIntervalMs: 20 })).base);
    assert.deepEqual(await nextConfigs(stream, 1), [1]);
    assert.deepEqual(await nextWithoutTotals(stream), { kind: "comment", text: "heartbeat" });
    assert.deepEqual(await nextWithoutTotals(stream), { kind: "comment", text: "heartbeat" });
  });

  test("a client going away removes its subscription", async () => {
    const core = newControlPlane();
    await publish(core, 1);
    const { controlPlane, activeReaches } = countingSubscriptions(core);
    const app = await startApp(controlPlane, { heartbeatIntervalMs: 20 });
    const stream = await openStream(app.base);
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
    const streams = await Promise.all(Array.from({ length: 50 }, () => openStream(app.base)));
    await activeReaches(50);
    for (const stream of streams) assert.deepEqual(await nextConfigs(stream, 1), [1]);
    await publish(core, 2);
    for (const stream of streams) assert.deepEqual(await nextConfigs(stream, 1), [2]);
    for (const stream of streams) stream.close();
    await activeReaches(0);
  });

  test("the same config reaching a stream twice during its connect read is sent once", async () => {
    const core = newControlPlane();
    await publish(core, 1);
    // Config 1 published again after the stream subscribed and before its read returns:
    // the reads issued for that publish go out first, the connect read's is not sent
    // after them, and the content is sent once.
    const controlPlane: ControlPlane = {
      ...core,
      async readConfig() {
        const read = await core.readConfig();
        await publish(core, 1);
        return read;
      },
    };
    const stream = await openStream((await startApp(controlPlane, { heartbeatIntervalMs: 20 })).base);
    assert.deepEqual(await nextConfigs(stream, 1), [1]);
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
      async readConfig() {
        const read = await core.readConfig();
        await readHeld;
        return read;
      },
    };
    const app = await startApp(controlPlane);
    const opened = openStream(app.base);
    await publish(core, 2);
    releaseRead();
    const stream = await opened;
    assert.deepEqual(await nextConfigs(stream, 1), [2]);
    await publish(core, 3);
    assert.deepEqual(await nextConfigs(stream, 1), [3]);
  });

  test("a store restored to an older config sends it to the open streams once its channel catches up", async () => {
    // Two memory stores as one store and its backup: the backup holds config 1 only.
    const backup = createMemoryStore();
    const { store, restore, listeners } = restorable(createMemoryStore(), backup);
    await publish(newControlPlane(backup), 1);
    const controlPlane = newControlPlane(store);
    await publish(controlPlane, 1, 2);
    const app = await startApp(controlPlane);
    const streams = [await openStream(app.base), await openStream(app.base)];
    for (const stream of streams) assert.deepEqual(await nextConfigs(stream, 1), [2]);

    // Restored: the store's channel comes back and announces a catch-up. Every stream
    // gets the restored config, and stays open.
    restore();
    for (const listener of listeners) listener({ type: "catch-up" });
    for (const stream of streams) assert.deepEqual(await nextConfigs(stream, 1), [1]);
    // The app publishes its own current config again: it reaches the streams.
    await publish(controlPlane, 2);
    for (const stream of streams) assert.deepEqual(await nextConfigs(stream, 1), [2]);
  });

  test("closing the app ends open streams", async () => {
    const core = newControlPlane();
    await publish(core, 1);
    const { controlPlane, activeReaches } = countingSubscriptions(core);
    const app = await startApp(controlPlane);
    const stream = await openStream(app.base);
    await activeReaches(1);
    await app.close();
    assert.deepEqual(await nextConfigs(stream, 1), [1]);
    assert.deepEqual(await nextConfigOrEnd(stream), { kind: "end" });
    await activeReaches(0);
  });

  // A stream ended while this core delivers configs writes nothing more — here the
  // plugin ends every stream when one read fails, and the next delivery runs in the
  // same turn, before the socket's close event.
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
    const reader = coreAtNow(flaky, { deliveryRetryDelaysMs: [] });
    const writer = coreAtNow(base);
    assert.ok((await writer.publishConfig(configNumbered(1))).ok);
    const errors: string[] = [];
    const app = await startApp(reader, {}, (raw) => {
      raw.on("error", (error: Error & { code?: string }) => errors.push(error.code ?? error.message));
    });
    const stream = await openStream(app.base);
    assert.equal(await nextConfig(stream), 1);

    // Two publishes at once: the first delivery read fails and ends the stream, the
    // second reads config 3 straight after.
    failNext = true;
    const results = await Promise.all([writer.publishConfig(configNumbered(2)), writer.publishConfig(configNumbered(3))]);
    assert.ok(results.every((result) => result.ok));
    let item = await stream.next();
    while (item.kind !== "end") item = await stream.next();
    await new Promise<void>((resolve) => setImmediate(resolve));
    assert.deepEqual(errors, [], "nothing was written after the end");
  });

  // After a restore the core heard nothing of, a stream that connected meanwhile runs
  // the restored config; the app publishing its current config again reaches it, since
  // each stream skips only what it sent itself.
  test("republishing after a restore reaches a stream that connected to the restored config", { timeout: 10_000 }, async () => {
    const backup = createMemoryStore();
    assert.ok((await coreAtNow(backup).publishConfig(configNumbered(1))).ok);
    const { store, restore } = restorable(createMemoryStore(), backup);
    const cp = coreAtNow(store);
    assert.ok((await cp.publishConfig(configNumbered(1))).ok);
    assert.ok((await cp.publishConfig(configNumbered(2))).ok);
    const app = await startApp(cp);
    const before = await openStream(app.base);
    assert.equal(await nextConfig(before), 2);

    restore();
    const after = await openStream(app.base);
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
    const reader = coreAtNow(holding);
    const writer = coreAtNow(base);
    assert.ok((await writer.publishConfig(configNumbered(1))).ok);
    const app = await startApp(reader);
    const early = await openStream(app.base);
    assert.equal(await nextConfig(early), 1);

    // The reader's delivery read for config 2 reads it, then waits; config 3 follows.
    holdNext = true;
    assert.ok((await writer.publishConfig(configNumbered(2))).ok);
    await entered.promise;
    assert.ok((await writer.publishConfig(configNumbered(3))).ok);
    const late = await openStream(app.base);
    assert.equal(await nextConfig(late), 3);
    release.resolve();

    assert.deepEqual([await nextConfig(early), await nextConfig(early)], [2, 3]);
    assert.ok((await writer.publishConfig(configNumbered(4))).ok);
    assert.equal(await nextConfig(late), 4, "config 2 never followed config 3");
  });

  // A catch-up re-reads the current config: a stream already running it gets nothing,
  // and stays open.
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
    const cp = coreAtNow(store);
    assert.ok((await cp.publishConfig(configNumbered(1))).ok);
    const batch = usageBatch({ instance: TEST_INSTANCE, epoch: "a".repeat(32), sequence: 1 }, [
      {
        record_id: "a1".padEnd(32, "0"),
        request_id: "req-a1",
        gateway_instance: TEST_INSTANCE,
        key_id: "key",
        groups: ["g"],
        model: "m",
        deployment: { backend: "b", model: "m" },
        units: { tokens_in: 100 },
        cost_nano_usd: 100,
        gateway_time: new Date(NOW).toISOString(),
      },
    ]);
    assert.ok((await cp.acceptUsageBatch(TEST_INSTANCE, batch)).ok);
    const app = await startApp(cp, { heartbeatIntervalMs: 20 });
    const stream = await openStream(app.base);
    assert.equal(await nextConfig(stream), 1);
    for (const listener of listeners) listener({ type: "catch-up" });
    assert.ok((await cp.publishConfig(configNumbered(2))).ok);
    assert.equal(await nextConfig(stream), 2, "the next config is the next published one");
  });

  // The stream sends the stored config text as it is, so config_hash is the hash of what
  // goes out whatever the text's spacing or member order.
  test("a config event carries the stored text as it is, its hash the hash of that text", { timeout: 10_000 }, async () => {
    const store = createMemoryStore();
    const config = configNumbered(1);
    const reordered = Object.fromEntries(Object.entries(config).reverse());
    const text = JSON.stringify(reordered, null, 1).replaceAll("\n", " ");
    const hash = createHash("sha256").update(text).digest("hex");
    assert.ok((await store.publishConfig({ text, hash, publishedAt: NOW }, undefined)).saved);
    const app = await startApp(coreAtNow(store));
    const stream = await openStream(app.base);
    const item = await stream.nextEvent();
    assert.ok(item.kind === "event" && item.event === "config");
    assert.equal(item.data, `{"config_hash":"${hash}","config":${text}}`);
  });
});
