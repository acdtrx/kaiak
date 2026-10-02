// POST /v1/usage over a real listening app.

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import path from "node:path";
import { afterEach, test } from "node:test";

import Fastify from "fastify";

import { createControlPlane } from "../control-plane/index.ts";
import type { ControlPlane } from "../control-plane/index.ts";
import { validateUsageAck } from "../messages/index.ts";
import type { UsageBatch } from "../messages/index.ts";
import { createMemoryStore } from "../storage/index.ts";

import { controlProtocolPlugin } from "./index.ts";

const TOKEN = "test-token";
const FIXTURES = path.resolve(import.meta.dirname, "../../../../protocol/fixtures");
const HEADERS = {
  authorization: `Bearer ${TOKEN}`,
  "kaiak-protocol": "4",
  "kaiak-instance": "gw-1",
  "content-type": "application/json",
};

function readFixture(relative: string): unknown {
  return JSON.parse(readFileSync(path.join(FIXTURES, relative), "utf8"));
}

// Instance gw-1, records for two of full.json's groups.
const BATCH = readFixture("messages/usage-batch/valid/mixed-groups.json") as UsageBatch;

const closers: (() => Promise<void>)[] = [];

afterEach(async () => {
  for (const close of closers.splice(0)) await close();
});

async function start(publish = true): Promise<{ base: string; controlPlane: ControlPlane }> {
  const controlPlane = createControlPlane({
    store: createMemoryStore(),
    token: TOKEN,
    clock: () => Date.UTC(2026, 8, 24, 10, 30),
    onListenerError: (error) => assert.fail(`listener failed: ${String(error)}`),
  });
  if (publish) assert.ok((await controlPlane.publishConfig(readFixture("config/valid/full.json"))).ok);
  const app = Fastify();
  await app.register(controlProtocolPlugin, { controlPlane });
  await app.listen({ host: "127.0.0.1", port: 0 });
  closers.push(() => app.close());
  const address = app.server.address();
  assert.ok(address && typeof address === "object", "listening on a TCP port");
  return { base: `http://127.0.0.1:${address.port}`, controlPlane };
}

async function postUsage(base: string, body: unknown, headers: Record<string, string> = HEADERS): Promise<Response> {
  return fetch(`${base}/v1/usage`, { method: "POST", headers, body: JSON.stringify(body) });
}

test("a batch is answered with its ack carrying the totals", async () => {
  const { base, controlPlane } = await start();
  const response = await postUsage(base, BATCH);
  assert.equal(response.status, 200);
  assert.equal(response.headers.get("kaiak-protocol"), "4");
  const ack: unknown = await response.json();
  const validation = validateUsageAck(ack);
  assert.ok(validation.ok, "the ack passes its schema");
  assert.deepEqual(validation.message.batch, BATCH.batch);
  assert.deepEqual(validation.message.totals, await controlPlane.totals("gw-1"));
  assert.ok(validation.message.totals.windows.length > 0, "the batch counted");

  // The resend after a lost ack gets the same answer.
  const resent = await postUsage(base, BATCH);
  assert.equal(resent.status, 200);
  assert.deepEqual(await resent.json(), ack);
});

test("a batch naming another instance than the header is refused", async () => {
  const { base, controlPlane } = await start();
  const response = await postUsage(base, BATCH, { ...HEADERS, "kaiak-instance": "gw-2" });
  assert.equal(response.status, 400);
  const body = (await response.json()) as { error: string; detail: string };
  assert.equal(body.error, "instance-mismatch");
  assert.deepEqual((await controlPlane.totals("gw-1"))?.windows, []);
});

test("invalid batches are refused with their codes", async () => {
  const { base } = await start();
  const schema = await postUsage(base, readFixture("messages/usage-batch/invalid/no-records.json"));
  assert.equal(schema.status, 400);
  assert.equal(((await schema.json()) as { error: string }).error, "usage-batch-invalid");

  const rule = await postUsage(base, readFixture("messages/usage-batch/invalid/record-id-duplicate.json"));
  assert.equal(rule.status, 400);
  assert.equal(((await rule.json()) as { error: string }).error, "record-id-duplicate");

  const notJson = await fetch(`${base}/v1/usage`, { method: "POST", headers: HEADERS, body: "{" });
  assert.equal(notJson.status, 400);
  assert.equal(((await notJson.json()) as { error: string }).error, "request-invalid");
});

test("a batch before any config is answered 503 config-unavailable", async () => {
  const { base } = await start(false);
  const response = await postUsage(base, BATCH);
  assert.equal(response.status, 503);
  assert.equal(((await response.json()) as { error: string }).error, "config-unavailable");
});

test("a batch without the token is refused before intake", async () => {
  const { base, controlPlane } = await start();
  const response = await postUsage(base, BATCH, { ...HEADERS, authorization: "Bearer wrong" });
  assert.equal(response.status, 401);
  assert.deepEqual((await controlPlane.totals("gw-1"))?.windows, []);
});

test("a full batch of 500 large records fits the body limit", async () => {
  const { base } = await start();
  const [template] = BATCH.records;
  assert.ok(template, "the fixture has records");
  const records = Array.from({ length: 500 }, (_, index) => ({
    ...template,
    record_id: index.toString(16).padStart(32, "0"),
    request_id: "r".repeat(128),
    key_id: "k".repeat(64),
  }));
  const response = await postUsage(base, { batch: BATCH.batch, records });
  assert.equal(response.status, 200);
});
