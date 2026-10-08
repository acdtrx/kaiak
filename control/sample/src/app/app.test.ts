import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { Writable } from "node:stream";
import { afterEach, test } from "node:test";

import { INSTANCE_HEADER, PROTOCOL_HEADER, PROTOCOL_VERSION } from "kaiak-control";

import { createSampleApp } from "./index.ts";
import type { SampleApp } from "./index.ts";

const MINIMAL = path.resolve(import.meta.dirname, "../../../../protocol/fixtures/config/valid/minimal.json");
const USAGE_BATCH = path.resolve(import.meta.dirname, "../../../../protocol/fixtures/messages/usage-batch/valid/one-record.json");
const TOKEN = "sample-token";
const GATEWAY_HEADERS = {
  authorization: `Bearer ${TOKEN}`,
  [PROTOCOL_HEADER]: String(PROTOCOL_VERSION),
  [INSTANCE_HEADER]: "gw-1",
};

// A valid config document distinguishable by its context length.
function configText(n: number): string {
  const config = JSON.parse(readFileSync(MINIMAL, "utf8")) as { models: { llama: { metadata: { context_length: number } } } };
  config.models.llama.metadata.context_length = 1000 + n;
  return JSON.stringify(config, null, 2);
}

type LogLine = Record<string, unknown>;

interface Fixture extends SampleApp {
  file: string;
  logs: LogLine[];
  // Resolves with the first line, logged from now on, that matches.
  nextLog(match: (line: LogLine) => boolean): Promise<LogLine>;
}

// The config_hash configText(n) is published under.
function hashOf(n: number): string {
  return createHash("sha256").update(JSON.stringify(JSON.parse(configText(n)))).digest("hex");
}

// Reads a gateway stream until its first config event and returns that event's data.
async function firstConfigEvent(url: string, cleanups: (() => void)[]): Promise<{ config_hash: string; config: { models: { llama: { metadata: { context_length: number } } } } }> {
  const abort = new AbortController();
  cleanups.push(() => abort.abort());
  const response = await fetch(url, { headers: GATEWAY_HEADERS, signal: abort.signal });
  assert.equal(response.status, 200);
  assert.equal(response.headers.get("kaiak-protocol"), String(PROTOCOL_VERSION));
  assert.ok(response.body);
  const reader = response.body.pipeThrough(new TextDecoderStream()).getReader();
  let text = "";
  for (;;) {
    const match = /event: config\ndata: (.*)\n\n/.exec(text);
    if (match) return JSON.parse(match[1] ?? "");
    const chunk = await reader.read();
    assert.ok(!chunk.done, "the stream stayed open");
    text += chunk.value;
  }
}

const cleanups: (() => Promise<void> | void)[] = [];

afterEach(async () => {
  for (const cleanup of cleanups.splice(0).reverse()) await cleanup();
});

// Quiet time after a change before the app reloads its config file.
const CONFIG_DEBOUNCE_MS = 20;

function setUp(initial: string, protocolReplicas = 0): Fixture {
  const dir = mkdtempSync(path.join(tmpdir(), "kaiak-sample-app-"));
  cleanups.push(() => rmSync(dir, { recursive: true, force: true }));
  const file = path.join(dir, "config.json");
  writeFileSync(file, initial);
  const logs: LogLine[] = [];
  const waiters: { match: (line: LogLine) => boolean; resolve: (line: LogLine) => void }[] = [];
  const stream = new Writable({
    write(chunk: Buffer, _encoding, done) {
      for (const text of chunk.toString("utf8").split("\n")) {
        if (text === "") continue;
        const line = JSON.parse(text) as LogLine;
        logs.push(line);
        for (const waiter of [...waiters]) {
          if (!waiter.match(line)) continue;
          waiters.splice(waiters.indexOf(waiter), 1);
          waiter.resolve(line);
        }
      }
      done();
    },
  });
  const sample = createSampleApp({
    configFile: file,
    token: TOKEN,
    logger: { level: "info", stream },
    configDebounceMs: CONFIG_DEBOUNCE_MS,
    protocolReplicas,
  });
  cleanups.push(() => sample.app.close());
  for (const replica of sample.replicas) cleanups.push(() => replica.app.close());
  const nextLog = (match: (line: LogLine) => boolean) =>
    new Promise<LogLine>((resolve) => waiters.push({ match, resolve }));
  return { ...sample, file, logs, nextLog };
}

