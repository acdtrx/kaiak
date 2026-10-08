// The page feed's endings, driven without a server: the reply is a real ServerResponse
// with no socket, so it never emits `close` and an ended stream stays in the window
// between `end()` and `close` for as long as a test needs. Node answers a write there
// with ERR_STREAM_WRITE_AFTER_END as an `error` event, which without a listener is an
// uncaught exception: a test that wrote would fail on it.

import assert from "node:assert/strict";
import { IncomingMessage, ServerResponse } from "node:http";
import { Socket } from "node:net";
import { test } from "node:test";
import type { TestContext } from "node:test";
import { setImmediate as nextMacrotask } from "node:timers/promises";

import type { FastifyBaseLogger, FastifyReply } from "fastify";
import { createControlPlane, createMemoryStore } from "kaiak-control";

import { createPageFeed } from "./feed.ts";
import type { PageFeed } from "./feed.ts";
import type { PageSources } from "./sections.ts";

const HEARTBEAT_INTERVAL_MS = 15_000;

interface Fixture {
  feed: PageFeed;
  // Every source read waits for this to be released.
  hold(): void;
  release(): void;
  // A stream on a fresh response; `afterEnd` lists what was written to it once ended.
  connect(): { response: ServerResponse; afterEnd: string[] };
  warnings: string[];
}

// The heartbeat interval is mocked: a response with no socket never closes, and a real
// interval would hold the test process open.
function setUp(t: TestContext, maxStreams = 4): Fixture {
  t.mock.timers.enable({ apis: ["setInterval"] });
  const core = createControlPlane({ store: createMemoryStore(), token: "feed-test-token" });
  let held: { promise: Promise<void>; resolve: () => void } | undefined;
  const gate = async (): Promise<void> => {
    await held?.promise;
  };
  const sources: PageSources = {
    core: {
      currentConfig: async () => (await gate(), core.currentConfig()),
      gateways: async () => (await gate(), core.gateways()),
      readTotals: async () => (await gate(), core.readTotals()),
      recentRecords: async (...args) => (await gate(), core.recentRecords(...args)),
    },
    configFile: () => ({ path: "/tmp/config.json", lastRun: undefined, lastFailure: undefined }),
    clock: Date.now,
  };
  const warnings: string[] = [];
  const log = {
    warn: (_fields: unknown, message: string) => warnings.push(message),
    error: (fields: unknown, message: string) => assert.fail(`${message}: ${String(fields)}`),
  } as unknown as FastifyBaseLogger;
  const feed = createPageFeed({
    sources,
    core,
    pushIntervalMs: 50,
    heartbeatIntervalMs: HEARTBEAT_INTERVAL_MS,
    stalledStreamTimeoutMs: 30_000,
    maxStreams,
    log,
  });
  return {
    feed,
    hold() {
      let resolve = (): void => {};
      const promise = new Promise<void>((done) => (resolve = done));
      held = { promise, resolve };
    },
    release() {
      held?.resolve();
      held = undefined;
    },
    connect() {
      const response = new ServerResponse(new IncomingMessage(new Socket()));
      const afterEnd: string[] = [];
      const write = response.write.bind(response);
      response.write = ((chunk: string) => {
        if (response.writableEnded) afterEnd.push(chunk);
        return write(chunk);
      }) as typeof response.write;
      const reply = { hijack() {}, raw: response } as unknown as FastifyReply;
      assert.ok(feed.stream(reply));
      return { response, afterEnd };
    },
    warnings,
  };
}

// Every source read and render in the feed runs on promises alone, and Node emits a
// write-after-end error on the next tick: both are settled by the next macrotask.
const settled = (): Promise<void> => nextMacrotask();

test("ending the feed while a browser's connect render runs writes nothing after end", async (t) => {
  const fixture = setUp(t);
  fixture.hold();
  const { response, afterEnd } = fixture.connect();
  fixture.feed.endAll();
  assert.ok(response.writableEnded);
  fixture.release();
  await settled();
  assert.deepEqual(afterEnd, []);
});

test("ending the feed while a push renders writes nothing after end", async (t) => {
  const fixture = setUp(t);
  const { response, afterEnd } = fixture.connect();
  await settled();
  fixture.hold();
  fixture.feed.sectionsChanged(["config"]);
  fixture.feed.endAll();
  assert.ok(response.writableEnded);
  fixture.release();
  await settled();
  assert.deepEqual(afterEnd, []);
});

test("no heartbeat is written after the feed ended", async (t) => {
  const fixture = setUp(t);
  const { response, afterEnd } = fixture.connect();
  await settled();
  fixture.feed.endAll();
  assert.ok(response.writableEnded);
  t.mock.timers.tick(HEARTBEAT_INTERVAL_MS);
  await settled();
  assert.deepEqual(afterEnd, []);
});

test("a connection error ends the stream, never the process", async (t) => {
  const fixture = setUp(t, 1);
  const { response } = fixture.connect();
  await settled();
  response.emit("error", new Error("connection reset"));
  assert.deepEqual(fixture.warnings, ["page stream: the connection failed; ending the stream"]);
  // The stream's slot is free again.
  fixture.connect();
  fixture.feed.endAll();
  await settled();
});
