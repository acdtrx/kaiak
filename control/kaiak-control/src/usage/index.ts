// Usage intake and totals (docs/specs/CONTROL-PROTOCOL.md, Usage batches, Usage intake,
// Budgets, Messages → Totals): batches taken exactly once per batch ID, their records
// stamped with the control plane's receipt time and counted into the hour and month
// windows of every scope on their path — each in its gateway_time window when that is
// the current or previous one — and totals of the current windows for acks and pushes,
// each read from one store snapshot under the store's totals sequence. Nothing here is
// held per process that another process could disagree with: the store decides which
// batches count and orders the totals.

import { validateUsageBatch } from "../messages/index.ts";
import type { BatchId, Totals, TotalsWindow, UsageAck, UsageBatch } from "../messages/index.ts";
import type { ControlPlaneStore, CurrentWindows, ReceivedRecord, StoredConfig } from "../storage/index.ts";

import { batchAdditions, limitedWindowsOf, windowKeyOf } from "./aggregate.ts";
import type { LimitedWindow } from "./aggregate.ts";
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
  // "config-unavailable" (nothing published yet, so there are no totals to answer
  // with — the gateway keeps the batch and retries).
  code: string;
  message: string;
  // The HTTP status an adapter answers with.
  status: 400 | 503;
}

export type UsageIntake =
  | { ok: true; ack: UsageAck; outcome: BatchOutcome; previous?: BatchId }
  | { ok: false; error: UsageBatchError };

export type TotalsChangedListener = () => void;

export interface Usage {
  // Takes one usage batch from `instance` (the requester's checked instance ID).
  acceptUsageBatch(instance: string, doc: unknown): Promise<UsageIntake>;
  // The totals of the current windows under the latest config as the gateway
  // `instance` gets them (its counted_through); undefined before the first config is
  // published.
  totals(instance: string): Promise<Totals | undefined>;
  // The newest received records, newest first.
  recentRecords(): Promise<ReceivedRecord[]>;
  // Calls listener after every batch any process counted; returns the unsubscribe,
  // which is safe to call more than once.
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
  // Called for each totals listener that throws (the batch is counted either way).
  onListenerError: (error: unknown) => void;
}

// used is at most 18 digits on the wire (below 10^18: a billion dollars in nano-USD).
// A window past it is reported at the ceiling — beyond any limit a config can express.
const MAX_USED = 10n ** 18n - 1n;

export function createUsage({ store, clock, recentRecordsSize, onListenerError }: UsageOptions): Usage {
  if (!Number.isSafeInteger(recentRecordsSize) || recentRecordsSize < 0) {
    throw Object.assign(new Error(`recent records size must be a non-negative integer, got ${recentRecordsSize}`), {
      code: "recent-records-size-invalid",
    });
  }

  const listeners = new Set<TotalsChangedListener>();
  // Batches from one instance run one at a time in this process, so a resend racing
  // its original here is decided without a refused write; across processes the
  // store's conditional write decides.
  const queues = new Map<string, Promise<unknown>>();
  // A cache of the latest config's limited windows, by version: derived from the
  // store's config, never a decision of its own.
  let limitedCache: { version: number; limited: LimitedWindow[] } | undefined;
  // The hour this process last pruned past windows in: pruning is idempotent, so
  // every process prunes on its own first batch of an hour.
  let prunedHourStart: number | undefined;

  const limitedOf = (config: StoredConfig): LimitedWindow[] => {
    if (limitedCache?.version !== config.version) {
      limitedCache = { version: config.version, limited: limitedWindowsOf(config.config) };
    }
    return limitedCache.limited;
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

  // Every listener hears of every counted batch, this process's or another's; a
  // listener's failure goes to onListenerError and never reaches the store.
  store.subscribe((change) => {
    if (change.type !== "batch-counted") return;
    for (const listener of [...listeners]) {
      try {
        listener();
      } catch (error) {
        onListenerError(error);
      }
    }
  });

  const totalsAt = async (instance: string, now: number): Promise<Totals | undefined> => {
    const windows = currentWindows(now);
    const snapshot = await store.totalsSnapshot(windows, instance);
    if (!snapshot.config) return undefined;
    return {
      revision: snapshot.sequence,
      config_epoch: await store.configEpoch(),
      config_version: snapshot.config.version,
      live_gateways: snapshot.liveGateways,
      counted_through: snapshot.last ? { epoch: snapshot.last.epoch, sequence: snapshot.last.sequence } : null,
      windows: listedWindows(limitedOf(snapshot.config), snapshot.windows, windows),
    };
  };

  // The previous windows stay: a late record still counts in its own window when that
  // is the previous one.
  const dropPastWindowsAt = async (windows: CurrentWindows): Promise<void> => {
    await store.dropPastWindowTotals(previousWindows(windows));
    prunedHourStart = windows.hourStart;
  };

  const countBatch = async ({ batch, records }: UsageBatch): Promise<UsageIntake> => {
    if (!(await store.latestConfig())) {
      return {
        ok: false,
        error: { code: "config-unavailable", message: "no config has been published yet", status: 503 },
      };
    }

    // The write is conditional on the last batch the outcome was decided against: when
    // another writer counted a batch of this instance in between, nothing is written
    // and the outcome is decided again against the store's last batch — so a batch is
    // counted once however many processes take it.
    let previous = await store.lastBatch(batch.instance);
    let outcome = outcomeOf(batch, previous);
    while (outcome !== "duplicate") {
      const receivedAt = clock();
      const windows = currentWindows(receivedAt);
      const written = await store.saveCountedBatch(
        {
          batch,
          countedAt: receivedAt,
          additions: batchAdditions(records, windows),
          records: records.map((record) => ({ receivedAt, record })),
        },
        previous,
        recentRecordsSize,
      );
      if (written.saved) {
        if (prunedHourStart !== windows.hourStart) await dropPastWindowsAt(windows);
        break;
      }
      previous = written.last;
      outcome = outcomeOf(batch, previous);
    }

    const totals = await totalsAt(batch.instance, clock());
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
    totals: (instance) => totalsAt(instance, clock()),
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

// The windows a totals message lists: every limited scope and type with usage in its
// current window, in the config's limit order. A window no limit names (a scope
// without that limit type, a deleted group) is counted but not listed.
function listedWindows(
  limited: readonly LimitedWindow[],
  stored: readonly { group?: string; type: LimitedWindow["type"]; windowStart: number; used: bigint }[],
  windows: CurrentWindows,
): TotalsWindow[] {
  const used = new Map(stored.map((total) => [windowKeyOf(total), total.used]));
  const listed: TotalsWindow[] = [];
  for (const { group, type } of limited) {
    const windowStart = windowStartFor(type, windows);
    const amount = used.get(windowKeyOf({ ...(group !== undefined && { group }), type, windowStart })) ?? 0n;
    if (amount === 0n) continue;
    listed.push({
      ...(group !== undefined && { group }),
      type,
      window_start: formatWindowStart(windowStart),
      used: (amount > MAX_USED ? MAX_USED : amount).toString(),
    });
  }
  return listed;
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
