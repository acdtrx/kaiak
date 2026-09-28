// Usage intake and totals (docs/specs/CONTROL-PROTOCOL.md, Usage batches, Usage intake,
// Budgets, Messages → Totals): batches taken exactly once per batch ID, their records
// stamped with the control plane's receipt time and counted into every hour and month
// limit window they apply to — each in its gateway_time window when that is the
// current or previous one — and totals of the current windows for acks and pushes,
// each a consistent snapshot under a revision.

import { randomBytes } from "node:crypto";

import { validateUsageBatch } from "../messages/index.ts";
import type { BatchId, Totals, TotalsLimitType, TotalsWindow, UsageAck, UsageBatch } from "../messages/index.ts";
import type { ControlPlaneStore, CurrentWindows, ReceivedRecord, StoredConfig, WindowTotal } from "../storage/index.ts";

import { batchAdditions, carryOvers, countedLimitsOf, windowKeyOf } from "./aggregate.ts";
import type { CountedLimits } from "./aggregate.ts";
import { currentWindows, formatWindowStart, previousWindows, windowStartFor } from "./windows.ts";

// How a batch relates to the last one counted for its instance:
// - first: the instance's first batch;
// - next: the next sequence in the same epoch;
// - gap: a later sequence in the same epoch, some skipped (lost on the gateway side);
// - new-epoch: a different epoch (a fresh spool);
// - duplicate: counted before (same epoch, sequence at or below the last) — acked again,
//   not counted.
export type BatchOutcome = "first" | "next" | "gap" | "new-epoch" | "duplicate";

export interface UsageBatchError {
  // "usage-batch-invalid" (schema violation), a message rule code
  // ("record-instance-mismatch", "record-id-duplicate", "timestamp-invalid"),
  // "instance-mismatch" (the body's instance is not the requester's), or
  // "config-unavailable" (nothing published yet, so there are no limits to count
  // toward — the gateway keeps the batch and retries).
  code: string;
  message: string;
  // The HTTP status an adapter answers with.
  status: 400 | 503;
}

export type UsageIntake =
  | { ok: true; ack: UsageAck; outcome: BatchOutcome; previous?: BatchId }
  | { ok: false; error: UsageBatchError };

export type TotalsChangedListener = () => void;

// A limit that kept its spend across a publish because only its model set changed
// (docs/specs/CONTROL-PROTOCOL.md, Budgets → Model-set edits): the new limit, the
// model sets of the limits it replaced (null = all models), and whether there were
// several — then the largest spend was carried.
export interface LimitCarryOver {
  // Absent for a global limit.
  group?: string;
  type: TotalsLimitType;
  models?: string[];
  from: (string[] | null)[];
  ambiguous: boolean;
}

export interface Usage {
  // Takes one usage batch from `instance` (the requester's checked instance ID).
  acceptUsageBatch(instance: string, doc: unknown): Promise<UsageIntake>;
  // The totals of the current windows under the current config as the gateway
  // `instance` gets them (its counted_through); undefined before the first config is
  // published.
  totals(instance: string): Promise<Totals | undefined>;
  // Moves the totals revision on for a change the usage module does not see itself: a
  // publish (new config version and limits) or a change to the live set. Call it
  // before the totals are pushed for the change.
  totalsChanged(): void;
  // Runs a publish in the totals' turn, so no batch counts between the publish and
  // what it changes in the totals: a limit whose only change is its model set keeps
  // its current windows' spend (Budgets → Model-set edits). Every publish goes
  // through it.
  publishing<T>(publish: () => Promise<T>): Promise<T>;
  // The newest received records, newest first.
  recentRecords(): Promise<ReceivedRecord[]>;
  // Calls listener after every batch that was counted; returns the unsubscribe, which
  // is safe to call more than once.
  onTotalsChanged(listener: TotalsChangedListener): () => void;
  // Lets the store drop totals of windows before the previous ones (a late record can
  // still count in the previous window). Intake runs it on the first batch of every
  // hour.
  dropPastWindows(): Promise<void>;
}

export interface UsageOptions {
  store: ControlPlaneStore;
  // Milliseconds since the epoch.
  clock: () => number;
  // How many received records recentRecords keeps.
  recentRecordsSize: number;
  // The live-gateway count every totals message carries.
  liveGateways: () => number | Promise<number>;
  // Called for each totals listener that throws; the batch is counted either way.
  onListenerError: (error: unknown) => void;
  // The revision's control-plane ID: 32 lowercase hex digits. Default: random, new
  // with every process (tests fix it).
  controlPlaneId?: string;
  // Hears of every limit that kept its spend across a publish. Default: nothing.
  onLimitCarriedOver?: (carry: LimitCarryOver) => void;
}

// used is at most 18 digits on the wire (below 10^18: a billion dollars in nano-USD).
// A window past it is reported at the ceiling — beyond any limit a config can express.
const MAX_USED = 10n ** 18n - 1n;

