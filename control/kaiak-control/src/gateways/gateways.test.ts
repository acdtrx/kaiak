import assert from "node:assert/strict";
import { once, EventEmitter } from "node:events";
import { readFileSync } from "node:fs";
import path from "node:path";
import { test } from "node:test";

import type { GatewayStatus } from "../messages/index.ts";
import { createMemoryStore } from "../storage/index.ts";
import type { ControlPlaneStore } from "../storage/index.ts";

import { createGateways } from "./index.ts";
import type { ExpirySweepRun, GatewaysChange, GatewaysOptions } from "./index.ts";

const STATUS_FIXTURES = path.resolve(import.meta.dirname, "../../../../protocol/fixtures/messages/status");
const READY = JSON.parse(readFileSync(path.join(STATUS_FIXTURES, "valid/ready.json"), "utf8")) as GatewayStatus;

function statusOf(instance: string, fields: Partial<GatewayStatus> = {}): GatewayStatus {
  return { ...READY, instance, ...fields };
}

function setup(options: Partial<GatewaysOptions> = {}) {
  let now = Date.UTC(2026, 8, 24, 10, 0);
  const runs: ExpirySweepRun[] = [];
  const changes: GatewaysChange[] = [];
  const gateways = createGateways({
    store: createMemoryStore(),
    clock: () => now,
    liveTimeoutMs: 30_000,
    forgetAfterMs: 600_000,
    batchCursorRetentionMs: 3_600_000,
    sweepIntervalMs: 5_000,
    onExpirySweep: (run) => runs.push(run),
    onListenerError: (error) => assert.fail(`listener failed: ${String(error)}`),
    ...options,
  });
  gateways.onGatewaysChanged((change) => changes.push(change));
  return {
    gateways,
    runs,
    changes,
    advance(ms: number) {
      now += ms;
    },
    now: () => now,
  };
}

test("a status stores the latest report with its receipt time and joins the live set", async () => {
  const { gateways, changes, advance, now } = setup();
  const first = await gateways.acceptStatus("gw-1", statusOf("gw-1", { state: "starting" }));
  assert.deepEqual(first, { ok: true, joined: true, conflictStarted: false });
  advance(10_000);
  const second = await gateways.acceptStatus("gw-1", statusOf("gw-1"));
  assert.deepEqual(second, { ok: true, joined: false, conflictStarted: false });

  assert.deepEqual(await gateways.gateways(), [{ instance: "gw-1", status: statusOf("gw-1"), receivedAt: now(), live: true }]);
  assert.equal(await gateways.liveGateways(), 1);
  assert.deepEqual(changes, [{ liveChanged: true }, { liveChanged: false }]);
});

test("gateways are listed by instance", async () => {
  const { gateways } = setup();
  for (const instance of ["gw-b", "gw-c", "gw-a"]) await gateways.acceptStatus(instance, statusOf(instance));
  assert.deepEqual(
    (await gateways.gateways()).map((gateway) => gateway.instance),
    ["gw-a", "gw-b", "gw-c"],
  );
  assert.equal(await gateways.liveGateways(), 3);
});

test("a gateway silent for the live timeout leaves the live set at the next sweep", async () => {
  const { gateways, changes, runs, advance, now } = setup();
  await gateways.acceptStatus("gw-1", statusOf("gw-1"));
  await gateways.acceptStatus("gw-2", statusOf("gw-2"));
  advance(20_000);
  await gateways.acceptStatus("gw-2", statusOf("gw-2"));

  advance(9_999);
  const early = await gateways.expireSilentGateways("manual");
  assert.deepEqual(early, { trigger: "manual", at: now(), ok: true, expired: [], forgotten: [], batchCursorsDropped: [] });
  assert.equal(await gateways.liveGateways(), 2);

  advance(1);
  const due = await gateways.expireSilentGateways("manual");
  assert.deepEqual(due.expired, ["gw-1"]);
  assert.equal(await gateways.liveGateways(), 1);
  // Still listed, as not live, for a host's view.
  assert.deepEqual(
    (await gateways.gateways()).map(({ instance, live }) => ({ instance, live })),
    [
      { instance: "gw-1", live: false },
      { instance: "gw-2", live: true },
    ],
  );
  assert.deepEqual(runs, [early, due]);
  // Joins, gw-2's repeat, then the leave; the early sweep changed nothing.
  assert.deepEqual(changes, [{ liveChanged: true }, { liveChanged: true }, { liveChanged: false }, { liveChanged: true }]);

  // A status brings it back.
  const back = await gateways.acceptStatus("gw-1", statusOf("gw-1"));
  assert.ok(back.ok && back.joined);
  assert.equal(await gateways.liveGateways(), 2);
});