// The rewrites that probe the watch must leave the debounce room to fire: every change
// restarts it, and where each write is reported (Linux inotify) rewrites closer than the
// debounce would hold the reload off for good.
const WATCH_PROBE_INTERVAL_MS = CONFIG_DEBOUNCE_MS + 30;

// Returns once the app's config watch is live. fs.watch gives no signal that it is: on
// macOS FSEvents starts a moment after watch() returns and a change made before then
// is never reported. The watch is live once a watch run is logged, so the file is
// rewritten as it is until one is; the runs it causes change nothing. The rewriting also
// stops at the test's cleanup, so a test that times out leaves no timer running.
async function watchLive(fixture: Fixture): Promise<void> {
  await fixture.app.ready();
  const logged = fixture.nextLog((line) => line["trigger"] === "file-changed");
  const content = readFileSync(fixture.file);
  writeFileSync(fixture.file, content);
  const rewrite = setInterval(() => writeFileSync(fixture.file, content), WATCH_PROBE_INTERVAL_MS);
  cleanups.push(() => clearInterval(rewrite));
  try {
    await logged;
  } finally {
    clearInterval(rewrite);
  }
}

test("the app warns at startup that its state lives in memory and resets on restart", async () => {
  const fixture = setUp(configText(1));
  await fixture.app.ready();
  const warning = fixture.logs.find((line) => line["level"] === 40 && /in memory/.test(String(line["msg"])));
  assert.ok(warning, "a warn-level line at startup");
  assert.match(String(warning["msg"]), /budgets, usage totals and usage batch de-duplication reset when this process restarts — the sample is not a billing system/);
});

test("the app serves the config file to gateways through kaiak-control's plugin", { timeout: 5000 }, async () => {
  const { app } = setUp(configText(1));
  await app.listen({ host: "127.0.0.1", port: 0 });
  const address = app.server.address();
  assert.ok(address && typeof address === "object");
  const event = await firstConfigEvent(`http://127.0.0.1:${address.port}/v1/stream`, cleanups as (() => void)[]);
  assert.equal(event.config_hash, hashOf(1));
  assert.equal(event.config.models.llama.metadata.context_length, 1001);

  const refused = await app.inject({ method: "GET", url: "/v1/stream", headers: { ...GATEWAY_HEADERS, authorization: "Bearer wrong" } });
  assert.equal(refused.statusCode, 401);
});

test("protocol replicas share the app's store: a batch counts once for all, a publish reaches them", async () => {
  const fixture = setUp(configText(1), 2);
  await fixture.app.ready();
  const [one, two] = fixture.replicas;
  assert.ok(one && two);
  await Promise.all([one.app.ready(), two.app.ready()]);

  for (const replica of [one, two]) assert.equal((await replica.controlPlane.currentConfig())?.hash, hashOf(1));

  const batch = readFileSync(USAGE_BATCH, "utf8");
  const post = (replica: { app: Fixture["app"] }) =>
    replica.app.inject({ method: "POST", url: "/v1/usage", headers: { ...GATEWAY_HEADERS, "content-type": "application/json" }, payload: batch });
  assert.equal((await post(two)).statusCode, 200);
  // The same batch resent to another core is acknowledged, not counted again.
  assert.equal((await post(fixture)).statusCode, 200);
  assert.equal((await fixture.controlPlane.recentRecords()).length, 1);
  const totals = await one.controlPlane.readTotals();
  assert.deepEqual(totals.cursors, [{ instance: "gw-1", epoch: "5d41402abc4b2a76b9719d911017c592", sequence: 1 }]);

  writeFileSync(fixture.file, configText(2));
  await fixture.configFile.reload("test");
  for (const replica of [one, two]) assert.equal((await replica.controlPlane.currentConfig())?.hash, hashOf(2));
});