export function createUsage({
  store,
  clock,
  recentRecordsSize,
  liveGateways,
  onListenerError,
  controlPlaneId = randomBytes(16).toString("hex"),
  onLimitCarriedOver = () => {},
}: UsageOptions): Usage {
  if (!Number.isSafeInteger(recentRecordsSize) || recentRecordsSize < 0) {
    throw Object.assign(new Error(`recent records size must be a non-negative integer, got ${recentRecordsSize}`), {
      code: "recent-records-size-invalid",
    });
  }

  const listeners = new Set<TotalsChangedListener>();
  // Batches from one instance run one at a time, so two copies of a batch (a resend
  // racing the original) cannot both be counted.
  const queues = new Map<string, Promise<unknown>>();
  let limitsCache: { version: number; limits: CountedLimits } | undefined;
  let prunedHourStart: number | undefined;
  // The revision's sequence: one more with every change to the totals.
  let sequence = 0;
  // Counting a batch (its store write and the sequence step) and reading totals (the
  // sequence, counted_through, then the windows) take turns, so every totals message
  // is a consistent snapshot: its windows hold exactly the batches counted at its
  // sequence. A gateway relies on it to stop counting its own usage the moment any
  // message shows it counted (CONTROL-PROTOCOL.md, Messages → Totals).
  let turn: Promise<unknown> = Promise.resolve();
  const exclusive = <T>(run: () => Promise<T>): Promise<T> => {
    const result = turn.then(run);
    turn = result.catch(() => {
      // The caller gets the failure from `result`; the chain only orders the turns.
    });
    return result;
  };

  const limitsOf = (config: StoredConfig): CountedLimits => {
    if (limitsCache?.version !== config.version) {
      limitsCache = { version: config.version, limits: countedLimitsOf(config.config) };
    }
    return limitsCache.limits;
  };

  const serialized = <T>(instance: string, run: () => Promise<T>): Promise<T> => {
    const result = (queues.get(instance) ?? Promise.resolve()).then(run);
    const tail = result.catch(() => {
      // The caller gets this batch's failure from `result`; the queue only orders batches.
    });
    queues.set(instance, tail);
    void tail.then(() => {
      if (queues.get(instance) === tail) queues.delete(instance);
    });
    return result;
  };

  const totalsAt = (instance: string, now: () => number): Promise<Totals | undefined> =>
    exclusive(async () => {
      const revision = { control_plane: controlPlaneId, sequence };
      const last = await store.lastBatch(instance);
      const config = await store.latestConfig();
      if (!config) return undefined;
      return {
        revision,
        ...(await windowTotals(config, now())),
        counted_through: last ? { epoch: last.epoch, sequence: last.sequence } : null,
      };
    }).then((totals) => totals && orderFields(totals));

  const windowTotals = async (
    config: StoredConfig,
    now: number,
  ): Promise<Pick<Totals, "config_epoch" | "config_version" | "live_gateways" | "windows">> => {
    const windows = currentWindows(now);
    const stored = new Map((await store.currentWindowTotals(windows)).map((total) => [windowKeyOf(total), total.used]));
    const listed: TotalsWindow[] = [];
    for (const counted of limitsOf(config).all) {
      const windowStart = windowStartFor(counted.type, windows);
      const used = stored.get(windowKeyOf({ ...counted, windowStart })) ?? 0n;
      if (used === 0n) continue;
      listed.push({
        ...(counted.group !== undefined && { group: counted.group }),
        type: counted.type,
        ...(counted.models !== undefined && { models: [...counted.models] }),
        window_start: formatWindowStart(windowStart),
        used: (used > MAX_USED ? MAX_USED : used).toString(),
      });
    }
    return {
      config_epoch: await store.configEpoch(),
      config_version: config.version,
      live_gateways: await liveGateways(),
      windows: listed,
    };
  };

  // The previous windows stay: a late record still counts in its own window when that
  // is the previous one.
  const dropPastWindowsAt = async (windows: CurrentWindows): Promise<void> => {
    await store.dropPastWindowTotals(previousWindows(windows));
    prunedHourStart = windows.hourStart;
  };

  // Carries each model-set-edited limit's spend in the current windows from the
  // limits it replaced: the new window is raised to the largest predecessor's amount
  // (it can already hold its own, from when a limit of that identity last existed in
  // the window). Runs in the publish's turn, before any batch counts under the new
  // config.
  const carryOver = async (before: StoredConfig, after: StoredConfig, now: number): Promise<void> => {
    const carries = carryOvers(countedLimitsOf(before.config), limitsOf(after));
    if (carries.length === 0) return;
    const windows = currentWindows(now);
    const stored = new Map((await store.currentWindowTotals(windows)).map((total) => [windowKeyOf(total), total.used]));
    const usedIn = (limit: { group?: string; type: TotalsLimitType; models?: string[] }): bigint =>
      stored.get(windowKeyOf({ ...limit, windowStart: windowStartFor(limit.type, windows) })) ?? 0n;
    const additions: WindowTotal[] = [];
    for (const { to, from } of carries) {
      const carried = from.reduce((most, old) => (usedIn(old) > most ? usedIn(old) : most), 0n);
      const own = usedIn(to);
      if (carried > own) {
        additions.push({
          ...(to.group !== undefined && { group: to.group }),
          type: to.type,
          ...(to.models !== undefined && { models: to.models }),
          windowStart: windowStartFor(to.type, windows),
          used: carried - own,
        });
      }
      onLimitCarriedOver({
        ...(to.group !== undefined && { group: to.group }),
        type: to.type,
        ...(to.models !== undefined && { models: [...to.models] }),
        from: from.map((old) => (old.models ? [...old.models] : null)),
        ambiguous: from.length > 1,
      });
    }
    if (additions.length > 0) await store.addWindowTotals(additions);
  };

  const notify = (): void => {
    for (const listener of [...listeners]) {
      try {
        listener();
      } catch (error) {
        onListenerError(error);
      }
    }
  };

  const countBatch = async ({ batch, records }: UsageBatch): Promise<UsageIntake> => {
    const config = await store.latestConfig();
    if (!config) {
      return {
        ok: false,
        error: { code: "config-unavailable", message: "no config has been published yet", status: 503 },
      };
    }

    // The write is conditional on the last batch the outcome was decided against: when
    // another writer counted a batch of this instance in between, nothing is written
    // and the outcome is decided again against the store's last batch — so a batch is
    // counted once even by two cores on one store.
    let previous = await store.lastBatch(batch.instance);
    let outcome = outcomeOf(batch, previous);
    while (outcome !== "duplicate") {
      const receivedAt = clock();
      const windows = currentWindows(receivedAt);
      const expected = previous;
      const result = await exclusive(async () => {
        // The config in force now: a publish in between (it runs in a turn of its own)
        // changed the limits the batch counts toward.
        const current = (await store.latestConfig()) ?? config;
        const written = await store.saveCountedBatch(
          {
            batch,
            countedAt: receivedAt,
            additions: batchAdditions(records, limitsOf(current), windows),
            records: records.map((record) => ({ receivedAt, record })),
          },
          expected,
          recentRecordsSize,
        );
        if (written.saved) sequence += 1;
        return written;
      });
      if (result.saved) {
        if (prunedHourStart !== windows.hourStart) await dropPastWindowsAt(windows);
        notify();
        break;
      }
      previous = result.last;
      outcome = outcomeOf(batch, previous);
    }

    const totals = await totalsAt(batch.instance, clock);
    // A config is published and versions are never withdrawn, so totals exist.
    if (!totals) throw Object.assign(new Error("no config after one was read"), { code: "config-missing" });
    return { ok: true, ack: { batch, totals }, outcome, ...(previous !== undefined && { previous }) };
  };

  return {
    async acceptUsageBatch(instance, doc) {
      const validation = validateUsageBatch(doc);
      if (!validation.ok) {
        const first = validation.issues[0];
        const code = first === undefined || first.code === "schema" ? "usage-batch-invalid" : first.code;
        return invalid(code, validation.issues.map((issue) => issue.message).join("; "));
      }
      const message = validation.message;
      if (message.batch.instance !== instance) {
        return invalid(
          "instance-mismatch",
          `the batch's instance "${message.batch.instance}" is not the requester's "${instance}"`,
        );
      }
      return serialized(instance, () => countBatch(message));
    },
    totals: (instance) => totalsAt(instance, clock),
    totalsChanged: () => {
      sequence += 1;
    },
    publishing: (publish) =>
      exclusive(async () => {
        const before = await store.latestConfig();
        const result = await publish();
        const after = await store.latestConfig();
        if (before && after && after.version !== before.version) await carryOver(before, after, clock());
        return result;
      }),
    recentRecords: () => store.recentRecords(recentRecordsSize),
    onTotalsChanged(listener) {
      // A wrapper, so the same function subscribed twice is two subscriptions.
      const subscription: TotalsChangedListener = () => listener();
      listeners.add(subscription);
      return () => {
        listeners.delete(subscription);
      };
    },
    dropPastWindows: () => dropPastWindowsAt(currentWindows(clock())),
  };
}

// The totals fields in the order the protocol lists them.
function orderFields({ revision, config_epoch, config_version, live_gateways, counted_through, windows }: Totals): Totals {
  return { revision, config_epoch, config_version, live_gateways, counted_through, windows };
}

function outcomeOf(batch: BatchId, previous: BatchId | undefined): BatchOutcome {
  if (!previous) return "first";
  if (batch.epoch !== previous.epoch) return "new-epoch";
  if (batch.sequence <= previous.sequence) return "duplicate";
  return batch.sequence === previous.sequence + 1 ? "next" : "gap";
}

function invalid(code: string, message: string): UsageIntake {
  return { ok: false, error: { code, message, status: 400 } };
}