test("an expired gateway is forgotten after the forget delay", async () => {
  const { gateways, advance } = setup();
  await gateways.acceptStatus("gw-1", statusOf("gw-1"));
  advance(30_000);
  await gateways.expireSilentGateways("manual");
  advance(570_000);
  const run = await gateways.expireSilentGateways("manual");
  assert.deepEqual({ expired: run.expired, forgotten: run.forgotten }, { expired: [], forgotten: ["gw-1"] });
  assert.deepEqual(await gateways.gateways(), []);
});

test("a gateway never swept is expired and forgotten by the same run", async () => {
  const { gateways, changes, advance } = setup();
  await gateways.acceptStatus("gw-1", statusOf("gw-1"));
  advance(600_000);
  const run = await gateways.expireSilentGateways("manual");
  assert.deepEqual({ expired: run.expired, forgotten: run.forgotten }, { expired: ["gw-1"], forgotten: ["gw-1"] });
  assert.equal(await gateways.liveGateways(), 0);
  assert.deepEqual(changes.at(-1), { liveChanged: true });
});

test("a draining gateway still counts as live until it stops reporting", async () => {
  const { gateways, advance } = setup();
  await gateways.acceptStatus("gw-1", statusOf("gw-1"));
  await gateways.acceptStatus("gw-1", statusOf("gw-1", { state: "draining" }));
  assert.equal(await gateways.liveGateways(), 1);
  advance(30_000);
  await gateways.expireSilentGateways("manual");
  assert.equal(await gateways.liveGateways(), 0);
});

test("the scheduled sweep runs on its timer with the schedule trigger until stopped", async () => {
  const sweeps = new EventEmitter();
  const { gateways, advance } = setup({
    sweepIntervalMs: 5,
    onExpirySweep: (run) => sweeps.emit("run", run),
  });
  await gateways.acceptStatus("gw-1", statusOf("gw-1"));
  advance(30_000);
  gateways.startExpirySweep();
  gateways.startExpirySweep();
  try {
    const [run] = (await once(sweeps, "run")) as [ExpirySweepRun];
    assert.equal(run.trigger, "schedule");
    assert.ok(run.ok);
    assert.deepEqual(run.expired, ["gw-1"]);
  } finally {
    gateways.stopExpirySweep();
  }
  assert.equal(await gateways.liveGateways(), 0);
});

test("a failed sweep is reported with its trigger and the next one runs", async () => {
  const store = createMemoryStore();
  let fail = true;
  const failing: ControlPlaneStore = {
    ...store,
    async gateways() {
      if (fail) throw Object.assign(new Error("store down"), { code: "store-down" });
      return store.gateways();
    },
  };
  const { gateways, runs } = setup({ store: failing });
  await assert.rejects(gateways.expireSilentGateways("manual"), { code: "store-down" });
  assert.equal(runs.length, 1);
  assert.ok(runs[0] && !runs[0].ok && runs[0].trigger === "manual");
  fail = false;
  assert.ok((await gateways.expireSilentGateways("manual")).ok);
});

test("a status is validated and must name the requester's instance", async () => {
  const { gateways, changes } = setup();
  const invalid = (file: string): unknown =>
    JSON.parse(readFileSync(path.join(STATUS_FIXTURES, "invalid", file), "utf8"));
  const cases = [
    { instance: "gw-1", doc: invalid("state-unknown.json"), code: "status-invalid" },
    { instance: "gw-1", doc: invalid("started-at-not-a-day.json"), code: "timestamp-invalid" },
    { instance: "gw-2", doc: statusOf("gw-1"), code: "instance-mismatch" },
  ];
  for (const { instance, doc, code } of cases) {
    const intake = await gateways.acceptStatus(instance, doc);
    assert.ok(!intake.ok, code);
    assert.equal(intake.error.code, code);
    assert.equal(intake.error.status, 400);
  }
  assert.deepEqual(await gateways.gateways(), []);
  assert.deepEqual(changes, []);
});

