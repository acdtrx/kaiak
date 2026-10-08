// GET /events over a real listening app and a real core: every section on connect,
// a pushed section after each kind of change, bursts coalesced, listeners released on
// disconnect and streams ended on close.

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import path from "node:path";
import { performance } from "node:perf_hooks";
import { afterEach, test } from "node:test";

import Fastify from "fastify";
import type { FastifyInstance } from "fastify";
import { createControlPlane, createMemoryStore } from "kaiak-control";
import type { ControlPlane, GatewayStatus } from "kaiak-control";

import type { ConfigFileState } from "../config-file/index.ts";

import { registerStatusPage } from "./index.ts";
import type { StatusPage, StatusPageOptions } from "./index.ts";

const FIXTURES = path.resolve(import.meta.dirname, "../../../../protocol/fixtures");
const TOKEN = "page-test-token";

function readFixture(relative: string): unknown {
  return JSON.parse(readFileSync(path.join(FIXTURES, relative), "utf8"));
}

const READY = readFixture("messages/status/valid/ready.json") as GatewayStatus;

interface SectionEvent {
  id: string;
  html: string;
  at: number;
}

// Reads section events off one page stream.
interface PageStream {
  next(): Promise<SectionEvent>;
  // Reads until a section event with this id arrives; returns it and every section
  // event read on the way, it included.
  until(id: string, match?: (html: string) => boolean): Promise<SectionEvent[]>;
  // Resolves when the server ends the stream.
  ended(): Promise<void>;
  abort(): void;
}

interface Running {
  app: FastifyInstance;
  page: StatusPage;
  core: ControlPlane;
  fileState: ConfigFileState;
  // Subscriptions to the core the page holds right now.
  subscriptions(): number;
  whenSubscriptions(n: number): Promise<void>;
  open(): Promise<PageStream>;
  // The page streams' URL.
  url: string;
}

const cleanups: (() => Promise<void> | void)[] = [];

afterEach(async () => {
  for (const cleanup of cleanups.splice(0).reverse()) await cleanup();
});

async function start(pushIntervalMs = 50, maxStreams?: number): Promise<Running> {
  const core = createControlPlane({ store: createMemoryStore(), token: TOKEN, onListenerError: (error) => assert.fail(String(error)) });
  let subscriptions = 0;
  const waiters: { n: number; resolve: () => void }[] = [];
  const counted =
    <L>(subscribe: (listener: L) => () => void) =>
    (listener: L): (() => void) => {
      const stop = subscribe(listener);
      subscriptions += 1;
      let stopped = false;
      return () => {
        stop();
        if (stopped) return;
        stopped = true;
        subscriptions -= 1;
        for (const waiter of [...waiters]) {
          if (waiter.n !== subscriptions) continue;
          waiters.splice(waiters.indexOf(waiter), 1);
          waiter.resolve();
        }
      };
    };
  const controlPlane: StatusPageOptions["controlPlane"] = {
    currentConfig: core.currentConfig,
    gateways: core.gateways,
    readTotals: core.readTotals,
    recentRecords: core.recentRecords,
    onConfigPublished: counted(core.onConfigPublished),
    onTotalsChanged: counted(core.onTotalsChanged),
    onGatewaysChanged: counted(core.onGatewaysChanged),
  };
  const fileState: ConfigFileState = { path: "/tmp/config.json", lastRun: undefined, lastFailure: undefined };
  const app = Fastify();
  const page = registerStatusPage(app, { controlPlane, configFile: () => fileState, pushIntervalMs, ...(maxStreams === undefined ? {} : { maxStreams }) });
  await app.listen({ host: "127.0.0.1", port: 0 });
  cleanups.push(() => app.close());
  const address = app.server.address();
  assert.ok(address && typeof address === "object");
  const url = `http://127.0.0.1:${address.port}/events`;
  return {
    app,
    page,
    core,
    fileState,
    subscriptions: () => subscriptions,
    whenSubscriptions: (n) =>
      n === subscriptions ? Promise.resolve() : new Promise<void>((resolve) => waiters.push({ n, resolve })),
    open: () => openPageStream(url),
    url,
  };
}

