import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import path from "node:path";
import { afterEach, describe, test } from "node:test";

import type { Config } from "../config/index.ts";
import type { ExpirySweepRun } from "../gateways/index.ts";
import type { UsageBatch } from "../messages/index.ts";
import { INSTANCE_HEADER, PROTOCOL_HEADER, PROTOCOL_VERSION } from "../protocol/index.ts";
import { createMemoryStore } from "../storage/index.ts";
import type { ControlPlaneStore, ForgetGateway, StoreChangeListener } from "../storage/index.ts";
import { configNumbered, fixture, gatewayStatus, testCore, usageBatch } from "../test-support/index.ts";

import { createControlPlane } from "./index.ts";
import type { ControlPlane, ControlPlaneOptions, ListenerEvent } from "./index.ts";

const FIXTURES = path.resolve(import.meta.dirname, "../../../../protocol/fixtures");
const MINIMAL = path.join(FIXTURES, "config/valid/minimal.json");
const FULL = path.join(FIXTURES, "config/valid/full.json");
const MIXED_BATCH = path.join(FIXTURES, "messages/usage-batch/valid/mixed-groups.json");
const READY_STATUS = path.join(FIXTURES, "messages/status/valid/ready.json");

const NOW = Date.UTC(2026, 9, 7, 12, 30);
const INSTANCE = "gw-1";

// A batch from INSTANCE in epoch epoch…epoch: one record of group g counting tokens
// input tokens and cost nano-USD, at NOW unless at says otherwise.
function oneRecordBatch(
  epoch: string,
  sequence: number,
  { tokens, cost, at = NOW }: { tokens: number; cost: number; at?: number },
): UsageBatch {
  return usageBatch({ instance: INSTANCE, epoch: epoch.repeat(32), sequence }, [
    {
      record_id: `${epoch}${sequence}`.padEnd(32, "0"),
      request_id: `req-${epoch}${sequence}`,
      gateway_instance: INSTANCE,
      key_id: "key",
      groups: ["g"],
      model: "m",
      deployment: { backend: "b", model: "m" },
      units: { tokens_in: tokens },
      cost_nano_usd: cost,
      gateway_time: new Date(at).toISOString(),
    },
  ]);
}

// A store whose `method` waits, on its first call, until released.
function held<K extends keyof ControlPlaneStore>(store: ControlPlaneStore, method: K) {
  const entered = Promise.withResolvers<void>();
  const release = Promise.withResolvers<void>();
  let first = true;
  const original = store[method] as (...args: unknown[]) => Promise<unknown>;
  const wrapped = {
    ...store,
    [method]: async (...args: unknown[]) => {
      if (first) {
        first = false;
        entered.resolve();
        await release.promise;
      }
      return original.apply(store, args);
    },
  } as ControlPlaneStore;
  return { store: wrapped, entered: entered.promise, release: () => release.resolve() };
}

const settle = () => new Promise<void>((resolve) => setImmediate(resolve));

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
  const headers = { [PROTOCOL_HEADER]: String(PROTOCOL_VERSION), [INSTANCE_HEADER]: "gw-1" };
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

// Totals list every window with usage, whatever the current config limits, so a
// gateway still running an older config — it rejected the new one — keeps the spend of
// a limit the new one dropped.
test("totals keep listing a window the current config no longer limits", async () => {
  const cp = testCore({ clock: () => NOW });
  const running = configNumbered(1);
  running.global.limits = [{ type: "usd_per_month", value: 1 }];
  assert.ok((await cp.publishConfig(running)).ok);
  assert.ok((await cp.acceptUsageBatch(INSTANCE, oneRecordBatch("a", 1, { tokens: 100, cost: 100 }))).ok);
  const dropped = configNumbered(2);
  dropped.global.limits = [];
  assert.ok((await cp.publishConfig(dropped)).ok);
  const totals = await cp.totals(INSTANCE);
  assert.equal(totals.windows.find((window) => window.group === undefined && window.type === "usd_per_month")?.used, "100");
});

