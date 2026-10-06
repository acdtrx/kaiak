import assert from "node:assert/strict";
import { readdirSync, readFileSync } from "node:fs";
import path from "node:path";
import { describe, test } from "node:test";

import type { Config } from "../config/index.ts";
import { validateTotals } from "../messages/index.ts";
import type { Totals, UsageBatch, UsageRecord } from "../messages/index.ts";
import { createMemoryStore } from "../storage/index.ts";
import type { ControlPlaneStore, CurrentWindows, WindowTotal } from "../storage/index.ts";

import { createUsage } from "./index.ts";
import type { Usage, UsageIntake, UsageOptions } from "./index.ts";

const FIXTURES = path.resolve(import.meta.dirname, "../../../../protocol/fixtures");
const INSTANCE = "gw-1";
const EPOCH_A = "a".repeat(32);
const EPOCH_B = "b".repeat(32);
const MAX_RECORD_COST = 2 ** 53 - 1;

// 2026-09-24T10:30:00Z: hour window 10:00, month window September.
const T0 = Date.UTC(2026, 8, 24, 10, 30);
const HOUR_10 = "2026-09-24T10:00:00Z";
const SEPTEMBER = "2026-09-01T00:00:00Z";

function fullConfig(): Config {
  return JSON.parse(readFileSync(path.join(FIXTURES, "config/valid/full.json"), "utf8")) as Config;
}

// What the store holds for the windows current at `current`, whatever the config limits.
async function storedWindows(store: ControlPlaneStore, current: CurrentWindows): Promise<WindowTotal[]> {
  return (await store.totalsSnapshot(current)).windows;
}

let recordCount = 0;

