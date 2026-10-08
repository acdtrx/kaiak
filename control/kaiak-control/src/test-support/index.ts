// Test support for kaiak-control's suites. The shared fixtures in protocol/fixtures/,
// which the gateway's suite runs too — a valid fixture passes both halves, an invalid
// one fails both, and a semantic fixture fails with the rule code its cases.json entry
// names. The builders of what a gateway sends (headers, usage records and batches,
// statuses), from the protocol constants and fixtures, so a protocol bump or a new unit
// edits no test. The harness of the HTTP tests: an app on a real port, a gateway's
// stream, the cleanups. Test tooling only: not in the package's exports, imported by
// tests alone.

import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { EventEmitter, once } from "node:events";
import { readdirSync, readFileSync } from "node:fs";
import path from "node:path";
import { test } from "node:test";

import Fastify from "fastify";

import type { Config } from "../config/index.ts";
import { createControlPlane } from "../control-plane/index.ts";
import type { ControlPlane, ControlPlaneOptions } from "../control-plane/index.ts";
import { controlProtocolPlugin } from "../fastify/index.ts";
import type { ControlProtocolPluginOptions } from "../fastify/index.ts";
import { validateConfigEvent, validateTotals } from "../messages/index.ts";
import type { BatchId, GatewayStatus, Totals, UsageBatch, UsageRecord } from "../messages/index.ts";
import { INSTANCE_HEADER, PROTOCOL_HEADER, PROTOCOL_VERSION } from "../protocol/index.ts";
import { createMemoryStore } from "../storage/index.ts";
import type { ControlPlaneStore, StoreChangeListener } from "../storage/index.ts";

import { openSseStream } from "./sse-client.ts";
import type { SseItem, SseStream } from "./sse-client.ts";

export { openSseStream };
export type { SseItem, SseStream };

const FIXTURES = path.resolve(import.meta.dirname, "../../../../protocol/fixtures");

// The file of a fixture directory that says what each fixture there breaks; not a
// fixture itself.
export const CASES_FILE = "cases.json";

// The path of protocol/fixtures/<parts>.
export function fixturePath(...parts: string[]): string {
  return path.join(FIXTURES, ...parts);
}

export function readJson(file: string): unknown {
  return JSON.parse(readFileSync(file, "utf8"));
}

// The parsed content of protocol/fixtures/<parts>.
export function fixture(...parts: string[]): unknown {
  return readJson(fixturePath(...parts));
}

// The fixtures in dir: its .json files but the cases file, sorted.
export function fixtureFiles(dir: string): string[] {
  return readdirSync(dir)
    .filter((name) => name.endsWith(".json") && name !== CASES_FILE)
    .sort();
}

// A cases.json entry of an invalid fixture: a schema case breaks the schema and names
// no code; a semantic case breaks the rule its code names.
export interface InvalidCase {
  kind: "schema" | "semantic";
  code?: string;
  reason: string;
}

export function readCases(dir: string): Map<string, InvalidCase> {
  const raw = readJson(path.join(dir, CASES_FILE));
  assert.ok(typeof raw === "object" && raw !== null && !Array.isArray(raw), `${CASES_FILE} is an object`);
  const cases = new Map<string, InvalidCase>();
  for (const [file, entry] of Object.entries(raw)) {
    assert.ok(typeof entry === "object" && entry !== null, `${file}: entry is an object`);
    const { kind, code, reason } = entry as Record<string, unknown>;
    assert.ok(kind === "schema" || kind === "semantic", `${file}: kind is schema or semantic`);
    assert.equal(typeof reason, "string", `${file}: reason is a string`);
    if (kind === "semantic") assert.equal(typeof code, "string", `${file}: semantic case names its code`);
    if (kind === "schema") assert.equal(code, undefined, `${file}: schema case names no code`);
    cases.set(file, { kind, reason: String(reason), ...(typeof code === "string" ? { code } : {}) });
  }
  return cases;
}

// A validator as the fixture runners call it: a document in, its issues out.
export type FixtureValidator = (doc: unknown) => { ok: true } | { ok: false; issues: readonly { code: string }[] };

// One test per fixture in dir, named prefix + file: validate accepts it.
export function testValidFixtures(dir: string, validate: FixtureValidator, prefix = ""): void {
  for (const file of fixtureFiles(dir)) {
    test(`${prefix}${file}`, () => {
      const result = validate(readJson(path.join(dir, file)));
      assert.deepEqual(result.ok ? [] : result.issues, []);
    });
  }
}

