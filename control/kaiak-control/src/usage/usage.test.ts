import assert from "node:assert/strict";
import { readdirSync, readFileSync } from "node:fs";
import path from "node:path";
import { describe, test } from "node:test";

import type { Config } from "../config/index.ts";
import type { BeforeSave } from "../config-versions/index.ts";
import { validateTotals } from "../messages/index.ts";
import type { Totals, UsageBatch, UsageRecord } from "../messages/index.ts";
import { createMemoryStore } from "../storage/index.ts";
import type { ControlPlaneStore } from "../storage/index.ts";

import { createUsage } from "./index.ts";
import type { Usage, UsageIntake, UsageOptions } from "./index.ts";

const FIXTURES = path.resolve(import.meta.dirname, "../../../../protocol/fixtures");
const INSTANCE = "gw-1";
const EPOCH_A = "a".repeat(32);
const EPOCH_B = "b".repeat(32);
const CONTROL_PLANE = "c".repeat(32);
const MAX_RECORD_COST = 2 ** 53 - 1;

// 2026-09-24T10:30:00Z: hour window 10:00, month window September.
const T0 = Date.UTC(2026, 8, 24, 10, 30);
const HOUR_10 = "2026-09-24T10:00:00Z";
const SEPTEMBER = "2026-09-01T00:00:00Z";

// full.json, with bob also given a tokens_per_hour limit for qwen3-32b alone: a model
// set his parent's child_defaults have not, so it follows the defaults.
function fullConfig(): Config {
  const config = JSON.parse(readFileSync(path.join(FIXTURES, "config/valid/full.json"), "utf8")) as Config;
  const bob = config.groups?.["bob"];
  assert.ok(bob?.limits, "full.json has bob with limits");
  bob.limits.push({ type: "tokens_per_hour", value: 1000, models: ["qwen3-32b"] });
  return config;
}

let recordCount = 0;

// units: tokens_in, tokens_cached, tokens_out, tokens_reasoning. gatewayTime defaults to
// a moment in T0's hour.
function record(
  groups: string[],
  model: string,
  units: [number, number, number, number],
  cost: number,
  gatewayTime = "2026-09-24T10:29:59.123456789Z",
): UsageRecord {
  recordCount += 1;
  const [tokensIn, tokensCached, tokensOut, tokensReasoning] = units;
  return {
    record_id: recordCount.toString(16).padStart(32, "0"),
    request_id: `req-${recordCount}`,
    gateway_instance: INSTANCE,
    key_id: "k-test",
    groups,
    model,
    deployment: { backend: "b", model },
    units: { tokens_in: tokensIn, tokens_cached: tokensCached, tokens_out: tokensOut, tokens_reasoning: tokensReasoning },
    cost_nano_usd: cost,
    estimated: false,
    partial: false,
    gateway_time: gatewayTime,
  };
}

function batch(epoch: string, sequence: number, records: UsageRecord[]): UsageBatch {
  return { batch: { instance: INSTANCE, epoch, sequence }, records };
}

const EVAL = ["research", "eval-pipeline"];
const SUPPORT = ["support", "support-bot"];

function oneTokenRecord(): UsageRecord {
  return record(["users", "carol"], "qwen3-32b", [1, 0, 0, 0], 1);
}

interface Harness {
  usage: Usage;
  store: ControlPlaneStore;
  setTime(ms: number): void;
  // Stores config as the next version; beforeSave (a publish's, from Usage.publishing)
  // runs first, as the config-versions module runs it.
  publish(config: Config, beforeSave?: BeforeSave): Promise<void>;
  listenerErrors: unknown[];
}

async function harness(options: Partial<Omit<UsageOptions, "clock">> = {}): Promise<Harness> {
  const store = options.store ?? createMemoryStore();
  let now = T0;
  let version = 0;
  const listenerErrors: unknown[] = [];
  const usage = createUsage({
    store,
    clock: () => now,
    recentRecordsSize: 100,
    liveGateways: () => 2,
    onListenerError: (error) => listenerErrors.push(error),
    controlPlaneId: CONTROL_PLANE,
    ...options,
  });
  const publish = async (config: Config, beforeSave?: BeforeSave): Promise<void> => {
    const next = { version: version + 1, config, publishedAt: now };
    await beforeSave?.(await store.latestConfig(), next);
    version += 1;
    await store.saveConfig(next, 100);
  };
  await publish(fullConfig());
  return { usage, store, setTime: (ms) => (now = ms), publish, listenerErrors };
}

function acked(intake: UsageIntake): Extract<UsageIntake, { ok: true }> {
  assert.ok(intake.ok, `batch accepted: ${intake.ok ? "" : intake.error.message}`);
  return intake;
}

