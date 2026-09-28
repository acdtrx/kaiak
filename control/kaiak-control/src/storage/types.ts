// The storage interface: a database implements it in the real control plane; the
// in-memory store is the reference implementation. Every method is async so a
// database fits without change.

import type { Config } from "../config/index.ts";
import type { BatchId, GatewayStatus, TotalsLimitType, UsageRecord } from "../messages/index.ts";

// One published config version.
export interface StoredConfig {
  // Integers from 1, increasing by one with every publish.
  version: number;
  config: Config;
  // When it was published, by the control plane's clock (milliseconds since the epoch).
  publishedAt: number;
}

// One limit's window: the limit's identity — its group (absent for a global limit),
// type and model set — and the window's start.
export interface WindowKey {
  group?: string;
  type: TotalsLimitType;
  // The limit's model set, sorted; absent = all models.
  models?: string[];
  // Milliseconds since the epoch: the top of a UTC hour (tokens_per_hour) or the first
  // of a UTC month (usd_per_month).
  windowStart: number;
}

// The amount used in one limit's window: tokens, or nano-USD for usd_per_month.
export interface WindowTotal extends WindowKey {
  used: bigint;
}

// The windows current at one instant.
export interface CurrentWindows {
  hourStart: number;
  monthStart: number;
}

// A usage record as the control plane took it.
export interface ReceivedRecord {
  // When the control plane received it, by its clock (milliseconds since the epoch).
  receivedAt: number;
  record: UsageRecord;
}

// What counting one usage batch changes.
export interface CountedBatch {
  // Becomes the instance's last counted batch.
  batch: BatchId;
  // When it was counted, by the control plane's clock (milliseconds since the epoch):
  // the last counted batch is kept for the batch cursor retention from then.
  countedAt: number;
  // Added to the stored totals, one entry per window (a window not stored yet starts
  // from 0).
  additions: WindowTotal[];
  records: ReceivedRecord[];
}

// What the conditional batch write found: the batch saved, or — when the instance's
// last counted batch was no longer the expected one — nothing written and the last
// batch the store holds, for the caller to decide again.
export type SaveCountedBatchResult = { saved: true } | { saved: false; last: BatchId | undefined };

// The store's lease: which control-plane process may write to it, until when
// (milliseconds since the epoch, by the holder's clock).
export interface StoreLease {
  holder: string;
  expiresAt: number;
}

export type AcquireLeaseResult = { ok: true } | { ok: false; lease: StoreLease };

// Two processes reporting under one instance name (docs/specs/CONTROL-PROTOCOL.md,
// Gateway status): their statuses alternate between two start times.
export interface GatewayConflict {
  reason: "started-at-alternating";
  // The latest status that showed it, by the control plane's clock (milliseconds since
  // the epoch).
  detectedAt: number;
}

// One gateway as the control plane knows it from its statuses.
export interface StoredGateway {
  instance: string;
  // The latest accepted status.
  status: GatewayStatus;
  // When it was received, by the control plane's clock (milliseconds since the epoch).
  receivedAt: number;
  // In the live set: from its first status until the expiry sweep finds it silent.
  live: boolean;
  // The start time the latest status replaced, and when a status last carried it — how
  // a start time coming back is told from a restart.
  replacedStart?: { startedAt: string; lastSeenAt: number };
  conflict?: GatewayConflict;
}

export interface ControlPlaneStore {
  // The store's config epoch: 32 random lowercase hex digits, created with the store
  // and kept for as long as it keeps its config versions. Versions count within it, so
  // gateways tell a store that started over (versions from 1 again) from the one they
  // followed (docs/specs/CONTROL-PROTOCOL.md, Config versions).
  configEpoch(): Promise<string>;
  // The newest config version, or undefined before the first publish.
  latestConfig(): Promise<StoredConfig | undefined>;
  // Stores entry as the newest version. Versions older than the newest `keep` are no
  // longer needed and may be dropped; a store may keep more.
  saveConfig(entry: StoredConfig, keep: number): Promise<void>;
  // The stored versions newer than `version`, oldest first.
  configsAfter(version: number): Promise<StoredConfig[]>;

  // One control-plane process per store (docs/specs/CONTROL-PROTOCOL.md, Control-plane
  // processes): takes the lease for `holder` until `expiresAt`, or renews it —
  // granted when no one holds it, when `holder` already does, or when the lease has
  // expired by `now`; otherwise nothing changes and the answer is the lease in force.
  // A database does the check and the write in one transaction.
  acquireLease(holder: string, now: number, expiresAt: number): Promise<AcquireLeaseResult>;
  // Gives the lease up when `holder` holds it; otherwise nothing changes.
  releaseLease(holder: string): Promise<void>;

  // The last batch counted for an instance, or undefined before its first.
  lastBatch(instance: string): Promise<BatchId | undefined>;
  // Stores a counted batch in one step — its ID as the instance's last batch, its
  // additions to the totals, its records — only when the instance's last counted batch
  // is still `expectedLast` (undefined: none yet), the one the caller decided against.
  // So the batch is either counted and remembered or neither, and no two writers can
  // both count one batch: the comparison and the write are one atomic operation (a
  // database does both in one transaction). Records older than the newest
  // `keepRecords` are no longer needed and may be dropped.
  saveCountedBatch(
    counted: CountedBatch,
    expectedLast: BatchId | undefined,
    keepRecords: number,
  ): Promise<SaveCountedBatchResult>;
  // Adds to the stored totals, one entry per window (a window not stored yet starts
  // from 0): a limit carrying its spend across a model-set edit.
  addWindowTotals(additions: WindowTotal[]): Promise<void>;
  // The stored totals of the current windows: tokens_per_hour windows starting at
  // hourStart, usd_per_month windows starting at monthStart.
  currentWindowTotals(current: CurrentWindows): Promise<WindowTotal[]>;
  // Totals of windows before `oldest` (tokens_per_hour windows starting before its
  // hourStart, usd_per_month windows before its monthStart) are no longer needed and
  // may be dropped. The caller passes the previous windows: late records still count
  // there.
  dropPastWindowTotals(oldest: CurrentWindows): Promise<void>;
  // The newest received records, newest first, at most `limit`.
  recentRecords(limit: number): Promise<ReceivedRecord[]>;

  // One gateway's record, or undefined before its first status or once forgotten.
  gateway(instance: string): Promise<StoredGateway | undefined>;
  // Every gateway record, in no particular order.
  gateways(): Promise<StoredGateway[]>;
  // Stores a gateway's record, replacing the previous one.
  saveGateway(gateway: StoredGateway): Promise<void>;
  // Forgets gateways' records; unknown instances are ignored. Their last counted
  // batches stay (dropBatchCursorsCountedBefore).
  deleteGateways(instances: string[]): Promise<void>;
  // Drops the last counted batch of every instance whose last batch was counted
  // before `cutoff` (milliseconds since the epoch), and names those instances.
  dropBatchCursorsCountedBefore(cutoff: number): Promise<string[]>;
}