// A test that dir's cases file has an entry for each fixture and no other, then one
// test per fixture, named prefix + file + its reason: validate refuses it with exactly
// the schema code (a schema case) or the case's code (a semantic one) — a semantic
// fixture breaks one rule, so no other code may appear.
export function testInvalidFixtures(dir: string, validate: FixtureValidator, prefix = ""): void {
  const cases = readCases(dir);

  test(`every invalid fixture has a ${CASES_FILE} entry and every entry has a fixture`, () => {
    assert.deepEqual([...cases.keys()].sort(), fixtureFiles(dir));
  });

  for (const file of fixtureFiles(dir)) {
    const expected = cases.get(file);
    if (!expected) continue;
    test(`${prefix}${file}: ${expected.reason}`, () => {
      const result = validate(readJson(path.join(dir, file)));
      assert.equal(result.ok, false, "the document is rejected");
      if (result.ok) return;
      const codes = [...new Set(result.issues.map((issue) => issue.code))];
      assert.deepEqual(codes, [expected.kind === "semantic" ? expected.code : "schema"]);
    });
  }
}

// The config format version every valid config carries: the config schema's const.
export const CONFIG_FORMAT_VERSION = configFormatVersion();

function configFormatVersion(): Config["format_version"] {
  const schema = readJson(path.join(FIXTURES, "../schema/config.schema.json")) as {
    properties: { format_version: { const: unknown } };
  };
  const version = schema.properties.format_version.const;
  assert.equal(typeof version, "number", "the config schema's format_version is a number");
  return version as Config["format_version"];
}

// A valid config distinguishable by its context length.
export function configNumbered(n: number): Config {
  const config = fixture("config", "valid", "minimal.json") as Config;
  const model = config.models["llama"];
  assert.ok(model, "the minimal fixture has model llama");
  model.metadata.context_length = 1000 + n;
  return config;
}

// Which configNumbered a config is.
export function numberOf(config: Config): number {
  return (config.models["llama"]?.metadata.context_length ?? 0) - 1000;
}

// Gateway messages.

export const TEST_TOKEN = "test-token";
export const TEST_INSTANCE = "gw-1";

// The headers a gateway sends on every request.
export function gatewayHeaders(instance = TEST_INSTANCE) {
  return {
    authorization: `Bearer ${TEST_TOKEN}`,
    [PROTOCOL_HEADER]: String(PROTOCOL_VERSION),
    [INSTANCE_HEADER]: instance,
  };
}

// A status: protocol/fixtures/messages/status/valid/ready.json from instance, with
// fields replaced.
export function gatewayStatus(instance: string, fields: Partial<GatewayStatus> = {}): GatewayStatus {
  return { ...(fixture("messages", "status", "valid", "ready.json") as GatewayStatus), instance, ...fields };
}

export type UsageRecordFields = Partial<Omit<UsageRecord, "units">> & { units?: Partial<UsageRecord["units"]> };

// A usage record: protocol/fixtures/messages/usage-record/valid/one-group.json with
// every unit at 0, neither estimated nor partial, then fields; fields.units names the
// units that are not 0. A unit the protocol adds reaches every record through the
// fixture.
export function usageRecord(fields: UsageRecordFields = {}): UsageRecord {
  const base = fixture("messages", "usage-record", "valid", "one-group.json") as UsageRecord;
  const zero = Object.fromEntries(Object.keys(base.units).map((unit) => [unit, 0])) as UsageRecord["units"];
  const { units, ...rest } = fields;
  return { ...base, estimated: false, partial: false, ...rest, units: { ...zero, ...units } };
}

export function usageBatch(batch: BatchId, records: UsageRecordFields[]): UsageBatch {
  return { batch, records: records.map(usageRecord) };
}

// The core and the app.

// A core over a memory store, checking the test token; options replace any of it.
export function testCore(options: Partial<ControlPlaneOptions> = {}): ControlPlane {
  return createControlPlane({ store: createMemoryStore(), token: TEST_TOKEN, ...options });
}

// A core's listener failure fails the test.
export function failOnListenerError(error: unknown): void {
  assert.fail(`listener failed: ${String(error)}`);
}

// What a test opened, closed after it, last opened first: each test file runs
// closeOpened after each test.
const opened: (() => Promise<void> | void)[] = [];

export function closeAfterTest(close: () => Promise<void> | void): void {
  opened.push(close);
}

export async function closeOpened(): Promise<void> {
  for (const close of opened.splice(0).reverse()) await close();
}

