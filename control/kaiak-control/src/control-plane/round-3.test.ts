// Regression tests from the third pre-merge review (docs/reviews/2026-10-07/AUDIT-3.md)
// on the core without HTTP: the publish checks what it writes, dropping past windows
// never fails a counted batch, and sweeps forget gateways in one order.

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";

import type { Config } from "../config/index.ts";
import type { ExpirySweepRun } from "../gateways/index.ts";
import type { GatewayStatus, UsageBatch } from "../messages/index.ts";
import { createMemoryStore } from "../storage/index.ts";
import type { ControlPlaneStore, ForgetGateway } from "../storage/index.ts";

import { createControlPlane } from "./index.ts";

const TOKEN = "round-3";
const NOW = Date.UTC(2026, 9, 7, 12, 30);
const fixture = (path: string): unknown =>
  JSON.parse(readFileSync(new URL(`../../../../protocol/fixtures/${path}`, import.meta.url), "utf8"));

function usageBatch(instance: string): UsageBatch {
  return {
    batch: { instance, epoch: "a".repeat(32), sequence: 1 },
    records: [
      {
        record_id: "1".padStart(32, "0"),
        request_id: "req",
        gateway_instance: instance,
        key_id: "key",
        groups: ["g"],
        model: "m",
        deployment: { backend: "b", model: "m" },
        units: { tokens_in: 10, tokens_cached: 0, tokens_cache_write: 0, tokens_out: 0, tokens_reasoning: 0 },
        cost_nano_usd: 0,
        estimated: false,
        partial: false,
        gateway_time: new Date(NOW).toISOString(),
      },
    ],
  };
}

// 3M3 ([C] C4): the parents rule is checked against the text the publish writes, not
// the caller's object, which the host may change while the publish waits on the store.
test("a publish checks the parents rule against what it writes, whatever the caller does to its object meanwhile", async () => {
  const store = createMemoryStore();
  const entered = Promise.withResolvers<void>();
  const release = Promise.withResolvers<void>();
  let hold = false;
  const core = createControlPlane({
    token: TOKEN,
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

// 3L1 ([R] R3-L1): dropping past windows is housekeeping; a store failing it never
// turns a counted batch into an error (the gateway would resend a counted batch), and
// the failure is reported with the sweep run that tried it.
test("a store failing to drop past windows never fails a batch; the sweep reports it", async () => {
  const store = createMemoryStore();
  const failing: ControlPlaneStore = {
    ...store,
    async dropPastWindowTotals() {
      throw new Error("prune failed");
    },
  };
  const runs: ExpirySweepRun[] = [];
  const core = createControlPlane({ token: TOKEN, clock: () => NOW, store: failing, onExpirySweep: (run) => runs.push(run) });
  try {
    const intake = await core.acceptUsageBatch("gw-1", usageBatch("gw-1"));
    assert.ok(intake.ok, `the batch is answered: ${JSON.stringify(intake)}`);
    assert.equal(intake.ok && intake.outcome, "first");
    await assert.rejects(core.expireSilentGateways("test"), /prune failed/);
    assert.equal(runs.length, 1);
    assert.equal(runs[0]?.ok, false);
  } finally {
    await core.stop();
  }
});

// 3M6 ([K] K3-L1): a sweep forgets gateways in instance order, so two sweeps over a
// database never lock the same records in opposite orders.
test("a sweep forgets gateways in instance order", async () => {
  const store = createMemoryStore();
  const forgets: ForgetGateway[][] = [];
  let clock = NOW;
  const core = createControlPlane({
    token: TOKEN,
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
    const ready = fixture("messages/status/valid/ready.json") as GatewayStatus;
    for (const instance of ["gw-c", "gw-a", "gw-b"]) {
      assert.ok((await core.acceptStatus(instance, { ...ready, instance })).ok);
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
