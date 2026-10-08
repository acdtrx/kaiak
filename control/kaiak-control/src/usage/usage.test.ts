import assert from "node:assert/strict";
import { readdirSync, readFileSync } from "node:fs";
import path from "node:path";
import { describe, test } from "node:test";

import type { Config } from "../config/index.ts";
import { configHash } from "../config-publishing/index.ts";
import { validateTotals } from "../messages/index.ts";
import type { Totals, TotalsWindow, UsageBatch, UsageRecord } from "../messages/index.ts";
import { createMemoryStore } from "../storage/index.ts";
import type { ControlPlaneStore, WindowStarts, WindowTotal } from "../storage/index.ts";
import { usageRecord } from "../test-support/index.ts";

import { createUsage } from "./index.ts";
import type { TotalsRead, Usage, UsageIntake, UsageOptions } from "./index.ts";

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
async function storedWindows(store: ControlPlaneStore, current: WindowStarts): Promise<WindowTotal[]> {
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
  return usageRecord({
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
    gateway_time: gatewayTime,
  });
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
  // Makes config the current one.
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
    liveGateways: async () => 0,
    onListenerError: (error) => listenerErrors.push(error),
    ...options,
  });
  // The core hands its modules every change the store announces.
  store.subscribe(usage.takeChange);
  const publish = async (config: Config): Promise<void> => {
    const current = (await store.currentConfig())?.hash;
    const text = JSON.stringify(config);
    const written = await store.publishConfig({ text, hash: configHash(config), publishedAt: now }, current);
    assert.ok(written.saved, "the harness's publish is stored");
  };
  return { usage, store, setTime: (ms) => (now = ms), publish, listenerErrors };
}

// Windows in a fixed order, global first, then by group and type: the totals list them
// in no particular order.
function sorted(windows: readonly TotalsWindow[]): TotalsWindow[] {
  const keyOf = (window: TotalsWindow): string => `${window.group === undefined ? "" : `~${window.group}`} ${window.type}`;
  return [...windows].sort((a, b) => (keyOf(a) < keyOf(b) ? -1 : keyOf(a) > keyOf(b) ? 1 : 0));
}

function acked(intake: UsageIntake): Extract<UsageIntake, { ok: true }> {
  assert.ok(intake.ok, `batch accepted: ${intake.ok ? "" : intake.error.message}`);
  return intake;
}

// The totals at one snapshot, windows sorted. Its windows and live count, as a stream
// sends them, pass the totals schema and rules.
async function currentTotals(usage: Usage): Promise<TotalsRead> {
  const read = await usage.readTotals();
  const message: Totals = { live_gateways: read.liveGateways, counted_through: [], windows: read.windows };
  assert.ok(validateTotals(message).ok, "totals pass the totals schema and rules");
  return { ...read, windows: sorted(read.windows) };
}

// carol's hour window.
async function carolHourUsed(usage: Usage): Promise<string | undefined> {
  const totals = await currentTotals(usage);
  return totals.windows.find((window) => window.group === "carol" && window.type === "tokens_per_hour")?.used;
}