async function openPageStream(url: string): Promise<PageStream> {
  const abort = new AbortController();
  cleanups.push(() => abort.abort());
  const response = await fetch(url, { signal: abort.signal });
  assert.equal(response.status, 200);
  assert.match(String(response.headers.get("content-type")), /^text\/event-stream/);
  assert.ok(response.body);
  const reader = response.body.pipeThrough(new TextDecoderStream()).getReader();
  let buffer = "";
  let done = false;
  const readEvent = async (): Promise<SectionEvent | undefined> => {
    for (;;) {
      const end = buffer.indexOf("\n\n");
      if (end !== -1) {
        const block = buffer.slice(0, end);
        buffer = buffer.slice(end + 2);
        if (block.startsWith(":")) continue;
        const lines = block.split("\n");
        assert.equal(lines[0], "event: section");
        const data = lines[1]?.startsWith("data: ") ? lines[1].slice(6) : "";
        const { id, html } = JSON.parse(data) as { id: string; html: string };
        return { id, html, at: performance.now() };
      }
      if (done) return undefined;
      const chunk = await reader.read();
      if (chunk.done) done = true;
      else buffer += chunk.value;
    }
  };
  const next = async (): Promise<SectionEvent> => {
    const event = await readEvent();
    assert.ok(event, "the stream stayed open");
    return event;
  };
  return {
    next,
    async until(id, match = () => true) {
      const seen: SectionEvent[] = [];
      for (;;) {
        const event = await next();
        seen.push(event);
        if (event.id === id && match(event.html)) return seen;
      }
    },
    async ended() {
      while ((await readEvent()) !== undefined) {
        // Events still buffered before the end are not what this waits for.
      }
    },
    abort: () => abort.abort(),
  };
}

// Page streams share the port with the gateways' config streams, so their
// number is capped; one past the cap is refused at once, and a slot frees when a
// stream ends.
test("page streams past the cap are refused with 503 until one ends", { timeout: 5000 }, async () => {
  const running = await start(50, 2);
  const first = await running.open();
  const second = await running.open();
  await readAllSections(first);
  await readAllSections(second);
  const refused = await fetch(running.url);
  assert.equal(refused.status, 503);
  assert.equal(refused.headers.get("retry-after"), "10");
  const body = (await refused.json()) as { error: string; detail?: string };
  assert.equal(body.error, "page-streams-full");
  assert.match(body.detail ?? "", /2 page streams/);
  first.abort();
  second.abort();
  await running.whenSubscriptions(0);
  const again = await running.open();
  assert.deepEqual([...(await readAllSections(again)).keys()], ["gateways", "config", "totals", "usage"]);
});

async function readAllSections(stream: PageStream): Promise<Map<string, string>> {
  const sections = new Map<string, string>();
  for (let i = 0; i < 4; i++) {
    const event = await stream.next();
    sections.set(event.id, event.html);
  }
  return sections;
}

test("a browser gets every section on connect, and again on reconnect", { timeout: 5000 }, async () => {
  const running = await start();
  assert.ok((await running.core.publishConfig(readFixture("config/valid/full.json"))).ok);
  const first = await running.open();
  const sections = await readAllSections(first);
  assert.deepEqual([...sections.keys()], ["gateways", "config", "totals", "usage"]);
  assert.match(sections.get("config") ?? "", /Config <span class="muted"><code>[0-9a-f]{12}<\/code><\/span>/);
  first.abort();

  const again = await running.open();
  assert.deepEqual([...(await readAllSections(again)).keys()], ["gateways", "config", "totals", "usage"]);
});