async function currentTotals(usage: Usage): Promise<Totals> {
  const totals = await usage.totals(INSTANCE);
  assert.ok(totals, "a config is published");
  assert.ok(validateTotals(totals).ok, "totals pass the totals schema and rules");
  return totals;
}

// One token counted toward carol's all-models hour limit, the one window it touches.
async function carolHourUsed(usage: Usage): Promise<string | undefined> {
  const totals = await currentTotals(usage);
  return totals.windows.find((window) => window.group === "carol" && window.type === "tokens_per_hour")?.used;
}

describe("aggregation", () => {
  test("a mixed batch counts into every applicable scope, limit and window", async () => {
    const { usage, store } = await harness();
    const records = [
      // global's USD limit covers only the gpt models; research's USD limit all;
      // eval-pipeline's hour limit only qwen3-32b.
      record(EVAL, "qwen3-32b", [800, 100, 300, 0], 5_000),
      record(EVAL, "gpt-4.1", [1000, 0, 500, 0], 7_000_000),
      // Group support has no limits; support-bot's per-minute limits are not counted.
      record(SUPPORT, "gpt-4.1-mini", [10, 0, 10, 0], 2_000),
      // alice: default hour limit, her own USD limit (same identity as the default's).
      record(["users", "alice"], "gpt-4.1-mini", [2048, 1024, 377, 0], 1_524_000),
      // bob: both hour limits cover qwen3-32b; reasoning is inside tokens_out. Cost 0
      // adds nothing to his USD limit.
      record(["users", "bob"], "qwen3-32b", [100, 0, 50, 20], 0),
      // Listed groups the config does not define are skipped; the rest and global count.
      record(["users", "ghost"], "gpt-4.1", [1, 1, 1, 0], 1_000),
      record(["research", "gone"], "bge-m3", [5, 0, 0, 0], 3),
    ];
    const intake = acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 1, records)));
    assert.equal(intake.outcome, "first");

    const totals = await currentTotals(usage);
    assert.deepEqual(totals, {
      revision: { control_plane: CONTROL_PLANE, sequence: 1 },
      config_epoch: await store.configEpoch(),
      config_version: 1,
      live_gateways: 2,
      counted_through: { epoch: EPOCH_A, sequence: 1 },
      windows: [
        { type: "usd_per_month", models: ["gpt-4.1", "gpt-4.1-mini"], window_start: SEPTEMBER, used: "8527000" },
        { group: "research", type: "usd_per_month", window_start: SEPTEMBER, used: "7005003" },
        { group: "eval-pipeline", type: "tokens_per_hour", models: ["qwen3-32b"], window_start: HOUR_10, used: "1200" },
        {
          group: "eval-pipeline",
          type: "usd_per_month",
          models: ["gpt-4.1", "gpt-4.1-mini"],
          window_start: SEPTEMBER,
          used: "7000000",
        },
        { group: "support-bot", type: "usd_per_month", window_start: SEPTEMBER, used: "2000" },
        { group: "alice", type: "tokens_per_hour", window_start: HOUR_10, used: "3449" },
        { group: "alice", type: "usd_per_month", window_start: SEPTEMBER, used: "1524000" },
        { group: "bob", type: "tokens_per_hour", window_start: HOUR_10, used: "150" },
        { group: "bob", type: "tokens_per_hour", models: ["qwen3-32b"], window_start: HOUR_10, used: "150" },
      ],
    });
    assert.deepEqual(intake.ack, { batch: { instance: INSTANCE, epoch: EPOCH_A, sequence: 1 }, totals });
  });

  test("a deep path counts toward every listed group with limits, and global", async () => {
    const { usage, publish } = await harness();
    const config = fullConfig();
    assert.ok(config.groups, "full.json has groups");
    const limits = (value: number) => [
      { type: "tokens_per_hour" as const, value },
      { type: "usd_per_month" as const, value },
    ];
    // Five levels; region, in the middle, has no limits of its own.
    Object.assign(config.groups, {
      team: { limits: limits(1_000_000) },
      project: { parent: "team", limits: limits(1_000_000) },
      region: { parent: "project" },
      env: { parent: "region", limits: limits(1_000_000) },
      workload: { parent: "env", limits: limits(1_000_000) },
    });
    await publish(config);

    const path = ["team", "project", "region", "env", "workload"];
    acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 1, [record(path, "gpt-4.1", [10, 0, 5, 0], 4_000)])));
    const levels = ["team", "project", "env", "workload"].flatMap((group) => [
      { group, type: "tokens_per_hour", window_start: HOUR_10, used: "15" },
      { group, type: "usd_per_month", window_start: SEPTEMBER, used: "4000" },
    ]);
    assert.deepEqual((await currentTotals(usage)).windows, [
      { type: "usd_per_month", models: ["gpt-4.1", "gpt-4.1-mini"], window_start: SEPTEMBER, used: "4000" },
      ...levels,
    ]);
  });

  test("a record whose last group was deleted counts toward its surviving listed ancestors and global", async () => {
    const { usage, publish } = await harness();
    const config = fullConfig();
    assert.ok(config.groups, "full.json has groups");
    delete config.groups["eval-pipeline"];
    delete config.keys["k-eval-ci"];
    await publish(config);

    acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 1, [record(EVAL, "gpt-4.1", [10, 0, 5, 0], 4_000)])));
    assert.deepEqual((await currentTotals(usage)).windows, [
      { type: "usd_per_month", models: ["gpt-4.1", "gpt-4.1-mini"], window_start: SEPTEMBER, used: "4000" },
      { group: "research", type: "usd_per_month", window_start: SEPTEMBER, used: "4000" },
    ]);
  });

  test("records count under the config in force when they arrive", async () => {
    const { usage, publish } = await harness();
    acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 1, [record(EVAL, "bge-m3", [1, 0, 0, 0], 10)])));

    // Version 2 drops research's limits: its window leaves the totals.
    const config = fullConfig();
    const research = config.groups?.["research"];
    assert.ok(research, "full.json has group research");
    delete research.limits;
    await publish(config);
    let totals = await currentTotals(usage);
    assert.equal(totals.config_version, 2);
    assert.equal(totals.windows.find((window) => window.group === "research"), undefined);

    // Version 3 brings the limit back: it holds what was counted while it existed.
    await publish(fullConfig());
    acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 2, [record(EVAL, "bge-m3", [1, 0, 0, 0], 5)])));
    totals = await currentTotals(usage);
    assert.equal(totals.config_version, 3);
    assert.equal(totals.windows.find((window) => window.group === "research")?.used, "15");
  });

  test("cost sums stay exact beyond 2^53 nano-USD", async () => {
    const { usage } = await harness();
    const records = [1, 2, 3].map(() => record(["users", "carol"], "gpt-4.1-mini", [0, 0, 0, 0], MAX_RECORD_COST));
    acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 1, records)));
    const totals = await currentTotals(usage);
    const carolUsd = totals.windows.find((window) => window.group === "carol" && window.type === "usd_per_month");
    assert.equal(carolUsd?.used, (3n * BigInt(MAX_RECORD_COST)).toString());
    assert.equal(carolUsd?.used, "27021597764222973");
  });

  test("a window past the 18-digit ceiling is reported at the ceiling", async () => {
    const { usage } = await harness();
    // 112 records at the per-record maximum pass 10^18 nano-USD.
    const records = Array.from({ length: 112 }, () => record(["users", "carol"], "gpt-4.1-mini", [0, 0, 0, 0], MAX_RECORD_COST));
    acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 1, records)));
    const totals = await currentTotals(usage);
    const carolUsd = totals.windows.find((window) => window.group === "carol" && window.type === "usd_per_month");
    assert.equal(carolUsd?.used, "999999999999999999");
  });

  test("no config published: the batch is refused with config-unavailable", async () => {
    const usage = createUsage({
      store: createMemoryStore(),
      clock: () => T0,
      recentRecordsSize: 10,
      liveGateways: () => 0,
      onListenerError: (error) => assert.fail(String(error)),
    });
    assert.equal(await usage.totals(INSTANCE), undefined);
    const intake = await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 1, [oneTokenRecord()]));
    assert.ok(!intake.ok);
    assert.deepEqual(intake.error, { code: "config-unavailable", message: "no config has been published yet", status: 503 });
  });
});

