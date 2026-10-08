// Usage intake and totals (docs/specs/CONTROL-PROTOCOL.md, Usage batches, Usage intake,
// Budgets, Messages → Totals): batches taken exactly once per batch ID, their records
// stamped with the control plane's receipt time and counted into the hour and month
// windows of every scope on their path — each in its gateway_time window when that is
// the current or previous one — whether or not a config is published, and totals of
// the current windows, every scope and type with usage whatever the config, each read
// from one store snapshot. Nothing here is held per process that another process could
// disagree with: the store decides which batches count.

import { createListeners } from "../listeners/index.ts";
import { checkIntake, validateUsageBatch } from "../messages/index.ts";
import type { BatchId, IntakeError, IntakeRules, TotalsLimitType, TotalsWindow, UsageAck, UsageBatch } from "../messages/index.ts";
import type { BatchCursor, ControlPlaneStore, ReceivedRecord, StoreChange, WindowStarts, WindowTotal } from "../storage/index.ts";

import { batchAdditions } from "./aggregate.ts";
import { currentWindows, formatWindowStart, formatWindowStarts, previousWindows, sameWindows } from "./windows.ts";

// How a batch relates to the last one counted for its instance:
// - first: the instance's first batch;
// - next: the next sequence in the same epoch;
// - gap: a later sequence in the same epoch, some skipped (lost on the gateway side);
// - new-epoch: an epoch with nothing counted yet, after batches of another (a new
//   gateway process);
// - duplicate: counted before (its epoch's last counted batch is at or past it) — acked
//   again, not counted.
export type BatchOutcome = "first" | "next" | "gap" | "new-epoch" | "duplicate";

// previous: the batch before it — its epoch's last counted batch, or for a new epoch
// the instance's batch counted last. A refused batch's error code is
// "usage-batch-invalid" for a schema violation, a message rule code
// ("record-instance-mismatch", "record-id-duplicate", "timestamp-invalid"), or
// "instance-mismatch".
export type UsageIntake =
  | { ok: true; ack: UsageAck; outcome: BatchOutcome; previous?: BatchId }
  | { ok: false; error: IntakeError };

export type TotalsChangedListener = () => void;

// The counted totals at one store snapshot, for every gateway at once: each stream
// takes its own instance's cursors (its counted_through) from it, and a host shows the
// windows against its limits.
export interface CountedTotals {
  // Every scope and type with usage in its current window, whatever the config.
  windows: TotalsWindow[];
  // Every instance's last counted batch of each epoch still kept.
  cursors: BatchId[];
  // The current windows the snapshot was read for, as window_start values.
  windowStarts: Record<TotalsLimitType, string>;
}

export interface Usage {
  // Takes one usage batch from `instance` (the requester's checked instance ID).
  acceptUsageBatch(instance: string, doc: unknown): Promise<UsageIntake>;
  // The counted totals at one snapshot, for every gateway (CountedTotals).
  readTotals(): Promise<CountedTotals>;
  // The newest received records, newest first.
  recentRecords(): Promise<ReceivedRecord[]>;
  // Calls listener after every batch any process counted (and on a store catch-up);
  // returns the unsubscribe, which is safe to call more than once.
  onTotalsChanged(listener: TotalsChangedListener): () => void;
  // Takes one change the store announced (the core passes every one on).
  takeChange(change: StoreChange): void;
  // Lets the store drop totals of windows before the previous ones (a late record can
  // still count in the previous window), once per hour in this process: a call within
  // the hour this process last dropped in does nothing. The core's expiry sweep runs
  // it, so a store that fails it fails the sweep run, reported to the host, never a
  // batch.
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

const BATCH_INTAKE: IntakeRules<UsageBatch> = {
  validate: validateUsageBatch,
  schemaCode: "usage-batch-invalid",
  noun: "batch",
  instanceOf: (message) => message.batch.instance,
};

export function createUsage({ store, clock, recentRecordsSize, onListenerError }: UsageOptions): Usage {
  if (!Number.isSafeInteger(recentRecordsSize) || recentRecordsSize < 0) {
    throw Object.assign(new Error(`recent records size must be a non-negative integer, got ${recentRecordsSize}`), {
      code: "recent-records-size-invalid",
    });
  }

  const listeners = createListeners<void>(onListenerError);
  // Batches from one instance run one at a time in this process, so a resend racing
  // its original here is decided without a refused write; across processes the
  // store's conditional write decides.
  const queues = new Map<string, Promise<unknown>>();
  // The windows current when this process last dropped past windows: dropping is
  // idempotent, so every process drops on its own, once an hour.
  let prunedIn: WindowStarts | undefined;

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
    listeners.emit();
  };

