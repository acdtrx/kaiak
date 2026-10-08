// POST /v1/usage over a real listening app.

import assert from "node:assert/strict";
import { afterEach, test } from "node:test";

import type { ControlPlane } from "../control-plane/index.ts";
import { validateUsageAck } from "../messages/index.ts";
import type { UsageBatch } from "../messages/index.ts";
import { PROTOCOL_VERSION } from "../protocol/index.ts";
import { closeOpened, failOnListenerError, fixture, gatewayHeaders, startApp, testCore } from "../test-support/index.ts";

const HEADERS: Record<string, string> = { ...gatewayHeaders(), "content-type": "application/json" };

// Instance gw-1, records for two of full.json's groups.
const BATCH = fixture("messages/usage-batch/valid/mixed-groups.json") as UsageBatch;

afterEach(closeOpened);

async function start(publish = true): Promise<{ base: string; controlPlane: ControlPlane }> {
  const controlPlane = testCore({ clock: () => Date.UTC(2026, 8, 24, 10, 30), onListenerError: failOnListenerError });
  if (publish) assert.ok((await controlPlane.publishConfig(fixture("config/valid/full.json"))).ok);
  const { base } = await startApp(controlPlane);
  return { base, controlPlane };
}

async function postUsage(base: string, body: unknown, headers: Record<string, string> = HEADERS): Promise<Response> {
  return fetch(`${base}/v1/usage`, { method: "POST", headers, body: JSON.stringify(body) });
}

test("a batch is answered with its ack naming the batch only", async () => {
  const { base, controlPlane } = await start();
  const response = await postUsage(base, BATCH);
  assert.equal(response.status, 200);
  assert.equal(response.headers.get("kaiak-protocol"), String(PROTOCOL_VERSION));
  const ack: unknown = await response.json();
  const validation = validateUsageAck(ack);
  assert.ok(validation.ok, "the ack passes its schema");
  assert.deepEqual(ack, { batch: BATCH.batch });
  assert.ok((await controlPlane.readTotals()).windows.length > 0, "the batch counted");

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
  assert.deepEqual((await controlPlane.readTotals()).windows, []);
});

test("invalid batches are refused with their codes", async () => {
  const { base } = await start();
  const schema = await postUsage(base, fixture("messages/usage-batch/invalid/no-records.json"));
  assert.equal(schema.status, 400);
  assert.equal(((await schema.json()) as { error: string }).error, "usage-batch-invalid");

  const rule = await postUsage(base, fixture("messages/usage-batch/invalid/record-id-duplicate.json"));
  assert.equal(rule.status, 400);
  assert.equal(((await rule.json()) as { error: string }).error, "record-id-duplicate");

  const notJson = await fetch(`${base}/v1/usage`, { method: "POST", headers: HEADERS, body: "{" });
  assert.equal(notJson.status, 400);
  assert.equal(((await notJson.json()) as { error: string }).error, "request-invalid");
});

test("a batch before any config is counted and acked", async () => {
  const { base } = await start(false);
  const response = await postUsage(base, BATCH);
  assert.equal(response.status, 200);
  assert.deepEqual(await response.json(), { batch: BATCH.batch });
});

test("a batch without the token is refused before intake", async () => {
  const { base, controlPlane } = await start();
  const response = await postUsage(base, BATCH, { ...HEADERS, authorization: "Bearer wrong" });
  assert.equal(response.status, 401);
  assert.deepEqual((await controlPlane.readTotals()).windows, []);
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