describe("model-set edits (D5)", () => {
  // A publish run in the totals' turn, as the core runs every publish.
  const publishIn = (h: Harness, config: Config): Promise<void> => h.usage.publishing((beforeSave) => h.publish(config, beforeSave));
  const globalUsd = async (usage: Usage): Promise<{ models?: string[]; used: string } | undefined> => {
    const window = (await currentTotals(usage)).windows.find((w) => w.group === undefined && w.type === "usd_per_month");
    return window && { ...(window.models && { models: window.models }), used: window.used };
  };

  test("a limit whose only change is its model set keeps its spend", async () => {
    const h = await harness();
    acked(await h.usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 1, [record(["users", "carol"], "gpt-4.1-mini", [1, 0, 0, 0], 700)])));
    assert.deepEqual(await globalUsd(h.usage), { models: ["gpt-4.1", "gpt-4.1-mini"], used: "700" });

    const edited = fullConfig();
    edited.global.limits = [{ type: "usd_per_month", value: 5000, models: ["gpt-4.1"] }];
    await publishIn(h, edited);
    assert.deepEqual(await globalUsd(h.usage), { models: ["gpt-4.1"], used: "700" });
    acked(await h.usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 2, [record(["users", "carol"], "gpt-4.1", [1, 0, 0, 0], 5)])));
    assert.deepEqual(await globalUsd(h.usage), { models: ["gpt-4.1"], used: "705" });

    // Back to both models within the month: the returning limit had 700 of its own;
    // it takes up the 705 its predecessor holds, not both.
    await publishIn(h, fullConfig());
    assert.deepEqual(await globalUsd(h.usage), { models: ["gpt-4.1", "gpt-4.1-mini"], used: "705" });
  });

  test("several predecessors carry the largest spend and are reported", async () => {
    const carried: unknown[] = [];
    const h = await harness({ onLimitCarriedOver: (carry) => carried.push(carry) });
    const split = fullConfig();
    split.global.limits = [
      { type: "usd_per_month", value: 5000, models: ["gpt-4.1"] },
      { type: "usd_per_month", value: 5000, models: ["gpt-4.1-mini"] },
    ];
    await publishIn(h, split);
    acked(
      await h.usage.acceptUsageBatch(
        INSTANCE,
        batch(EPOCH_A, 1, [record(["users", "carol"], "gpt-4.1", [1, 0, 0, 0], 300), record(["users", "carol"], "gpt-4.1-mini", [1, 0, 0, 0], 500)]),
      ),
    );
    const merged = fullConfig();
    merged.global.limits = [{ type: "usd_per_month", value: 5000, models: ["gpt-4.1", "gpt-4.1-mini"] }];
    carried.length = 0;
    await publishIn(h, merged);
    assert.deepEqual(await globalUsd(h.usage), { models: ["gpt-4.1", "gpt-4.1-mini"], used: "500" });
    assert.deepEqual(carried, [
      {
        type: "usd_per_month",
        models: ["gpt-4.1", "gpt-4.1-mini"],
        from: [["gpt-4.1"], ["gpt-4.1-mini"]],
        ambiguous: true,
      },
    ]);
  });
});