test("an edit to the file is pushed on the gateway stream as the current config", { timeout: 5000 }, async () => {
  const fixture = setUp(configText(1));
  await fixture.app.listen({ host: "127.0.0.1", port: 0 });
  await watchLive(fixture);
  const address = fixture.app.server.address();
  assert.ok(address && typeof address === "object");

  const abort = new AbortController();
  cleanups.push(() => abort.abort());
  const response = await fetch(`http://127.0.0.1:${address.port}/v1/stream`, {
    headers: GATEWAY_HEADERS,
    signal: abort.signal,
  });
  assert.equal(response.status, 200);
  assert.ok(response.body);
  const reader = response.body.pipeThrough(new TextDecoderStream()).getReader();

  writeFileSync(fixture.file, configText(2));
  let text = "";
  // The stream's first config event is the current config on connect (config 1); the
  // edit is the next one.
  for (;;) {
    const events = [...text.matchAll(/event: config\ndata: (.*)\n\n/g)];
    const edit = events[1];
    if (edit) {
      const event = JSON.parse(edit[1] ?? "") as { config_hash: string; config: { models: { llama: { metadata: { context_length: number } } } } };
      assert.equal(event.config_hash, hashOf(2));
      assert.equal(event.config.models.llama.metadata.context_length, 1002);
      break;
    }
    const chunk = await reader.read();
    assert.ok(!chunk.done, "the stream stayed open");
    text += chunk.value;
  }
  assert.ok(fixture.logs.some((line) => line["msg"] === "config file published" && line["configHash"] === hashOf(2) && line["trigger"] === "file-changed"));
});

test("an invalid file at startup: nothing is published until the file is fixed, the error is logged", { timeout: 5000 }, async () => {
  const fixture = setUp("{ not json");
  await fixture.app.ready();
  assert.equal(await fixture.controlPlane.currentConfig(), undefined);
  assert.equal(fixture.configFile.state().lastFailure?.trigger, "startup");
  const logged = fixture.logs.find((line) => line["trigger"] === "startup");
  assert.equal(logged?.["level"], 50, "logged as an error");
  assert.match(String(logged?.["msg"]), /^config file rejected \(json-invalid\)/);

  await watchLive(fixture);
  const published = fixture.nextLog((line) => line["trigger"] === "file-changed" && line["configHash"] === hashOf(1));
  writeFileSync(fixture.file, configText(1));
  await published;
  assert.equal((await fixture.controlPlane.currentConfig())?.hash, hashOf(1));
  assert.equal(fixture.configFile.state().lastFailure, undefined);
});

test("the status page is served without the gateway token and shows a rejected edit live", { timeout: 5000 }, async () => {
  const fixture = setUp(configText(1));
  await fixture.app.listen({ host: "127.0.0.1", port: 0 });
  const address = fixture.app.server.address();
  assert.ok(address && typeof address === "object");
  const base = `http://127.0.0.1:${address.port}`;

  const page = await fetch(`${base}/`);
  assert.equal(page.status, 200);
  const body = await page.text();
  assert.ok(body.includes(`<section id="config"><h2>Config <span class="muted"><code>${hashOf(1).slice(0, 12)}</code></span>`));
  assert.ok(!body.includes(TOKEN), "the page never holds the gateway token");

  const abort = new AbortController();
  cleanups.push(() => abort.abort());
  const events = await fetch(`${base}/events`, { signal: abort.signal });
  assert.ok(events.body);
  const reader = events.body.pipeThrough(new TextDecoderStream()).getReader();
  let text = "";
  const readUntil = async (pattern: RegExp): Promise<void> => {
    while (!pattern.test(text)) {
      const chunk = await reader.read();
      assert.ok(!chunk.done, "the stream stayed open");
      text += chunk.value;
    }
  };
  await readUntil(/"id":"usage"/);
  await watchLive(fixture);
  writeFileSync(fixture.file, "{ broken");
  await readUntil(/"id":"config".*rejected \(json-invalid\)/);
  assert.ok(!text.includes(TOKEN), "the page stream never holds the gateway token");
});
