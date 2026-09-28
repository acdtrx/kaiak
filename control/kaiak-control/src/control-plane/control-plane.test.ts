import assert from "node:assert/strict";
import { EventEmitter, once } from "node:events";
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

test("the core publishes, serves and resumes config versions with its clock", async () => {
  const controlPlane = createControlPlane({ store: createMemoryStore(), token: "t", clock: () => 42 });
  const heard: number[] = [];
  controlPlane.onConfigPublished((published) => heard.push(published.version));

  const result = await controlPlane.publishConfig(JSON.parse(readFileSync(MINIMAL, "utf8")));
  assert.ok(result.ok);
  assert.equal(result.published.publishedAt, 42);
  assert.equal((await controlPlane.currentConfig())?.version, 1);
  const epoch = await controlPlane.configEpoch();
  assert.deepEqual(await controlPlane.configsSince({ epoch, version: 1 }), { resync: false, epoch, configs: [] });
  assert.deepEqual(heard, [1]);
});

test("the core's history size bounds resuming", async () => {
  const controlPlane = createControlPlane({ store: createMemoryStore(), token: "t", configHistorySize: 1 });
  const doc: unknown = JSON.parse(readFileSync(MINIMAL, "utf8"));
  for (let n = 0; n < 3; n++) await controlPlane.publishConfig(doc);
  const epoch = await controlPlane.configEpoch();
  assert.deepEqual(await controlPlane.configsSince({ epoch, version: 1 }), { resync: true });
  assert.equal((await controlPlane.configsSince({ epoch, version: 2 })).resync, false);
});

test("the core checks gateway requests against its token", () => {
  const controlPlane = createControlPlane({ store: createMemoryStore(), token: "right" });
  const headers = { "kaiak-protocol": "2", "kaiak-instance": "gw-1" };
  assert.deepEqual(controlPlane.checkGatewayRequest({ ...headers, authorization: "Bearer right" }), {
    ok: true,
    instance: "gw-1",
  });
  const wrong = controlPlane.checkGatewayRequest({ ...headers, authorization: "Bearer wrong" });
  assert.ok(!wrong.ok && wrong.error.code === "unauthorized");
});

test("a listener's failure goes to the host's handler, not to the publish", async () => {
  const failures: number[] = [];
  const controlPlane = createControlPlane({
    store: createMemoryStore(),
    token: "t",
    onListenerError: (_error, event) => {
      assert.equal(event.type, "config-published");
      if (event.type === "config-published") failures.push(event.published.version);
    },
  });
  controlPlane.onConfigPublished(() => {
    throw new Error("broken");
  });
  const result = await controlPlane.publishConfig(JSON.parse(readFileSync(MINIMAL, "utf8")));
  assert.ok(result.ok);
  assert.deepEqual(failures, [1]);
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
  const intake = await controlPlane.acceptUsageBatch("gw-1", JSON.parse(readFileSync(MIXED_BATCH, "utf8")));
  assert.ok(intake.ok);
  assert.equal(intake.ack.totals.live_gateways, 0);
  assert.deepEqual(intake.ack.totals, await controlPlane.totals("gw-1"));
  assert.equal((await controlPlane.recentRecords()).length, 1);
  assert.deepEqual(events, ["totals-changed"]);
});

test("the live set's size is the live-gateway count in totals and acks", async () => {
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
  const intake = await controlPlane.acceptUsageBatch("gw-1", JSON.parse(readFileSync(MIXED_BATCH, "utf8")));
  assert.ok(intake.ok);
  assert.equal(intake.ack.totals.live_gateways, 2);

  now += 1_000;
  assert.deepEqual((await controlPlane.expireSilentGateways("manual")).expired, ["gw-1", "gw-2"]);
  assert.equal((await controlPlane.totals("gw-1"))?.live_gateways, 0);
  assert.deepEqual(events, ["gateways-changed", "gateways-changed", "gateways-changed"]);
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
  assert.equal((await controlPlane.totals("gw-1"))?.counted_through, null);
});

describe("one process per store", () => {
  const cores: ControlPlane[] = [];
  afterEach(async () => {
    for (const core of cores.splice(0)) await core.stop();
  });
  const core = (store: ControlPlaneStore, options: Partial<ControlPlaneOptions> = {}): ControlPlane => {
    const created = createControlPlane({ store, token: "t", ...options });
    cores.push(created);
    return created;
  };

  test("a second core on the store refuses to start until the first stops", async () => {
    const store = createMemoryStore();
    const first = core(store, { controlPlaneId: "1".repeat(32) });
    const second = core(store, { controlPlaneId: "2".repeat(32) });
    await first.start();
    await first.start(); // started already: nothing more
    await assert.rejects(second.start(), {
      code: "store-lease-held",
      message: /holder 1{32}.*run one control-plane process per store/,
    });
    await first.stop();
    await second.start();
  });

  test("a lease its holder stopped renewing expires", async () => {
    let now = 0;
    const store = createMemoryStore();
    await store.acquireLease("crashed", now, now + 30_000);
    const next = core(store, { clock: () => now });
    await assert.rejects(next.start(), { code: "store-lease-held" });
    now = 30_000;
    await next.start();
  });

  test("the core renews its lease and hears when it is lost", async () => {
    const store = createMemoryStore();
    const lost: unknown[] = [];
    const renewed = new EventEmitter();
    const watched: ControlPlaneStore = {
      ...store,
      async acquireLease(holder, now, expiresAt) {
        const result = await store.acquireLease(holder, now, expiresAt);
        renewed.emit("attempt", result);
        return result;
      },
    };
    const running = core(watched, { storeLeaseTtlMs: 30, onStoreLeaseLost: (error) => lost.push(error) });
    await running.start();
    const [first] = (await once(renewed, "attempt")) as [{ ok: boolean }];
    assert.ok(first.ok, "the holder renews its lease");

    // Another process took the store (this one stalled past its lease).
    await store.releaseLease((await leaseHolder(store)) ?? "");
    await store.acquireLease("intruder", Date.now(), Date.now() + 60_000);
    const [attempt] = (await once(renewed, "attempt")) as [{ ok: boolean }];
    assert.equal(attempt.ok, false);
    await new Promise((resolve) => setImmediate(resolve));
    assert.ok(lost.length > 0, "the loss is reported");
    assert.equal((lost[0] as { code?: string }).code, "store-lease-held");
  });

  test("the lease TTL must allow a renewal before it runs out", () => {
    for (const storeLeaseTtlMs of [0, 2, 1.5]) {
      assert.throws(() => createControlPlane({ store: createMemoryStore(), token: "t", storeLeaseTtlMs }), {
        code: "store-lease-ttl-invalid",
      });
    }
  });
});

// The holder of the store's lease, read by asking under a holder that cannot hold it.
async function leaseHolder(store: ControlPlaneStore): Promise<string | undefined> {
  const probe = await store.acquireLease("probe", 0, 0);
  return probe.ok ? undefined : probe.lease.holder;
}