describe("model-set edits of group limits", () => {
  test("each group's limit carries its own spend; limits of other groups and other types stay", async () => {
    const carried: unknown[] = [];
    const h = await harness({ onLimitCarriedOver: (carry) => carried.push(carry) });
    acked(await h.usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 1, [record(["users", "carol"], "gpt-4.1-mini", [1, 0, 0, 0], 700)])));

    // The users' default USD limit narrows to gpt-4.1-mini: carol and bob take it from
    // child_defaults, alice overrides it with her own all-models limit.
    const edited = fullConfig();
    const defaults = edited.groups?.["users"]?.child_defaults;
    assert.ok(defaults?.limits, "full.json's users group has default limits");
    defaults.limits = defaults.limits.map((limit) =>
      limit.type === "usd_per_month" ? { ...limit, models: ["gpt-4.1-mini"] } : limit,
    );
    await h.usage.publishing((beforeSave) => h.publish(edited, beforeSave));

    const usd = (await currentTotals(h.usage)).windows.filter((window) => window.type === "usd_per_month");
    assert.deepEqual(usd, [
      { type: "usd_per_month", models: ["gpt-4.1", "gpt-4.1-mini"], window_start: SEPTEMBER, used: "700" },
      { group: "carol", type: "usd_per_month", models: ["gpt-4.1-mini"], window_start: SEPTEMBER, used: "700" },
    ]);
    assert.deepEqual(carried, [
      { group: "bob", type: "usd_per_month", models: ["gpt-4.1-mini"], from: [null], ambiguous: false },
      { group: "carol", type: "usd_per_month", models: ["gpt-4.1-mini"], from: [null], ambiguous: false },
    ]);
  });
});