// The parents rule is checked against the text the publish writes, not the caller's
// object, which the host may change while the publish waits on the store.
test("a publish checks the parents rule against what it writes, whatever the caller does to its object meanwhile", async () => {
  const store = createMemoryStore();
  const entered = Promise.withResolvers<void>();
  const release = Promise.withResolvers<void>();
  let hold = false;
  const core = testCore({
    store: {
      ...store,
      async currentConfig() {
        if (hold) {
          hold = false;
          entered.resolve();
          await release.promise;
        }
        return store.currentConfig();
      },
    },
  });
  try {
    const current = fixture("config/valid/minimal.json") as Config;
    current.groups = { me: { parent: "a" }, a: {}, b: {} };
    assert.ok((await core.publishConfig(current)).ok);
    const edit = structuredClone(current);
    edit.groups!["me"]!.parent = "b";
    hold = true;
    const pending = core.publishConfig(edit);
    await entered.promise;
    // The host edits its object back while the publish waits on the store.
    edit.groups!["me"]!.parent = "a";
    release.resolve();
    const result = await pending;
    assert.equal(result.ok, false, "the move to b that the publish captured is refused");
    assert.equal(result.ok === false && result.issues[0]?.code, "group-parent-changed");
    assert.equal(JSON.parse((await core.currentConfig())!.text).groups.me.parent, "a");
  } finally {
    release.resolve();
    await core.stop();
  }
});

// Dropping past windows is housekeeping; a store failing it never turns a counted batch
// into an error (the gateway would resend a counted batch), and the failure is reported
// with the sweep run that tried it.
test("a store failing to drop past windows never fails a batch; the sweep reports it", async () => {
  const store = createMemoryStore();
  const failing: ControlPlaneStore = {
    ...store,
    async dropPastWindowTotals() {
      throw new Error("prune failed");
    },
  };
  const runs: ExpirySweepRun[] = [];
  const core = testCore({ clock: () => NOW, store: failing, onExpirySweep: (run) => runs.push(run) });
  try {
    const intake = await core.acceptUsageBatch(INSTANCE, oneRecordBatch("a", 1, { tokens: 10, cost: 0 }));
    assert.ok(intake.ok, `the batch is answered: ${JSON.stringify(intake)}`);
    assert.equal(intake.ok && intake.outcome, "first");
    await assert.rejects(core.expireSilentGateways("test"), /prune failed/);
    assert.equal(runs.length, 1);
    assert.equal(runs[0]?.ok, false);
  } finally {
    await core.stop();
  }
});