test("a status, a usage batch, a publish and a config file rejection each push their sections", { timeout: 5000 }, async () => {
  const running = await start();
  const stream = await running.open();
  await readAllSections(stream);

  assert.ok((await running.core.acceptStatus("gw-1", READY)).ok);
  await stream.until("gateways", (html) => html.includes("gw-1") && html.includes("1 live of 1") && html.includes("circuit open"));
  // The circuit closes and the queue empties: the next status is pushed as it is.
  const recovered: GatewayStatus = {
    ...READY,
    backends: { ...READY.backends, "vllm-b": { in_flight: 1, deployments: { "Qwen/Qwen3-32B": { circuit: "closed" }, "BAAI/bge-m3": { circuit: "closed" } } } },
    models: { "qwen3-32b": { queued: 0 }, "gpt-4.1": { queued: 0 }, "bge-m3": { queued: 0 } },
  };
  assert.ok((await running.core.acceptStatus("gw-1", recovered)).ok);
  const [pushed] = (await stream.until("gateways", (html) => !html.includes("circuit open"))).slice(-1);
  assert.ok(pushed);
  assert.match(pushed.html, /Queued: <span class="muted">none<\/span>/);
  assert.match(pushed.html, /<td class="id">vllm-b<\/td>\s*<td class="num">1 \/ —<\/td>/);

  assert.ok((await running.core.publishConfig(readFixture("config/valid/full.json"))).ok);
  await stream.until("config", (html) => html.includes("v1"));

  const batch = readFixture("messages/usage-batch/valid/mixed-groups.json");
  assert.ok((await running.core.acceptUsageBatch("gw-1", batch)).ok);
  await stream.until("usage", (html) => html.includes("k-alice-laptop") && html.includes("last 2"));

  running.fileState.lastFailure = {
    trigger: "file-changed",
    at: Date.now(),
    ok: false,
    error: { code: "json-invalid", message: "Unexpected token" },
  };
  running.page.configFileChanged();
  await stream.until("config", (html) => html.includes("rejected (json-invalid)"));
});

test("a burst of changes is pushed once per push interval, carrying the latest state", { timeout: 5000 }, async () => {
  const interval = 300;
  const running = await start(interval);
  const stream = await running.open();
  await readAllSections(stream);

  const status = (n: number): GatewayStatus => ({
    ...READY,
    backends: { "vllm-a": { in_flight: n, deployments: {} } },
  });
  assert.ok((await running.core.acceptStatus("gw-1", status(1))).ok);
  const leading = (await stream.until("gateways")).at(-1);
  assert.ok(leading);
  for (let n = 2; n <= 6; n++) assert.ok((await running.core.acceptStatus("gw-1", status(n))).ok);
  // A marker after the burst: whatever the burst pushes arrives before it.
  const trailing = await stream.until("gateways");
  running.page.configFileChanged();
  const rest = await stream.until("config");

  const gatewayPushes = [...trailing, ...rest].filter((event) => event.id === "gateways");
  assert.equal(gatewayPushes.length, 1, "the burst was pushed once");
  const [pushed] = gatewayPushes;
  assert.ok(pushed);
  assert.match(pushed.html, /<td class="id">vllm-a<\/td>\s*<td class="num">6 \/ —<\/td>/);
  assert.ok(pushed.at - leading.at >= interval - 20, `waited for the interval (${Math.round(pushed.at - leading.at)} ms)`);
});

test("the page subscribes to the core while a browser is connected, and lets go when the last leaves", { timeout: 5000 }, async () => {
  const running = await start();
  assert.equal(running.subscriptions(), 0);
  const first = await running.open();
  await readAllSections(first);
  const second = await running.open();
  await readAllSections(second);
  assert.equal(running.subscriptions(), 3, "one subscription per core event, shared by every browser");

  first.abort();
  second.abort();
  await running.whenSubscriptions(0);
  // Changes with nobody connected render nothing and schedule nothing.
  assert.ok((await running.core.acceptStatus("gw-1", READY)).ok);
  running.page.configFileChanged();
});

test("closing the app ends every page stream", { timeout: 5000 }, async () => {
  const running = await start();
  const stream = await running.open();
  await readAllSections(stream);
  await running.app.close();
  await stream.ended();
  assert.equal(running.subscriptions(), 0);
});