describe("de-duplication", () => {
  test("a resent batch is acked again without counting", async () => {
    const { usage } = await harness();
    const first = batch(EPOCH_A, 1, [oneTokenRecord()]);
    const counted = acked(await usage.acceptUsageBatch(INSTANCE, first));
    const resent = acked(await usage.acceptUsageBatch(INSTANCE, structuredClone(first)));
    assert.equal(resent.outcome, "duplicate");
    assert.deepEqual(resent.previous, first.batch);
    assert.deepEqual(resent.ack, counted.ack);
    assert.equal(await carolHourUsed(usage), "1");

    // Any sequence at or below the last counted one in the epoch is a duplicate.
    acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 2, [oneTokenRecord()])));
    const older = acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 1, [oneTokenRecord()])));
    assert.equal(older.outcome, "duplicate");
    assert.equal(await carolHourUsed(usage), "2");
  });

  test("two copies of a batch arriving together are counted once", async () => {
    const { usage } = await harness();
    const copy = batch(EPOCH_A, 1, [oneTokenRecord()]);
    const intakes = await Promise.all([
      usage.acceptUsageBatch(INSTANCE, copy),
      usage.acceptUsageBatch(INSTANCE, structuredClone(copy)),
    ]);
    assert.deepEqual(intakes.map((intake) => acked(intake).outcome).sort(), ["duplicate", "first"]);
    assert.equal(await carolHourUsed(usage), "1");
  });

  // The audit's reproduction (H12): two cores on one store, each serializing only its
  // own intake, take the same batch at once. The store's conditional write makes the
  // second one find the batch counted.
  test("two cores sharing one store count a batch once", async () => {
    const store = createMemoryStore();
    const first = await harness({ store });
    const second = await harness({ store });
    const copy = batch(EPOCH_A, 1, [oneTokenRecord()]);
    const intakes = await Promise.all([
      first.usage.acceptUsageBatch(INSTANCE, copy),
      second.usage.acceptUsageBatch(INSTANCE, structuredClone(copy)),
    ]);
    assert.deepEqual(intakes.map((intake) => acked(intake).outcome).sort(), ["duplicate", "first"]);
    assert.equal(await carolHourUsed(first.usage), "1");
    assert.equal((await store.recentRecords(10)).length, 1);
  });

  test("a batch that loses the conditional write is decided again against the one that won", async () => {
    const store = createMemoryStore();
    const first = await harness({ store });
    const second = await harness({ store });
    const [one, two] = await Promise.all([
      first.usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 1, [oneTokenRecord()])),
      second.usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 2, [oneTokenRecord()])),
    ]);
    // Both decided "first" against no batch; batch 1 was written first, so batch 2's
    // write found it and batch 2 became the next one after it.
    assert.deepEqual([acked(one).outcome, acked(two).outcome], ["first", "next"]);
    assert.deepEqual(acked(two).previous, { instance: INSTANCE, epoch: EPOCH_A, sequence: 1 });
    assert.equal(await carolHourUsed(first.usage), "2");
  });

  test("a new epoch is counted from any sequence", async () => {
    const { usage } = await harness();
    acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 5, [oneTokenRecord()])));
    const next = acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_B, 1, [oneTokenRecord()])));
    assert.equal(next.outcome, "new-epoch");
    assert.deepEqual(next.previous, { instance: INSTANCE, epoch: EPOCH_A, sequence: 5 });
    assert.equal(await carolHourUsed(usage), "2");
  });

  test("a gap in the sequence is counted", async () => {
    const { usage } = await harness();
    acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 1, [oneTokenRecord()])));
    assert.equal(acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 2, [oneTokenRecord()]))).outcome, "next");
    const gap = acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 5, [oneTokenRecord()])));
    assert.equal(gap.outcome, "gap");
    assert.equal(await carolHourUsed(usage), "3");
  });

  test("instances are de-duplicated separately", async () => {
    const { usage } = await harness();
    acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 1, [oneTokenRecord()])));
    const other: UsageBatch = {
      batch: { instance: "gw-2", epoch: EPOCH_A, sequence: 1 },
      records: [{ ...oneTokenRecord(), gateway_instance: "gw-2" }],
    };
    assert.equal(acked(await usage.acceptUsageBatch("gw-2", other)).outcome, "first");
    assert.equal(await carolHourUsed(usage), "2");
  });
});