  // The windows a snapshot reads are the ones current once the read is over: a read
  // that crossed an hour or month boundary — while a batch past it may have been
  // counted — reads again with the new windows, so a message never names a batch
  // counted while leaving out the window it was counted in.
  const readTotals = async (): Promise<CountedTotals> => {
    let windows = currentWindows(clock());
    for (;;) {
      const snapshot = await store.totalsSnapshot(windows);
      const after = currentWindows(clock());
      if (!sameWindows(after, windows)) {
        windows = after;
        continue;
      }
      return {
        windows: listedWindows(snapshot.windows, windows),
        cursors: snapshot.cursors,
        windowStarts: formatWindowStarts(windows),
      };
    }
  };

  // The previous windows stay: a late record still counts in its own window when that
  // is the previous one.
  const dropPastWindows = async (): Promise<void> => {
    const windows = currentWindows(clock());
    if (prunedIn && sameWindows(prunedIn, windows)) return;
    await store.dropPastWindowTotals(previousWindows(windows));
    prunedIn = windows;
  };

  const countBatch = async ({ batch, records }: UsageBatch): Promise<UsageIntake> => {
    // The write is conditional on the last batch of the batch's epoch the outcome was
    // decided against: when another writer counted a batch of this instance in that
    // epoch in between, nothing is written and the outcome is decided again against the
    // store's — so a batch is counted once however many processes take it, and however
    // many epochs its gateway went through meanwhile.
    let cursors = await store.lastBatches(batch.instance);
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
        inEpochOf(batch, cursors),
        recentRecordsSize,
      );
      if (written.saved) break;
      cursors = written.cursors;
      outcome = outcomeOf(batch, cursors);
    }

    const previous = inEpochOf(batch, cursors) ?? countedLastOf(cursors);
    return { ok: true, ack: { batch }, outcome, ...(previous !== undefined && { previous }) };
  };

  return {
    async acceptUsageBatch(instance, doc) {
      const intake = checkIntake(BATCH_INTAKE, instance, doc);
      if (!intake.ok) return intake;
      return serialized(instance, () => countBatch(intake.message));
    },
    readTotals,
    recentRecords: () => store.recentRecords(recentRecordsSize),
    onTotalsChanged: listeners.add,
    dropPastWindows,
    takeChange,
  };
}

// The windows a totals message lists: every scope and type with usage in its current
// window, whatever the config.
function listedWindows(stored: readonly WindowTotal[], windows: WindowStarts): TotalsWindow[] {
  const listed: TotalsWindow[] = [];
  for (const { group, type, windowStart, used } of stored) {
    if (windowStart !== windows[type] || used === 0n) continue;
    listed.push({
      ...(group !== undefined && { group }),
      type,
      window_start: formatWindowStart(windowStart),
      used: (used > MAX_USED ? MAX_USED : used).toString(),
    });
  }
  return listed;
}

function outcomeOf(batch: BatchId, cursors: readonly BatchCursor[]): BatchOutcome {
  if (cursors.length === 0) return "first";
  const inEpoch = inEpochOf(batch, cursors);
  if (!inEpoch) return "new-epoch";
  if (batch.sequence <= inEpoch.sequence) return "duplicate";
  return batch.sequence === inEpoch.sequence + 1 ? "next" : "gap";
}

// The last batch counted in the batch's own epoch.
function inEpochOf(batch: BatchId, cursors: readonly BatchCursor[]): BatchId | undefined {
  return cursors.find((cursor) => cursor.batch.epoch === batch.epoch)?.batch;
}

// The instance's batch counted last, in any epoch: of two counted at one instant,
// either — it only names the batch before in the intake's answer.
function countedLastOf(cursors: readonly BatchCursor[]): BatchId | undefined {
  let last: BatchCursor | undefined;
  for (const cursor of cursors) if (!last || cursor.countedAt > last.countedAt) last = cursor;
  return last?.batch;
}