describe("aggregation", () => {
  test("a mixed batch counts into every scope on each path; the totals list every window with usage", async () => {
    const { usage } = await harness();
    const records = [
      // Every model counts toward every scope on the path. Token windows leave out
      // input read from the cache (eval-pipeline's 100, alice's 1024).
      record(EVAL, "qwen3-32b", [800, 100, 0, 300, 0], 5_000),
      record(EVAL, "gpt-4.1", [1000, 0, 0, 500, 0], 7_000_000),
      // Group support has no limits in full.json: counted and listed all the same.
      record(SUPPORT, "gpt-4.1-mini", [10, 0, 0, 10, 0], 2_000),
      record(["users", "alice"], "gpt-4.1-mini", [2048, 1024, 0, 377, 0], 1_524_000),
      // Reasoning is inside tokens_out. Cost 0 leaves bob's USD window empty, so it is
      // not listed.
      record(["users", "bob"], "qwen3-32b", [100, 0, 0, 50, 20], 0),
      // Groups no config defines count too, as does every scope on the path.
      record(["users", "ghost"], "gpt-4.1", [1, 1, 0, 1, 0], 1_000),
      record(["research", "gone"], "bge-m3", [5, 0, 0, 0, 0], 3),
    ];
    const intake = acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 1, records)));
    assert.equal(intake.outcome, "first");

    const totals = await currentTotals(usage);
    const hour = (group: string | undefined, used: string): TotalsWindow => ({
      ...(group !== undefined && { group }),
      type: "tokens_per_hour",
      window_start: HOUR_10,
      used,
    });
    const month = (group: string | undefined, used: string): TotalsWindow => ({
      ...(group !== undefined && { group }),
      type: "usd_per_month",
      window_start: SEPTEMBER,
      used,
    });
    assert.deepEqual(totals, {
      liveGateways: 0,
      cursors: [{ instance: INSTANCE, epoch: EPOCH_A, sequence: 1 }],
      windowStarts: { tokens_per_hour: HOUR_10, usd_per_month: SEPTEMBER },
      windows: sorted([
        hour(undefined, "5202"),
        month(undefined, "8532003"),
        hour("research", "2605"),
        month("research", "7005003"),
        hour("eval-pipeline", "2600"),
        month("eval-pipeline", "7005000"),
        hour("gone", "5"),
        month("gone", "3"),
        hour("support", "20"),
        month("support", "2000"),
        hour("support-bot", "20"),
        month("support-bot", "2000"),
        hour("users", "2577"),
        month("users", "1525000"),
        hour("alice", "2425"),
        month("alice", "1524000"),
        hour("bob", "150"),
        hour("ghost", "2"),
        month("ghost", "1000"),
      ]),
    });
    assert.deepEqual(intake.ack, { batch: { instance: INSTANCE, epoch: EPOCH_A, sequence: 1 } });
  });

  test("a token limit counts plain input, input written to the cache and output — not input read from it", async () => {
    const { usage } = await harness();
    // 3 plain, 1024 read, 1009 written, 40 out (12 of them reasoning, already inside).
    const written = record(["users", "carol"], "gpt-4.1-mini", [3, 1024, 1009, 40, 12], 1);
    acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 1, [written])));
    assert.equal(await carolHourUsed(usage), "1052"); // 3 + 1009 + 40
  });

  test("a deep path counts toward every group on it, and global", async () => {
    const { usage } = await harness();
    const path = ["team", "project", "region", "env", "workload"];
    acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 1, [record(path, "gpt-4.1", [10, 0, 0, 5, 0], 4_000)])));
    const levels = [undefined, ...path].flatMap((group) => [
      { ...(group !== undefined && { group }), type: "tokens_per_hour" as const, window_start: HOUR_10, used: "15" },
      { ...(group !== undefined && { group }), type: "usd_per_month" as const, window_start: SEPTEMBER, used: "4000" },
    ]);
    assert.deepEqual((await currentTotals(usage)).windows, sorted(levels));
  });

  test("a publish changes no totals: limits removed, edited or added leave every window as it is", async () => {
    const { usage, publish } = await harness();
    await publish(fullConfig());
    acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 1, [record(EVAL, "bge-m3", [1, 0, 0, 0, 0], 10)])));
    acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 2, [record(SUPPORT, "gpt-4.1-mini", [1, 0, 0, 0, 0], 2_000)])));
    const before = await currentTotals(usage);

    const config = fullConfig();
    const research = config.groups?.["research"];
    const support = config.groups?.["support"];
    assert.ok(research && support, "full.json has groups research and support");
    delete research.limits;
    support.limits = [{ type: "usd_per_month", value: 100 }];
    config.global.limits = [{ type: "usd_per_month", value: 9000 }];
    await publish(config);
    assert.deepEqual(await currentTotals(usage), before);
    assert.equal(before.windows.find((window) => window.group === "research" && window.type === "usd_per_month")?.used, "10");
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

  test("with no config published, the totals list the batch's windows", async () => {
    const { usage, store } = await harness();
    assert.equal(await store.currentConfig(), undefined);
    assert.deepEqual(await currentTotals(usage), {
      liveGateways: 0,
      cursors: [],
      windowStarts: { tokens_per_hour: HOUR_10, usd_per_month: SEPTEMBER },
      windows: [],
    });
    acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 1, [record(["users", "carol"], "gpt-4.1-mini", [3, 0, 0, 0, 0], 4)])));
    assert.equal(await carolHourUsed(usage), "3");
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

  // Two cores on one store, each serializing only its own intake, take the same batch
  // at once. The store's conditional write makes the second one find the batch
  // counted.
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
    acked(
      await usage.acceptUsageBatch(
        INSTANCE,
        batch(EPOCH_A, 2, [record(["users", "carol"], "gpt-4.1-mini", [3, 0, 0, 0, 0], 1, "2026-09-24T11:00:00Z")]),
      ),
    );
    totals = await currentTotals(usage);
    assert.deepEqual(
      totals.windows.filter((window) => window.group === "carol"),
      [
        { group: "carol", type: "tokens_per_hour", window_start: "2026-09-24T11:00:00Z", used: "3" },
        { group: "carol", type: "usd_per_month", window_start: SEPTEMBER, used: "8" },
      ],
    );

    // The 10:00 window is the previous one now: it stays (late records still count
    // there) until a drop of past windows in a later hour (the expiry sweep's).
    const at10 = { tokens_per_hour: Date.UTC(2026, 8, 24, 10), usd_per_month: Date.UTC(2026, 8, 1) };
    // global, users and carol: each counted in its hour and its month.
    assert.deepEqual(
      (await storedWindows(store, at10)).map((total) => total.type).sort(),
      ["tokens_per_hour", "tokens_per_hour", "tokens_per_hour", "usd_per_month", "usd_per_month", "usd_per_month"],
    );
    await usage.dropPastWindows();
    assert.equal((await storedWindows(store, at10)).length, 6);
    setTime(Date.UTC(2026, 8, 24, 12));
    acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 3, [record(["users", "carol"], "gpt-4.1-mini", [1, 0, 0, 0, 0], 1, "2026-09-24T12:00:00Z")])));
    // Counting drops nothing.
    assert.equal((await storedWindows(store, at10)).length, 6);
    await usage.dropPastWindows();
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
    const at11 = await storedWindows(store, { tokens_per_hour: Date.UTC(2026, 8, 24, 11), usd_per_month: Date.UTC(2026, 7, 1) });
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
    const september = await storedWindows(store, { tokens_per_hour: Date.UTC(2026, 8, 30, 23), usd_per_month: Date.UTC(2026, 8, 1) });
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
      { group: "users", type: "usd_per_month", window_start: "2026-10-01T00:00:00Z", used: "20" },
    ]);
    // September is the previous month now: its windows stay while late records can
    // still count there.
    const september = await storedWindows(store, { tokens_per_hour: Date.UTC(2026, 8, 30, 23), usd_per_month: Date.UTC(2026, 8, 1) });
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
    const t0Windows = { tokens_per_hour: Date.UTC(2026, 8, 24, 10), usd_per_month: Date.UTC(2026, 8, 1) };
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
          liveGateways: async () => 0,
          onListenerError: () => undefined,
        }),
      { code: "recent-records-size-invalid" },
    );
  });
});