describe("windows by the control plane's clock", () => {
  test("an hour rollover starts a new hour window; the month window carries on", async () => {
    const { usage, store, setTime } = await harness();
    const lastMs = Date.UTC(2026, 8, 24, 10, 59, 59, 999);
    setTime(lastMs);
    acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 1, [record(["users", "carol"], "gpt-4.1-mini", [4, 0, 0, 0], 7)])));

    // The next batch, settled in the new hour, lands in it.
    setTime(Date.UTC(2026, 8, 24, 11));
    let totals = await currentTotals(usage);
    assert.deepEqual(
      totals.windows.filter((window) => window.group === "carol"),
      [{ group: "carol", type: "usd_per_month", window_start: SEPTEMBER, used: "7" }],
    );
    const intake = acked(
      await usage.acceptUsageBatch(
        INSTANCE,
        batch(EPOCH_A, 2, [record(["users", "carol"], "gpt-4.1-mini", [3, 0, 0, 0], 1, "2026-09-24T11:00:00Z")]),
      ),
    );
    totals = await currentTotals(usage);
    assert.deepEqual(intake.ack.totals, totals);
    assert.deepEqual(
      totals.windows.filter((window) => window.group === "carol"),
      [
        { group: "carol", type: "tokens_per_hour", window_start: "2026-09-24T11:00:00Z", used: "3" },
        { group: "carol", type: "usd_per_month", window_start: SEPTEMBER, used: "8" },
      ],
    );

    // The 10:00 window is the previous one now: it stays (late records still count
    // there) until the next hour's first batch lets the store drop it.
    const at10 = { hourStart: Date.UTC(2026, 8, 24, 10), monthStart: Date.UTC(2026, 8, 1) };
    assert.deepEqual(
      (await store.currentWindowTotals(at10)).map((total) => total.type).sort(),
      ["tokens_per_hour", "usd_per_month", "usd_per_month"],
    );
    setTime(Date.UTC(2026, 8, 24, 12));
    acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 3, [record(["users", "carol"], "gpt-4.1-mini", [1, 0, 0, 0], 1, "2026-09-24T12:00:00Z")])));
    // Left: September's windows for carol and global (still current).
    assert.deepEqual(
      (await store.currentWindowTotals(at10)).map((total) => total.type),
      ["usd_per_month", "usd_per_month"],
    );
  });

  // D4 and [A]'s scenario: a workload using 60% of its hour limit through a 2-hour
  // outage. After recovery its backlog counts in the hours it was settled in when
  // they are the current or previous hour, so the current hour is not charged with
  // both; usage older than the previous window counts in the current one.
  test("a record counts in its gateway_time window when that is the current or previous one", async () => {
    const { usage, store, setTime } = await harness();
    const carol = (units: number, at: string): UsageRecord =>
      record(["users", "carol"], "gpt-4.1-mini", [units, 0, 0, 0], units, at);
    // Recovery at 12:30: a backlog from 10:xx (two hours back), 11:xx (previous), 12:xx.
    setTime(Date.UTC(2026, 8, 24, 12, 30));
    acked(
      await usage.acceptUsageBatch(
        INSTANCE,
        batch(EPOCH_A, 1, [carol(600, "2026-09-24T10:40:00Z"), carol(600, "2026-09-24T11:40:00Z"), carol(100, "2026-09-24T12:10:00Z")]),
      ),
    );
    const totals = await currentTotals(usage);
    assert.deepEqual(
      totals.windows.filter((window) => window.group === "carol"),
      [
        // 12:xx's 100 plus 10:xx's 600, older than the previous hour.
        { group: "carol", type: "tokens_per_hour", window_start: "2026-09-24T12:00:00Z", used: "700" },
        { group: "carol", type: "usd_per_month", window_start: SEPTEMBER, used: "1300" },
      ],
    );
    const at11 = await store.currentWindowTotals({ hourStart: Date.UTC(2026, 8, 24, 11), monthStart: Date.UTC(2026, 7, 1) });
    assert.deepEqual(
      at11.filter((total) => total.group === "carol").map((total) => [total.type, total.used]),
      [["tokens_per_hour", 600n]],
    );
  });

  test("a record from last month counts in last month's window, not the new one", async () => {
    const { usage, store, setTime } = await harness();
    setTime(Date.UTC(2026, 9, 1, 0, 5));
    acked(
      await usage.acceptUsageBatch(
        INSTANCE,
        batch(EPOCH_A, 1, [
          record(["users", "carol"], "gpt-4.1-mini", [0, 0, 0, 0], 50, "2026-09-30T23:59:59Z"),
          record(["users", "carol"], "gpt-4.1-mini", [0, 0, 0, 0], 7, "2026-08-31T12:00:00Z"),
          record(["users", "carol"], "gpt-4.1-mini", [0, 0, 0, 0], 20, "2026-10-01T00:01:00Z"),
        ]),
      ),
    );
    // October holds its own record and August's, older than the previous month.
    assert.deepEqual(
      (await currentTotals(usage)).windows.filter((window) => window.group === "carol"),
      [{ group: "carol", type: "usd_per_month", window_start: "2026-10-01T00:00:00Z", used: "27" }],
    );
    const september = await store.currentWindowTotals({ hourStart: Date.UTC(2026, 8, 30, 23), monthStart: Date.UTC(2026, 8, 1) });
    assert.deepEqual(
      september.filter((total) => total.group === "carol").map((total) => [total.type, total.used]),
      [["usd_per_month", 50n]],
    );
  });

  test("a month rollover starts a new month window", async () => {
    const { usage, store, setTime } = await harness();
    setTime(Date.UTC(2026, 8, 30, 23, 59, 59, 999));
    acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 1, [record(["users", "carol"], "gpt-4.1-mini", [0, 0, 0, 0], 50)])));

    setTime(Date.UTC(2026, 9, 1));
    assert.deepEqual((await currentTotals(usage)).windows, []);
    acked(
      await usage.acceptUsageBatch(
        INSTANCE,
        batch(EPOCH_A, 2, [record(["users", "carol"], "gpt-4.1-mini", [0, 0, 0, 0], 20, "2026-10-01T00:00:00Z")]),
      ),
    );
    assert.deepEqual((await currentTotals(usage)).windows, [
      { type: "usd_per_month", models: ["gpt-4.1", "gpt-4.1-mini"], window_start: "2026-10-01T00:00:00Z", used: "20" },
      { group: "carol", type: "usd_per_month", window_start: "2026-10-01T00:00:00Z", used: "20" },
    ]);
    // September is the previous month now: its windows stay while late records can
    // still count there.
    const september = await store.currentWindowTotals({ hourStart: Date.UTC(2026, 8, 30, 23), monthStart: Date.UTC(2026, 8, 1) });
    assert.deepEqual(
      september.map((total) => [total.group ?? "global", total.type, total.used]).sort(),
      [
        ["carol", "usd_per_month", 50n],
        ["global", "usd_per_month", 50n],
      ],
    );
  });

  test("dropping past windows can be run by hand", async () => {
    const { usage, store, setTime } = await harness();
    acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 1, [oneTokenRecord()])));
    const t0Windows = { hourStart: Date.UTC(2026, 8, 24, 10), monthStart: Date.UTC(2026, 8, 1) };
    assert.equal((await store.currentWindowTotals(t0Windows)).length, 2);
    // Two months on: September is older than the previous month.
    setTime(Date.UTC(2026, 10, 2));
    await usage.dropPastWindows();
    assert.deepEqual(await store.currentWindowTotals(t0Windows), []);
  });
});