// units: tokens_in, tokens_cached, tokens_cache_write, tokens_out, tokens_reasoning.
// gatewayTime defaults to a moment in T0's hour.
function record(
  groups: string[],
  model: string,
  units: [number, number, number, number, number],
  cost: number,
  gatewayTime = "2026-09-24T10:29:59.123456789Z",
): UsageRecord {
  recordCount += 1;
  const [tokensIn, tokensCached, tokensCacheWrite, tokensOut, tokensReasoning] = units;
  return {
    record_id: recordCount.toString(16).padStart(32, "0"),
    request_id: `req-${recordCount}`,
    gateway_instance: INSTANCE,
    key_id: "k-test",
    groups,
    model,
    deployment: { backend: "b", model },
    units: {
      tokens_in: tokensIn,
      tokens_cached: tokensCached,
      tokens_cache_write: tokensCacheWrite,
      tokens_out: tokensOut,
      tokens_reasoning: tokensReasoning,
    },
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
  return record(["users", "carol"], "qwen3-32b", [1, 0, 0, 0, 0], 1);
}

interface Harness {
  usage: Usage;
  store: ControlPlaneStore;
  setTime(ms: number): void;
  // Stores config as the next version.
  publish(config: Config): Promise<void>;
  listenerErrors: unknown[];
}

async function harness(options: Partial<Omit<UsageOptions, "clock">> = {}): Promise<Harness> {
  const store = options.store ?? createMemoryStore();
  let now = T0;
  const listenerErrors: unknown[] = [];
  const usage = createUsage({
    store,
    clock: () => now,
    recentRecordsSize: 100,
    onListenerError: (error) => listenerErrors.push(error),
    ...options,
  });
  const publish = async (config: Config): Promise<void> => {
    const latest = (await store.latestConfig())?.version;
    const written = await store.publishConfig({ version: (latest ?? 0) + 1, config, publishedAt: now }, latest, 100);
    assert.ok(written.saved, "the harness's publish is stored");
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

// carol's hour window, which her default tokens_per_hour limit lists.
async function carolHourUsed(usage: Usage): Promise<string | undefined> {
  const totals = await currentTotals(usage);
  return totals.windows.find((window) => window.group === "carol" && window.type === "tokens_per_hour")?.used;
}

describe("aggregation", () => {
  test("a mixed batch counts into every scope on each path; the totals list the limited windows", async () => {
    const { usage, store } = await harness();
    const records = [
      // Every model counts toward every scope on the path. Token windows leave out
      // input read from the cache (eval-pipeline's 100, alice's 1024).
      record(EVAL, "qwen3-32b", [800, 100, 0, 300, 0], 5_000),
      record(EVAL, "gpt-4.1", [1000, 0, 0, 500, 0], 7_000_000),
      // Group support has no limits: counted, not listed. support-bot's per-minute
      // limits are each gateway's own.
      record(SUPPORT, "gpt-4.1-mini", [10, 0, 0, 10, 0], 2_000),
      // alice: the default hour limit and her own USD limit, which replaces the
      // default's of the same type.
      record(["users", "alice"], "gpt-4.1-mini", [2048, 1024, 0, 377, 0], 1_524_000),
      // bob: his own hour limit; reasoning is inside tokens_out. Cost 0 leaves his USD
      // window empty, so it is not listed.
      record(["users", "bob"], "qwen3-32b", [100, 0, 0, 50, 20], 0),
      // Listed groups the config does not define count too, unlisted; the rest and
      // global count as always.
      record(["users", "ghost"], "gpt-4.1", [1, 1, 0, 1, 0], 1_000),
      record(["research", "gone"], "bge-m3", [5, 0, 0, 0, 0], 3),
    ];
    const intake = acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 1, records)));
    assert.equal(intake.outcome, "first");

    const totals = await currentTotals(usage);
    assert.deepEqual(totals, {
      // The publish moved the store's sequence to 1, the batch to 2.
      revision: 2,
      config_epoch: await store.configEpoch(),
      config_version: 1,
      live_gateways: 0,
      counted_through: { epoch: EPOCH_A, sequence: 1 },
      windows: [
        { type: "usd_per_month", window_start: SEPTEMBER, used: "8532003" },
        { group: "research", type: "usd_per_month", window_start: SEPTEMBER, used: "7005003" },
        { group: "eval-pipeline", type: "tokens_per_hour", window_start: HOUR_10, used: "2600" },
        { group: "eval-pipeline", type: "usd_per_month", window_start: SEPTEMBER, used: "7005000" },
        { group: "support-bot", type: "usd_per_month", window_start: SEPTEMBER, used: "2000" },
        { group: "alice", type: "tokens_per_hour", window_start: HOUR_10, used: "2425" },
        { group: "alice", type: "usd_per_month", window_start: SEPTEMBER, used: "1524000" },
        { group: "bob", type: "tokens_per_hour", window_start: HOUR_10, used: "150" },
      ],
    });
    assert.deepEqual(intake.ack, { batch: { instance: INSTANCE, epoch: EPOCH_A, sequence: 1 }, totals });
  });

  test("a token limit counts plain input, input written to the cache and output — not input read from it", async () => {
    const { usage } = await harness();
    // 3 plain, 1024 read, 1009 written, 40 out (12 of them reasoning, already inside).
    const written = record(["users", "carol"], "gpt-4.1-mini", [3, 1024, 1009, 40, 12], 1);
    acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 1, [written])));
    assert.equal(await carolHourUsed(usage), "1052"); // 3 + 1009 + 40
  });

  test("a deep path counts toward every listed group, and global; the limited ones are listed", async () => {
    const { usage, store, publish } = await harness();
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
    acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 1, [record(path, "gpt-4.1", [10, 0, 0, 5, 0], 4_000)])));
    const levels = ["team", "project", "env", "workload"].flatMap((group) => [
      { group, type: "tokens_per_hour", window_start: HOUR_10, used: "15" },
      { group, type: "usd_per_month", window_start: SEPTEMBER, used: "4000" },
    ]);
    assert.deepEqual((await currentTotals(usage)).windows, [
      { type: "usd_per_month", window_start: SEPTEMBER, used: "4000" },
      ...levels,
    ]);
    // region, with no limits, is counted all the same.
    const region = (await storedWindows(store, { hourStart: Date.UTC(2026, 8, 24, 10), monthStart: Date.UTC(2026, 8, 1) }))
      .filter((total) => total.group === "region")
      .map((total) => [total.type, total.used]);
    assert.deepEqual(region, [
      ["tokens_per_hour", 15n],
      ["usd_per_month", 4000n],
    ]);
  });

  test("a record whose last group was deleted is listed under its surviving ancestors and global", async () => {
    const { usage, publish } = await harness();
    const config = fullConfig();
    assert.ok(config.groups, "full.json has groups");
    delete config.groups["eval-pipeline"];
    delete config.keys["k-eval-ci"];
    await publish(config);

    acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 1, [record(EVAL, "gpt-4.1", [10, 0, 0, 5, 0], 4_000)])));
    assert.deepEqual((await currentTotals(usage)).windows, [
      { type: "usd_per_month", window_start: SEPTEMBER, used: "4000" },
      { group: "research", type: "usd_per_month", window_start: SEPTEMBER, used: "4000" },
    ]);
  });

  test("records count whatever the config's limits; a limit removed and added back shows all of it", async () => {
    const { usage, publish } = await harness();
    acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 1, [record(EVAL, "bge-m3", [1, 0, 0, 0, 0], 10)])));

    // Version 2 drops research's limits: its window leaves the totals, and keeps counting.
    const config = fullConfig();
    const research = config.groups?.["research"];
    assert.ok(research, "full.json has group research");
    delete research.limits;
    await publish(config);
    acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 2, [record(EVAL, "bge-m3", [1, 0, 0, 0, 0], 5)])));
    let totals = await currentTotals(usage);
    assert.equal(totals.config_version, 2);
    assert.equal(totals.windows.find((window) => window.group === "research"), undefined);

    // Version 3 brings the limit back: it holds everything counted in the window.
    await publish(fullConfig());
    totals = await currentTotals(usage);
    assert.equal(totals.config_version, 3);
    assert.equal(totals.windows.find((window) => window.group === "research")?.used, "15");
  });

  test("cost sums stay exact beyond 2^53 nano-USD", async () => {
    const { usage } = await harness();
    const records = [1, 2, 3].map(() => record(["users", "carol"], "gpt-4.1-mini", [0, 0, 0, 0, 0], MAX_RECORD_COST));
    acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 1, records)));
    const totals = await currentTotals(usage);
    const carolUsd = totals.windows.find((window) => window.group === "carol" && window.type === "usd_per_month");
    assert.equal(carolUsd?.used, (3n * BigInt(MAX_RECORD_COST)).toString());
    assert.equal(carolUsd?.used, "27021597764222973");
  });

  test("a window past the 18-digit ceiling is reported at the ceiling", async () => {
    const { usage } = await harness();
    // 112 records at the per-record maximum pass 10^18 nano-USD.
    const records = Array.from({ length: 112 }, () => record(["users", "carol"], "gpt-4.1-mini", [0, 0, 0, 0, 0], MAX_RECORD_COST));
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
      onListenerError: (error) => assert.fail(String(error)),
    });
    assert.equal(await usage.totals(INSTANCE), undefined);
    const intake = await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 1, [oneTokenRecord()]));
    assert.ok(!intake.ok);
    assert.deepEqual(intake.error, { code: "config-unavailable", message: "no config has been published yet", status: 503 });
  });
});

