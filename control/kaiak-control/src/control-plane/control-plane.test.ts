import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import path from "node:path";
import { afterEach, describe, test } from "node:test";

import { createMemoryStore } from "../storage/index.ts";
import type { ControlPlaneStore } from "../storage/index.ts";

import { createControlPlane } from "./index.ts";
import type { ControlPlane, ControlPlaneOptions } from "./index.ts";

const FIXTURES = path.resolve(import.meta.dirname, "../../../../protocol/fixtures");
const MINIMAL = path.join(FIXTURES, "config/valid/minimal.json");
const FULL = path.join(FIXTURES, "config/valid/full.json");
const MIXED_BATCH = path.join(FIXTURES, "messages/usage-batch/valid/mixed-groups.json");
const READY_STATUS = path.join(FIXTURES, "messages/status/valid/ready.json");

test("the core publishes and serves the current config with its clock", async () => {
  const controlPlane = createControlPlane({ store: createMemoryStore(), token: "t", clock: () => 42 });
  const heard: string[] = [];
  controlPlane.onConfigPublished((published) => heard.push(published.hash));

  const result = await controlPlane.publishConfig(JSON.parse(readFileSync(MINIMAL, "utf8")));
  assert.ok(result.ok);
  assert.equal(result.published.publishedAt, 42);
  assert.equal((await controlPlane.currentConfig())?.hash, result.published.hash);
  // Heard for the store's announcement and for the publish's own read.
  assert.deepEqual(heard, [result.published.hash, result.published.hash]);
});

test("the core checks gateway requests against its token", () => {
  const controlPlane = createControlPlane({ store: createMemoryStore(), token: "right" });
  const headers = { "kaiak-protocol": "5", "kaiak-instance": "gw-1" };
  assert.deepEqual(controlPlane.checkGatewayRequest({ ...headers, authorization: "Bearer right" }), {
    ok: true,
    instance: "gw-1",
  });
  const wrong = controlPlane.checkGatewayRequest({ ...headers, authorization: "Bearer wrong" });
  assert.ok(!wrong.ok && wrong.error.code === "unauthorized");
});

test("a listener's failure goes to the host's handler, not to the publish", async () => {
  const failures: string[] = [];
  const controlPlane = createControlPlane({
    store: createMemoryStore(),
    token: "t",
    onListenerError: (_error, event) => {
      assert.equal(event.type, "config-published");
      if (event.type === "config-published") failures.push(event.published.hash);
    },
  });
  controlPlane.onConfigPublished(() => {
    throw new Error("broken");
  });
  const result = await controlPlane.publishConfig(JSON.parse(readFileSync(MINIMAL, "utf8")));
  assert.ok(result.ok);
  assert.deepEqual(failures, [result.published.hash, result.published.hash]);
});

test("an empty token is refused", () => {
  assert.throws(() => createControlPlane({ store: createMemoryStore(), token: "" }), { code: "token-missing" });
});

test("the core takes usage batches with its clock and reports totals listener failures", async () => {
  const events: string[] = [];
  const controlPlane = createControlPlane({
    store: createMemoryStore(),
    token: "t",
    recentRecordsSize: 1,
    clock: () => Date.UTC(2026, 8, 24, 10, 30),
    onListenerError: (_error, event) => events.push(event.type),
  });
  controlPlane.onTotalsChanged(() => {
    throw new Error("broken");
  });
  assert.ok((await controlPlane.publishConfig(JSON.parse(readFileSync(FULL, "utf8")))).ok);
  const batch = JSON.parse(readFileSync(MIXED_BATCH, "utf8")) as { batch: unknown };
  const intake = await controlPlane.acceptUsageBatch("gw-1", batch);
  assert.ok(intake.ok);
  // The ack names the batch only; the totals reach the gateway on its stream.
  assert.deepEqual(intake.ack, { batch: batch.batch });
  assert.equal((await controlPlane.totals("gw-1"))?.live_gateways, 0);
  assert.equal((await controlPlane.recentRecords()).length, 1);
  assert.deepEqual(events, ["totals-changed"]);
});