describe("validation", () => {
  test("a batch whose instance is not the requester's is refused", async () => {
    const { usage } = await harness();
    const intake = await usage.acceptUsageBatch("gw-2", batch(EPOCH_A, 1, [oneTokenRecord()]));
    assert.ok(!intake.ok);
    assert.equal(intake.error.code, "instance-mismatch");
    assert.equal(intake.error.status, 400);
    assert.deepEqual((await currentTotals(usage)).windows, []);
  });

  test("the shared invalid batch fixtures are refused with their codes", async () => {
    const { usage } = await harness();
    const dir = path.join(FIXTURES, "messages/usage-batch/invalid");
    const cases = JSON.parse(readFileSync(path.join(dir, "cases.json"), "utf8")) as Record<
      string,
      { kind: "schema" | "semantic"; code?: string }
    >;
    const files = readdirSync(dir).filter((name) => name.endsWith(".json") && name !== "cases.json");
    assert.ok(files.length > 0, "fixtures found");
    for (const file of files) {
      const entry = cases[file];
      assert.ok(entry, `${file} has a cases.json entry`);
      const intake = await usage.acceptUsageBatch(INSTANCE, JSON.parse(readFileSync(path.join(dir, file), "utf8")));
      assert.ok(!intake.ok, `${file} is refused`);
      assert.equal(intake.error.status, 400, file);
      assert.equal(intake.error.code, entry.kind === "schema" ? "usage-batch-invalid" : entry.code, file);
    }
    assert.deepEqual((await currentTotals(usage)).windows, []);
  });
});