describe("edited limits", () => {
  const usdOf = async (usage: Usage, group?: string): Promise<string | undefined> =>
    (await currentTotals(usage)).windows.find((w) => w.group === group && w.type === "usd_per_month")?.used;

  test("a limit whose value changes keeps its window, globally and through child_defaults", async () => {
    const h = await harness();
    acked(await h.usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 1, [record(["users", "carol"], "gpt-4.1-mini", [1, 0, 0, 0, 0], 700)])));
    assert.equal(await usdOf(h.usage), "700");
    assert.equal(await usdOf(h.usage, "carol"), "700");

    const edited = fullConfig();
    edited.global.limits = [{ type: "usd_per_month", value: 9000 }];
    const defaults = edited.groups?.["users"]?.child_defaults;
    assert.ok(defaults?.limits, "full.json's users group has default limits");
    defaults.limits = defaults.limits.map((limit) => (limit.type === "usd_per_month" ? { ...limit, value: 50 } : limit));
    await h.publish(edited);
    assert.equal(await usdOf(h.usage), "700");
    assert.equal(await usdOf(h.usage, "carol"), "700");
  });

  test("a limit added mid-window starts with the window's usage so far", async () => {
    const h = await harness();
    acked(await h.usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 1, [record(SUPPORT, "gpt-4.1-mini", [1, 0, 0, 0, 0], 2_000)])));
    assert.equal(await usdOf(h.usage, "support"), undefined, "support has no limit yet");

    const config = fullConfig();
    const support = config.groups?.["support"];
    assert.ok(support, "full.json has group support");
    support.limits = [{ type: "usd_per_month", value: 100 }];
    await h.publish(config);
    assert.equal(await usdOf(h.usage, "support"), "2000");
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
    acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 1, [record(["users", "carol"], "gpt-4.1-mini", [4, 0, 0, 0, 0], 7)])));

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
        batch(EPOCH_A, 2, [record(["users", "carol"], "gpt-4.1-mini", [3, 0, 0, 0, 0], 1, "2026-09-24T11:00:00Z")]),
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
    // global, users and carol: each counted in its hour and its month.
    assert.deepEqual(
      (await storedWindows(store, at10)).map((total) => total.type).sort(),
      ["tokens_per_hour", "tokens_per_hour", "tokens_per_hour", "usd_per_month", "usd_per_month", "usd_per_month"],
    );
    setTime(Date.UTC(2026, 8, 24, 12));
    acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 3, [record(["users", "carol"], "gpt-4.1-mini", [1, 0, 0, 0, 0], 1, "2026-09-24T12:00:00Z")])));
    // Left: September's windows for global, users and carol (still current).
    assert.deepEqual(
      (await storedWindows(store, at10)).map((total) => total.type),
      ["usd_per_month", "usd_per_month", "usd_per_month"],
    );
  });

  // D4 and [A]'s scenario: a workload using 60% of its hour limit through a 2-hour
  // outage. After recovery its backlog counts in the hours it was settled in when
  // they are the current or previous hour, so the current hour is not charged with
  // both; usage older than the previous window counts in the current one.
  test("a record counts in its gateway_time window when that is the current or previous one", async () => {
    const { usage, store, setTime } = await harness();
    const carol = (units: number, at: string): UsageRecord =>
      record(["users", "carol"], "gpt-4.1-mini", [units, 0, 0, 0, 0], units, at);
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
    const at11 = await storedWindows(store, { hourStart: Date.UTC(2026, 8, 24, 11), monthStart: Date.UTC(2026, 7, 1) });
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
          record(["users", "carol"], "gpt-4.1-mini", [0, 0, 0, 0, 0], 50, "2026-09-30T23:59:59Z"),
          record(["users", "carol"], "gpt-4.1-mini", [0, 0, 0, 0, 0], 7, "2026-08-31T12:00:00Z"),
          record(["users", "carol"], "gpt-4.1-mini", [0, 0, 0, 0, 0], 20, "2026-10-01T00:01:00Z"),
        ]),
      ),
    );
    // October holds its own record and August's, older than the previous month.
    assert.deepEqual(
      (await currentTotals(usage)).windows.filter((window) => window.group === "carol"),
      [{ group: "carol", type: "usd_per_month", window_start: "2026-10-01T00:00:00Z", used: "27" }],
    );
    const september = await storedWindows(store, { hourStart: Date.UTC(2026, 8, 30, 23), monthStart: Date.UTC(2026, 8, 1) });
    assert.deepEqual(
      september.filter((total) => total.group === "carol").map((total) => [total.type, total.used]),
      [["usd_per_month", 50n]],
    );
  });

  test("a month rollover starts a new month window", async () => {
    const { usage, store, setTime } = await harness();
    setTime(Date.UTC(2026, 8, 30, 23, 59, 59, 999));
    acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 1, [record(["users", "carol"], "gpt-4.1-mini", [0, 0, 0, 0, 0], 50)])));

    setTime(Date.UTC(2026, 9, 1));
    assert.deepEqual((await currentTotals(usage)).windows, []);
    acked(
      await usage.acceptUsageBatch(
        INSTANCE,
        batch(EPOCH_A, 2, [record(["users", "carol"], "gpt-4.1-mini", [0, 0, 0, 0, 0], 20, "2026-10-01T00:00:00Z")]),
      ),
    );
    assert.deepEqual((await currentTotals(usage)).windows, [
      { type: "usd_per_month", window_start: "2026-10-01T00:00:00Z", used: "20" },
      { group: "carol", type: "usd_per_month", window_start: "2026-10-01T00:00:00Z", used: "20" },
    ]);
    // September is the previous month now: its windows stay while late records can
    // still count there.
    const september = await storedWindows(store, { hourStart: Date.UTC(2026, 8, 30, 23), monthStart: Date.UTC(2026, 8, 1) });
    assert.deepEqual(
      september.map((total) => [total.group ?? "global", total.type, total.used]).sort(),
      [
        ["carol", "usd_per_month", 50n],
        ["global", "usd_per_month", 50n],
        ["users", "usd_per_month", 50n],
      ],
    );
  });

  test("dropping past windows can be run by hand", async () => {
    const { usage, store, setTime } = await harness();
    acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 1, [oneTokenRecord()])));
    const t0Windows = { hourStart: Date.UTC(2026, 8, 24, 10), monthStart: Date.UTC(2026, 8, 1) };
    // global, users and carol, each an hour and a month window.
    assert.equal((await storedWindows(store, t0Windows)).length, 6);
    // Two months on: September is older than the previous month.
    setTime(Date.UTC(2026, 10, 2));
    await usage.dropPastWindows();
    assert.deepEqual(await storedWindows(store, t0Windows), []);
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

  test("totals listeners hear of batches another process counted over the same store", async () => {
    const store = createMemoryStore();
    const here = await harness({ store });
    const there = await harness({ store });
    let heard = 0;
    here.usage.onTotalsChanged(() => (heard += 1));
    acked(await there.usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 1, [oneTokenRecord()])));
    assert.equal(heard, 1);
    assert.equal(await carolHourUsed(here.usage), "1");
  });

  test("a bad recent records size is refused", () => {
    assert.throws(
      () =>
        createUsage({
          store: createMemoryStore(),
          clock: () => T0,
          recentRecordsSize: -1,
          onListenerError: () => undefined,
        }),
      { code: "recent-records-size-invalid" },
    );
  });
});

