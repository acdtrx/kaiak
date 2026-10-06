// The storage interface: a database implements it in the real control plane; the
// in-memory store is the reference implementation. Every method is async so a
// database fits without change.
//
// The store is where control-plane processes agree (docs/specs/CONTROL-PROTOCOL.md,
// Control-plane processes): any number of cores may run over one store, and what they
// must agree on — the totals sequence, the config versions, the counted batches, the
// live set — is decided here, by conditional writes and consistent reads, and
// announced to every core through subscribe(). A store meant for several processes
// holds each guarantee below across them; the in-memory store holds them across the
// cores of one process. The contract tests (`kaiak-control/store-contract`) check
// them against any store.

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

// One scope's window for one limit type: the scope — a group, absent for global — the
// type, and the window's start. Usage counts toward every scope on a record's path
// whatever the config's limits (CONTROL-PROTOCOL.md, Usage intake → Counted toward);
// totals list the windows the config limits.
export interface WindowKey {
  group?: string;
  type: TotalsLimitType;
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

// What the conditional batch write found: the batch saved with the totals sequence it
// moved to, or — when the instance's last counted batch was no longer the expected
// one — nothing written, and the last batch the store holds, for the caller to decide
// again.
export type SaveCountedBatchResult = { saved: true; sequence: number } | { saved: false; last: BatchId | undefined };

// What the conditional publish found: saved with the totals sequence it moved to, or
// nothing written and the latest version the store holds now.
export type PublishConfigResult =
  | { saved: true; sequence: number }
  | { saved: false; latestVersion: number | undefined };

// The totals at one store snapshot (CONTROL-PROTOCOL.md, Messages → Totals: consistent
// snapshot): every member read together, so the windows hold exactly the batches
// counted at `sequence` — `last` among them.
export interface TotalsSnapshot {
  // The totals sequence: the revision of a totals message made from this snapshot.
  sequence: number;
  // The latest config version, or undefined before the first publish.
  config: StoredConfig | undefined;
  // The asked instance's last counted batch (undefined before its first, or when no
  // instance was asked).
  last: BatchId | undefined;
  // The stored totals of the current windows asked for.
  windows: WindowTotal[];
  // The size of the live set.
  liveGateways: number;
}

// Two processes reporting under one instance name (docs/specs/CONTROL-PROTOCOL.md,
// Gateway status): their statuses alternate between two start times.
export interface GatewayConflict {
  reason: "started-at-alternating";
  // The latest status that showed it, by the control plane's clock (milliseconds since
  // the epoch).
  detectedAt: number;
}

// One gateway as the control plane knows it from its statuses, as a core writes it.
export interface GatewayRecord {
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

// A gateway as the store holds it: the record and its write count, which a conditional
// write names to say which record it was computed from.
export interface StoredGateway extends GatewayRecord {
  // Starts at 1 with the first write of the instance and moves on by one with every
  // write; a forgotten gateway that reports again starts at 1 again.
  revision: number;
}

// What a conditional gateway write found: saved (with the totals sequence, which moved
// when the write changed the live set), or nothing written and the record the store
// holds now.
export type SaveGatewayResult =
  | { saved: true; revision: number; sequence: number }
  | { saved: false; current: StoredGateway | undefined };

// A gateway to forget, with the revision of the record the sweep judged.
export interface ForgetGateway {
  instance: string;
  revision: number;
}

// What any process changed in the store, as every subscriber hears of it. `sequence` is
// the totals sequence after the change.
export type StoreChange =
  // A config version was published (the totals moved with it).
  | { type: "config-published"; version: number; sequence: number }
  // A usage batch was counted.
  | { type: "batch-counted"; instance: string; sequence: number }
  // A gateway record was written or forgotten; liveChanged when the live set gained or
  // lost a member (the totals moved with it).
  | { type: "gateways-changed"; liveChanged: boolean; sequence: number };

export type StoreChangeListener = (change: StoreChange) => void;

export interface ControlPlaneStore {
  // The store's config epoch: 32 random lowercase hex digits, created with the store
  // and kept for as long as it keeps its config versions and its totals sequence. Both
  // count within it, so gateways tell a store that started over (versions from 1
  // again, the sequence from 0) from the one they followed; a store that loses or
  // rolls back its state takes a new one (CONTROL-PROTOCOL.md, Config versions).
  configEpoch(): Promise<string>;