test("the live set's size is the live-gateway count in totals", async () => {
  let now = Date.UTC(2026, 8, 24, 10, 30);
  const events: string[] = [];
  const controlPlane = createControlPlane({
    store: createMemoryStore(),
    token: "t",
    clock: () => now,
    gatewayLiveTimeoutMs: 1_000,
    onListenerError: (_error, event) => events.push(event.type),
  });
  controlPlane.onGatewaysChanged(() => {
    throw new Error("broken");
  });
  assert.ok((await controlPlane.publishConfig(JSON.parse(readFileSync(FULL, "utf8")))).ok);
  const status = JSON.parse(readFileSync(READY_STATUS, "utf8")) as { instance: string };
  for (const instance of ["gw-1", "gw-2"]) {
    assert.ok((await controlPlane.acceptStatus(instance, { ...status, instance })).ok);
  }
  assert.equal((await controlPlane.totals("gw-1"))?.live_gateways, 2);

  now += 1_000;
  assert.deepEqual((await controlPlane.expireSilentGateways("manual")).expired, ["gw-1", "gw-2"]);
  assert.equal((await controlPlane.totals("gw-1"))?.live_gateways, 0);
  // Each status and each expiry is its own change to the gateway records.
  assert.deepEqual(events, ["gateways-changed", "gateways-changed", "gateways-changed", "gateways-changed"]);
});

test("a gateway forgotten while silent keeps its last counted batch for the cursor retention", async () => {
  let now = Date.UTC(2026, 8, 24, 10, 30);
  const controlPlane = createControlPlane({
    store: createMemoryStore(),
    token: "t",
    clock: () => now,
    gatewayLiveTimeoutMs: 1_000,
    gatewayForgetAfterMs: 2_000,
    batchCursorRetentionMs: 10_000,
  });
  assert.ok((await controlPlane.publishConfig(JSON.parse(readFileSync(FULL, "utf8")))).ok);
  const status = JSON.parse(readFileSync(READY_STATUS, "utf8")) as { instance: string };
  assert.ok((await controlPlane.acceptStatus("gw-1", { ...status, instance: "gw-1" })).ok);
  const batch = JSON.parse(readFileSync(MIXED_BATCH, "utf8"));
  assert.ok((await controlPlane.acceptUsageBatch("gw-1", batch)).ok);

  // Partitioned past the forget delay: forgotten, but a resend of the batch it had
  // counted (its ack was lost) is still recognized.
  now += 2_000;
  assert.deepEqual((await controlPlane.expireSilentGateways("manual")).forgotten, ["gw-1"]);
  const resend = await controlPlane.acceptUsageBatch("gw-1", batch);
  assert.ok(resend.ok);
  assert.equal(resend.outcome, "duplicate");

  // Past the cursor retention (counted 10 s ago and more) the sweep drops the cursor.
  now += 8_001;
  const run = await controlPlane.expireSilentGateways("manual");
  assert.deepEqual(run.batchCursorsDropped, ["gw-1"]);
  assert.deepEqual((await controlPlane.totals("gw-1")).counted_through, []);
});