describe("revision and counted_through", () => {
  test("the revision is the store's sequence: it moves with every counted batch and publish, not with a duplicate", async () => {
    const { usage, publish } = await harness();
    // The harness's publish moved the sequence to 1.
    assert.equal((await currentTotals(usage)).revision, 1);
    const first = batch(EPOCH_A, 1, [oneTokenRecord()]);
    const intake = acked(await usage.acceptUsageBatch(INSTANCE, first));
    assert.equal(intake.ack.totals.revision, 2);
    const duplicate = acked(await usage.acceptUsageBatch(INSTANCE, first));
    assert.equal(duplicate.ack.totals.revision, 2);
    await publish(fullConfig());
    assert.equal((await currentTotals(usage)).revision, 3);
  });

  test("two processes over one store read one revision", async () => {
    const store = createMemoryStore();
    const a = await harness({ store });
    const b = await harness({ store });
    acked(await a.usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 1, [oneTokenRecord()])));
    const [ta, tb] = [await currentTotals(a.usage), await currentTotals(b.usage)];
    assert.deepEqual(ta, tb);
    // Two publishes (one per harness) and the batch.
    assert.equal(ta.revision, 3);
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

  test("an ack's totals hold the batch it acknowledges", async () => {
    const { usage } = await harness();
    const ack = acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 1, [oneTokenRecord()]))).ack.totals;
    assert.equal(ack.revision, 2);
    assert.deepEqual(ack.counted_through, { epoch: EPOCH_A, sequence: 1 });
    assert.ok(ack.windows.some((window) => window.group === "carol"), "the ack's windows hold the batch");
  });
});