  // The newest config version, or undefined before the first publish.
  latestConfig(): Promise<StoredConfig | undefined>;
  // Stores entry as the newest version, only when the latest version is still
  // expectedVersion (undefined: none yet); the write moves the totals sequence on by
  // one (the totals list the new config's limits). entry.version must be
  // expectedVersion + 1 (1 for the first). Otherwise nothing changes, and the answer
  // names the latest version. A publish depends on no usage: counted batches never
  // refuse it, and it never refuses them. Versions older than the newest `keep` are no
  // longer needed and may be dropped; a store may keep more.
  publishConfig(entry: StoredConfig, expectedVersion: number | undefined, keep: number): Promise<PublishConfigResult>;
  // The stored versions newer than `version`, oldest first.
  configsAfter(version: number): Promise<StoredConfig[]>;

  // The last batch counted for an instance, or undefined before its first.
  lastBatch(instance: string): Promise<BatchId | undefined>;
  // Stores a counted batch in one write — its ID as the instance's last batch, its
  // additions to the totals, its records, a step of the totals sequence — only when the
  // instance's last counted batch is still `expectedLast` (undefined: none yet). So the
  // batch is either counted and remembered or neither, and no two writers can both
  // count one batch. The config plays no part: a publish never refuses a batch.
  // Records older than the newest `keepRecords` are no longer needed and may be
  // dropped.
  saveCountedBatch(
    counted: CountedBatch,
    expectedLast: BatchId | undefined,
    keepRecords: number,
  ): Promise<SaveCountedBatchResult>;

  // The totals at one snapshot: the sequence, the latest config, `instance`'s last
  // counted batch (when given), the stored totals of the current windows (tokens_per_hour
  // windows starting at hourStart, usd_per_month windows at monthStart) and the live
  // set's size, all as of one point between writes.
  totalsSnapshot(current: CurrentWindows, instance?: string): Promise<TotalsSnapshot>;
  // Totals of windows before `oldest` (tokens_per_hour windows starting before its
  // hourStart, usd_per_month windows before its monthStart) are no longer needed and
  // may be dropped. The caller passes the previous windows: late records still count
  // there. Dropping changes no current window, so it does not move the sequence.
  dropPastWindowTotals(oldest: CurrentWindows): Promise<void>;

  // The newest received records, newest first, at most `limit`.
  recentRecords(limit: number): Promise<ReceivedRecord[]>;

  // One gateway's record, or undefined before its first status or once forgotten.
  gateway(instance: string): Promise<StoredGateway | undefined>;
  // Every gateway record, in no particular order.
  gateways(): Promise<StoredGateway[]>;
  // Writes a gateway's record, replacing the previous one, only when the stored record
  // is still the one at `expectedRevision` (undefined: none stored). A write that
  // changes `live` (or adds a live gateway) moves the totals sequence in the same write.
  saveGateway(record: GatewayRecord, expectedRevision: number | undefined): Promise<SaveGatewayResult>;
  // Forgets each gateway whose stored record is still at the given revision; returns the
  // instances forgotten. Forgetting a live gateway moves the totals sequence (once for
  // the call). Their last counted batches stay (dropBatchCursorsCountedBefore).
  forgetGateways(gateways: ForgetGateway[]): Promise<string[]>;
  // Drops the last counted batch of every instance whose last batch was counted
  // before `cutoff` (milliseconds since the epoch), and names those instances.
  dropBatchCursorsCountedBefore(cutoff: number): Promise<string[]>;

  // Calls listener with every change any process makes from now on — this one's
  // included — after the write it describes; returns the unsubscribe, which is safe to
  // call more than once. A listener hears changes in the order the store made them. The
  // store calls every listener whatever another one does; a listener that throws is
  // the store's to report (the memory store rethrows it from a microtask).
  subscribe(listener: StoreChangeListener): () => void;
}