describe("counted_through", () => {
  test("two processes over one store read the same totals", async () => {
    const store = createMemoryStore();
    const a = await harness({ store });
    const b = await harness({ store });
    acked(await a.usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 1, [oneTokenRecord()])));
    assert.deepEqual(await currentTotals(a.usage), await currentTotals(b.usage));
  });

  test("the cursors are each instance's last counted batch of each epoch", async () => {
    const { usage } = await harness();
    assert.deepEqual((await currentTotals(usage)).cursors, []);
    acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_A, 3, [oneTokenRecord()])));
    acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_B, 1, [oneTokenRecord()])));
    acked(await usage.acceptUsageBatch(INSTANCE, batch(EPOCH_B, 2, [oneTokenRecord()])));
    const other = { ...oneTokenRecord(), gateway_instance: "gw-2" };
    acked(
      await usage.acceptUsageBatch("gw-2", { batch: { instance: "gw-2", epoch: EPOCH_B, sequence: 7 }, records: [other] }),
    );
    const keyOf = ({ instance, epoch }: { instance: string; epoch: string }): string => `${instance} ${epoch}`;
    assert.deepEqual(
      [...(await currentTotals(usage)).cursors].sort((a, b) => (keyOf(a) < keyOf(b) ? -1 : 1)),
      [
        { instance: INSTANCE, epoch: EPOCH_A, sequence: 3 },
        { instance: INSTANCE, epoch: EPOCH_B, sequence: 2 },
        { instance: "gw-2", epoch: EPOCH_B, sequence: 7 },
      ],
    );
  });

  test("an ack names the batch it acknowledges and nothing else", async () => {
    const { usage } = await harness();
    const sent = batch(EPOCH_A, 1, [oneTokenRecord()]);
    const intake = acked(await usage.acceptUsageBatch(INSTANCE, sent));
    assert.deepEqual(intake.ack, { batch: sent.batch });
    const duplicate = acked(await usage.acceptUsageBatch(INSTANCE, sent));
    assert.deepEqual(duplicate.ack, { batch: sent.batch });
  });
});