describe("several cores over one store", () => {
  const cores: ControlPlane[] = [];
  afterEach(async () => {
    for (const core of cores.splice(0)) await core.stop();
  });
  const core = (store: ControlPlaneStore, options: Partial<ControlPlaneOptions> = {}): ControlPlane => {
    const created = createControlPlane({ store, token: "t", clock: () => Date.UTC(2026, 8, 24, 10, 30), ...options });
    cores.push(created);
    return created;
  };
  const full = (): unknown => JSON.parse(readFileSync(FULL, "utf8"));

  // One record for carol, costing `cost` nano-USD, from `instance`.
  const carolBatch = (instance: string, sequence: number, cost: number): unknown => ({
    batch: { instance, epoch: "e".repeat(32), sequence },
    records: [
      {
        record_id: `${instance}-${sequence}`.padEnd(32, "0").replace(/[^0-9a-f]/g, "0"),
        request_id: `req-${instance}-${sequence}`,
        gateway_instance: instance,
        key_id: "k-carol",
        groups: ["users", "carol"],
        model: "gpt-4.1-mini",
        deployment: { backend: "b", model: "gpt-4.1-mini" },
        units: { tokens_in: 1, tokens_cached: 0, tokens_cache_write: 0, tokens_out: 0, tokens_reasoning: 0 },
        cost_nano_usd: cost,
        estimated: false,
        partial: false,
        gateway_time: "2026-09-24T10:29:00Z",
      },
    ],
  });
  const usd = async (controlPlane: ControlPlane, group?: string): Promise<string | undefined> =>
    (await controlPlane.totals("probe"))?.windows.find((w) => w.group === group && w.type === "usd_per_month")?.used;

  test("every core starts: none holds the store alone", async () => {
    const store = createMemoryStore();
    const [a, b] = [core(store), core(store)];
    await a.start();
    await a.start(); // started already: nothing more
    await b.start();
  });

  test("batches from many instances split across two cores, each resent to both, count once", async () => {
    const store = createMemoryStore();
    const [a, b] = [core(store), core(store)];
    assert.ok((await a.publishConfig(full())).ok);
    const instances = Array.from({ length: 8 }, (_, n) => `gw-${n}`);
    const sends = instances.flatMap((instance, n) =>
      [1, 2, 3].flatMap((sequence) => {
        const doc = carolBatch(instance, sequence, 10);
        // Every batch goes to one core first, and its resend to the other at once.
        const [first, second] = (n + sequence) % 2 === 0 ? [a, b] : [b, a];
        return [first.acceptUsageBatch(instance, doc), second.acceptUsageBatch(instance, structuredClone(doc))];
      }),
    );
    const intakes = await Promise.all(sends);
    assert.ok(intakes.every((intake) => intake.ok));
    const outcomes = intakes.map((intake) => (intake.ok ? intake.outcome : "refused"));
    assert.equal(outcomes.filter((outcome) => outcome === "duplicate").length, 24);
    // 8 instances × 3 batches × 10 nano-USD, once each.
    assert.equal(await usd(a), "240");
    assert.equal(await usd(b, "carol"), "240");
  });

  test("totals read through either core are the same", async () => {
    const store = createMemoryStore();
    const [a, b] = [core(store), core(store)];
    assert.ok((await a.publishConfig(full())).ok);
    assert.ok((await b.acceptUsageBatch("gw-1", carolBatch("gw-1", 1, 5))).ok);
    const [ta, tb] = [await a.totals("gw-1"), await b.totals("gw-1")];
    assert.deepEqual(ta, tb);
    assert.deepEqual(ta.counted_through, [{ epoch: "e".repeat(32), sequence: 1 }]);
  });

  test("a publish through one core reaches the other core's listeners, and their totals follow", async () => {
    const store = createMemoryStore();
    const [a, b] = [core(store), core(store)];
    const heardByB: string[] = [];
    let totalsHeardByB = 0;
    b.onConfigPublished((published) => heardByB.push(published.hash));
    b.onTotalsChanged(() => (totalsHeardByB += 1));
    const result = await a.publishConfig(full());
    assert.ok(result.ok);
    assert.ok((await a.acceptUsageBatch("gw-1", carolBatch("gw-1", 1, 5))).ok);
    await new Promise((resolve) => setImmediate(resolve));
    assert.deepEqual(heardByB, [result.published.hash]);
    assert.equal(totalsHeardByB, 1);
    assert.equal(await usd(b, "carol"), "5");
  });

  test("publishes and batches racing on two cores never refuse each other", async () => {
    const store = createMemoryStore();
    const [a, b] = [core(store), core(store)];
    assert.ok((await a.publishConfig(full())).ok);
    const work: Promise<{ ok: boolean }>[] = [];
    for (let n = 1; n <= 10; n++) {
      work.push(a.publishConfig(full()));
      work.push(b.acceptUsageBatch("gw-1", carolBatch("gw-1", n, 1)));
    }
    const results = await Promise.all(work);
    assert.ok(results.every((result) => result.ok));
    assert.deepEqual((await b.currentConfig())?.config, full());
    assert.equal(await usd(a, "carol"), "10");
  });

  test("an edited limit keeps its window across cores", async () => {
    const store = createMemoryStore();
    const [a, b] = [core(store), core(store)];
    assert.ok((await a.publishConfig(full())).ok);
    assert.ok((await b.acceptUsageBatch("gw-1", carolBatch("gw-1", 1, 700))).ok);
    const edited = full() as { global: { limits: { type: string; value: number }[] } };
    edited.global.limits = [{ type: "usd_per_month", value: 9000 }];
    assert.ok((await b.publishConfig(edited)).ok);
    assert.equal(await usd(a), "700");
  });

  test("two sweeps together expire a silent gateway once; a sweep never expires a gateway that just reported", async () => {
    const store = createMemoryStore();
    let now = Date.UTC(2026, 8, 24, 10, 30);
    const options = { clock: () => now, gatewayLiveTimeoutMs: 1_000 };
    const [a, b] = [core(store, options), core(store, options)];
    assert.ok((await a.publishConfig(full())).ok);
    const status = JSON.parse(readFileSync(READY_STATUS, "utf8")) as { instance: string };
    for (const instance of ["gw-1", "gw-2"]) assert.ok((await a.acceptStatus(instance, { ...status, instance })).ok);

    now += 1_000;
    const [runA, runB] = await Promise.all([a.expireSilentGateways("manual"), b.expireSilentGateways("manual")]);
    assert.deepEqual([...runA.expired, ...runB.expired].sort(), ["gw-1", "gw-2"]);
    assert.equal(await a.liveGateways(), 0);

    // gw-1 reports again while a sweep that judged it silent is about to write.
    const reported = b.acceptStatus("gw-1", { ...status, instance: "gw-1" });
    const swept = a.expireSilentGateways("manual");
    assert.ok((await reported).ok);
    await swept;
    assert.equal(await b.liveGateways(), 1);
  });
});