describe("recent records and listeners", () => {
  test("recent records are bounded, newest first, with their receipt time", async () => {
    const { usage, setTime } = await harness({ recentRecordsSize: 3 });
    const first = [oneTokenRecord(), oneTokenRecord()];
    const second = [oneTokenRecord(), oneTokenRecord()];
    acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 1, first)));
    setTime(T0 + 5_000);
    acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 2, second)));
    // A duplicate adds no records.
    acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 2, second)));
    assert.deepEqual(await usage.recentRecords(), [
      { receivedAt: T0 + 5_000, record: second[1] },
      { receivedAt: T0 + 5_000, record: second[0] },
      { receivedAt: T0, record: first[1] },
    ]);
  });

  test("totals listeners hear of counted batches only; a failing one is isolated", async () => {
    const { usage, listenerErrors } = await harness();
    let heard = 0;
    const unsubscribe = usage.onTotalsChanged(() => (heard += 1));
    usage.onTotalsChanged(() => {
      throw new Error("broken listener");
    });
    const first = batch(EPOCH_A, 1, [oneTokenRecord()]);
    acked(await usage.acceptUsageBatch(INSTANCE, first));
    acked(await usage.acceptUsageBatch(INSTANCE, first));
    assert.equal(heard, 1);
    assert.equal(listenerErrors.length, 1);
    assert.equal(await carolHourUsed(usage), "1");

    unsubscribe();
    unsubscribe();
    acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 2, [oneTokenRecord()])));
    assert.equal(heard, 1);
    assert.equal(listenerErrors.length, 2);
  });

  test("a bad recent records size is refused", () => {
    assert.throws(
      () =>
        createUsage({
          store: createMemoryStore(),
          clock: () => T0,
          recentRecordsSize: -1,
          liveGateways: () => 0,
          onListenerError: () => undefined,
        }),
      { code: "recent-records-size-invalid" },
    );
  });
});

describe("revision and counted_through", () => {
  test("the sequence moves on with every counted batch and every other change, not with a duplicate", async () => {
    const { usage } = await harness();
    assert.deepEqual((await currentTotals(usage)).revision, { control_plane: CONTROL_PLANE, sequence: 0 });
    const first = batch(EPOCH_A, 1, [oneTokenRecord()]);
    const intake = acked(await usage.acceptUsageBatch(INSTANCE, first));
    assert.equal(intake.ack.totals.revision.sequence, 1);
    const duplicate = acked(await usage.acceptUsageBatch(INSTANCE, first));
    assert.equal(duplicate.ack.totals.revision.sequence, 1);
    usage.totalsChanged();
    assert.equal((await currentTotals(usage)).revision.sequence, 2);
  });

  test("each process has its own control-plane ID", async () => {
    const options = { store: createMemoryStore(), clock: () => T0, recentRecordsSize: 1, liveGateways: () => 1 };
    const a = createUsage({ ...options, onListenerError: () => undefined });
    const b = createUsage({ ...options, onListenerError: () => undefined });
    await options.store.saveConfig({ version: 1, config: fullConfig(), publishedAt: T0 }, 1);
    const [ta, tb] = [await a.totals(INSTANCE), await b.totals(INSTANCE)];
    assert.match(ta?.revision.control_plane ?? "", /^[0-9a-f]{32}$/);
    assert.notEqual(ta?.revision.control_plane, tb?.revision.control_plane);
  });

  test("counted_through is the recipient instance's last counted batch", async () => {
    const { usage } = await harness();
    assert.equal((await currentTotals(usage)).counted_through, null);
    acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 3, [oneTokenRecord()])));
    const other = { ...oneTokenRecord(), gateway_instance: "gw-2" };
    const intake = acked(
      await usage.acceptUsageBatch("gw-2", { batch: { instance: "gw-2", epoch: EPOCH_B, sequence: 7 }, records: [other] }),
    );
    assert.deepEqual(intake.ack.totals.counted_through, { epoch: EPOCH_B, sequence: 7 });
    assert.deepEqual((await currentTotals(usage)).counted_through, { epoch: EPOCH_A, sequence: 3 });
    assert.equal((await usage.totals("gw-3"))?.counted_through, null);
  });

  test("totals are a consistent snapshot: a batch counted during a read waits for it", async () => {
    const memory = createMemoryStore();
    let releaseRead: () => void = () => undefined;
    let gated = true;
    const store: ControlPlaneStore = {
      ...memory,
      async currentWindowTotals(current) {
        if (gated) {
          gated = false;
          await new Promise<void>((resolve) => (releaseRead = resolve));
        }
        return memory.currentWindowTotals(current);
      },
    };
    const { usage } = await harness({ store });
    // The read has taken its revision and counted_through and waits on the windows.
    const reading = usage.totals(INSTANCE);
    await new Promise((resolve) => setImmediate(resolve));
    const counting = usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 1, [oneTokenRecord()]));
    await new Promise((resolve) => setImmediate(resolve));
    releaseRead();
    const before = await reading;
    const ack = acked(await counting).ack.totals;
    assert.deepEqual([before?.revision.sequence, before?.counted_through, before?.windows], [0, null, []]);
    assert.equal(ack.revision.sequence, 1);
    assert.deepEqual(ack.counted_through, { epoch: EPOCH_A, sequence: 1 });
    assert.ok(ack.windows.some((window) => window.group === "carol"), "the ack's windows hold the batch");
  });
});