test("a rejection below the applied version is accepted (a restarted control plane counts from 1)", async () => {
  const { gateways } = setup();
  const status = statusOf("gw-1", {
    applied_config_version: 42,
    last_rejection: { version: 1, codes: ["key-group-unknown"] },
  });
  assert.ok((await gateways.acceptStatus("gw-1", status)).ok);
  assert.deepEqual((await gateways.gateways())[0]?.status, status);
});

test("a restart (a new start time once) is not a conflict", async () => {
  const { gateways, advance } = setup();
  await gateways.acceptStatus("gw-1", statusOf("gw-1", { started_at: "2026-09-24T09:00:00Z" }));
  advance(2_000);
  const restarted = await gateways.acceptStatus("gw-1", statusOf("gw-1", { started_at: "2026-09-24T10:00:02Z" }));
  assert.ok(restarted.ok && !restarted.conflictStarted);
  advance(10_000);
  await gateways.acceptStatus("gw-1", statusOf("gw-1", { started_at: "2026-09-24T10:00:02Z" }));
  assert.equal((await gateways.gateways())[0]?.conflict, undefined);
});

test("statuses alternating between two start times flag a conflict, which clears once one stops", async () => {
  const { gateways, advance, now } = setup();
  const a = statusOf("gw-1", { started_at: "2026-09-24T09:00:00Z" });
  const b = statusOf("gw-1", { started_at: "2026-09-24T09:30:00Z" });
  await gateways.acceptStatus("gw-1", a);
  advance(5_000);
  await gateways.acceptStatus("gw-1", b);
  advance(5_000);
  const flipped = await gateways.acceptStatus("gw-1", a);
  assert.ok(flipped.ok && flipped.conflictStarted);
  assert.deepEqual((await gateways.gateways())[0]?.conflict, { reason: "started-at-alternating", detectedAt: now() });

  advance(5_000);
  const again = await gateways.acceptStatus("gw-1", b);
  assert.ok(again.ok && !again.conflictStarted, "an ongoing conflict is not raised again");
  assert.equal((await gateways.gateways())[0]?.conflict?.detectedAt, now());

  // Only b reports from now on: the flag stays for the live timeout, then clears.
  advance(20_000);
  await gateways.acceptStatus("gw-1", b);
  assert.ok((await gateways.gateways())[0]?.conflict);
  advance(10_000);
  await gateways.acceptStatus("gw-1", b);
  assert.equal((await gateways.gateways())[0]?.conflict, undefined);
});

test("a start time coming back after the live timeout is not a conflict", async () => {
  const { gateways, advance } = setup();
  const a = statusOf("gw-1", { started_at: "2026-09-24T09:00:00Z" });
  const b = statusOf("gw-1", { started_at: "2026-09-24T09:30:00Z" });
  await gateways.acceptStatus("gw-1", a);
  advance(1_000);
  await gateways.acceptStatus("gw-1", b);
  advance(30_000);
  const late = await gateways.acceptStatus("gw-1", a);
  assert.ok(late.ok && !late.conflictStarted);
});

test("a throwing listener goes to the handler; the status stands", async () => {
  const failures: GatewaysChange[] = [];
  const { gateways } = setup({ onListenerError: (_error, change) => failures.push(change) });
  gateways.onGatewaysChanged(() => {
    throw new Error("broken");
  });
  const intake = await gateways.acceptStatus("gw-1", statusOf("gw-1"));
  assert.ok(intake.ok);
  assert.deepEqual(failures, [{ liveChanged: true }]);
  assert.equal(await gateways.liveGateways(), 1);
});

test("options must be positive integers", () => {
  assert.throws(() => setup({ liveTimeoutMs: 0 }), { code: "gateways-option-invalid" });
  assert.throws(() => setup({ sweepIntervalMs: 1.5 }), { code: "gateways-option-invalid" });
  assert.throws(() => setup({ batchCursorRetentionMs: -1 }), { code: "gateways-option-invalid" });
});