export interface RunningApp {
  base: string;
  port: number;
  // Closes the app once; later calls do nothing.
  close(): Promise<void>;
}

// An app with the control-protocol plugin over controlPlane, listening on a free local
// port until the test ends. onRequest, when given, sees each response before its route
// runs.
export async function startApp(
  controlPlane: ControlPlane,
  options: Omit<ControlProtocolPluginOptions, "controlPlane"> = {},
  onRequest?: (raw: NodeJS.WritableStream) => void,
): Promise<RunningApp> {
  const app = Fastify();
  if (onRequest) {
    app.addHook("onRequest", async (_request, reply) => {
      onRequest(reply.raw);
    });
  }
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
  closeAfterTest(close);
  return { base: `http://127.0.0.1:${address.port}`, port: address.port, close };
}

// A gateway's stream from the app at base, open until the test ends.
export async function openStream(base: string, instance = TEST_INSTANCE): Promise<SseStream> {
  const stream = await openSseStream(`${base}/v1/stream`, gatewayHeaders(instance));
  closeAfterTest(() => stream.close());
  assert.equal(stream.response.status, 200);
  return stream;
}

// Wraps a core so a test sees how many streams are open: each holds a config
// subscription while it is.
export function countingSubscriptions(controlPlane: ControlPlane) {
  let active = 0;
  const changes = new EventEmitter();
  const counted: ControlPlane = {
    ...controlPlane,
    onConfigRead(listener) {
      const unsubscribe = controlPlane.onConfigRead(listener);
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

// A store that can be switched to a backup, as a store restored from one reads; only
// the active store's changes are announced. listeners are the store's subscribers, for
// a test to announce what the store's channel would.
export function restorable(live: ControlPlaneStore, backup: ControlPlaneStore) {
  let active = live;
  const listeners: StoreChangeListener[] = [];
  const store = new Proxy({} as ControlPlaneStore, {
    get(_, name: keyof ControlPlaneStore) {
      if (name === "subscribe") {
        return (listener: StoreChangeListener) => {
          listeners.push(listener);
          const fromLive = live.subscribe((change) => active === live && listener(change));
          const fromBackup = backup.subscribe((change) => active === backup && listener(change));
          return () => {
            fromLive();
            fromBackup();
          };
        };
      }
      return (...args: unknown[]) => (active[name] as (...a: unknown[]) => unknown).apply(active, args);
    },
  });
  return { store, restore: () => (active = backup), listeners };
}

// What the stream sends.

// The config_hash of a config as the control plane publishes it: lowercase hex SHA-256
// of its JSON text, written by JSON.stringify.
export function configHash(config: Config): string {
  return createHash("sha256").update(JSON.stringify(config)).digest("hex");
}

// Which configNumbered a config event carries, after checking the event is a valid
// config event whose hash is its config's.
export function configNumberOf(item: SseItem): number {
  assert.equal(item.kind, "event");
  assert.ok(item.kind === "event");
  assert.equal(item.event, "config");
  assert.equal(item.id, undefined, "config events carry no id: there is nothing to resume from");
  const validation = validateConfigEvent(JSON.parse(item.data));
  assert.ok(validation.ok, "the data is a valid config event");
  const { config, config_hash } = validation.message;
  assert.equal(config_hash, configHash(config));
  return numberOf(config);
}

// The next item that is neither a totals event nor a comment: a config event, or the
// stream's end.
export async function nextConfigOrEnd(stream: SseStream): Promise<SseItem> {
  for (;;) {
    const item = await stream.next();
    if (item.kind === "comment" || (item.kind === "event" && item.event === "totals")) continue;
    return item;
  }
}

// The next config event's configNumbered, past totals events and comments.
export async function nextConfig(stream: SseStream): Promise<number> {
  return configNumberOf(await nextConfigOrEnd(stream));
}

// A totals event's message, after checking it is valid and carries no id.
export function totalsOf(item: SseItem): Totals {
  assert.ok(item.kind === "event", `an event, got ${item.kind}`);
  assert.equal(item.event, "totals");
  assert.equal(item.id, undefined, "totals events carry no id");
  const validation = validateTotals(JSON.parse(item.data));
  assert.ok(validation.ok, "the data is valid totals");
  return validation.message;
}

// The next totals event's message, past config events and comments.
export async function nextTotals(stream: SseStream): Promise<Totals> {
  let item = await stream.nextEvent();
  while (item.kind === "event" && item.event === "config") item = await stream.nextEvent();
  return totalsOf(item);
}
