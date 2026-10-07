// Usage intake and totals (docs/specs/CONTROL-PROTOCOL.md, Usage batches, Usage intake,
// Budgets, Messages → Totals): batches taken exactly once per batch ID, their records
// stamped with the control plane's receipt time and counted into the hour and month
// windows of every scope on their path — each in its gateway_time window when that is
// the current or previous one — whether or not a config is published, and totals of
// the current windows for stream pushes, each read from one store snapshot. Nothing
// here is held per process that another process could disagree with: the store decides
// which batches count.

import { validateUsageBatch } from "../messages/index.ts";
import type { BatchId, Totals, TotalsWindow, UsageAck, UsageBatch } from "../messages/index.ts";
import type { BatchCursors, ControlPlaneStore, CurrentConfig, CurrentWindows, ReceivedRecord, StoreChange } from "../storage/index.ts";

import { batchAdditions, limitedWindowsOf, windowKeyOf } from "./aggregate.ts";
import type { LimitedWindow } from "./aggregate.ts";
import { currentWindows, formatWindowStart, previousWindows, windowStartFor } from "./windows.ts";

// How a batch relates to the last one counted for its instance:
// - first: the instance's first batch;
// - next: the next sequence in the same epoch;
// - gap: a later sequence in the same epoch, some skipped (lost on the gateway side);
// - new-epoch: an epoch with nothing counted yet, after batches of another (a fresh
//   spool);
// - duplicate: counted before (its epoch's last counted batch is at or past it) — acked
//   again, not counted.
export type BatchOutcome = "first" | "next" | "gap" | "new-epoch" | "duplicate";

export interface UsageBatchError {
  // "usage-batch-invalid" (schema violation), a message rule code
  // ("record-instance-mismatch", "record-id-duplicate", "timestamp-invalid"), or
  // "instance-mismatch" (the body's instance is not the requester's).
  code: string;
  message: string;
  // The HTTP status an adapter answers with.
  status: 400;
}

export type UsageIntake =
  | { ok: true; ack: UsageAck; outcome: BatchOutcome; previous?: BatchId }
  | { ok: false; error: UsageBatchError };

export type TotalsChangedListener = () => void;

export interface Usage {
  // Takes one usage batch from `instance` (the requester's checked instance ID).
  acceptUsageBatch(instance: string, doc: unknown): Promise<UsageIntake>;
  // The totals of the current windows the current config limits, as the gateway
  // `instance` gets them on its stream (its counted_through); undefined before the
  // first config is published.
  totals(instance: string): Promise<Totals | undefined>;
  // The newest received records, newest first.
  recentRecords(): Promise<ReceivedRecord[]>;
  // Calls listener after every batch any process counted (and on a store catch-up);
  // returns the unsubscribe, which is safe to call more than once.
  onTotalsChanged(listener: TotalsChangedListener): () => void;
  // Takes one change the store announced (the core passes every one on).
  takeChange(change: StoreChange): void;
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
  // Hears of every store sequence a totals read sees (CONTROL-PROTOCOL.md, Config
  // stream → Rollback).
  observeSequence: (sequence: number) => void;
  // Called for each totals listener that throws (the batch is counted either way).
  onListenerError: (error: unknown) => void;
}

// used is at most 18 digits on the wire (below 10^18: a billion dollars in nano-USD).
// A window past it is reported at the ceiling — beyond any limit a config can express.
const MAX_USED = 10n ** 18n - 1n;

export function createUsage({ store, clock, recentRecordsSize, observeSequence, onListenerError }: UsageOptions): Usage {
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
  // A cache of the current config's limited windows, by its hash: derived from the
  // store's config, never a decision of its own.
  let limitedCache: { hash: string; limited: LimitedWindow[] } | undefined;
  // The hour this process last pruned past windows in: pruning is idempotent, so
  // every process prunes on its own first batch of an hour.
  let prunedHourStart: number | undefined;

  const limitedOf = (config: CurrentConfig): LimitedWindow[] => {
    if (limitedCache?.hash !== config.hash) {
      limitedCache = { hash: config.hash, limited: limitedWindowsOf(config.config) };
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
  const takeChange = (change: StoreChange): void => {
    if (change.type !== "batch-counted" && change.type !== "catch-up") return;
    for (const listener of [...listeners]) {
      try {
        listener();
      } catch (error) {
        onListenerError(error);
      }
    }
  };

  // The windows a snapshot reads are the ones current once the read is over: a read
  // that crossed an hour or month boundary — while a batch past it may have been
  // counted — reads again with the new windows, so a message never names a batch
  // counted while leaving out the window it was counted in.
  const totalsAt = async (instance: string): Promise<Totals | undefined> => {
    let windows = currentWindows(clock());
    for (;;) {
      const snapshot = await store.totalsSnapshot(windows, instance);
      observeSequence(snapshot.sequence);
      const after = currentWindows(clock());
      if (after.hourStart !== windows.hourStart || after.monthStart !== windows.monthStart) {
        windows = after;
        continue;
      }
      if (!snapshot.config) return undefined;
      return {
        live_gateways: snapshot.liveGateways,
        counted_through: snapshot.last ? { epoch: snapshot.last.epoch, sequence: snapshot.last.sequence } : null,
        windows: listedWindows(limitedOf(snapshot.config), snapshot.windows, windows),
      };
    }
  };

  // The previous windows stay: a late record still counts in its own window when that
  // is the previous one.
  const dropPastWindowsAt = async (windows: CurrentWindows): Promise<void> => {
    await store.dropPastWindowTotals(previousWindows(windows));
    prunedHourStart = windows.hourStart;
  };

  const countBatch = async ({ batch, records }: UsageBatch): Promise<UsageIntake> => {
    // The write is conditional on the last batch of the batch's epoch the outcome was
    // decided against: when another writer counted a batch of this instance in that
    // epoch in between, nothing is written and the outcome is decided again against the
    // store's — so a batch is counted once however many processes take it, and however
    // many epochs its gateway went through meanwhile.
    let cursors = await store.lastBatch(batch.instance, batch.epoch);
    let outcome = outcomeOf(batch, cursors);
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
        cursors.inEpoch,
        recentRecordsSize,
      );
      if (written.saved) {
        if (prunedHourStart !== windows.hourStart) await dropPastWindowsAt(windows);
        break;
      }
      cursors = written.cursors;
      outcome = outcomeOf(batch, cursors);
    }

    const previous = cursors.inEpoch ?? cursors.latest;
    return { ok: true, ack: { batch }, outcome, ...(previous !== undefined && { previous }) };
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
    totals: totalsAt,
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
    takeChange,
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

function outcomeOf(batch: BatchId, { inEpoch, latest }: BatchCursors): BatchOutcome {
  if (!latest) return "first";
  if (!inEpoch) return "new-epoch";
  if (batch.sequence <= inEpoch.sequence) return "duplicate";
  return batch.sequence === inEpoch.sequence + 1 ? "next" : "gap";
}

function invalid(code: string, message: string): UsageIntake {
  return { ok: false, error: { code, message, status: 400 } };
}