// A sweep forgets gateways in instance order, so two sweeps over a database never lock
// the same records in opposite orders.
test("a sweep forgets gateways in instance order", async () => {
  const store = createMemoryStore();
  const forgets: ForgetGateway[][] = [];
  let clock = NOW;
  const core = testCore({
    clock: () => clock,
    store: {
      ...store,
      async forgetGateways(gateways) {
        forgets.push(gateways);
        return store.forgetGateways(gateways);
      },
    },
  });
  try {
    for (const instance of ["gw-c", "gw-a", "gw-b"]) {
      assert.ok((await core.acceptStatus(instance, gatewayStatus(instance))).ok);
    }
    clock += 2 * 3_600_000;
    await core.expireSilentGateways("test");
    assert.deepEqual(
      forgets.map((call) => call.map((gateway) => gateway.instance)),
      [["gw-a", "gw-b", "gw-c"]],
    );
  } finally {
    await core.stop();
  }
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
  // A core whose clock reads NOW unless options say otherwise.
  const coreAtNow = (store: ControlPlaneStore, options: Partial<ControlPlaneOptions> = {}): ControlPlane =>
    core(store, { clock: () => NOW, ...options });
  const minimal = (): unknown => fixture("config/valid/minimal.json");
  // One token at one nano-USD.
  const tiny = { tokens: 1, cost: 1 };

  // One record for carol, costing `cost` nano-USD, from `instance`.
  const carolBatch = (instance: string, sequence: number, cost: number): unknown =>
    usageBatch({ instance, epoch: "e".repeat(32), sequence }, [
      {
        record_id: `${instance}-${sequence}`.padEnd(32, "0").replace(/[^0-9a-f]/g, "0"),
        request_id: `req-${instance}-${sequence}`,
        gateway_instance: instance,
        key_id: "k-carol",
        groups: ["users", "carol"],
        model: "gpt-4.1-mini",
        deployment: { backend: "b", model: "gpt-4.1-mini" },
        units: { tokens_in: 1 },
        cost_nano_usd: cost,
        gateway_time: "2026-09-24T10:29:00Z",
      },
    ]);
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

  // A gateway process died with its batch write stalled, and its replacement (same
  // instance, new epoch) got B/1 counted before the stalled A/1 committed.
  // counted_through names both epochs, so the new process sees B/1 covered.
  test("counted_through covers the acknowledged epoch after an older epoch's late write", async () => {
    const store = createMemoryStore();
    const entered = Promise.withResolvers<void>();
    const release = Promise.withResolvers<void>();
    const stalled = coreAtNow({
      ...store,
      async saveCountedBatch(...args) {
        entered.resolve();
        await release.promise;
        return store.saveCountedBatch(...args);
      },
    });
    const replacement = coreAtNow(store);
    const old = stalled.acceptUsageBatch(INSTANCE, oneRecordBatch("a", 1, { tokens: 100, cost: 100 }));
    await entered.promise;
    assert.ok((await replacement.acceptUsageBatch(INSTANCE, oneRecordBatch("b", 1, { tokens: 100, cost: 100 }))).ok);
    release.resolve();
    assert.ok((await old).ok);
    const totals = await replacement.totals(INSTANCE);
    assert.equal(totals.windows.find((window) => window.group === undefined && window.type === "tokens_per_hour")?.used, "200");
    assert.deepEqual(
      [...totals.counted_through].sort((x, y) => (x.epoch < y.epoch ? -1 : 1)),
      [
        { epoch: "a".repeat(32), sequence: 1 },
        { epoch: "b".repeat(32), sequence: 1 },
      ],
    );
  });

  // An older status whose first record read is delayed past a newer status's write is
  // dropped, not written over it.
  test("a delayed first status read does not overwrite a newer status", async () => {
    const store = createMemoryStore();
    const entered = Promise.withResolvers<void>();
    const release = Promise.withResolvers<void>();
    const delayed = coreAtNow({
      ...store,
      async gateway(instance) {
        entered.resolve();
        await release.promise;
        return store.gateway(instance);
      },
    });
    const newer = coreAtNow(store, { clock: () => NOW + 1000 });
    const pending = delayed.acceptStatus(INSTANCE, gatewayStatus(INSTANCE, { started_at: "2026-10-07T10:00:00Z" }));
    await entered.promise;
    assert.ok((await newer.acceptStatus(INSTANCE, gatewayStatus(INSTANCE, { started_at: "2026-10-07T12:00:00Z" }))).ok);
    release.resolve();
    assert.ok((await pending).ok);
    assert.equal((await store.gateway(INSTANCE))?.status.started_at, "2026-10-07T12:00:00Z");
  });

  // A host listener throwing when a config read failed goes to onListenerError, and
  // later deliveries go on.
  test("a throwing delivery-failed listener does not stop later deliveries", async () => {
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
    const reported: ListenerEvent["type"][] = [];
    const reader = coreAtNow(flaky, { deliveryRetryDelaysMs: [], onListenerError: (_error, event) => reported.push(event.type) });
    const writer = coreAtNow(base);
    reader.onDeliveryFailed(() => {
      throw new Error("broken host listener");
    });
    const heard: string[] = [];
    reader.onConfigPublished((published) => heard.push(published.hash));
    failNext = true;
    assert.ok((await writer.publishConfig(configNumbered(1))).ok);
    for (let i = 0; i < 50 && reported.length === 0; i += 1) await settle();
    assert.deepEqual(reported, ["delivery-failed"]);
    const next = await writer.publishConfig(configNumbered(2));
    assert.ok(next.ok);
    for (let i = 0; i < 50 && heard.length === 0; i += 1) await settle();
    assert.deepEqual(heard, [next.published.hash]);
  });

  // A write stalled on one core, while the gateway's resend was counted by another and
  // the gateway moved to a new batch epoch, is a duplicate.
  test("a stalled write of an older epoch's batch is not counted again after a new epoch", async () => {
    const store = createMemoryStore();
    const stalled = held(store, "saveCountedBatch");
    const a = coreAtNow(stalled.store);
    const b = coreAtNow(store);
    assert.ok((await b.publishConfig(minimal())).ok);
    const old = oneRecordBatch("a", 1, tiny);
    const inProgress = a.acceptUsageBatch(INSTANCE, old);
    await stalled.entered;
    assert.ok((await b.acceptUsageBatch(INSTANCE, old)).ok);
    assert.ok((await b.acceptUsageBatch(INSTANCE, oneRecordBatch("b", 1, tiny))).ok);
    stalled.release();
    const late = await inProgress;
    assert.ok(late.ok);
    assert.equal(late.outcome, "duplicate");
    assert.equal((await store.recentRecords(100)).length, 2, "two batches, two records");
  });

  // The crash variant: the stalled batch was never counted elsewhere, so it is counted
  // once, and the newer epoch's batches are still known as counted.
  test("a stalled first write of an older epoch's batch is counted once, newer epochs kept", async () => {
    const store = createMemoryStore();
    const stalled = held(store, "saveCountedBatch");
    const a = coreAtNow(stalled.store);
    const b = coreAtNow(store);
    assert.ok((await b.publishConfig(minimal())).ok);
    const inProgress = a.acceptUsageBatch(INSTANCE, oneRecordBatch("a", 1, tiny));
    await stalled.entered;
    assert.ok((await b.acceptUsageBatch(INSTANCE, oneRecordBatch("b", 1, tiny))).ok);
    stalled.release();
    assert.ok((await inProgress).ok);
    const resend = await b.acceptUsageBatch(INSTANCE, oneRecordBatch("b", 1, tiny));
    assert.ok(resend.ok);
    assert.equal(resend.outcome, "duplicate", "the newer epoch's batch is still known as counted");
    assert.equal((await store.recentRecords(100)).length, 2);
  });

  // A sweep that read a gateway's record before it was forgotten and recreated must not
  // forget the recreated record.
  test("a stale forget does not drop a gateway forgotten and joined again meanwhile", async () => {
    const store = createMemoryStore();
    let now = NOW;
    const stalled = held(store, "forgetGateways");
    const a = coreAtNow(stalled.store, { clock: () => now });
    const b = coreAtNow(store, { clock: () => now });
    assert.ok((await b.acceptStatus(INSTANCE, gatewayStatus(INSTANCE, { started_at: "2026-10-07T12:00:00.000Z" }))).ok);
    now += 3_600_001;
    const sweep = a.expireSilentGateways("held");
    await stalled.entered;
    await b.expireSilentGateways("other-core");
    assert.ok((await b.acceptStatus(INSTANCE, gatewayStatus(INSTANCE, { started_at: "2026-10-07T13:30:00.000Z" }))).ok);
    assert.equal(await b.liveGateways(), 1);
    stalled.release();
    await sweep;
    assert.equal(await b.liveGateways(), 1, "the recreated gateway stays live");
  });

  // The old process's last status, written late by one core after another core stored
  // the restarted process's status, is older than the stored one: dropped, no conflict.
  test("a late status from a replaced process does not raise a conflict or win", async () => {
    const store = createMemoryStore();
    let nowA = NOW;
    let nowB = NOW;
    const stalled = held(store, "saveGateway");
    const a = coreAtNow(stalled.store, { clock: () => nowA });
    const b = coreAtNow(store, { clock: () => nowB });
    const old = "2026-10-07T11:00:00.000Z";
    const restarted = "2026-10-07T12:29:00.000Z";
    const late = a.acceptStatus(INSTANCE, gatewayStatus(INSTANCE, { started_at: old }));
    await stalled.entered;
    nowB += 1_000;
    assert.ok((await b.acceptStatus(INSTANCE, gatewayStatus(INSTANCE, { started_at: restarted }))).ok);
    nowA += 2_000;
    stalled.release();
    const intake = await late;
    assert.ok(intake.ok);
    assert.equal(intake.conflictStarted, false);
    const [stored] = await b.gateways();
    assert.equal(stored?.status.started_at, restarted, "the newer status stays stored");
    assert.equal(stored?.conflict, undefined);
  });

  // A read of the current config that fails after a notification is retried; the config
  // reaches this core's listeners.
  test("a config read that fails once after a publish is retried and delivered", async () => {
    const store = createMemoryStore();
    let failures = 1;
    const flaky: ControlPlaneStore = {
      ...store,
      async currentConfig() {
        if (failures > 0) {
          failures -= 1;
          throw new Error("temporary database read failure");
        }
        return store.currentConfig();
      },
    };
    const a = coreAtNow(flaky, { deliveryRetryDelaysMs: [1, 1, 1] });
    const b = coreAtNow(store);
    const delivered: string[] = [];
    a.onConfigPublished((published) => delivered.push(published.hash));
    const published = await b.publishConfig(minimal());
    assert.ok(published.ok);
    for (let i = 0; i < 50 && delivered.length === 0; i += 1) await new Promise((resolve) => setTimeout(resolve, 2));
    assert.deepEqual(delivered, [published.published.hash]);
  });

  // A read that keeps failing is announced, so the host can end the streams that would
  // otherwise keep the old config.
  test("a config read that keeps failing is announced once the retries run out", async () => {
    const store = createMemoryStore();
    const failure = new Error("database down");
    const failing: ControlPlaneStore = {
      ...store,
      async currentConfig() {
        throw failure;
      },
    };
    const a = coreAtNow(failing, { deliveryRetryDelaysMs: [1, 1] });
    const b = coreAtNow(store);
    const announced: unknown[] = [];
    a.onDeliveryFailed((error) => announced.push(error));
    assert.ok((await b.publishConfig(minimal())).ok);
    for (let i = 0; i < 50 && announced.length === 0; i += 1) await new Promise((resolve) => setTimeout(resolve, 2));
    assert.deepEqual(announced, [failure]);
  });

  // A totals read that started before an hour boundary and returned after a batch
  // counted past it lists the window the batch was counted in.
  test("a totals read across an hour boundary lists the windows current after the read", async () => {
    const store = createMemoryStore();
    const boundary = Date.UTC(2026, 9, 7, 13);
    let now = boundary - 1;
    const stalled = held(store, "totalsSnapshot");
    const a = coreAtNow(stalled.store, { clock: () => now });
    const b = coreAtNow(store, { clock: () => now });
    const pending = a.totals(INSTANCE);
    await stalled.entered;
    now = boundary + 1;
    assert.ok((await b.acceptUsageBatch(INSTANCE, oneRecordBatch("a", 1, { ...tiny, at: now }))).ok);
    stalled.release();
    const late = await pending;
    assert.deepEqual(late, await b.totals(INSTANCE));
    assert.ok(
      late.windows.some((window) => window.type === "tokens_per_hour" && window.window_start === "2026-10-07T13:00:00Z"),
      "the batch's hour window is listed",
    );
  });

  // A store whose change channel reconnected announces a catch-up; a config published
  // while the channel was down then reaches the listeners.
  test("a catch-up from the store delivers what was missed while its channel was down", async () => {
    const store = createMemoryStore();
    let channelUp = true;
    const subscribers: StoreChangeListener[] = [];
    const reconnecting: ControlPlaneStore = {
      ...store,
      subscribe(listener) {
        subscribers.push(listener);
        return store.subscribe((change) => {
          if (channelUp) listener(change);
        });
      },
    };
    const a = coreAtNow(reconnecting);
    const b = coreAtNow(store);
    const configs: string[] = [];
    let totalsHeard = 0;
    let liveChanges = 0;
    a.onConfigPublished((published) => configs.push(published.hash));
    a.onTotalsChanged(() => (totalsHeard += 1));
    a.onGatewaysChanged((change) => (liveChanges += change.liveChanged ? 1 : 0));
    channelUp = false;
    const published = await b.publishConfig(minimal());
    assert.ok(published.ok);
    await settle();
    assert.deepEqual(configs, []);
    channelUp = true;
    for (const listener of subscribers) listener({ type: "catch-up" });
    for (let i = 0; i < 50 && configs.length === 0; i += 1) await settle();
    assert.deepEqual(configs, [published.published.hash]);
    assert.ok(totalsHeard >= 1, "totals listeners hear of a possible change");
    assert.ok(liveChanges >= 1, "gateway listeners hear of a possible live-set change");
  });

  // stop releases the core's store subscription; start takes it again and catches up on
  // what changed meanwhile.
  test("stop releases the store subscription; start takes it again and catches up", async () => {
    const store = createMemoryStore();
    let subscribed = 0;
    const counting: ControlPlaneStore = {
      ...store,
      subscribe(listener) {
        subscribed += 1;
        const unsubscribe = store.subscribe(listener);
        return () => {
          subscribed -= 1;
          unsubscribe();
        };
      },
    };
    const a = coreAtNow(counting);
    const b = coreAtNow(store);
    const configs: string[] = [];
    a.onConfigPublished((published) => configs.push(published.hash));
    await a.start();
    assert.equal(subscribed, 1);
    await a.stop();
    assert.equal(subscribed, 0, "a stopped core holds no store subscription");
    const published = await b.publishConfig(minimal());
    assert.ok(published.ok);
    await a.start();
    assert.equal(subscribed, 1);
    for (let i = 0; i < 50 && configs.length === 0; i += 1) await settle();
    assert.deepEqual(configs, [published.published.hash]);
    await a.stop();
  });
});
