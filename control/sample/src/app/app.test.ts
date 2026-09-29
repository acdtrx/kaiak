import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { Writable } from "node:stream";
import { afterEach, test } from "node:test";

import { createSampleApp } from "./index.ts";
import type { SampleApp } from "./index.ts";

const MINIMAL = path.resolve(import.meta.dirname, "../../../../protocol/fixtures/config/valid/minimal.json");
const TOKEN = "sample-token";
const GATEWAY_HEADERS = { authorization: `Bearer ${TOKEN}`, "kaiak-protocol": "3", "kaiak-instance": "gw-1" };

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

const cleanups: (() => Promise<void> | void)[] = [];

afterEach(async () => {
  for (const cleanup of cleanups.splice(0).reverse()) await cleanup();
});

// Quiet time after a change before the app reloads its config file.
const CONFIG_DEBOUNCE_MS = 20;

function setUp(initial: string): Fixture {
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
  const sample = createSampleApp({ configFile: file, token: TOKEN, logger: { level: "info", stream }, configDebounceMs: CONFIG_DEBOUNCE_MS });
  cleanups.push(() => sample.app.close());
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

test("the app serves the config file to gateways through kaiak-control's plugin", async () => {
  const { app } = setUp(configText(1));
  const response = await app.inject({ method: "GET", url: "/v1/config", headers: GATEWAY_HEADERS });
  assert.equal(response.statusCode, 200);
  assert.equal(response.headers["kaiak-protocol"], "3");
  const snapshot = response.json<{ version: number; config: { models: { llama: { metadata: { context_length: number } } } } }>();
  assert.equal(snapshot.version, 1);
  assert.equal(snapshot.config.models.llama.metadata.context_length, 1001);

  const refused = await app.inject({ method: "GET", url: "/v1/config", headers: { ...GATEWAY_HEADERS, authorization: "Bearer wrong" } });
  assert.equal(refused.statusCode, 401);
});

test("an edit to the file is pushed on the gateway stream as a new version", { timeout: 5000 }, async () => {
  const fixture = setUp(configText(1));
  await fixture.app.listen({ host: "127.0.0.1", port: 0 });
  await watchLive(fixture);
  const address = fixture.app.server.address();
  assert.ok(address && typeof address === "object");

  const abort = new AbortController();
  cleanups.push(() => abort.abort());
  const epoch = await fixture.controlPlane.configEpoch();
  const response = await fetch(`http://127.0.0.1:${address.port}/v1/stream?since=1&config_epoch=${epoch}`, {
    headers: GATEWAY_HEADERS,
    signal: abort.signal,
  });
  assert.equal(response.status, 200);
  assert.ok(response.body);
  const reader = response.body.pipeThrough(new TextDecoderStream()).getReader();

  writeFileSync(fixture.file, configText(2));
  let text = "";
  for (;;) {
    const match = /event: config\nid: (\d+)\ndata: (.*)\n\n/.exec(text);
    if (match) {
      assert.equal(match[1], "2");
      const snapshot = JSON.parse(match[2] ?? "") as { version: number; config: { models: { llama: { metadata: { context_length: number } } } } };
      assert.equal(snapshot.version, 2);
      assert.equal(snapshot.config.models.llama.metadata.context_length, 1002);
      break;
    }
    const chunk = await reader.read();
    assert.ok(!chunk.done, "the stream stayed open");
    text += chunk.value;
  }
  assert.ok(fixture.logs.some((line) => line["msg"] === "config file published as version 2" && line["trigger"] === "file-changed"));
});

test("an invalid file at startup: gateways get 503 until the file is fixed, the error is logged", { timeout: 5000 }, async () => {
  const fixture = setUp("{ not json");
  const unavailable = await fixture.app.inject({ method: "GET", url: "/v1/config", headers: GATEWAY_HEADERS });
  assert.equal(unavailable.statusCode, 503);
  assert.equal(unavailable.json<{ error: string }>().error, "config-unavailable");
  assert.equal(fixture.configFile.state().lastFailure?.trigger, "startup");
  const logged = fixture.logs.find((line) => line["trigger"] === "startup");
  assert.equal(logged?.["level"], 50, "logged as an error");
  assert.match(String(logged?.["msg"]), /^config file rejected \(json-invalid\)/);

  await watchLive(fixture);
  const published = fixture.nextLog((line) => line["trigger"] === "file-changed" && line["version"] === 1);
  writeFileSync(fixture.file, configText(1));
  await published;
  const served = await fixture.app.inject({ method: "GET", url: "/v1/config", headers: GATEWAY_HEADERS });
  assert.equal(served.statusCode, 200);
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
  assert.match(body, /<section id="config"><h2>Config <span class="muted">v1<\/span>/);
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
