// The storage interface: a database implements it in the real control plane; the
// in-memory store is the reference implementation. Every method is async so a
// database fits without change.
//
// The store is where control-plane processes agree (docs/specs/CONTROL-PROTOCOL.md,
// Control-plane processes): any number of cores may run over one store, and what they
// must agree on — the sequence, the current config, the counted batches, the live set
// — is decided here, by conditional writes and consistent reads, and
// announced to every core through subscribe(). A store meant for several processes
// holds each guarantee below across them; the in-memory store holds them across the
// cores of one process. The contract tests (`kaiak-control/store-contract`) check
// them against any store.

import type { Config } from "../config/index.ts";
import type { BatchId, GatewayStatus, TotalsLimitType, UsageRecord } from "../messages/index.ts";

// A config to publish: the document, its content hash and when it was published.
export interface ConfigEntry {
  config: Config;
  // The config_hash (docs/specs/CONTROL-PROTOCOL.md, Current config): lowercase hex
  // SHA-256 of the config's JSON text as the control plane sends it. It identifies
  // content and carries no order.
  hash: string;
  // When it was published, by the control plane's clock (milliseconds since the epoch).
  publishedAt: number;
}

// The current config as the store holds it: the entry and the sequence its publish
// moved the store to, which orders it against what a core already sent on a stream.
export interface CurrentConfig extends ConfigEntry {
  sequence: number;
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

// Where an instance's counting stands, as a batch is decided against it: the last batch
// counted in the batch's own epoch, and the last batch counted in any epoch — the one
// totals name as counted_through. Each epoch keeps its own last batch for the batch
// cursor retention, so a batch of an earlier epoch is still known as counted after its
// gateway moved to a new epoch (docs/specs/CONTROL-PROTOCOL.md, Usage intake).
export interface BatchCursors {
  inEpoch: BatchId | undefined;
  latest: BatchId | undefined;
}

// What the conditional batch write found: the batch saved with the sequence it moved
// to, or — when the instance's last counted batch in the batch's epoch was no longer
// the expected one — nothing written, and where the instance's counting stands now,
// for the caller to decide again.
export type SaveCountedBatchResult = { saved: true; sequence: number } | { saved: false; cursors: BatchCursors };

// What the conditional publish found: saved with the sequence it moved to, or nothing
// written and the current config the store holds now.
export type PublishConfigResult =
  | { saved: true; sequence: number }
  | { saved: false; current: CurrentConfig | undefined };

// The totals at one store snapshot (CONTROL-PROTOCOL.md, Messages → Totals: consistent
// snapshot): every member read together, so the windows hold exactly the batches
// counted at `sequence` — `last` among them.
export interface TotalsSnapshot {
  // The sequence at the snapshot. It orders what a core sends on a stream and never
  // goes on the wire.
  sequence: number;
  // The current config, or undefined before the first publish.
  config: CurrentConfig | undefined;
  // The asked instance's last counted batch in any epoch (undefined before its first,
  // or when no instance was asked).
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
  // Changes with every write of the instance and never repeats for it, a gateway
  // forgotten and recreated included: a conditional write naming a revision can only
  // match the record it was computed from (the in-memory store numbers every gateway
  // write of the store).
  revision: number;
}

// What a conditional gateway write found: saved (with the sequence, which moved when
// the write changed the live set), or nothing written and the record the store
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
// the store's sequence after the change.
export type StoreChange =
  // A config was published and is the current one (the totals moved with it).
  | { type: "config-published"; hash: string; sequence: number }
  // A usage batch was counted.
  | { type: "batch-counted"; instance: string; sequence: number }
  // A gateway record was written or forgotten; liveChanged when the live set gained or
  // lost a member (the totals moved with it).
  | { type: "gateways-changed"; liveChanged: boolean; sequence: number }
  // The store's change channel may have missed changes (it reconnected): anything may
  // have changed, and every subscriber reads what it depends on again.
  | { type: "catch-up"; sequence: number };

export type StoreChangeListener = (change: StoreChange) => void;

export interface ControlPlaneStore {
  // The current config, or undefined before the first publish.
  currentConfig(): Promise<CurrentConfig | undefined>;
  // Replaces the current config with entry, only when the current config is still the
  // one whose hash is expectedHash (undefined: none published yet) — the config the
  // publish was checked against; the write moves the sequence on by one (the totals
  // list the new config's limits). Otherwise nothing changes, and the answer names the
  // current config. The store keeps no earlier configs. A publish depends on no usage:
  // counted batches never refuse it, and it never refuses them.
  publishConfig(entry: ConfigEntry, expectedHash: string | undefined): Promise<PublishConfigResult>;

  // Where an instance's counting stands for a batch of `epoch`: its last counted batch in
  // that epoch, and in any epoch.
  lastBatch(instance: string, epoch: string): Promise<BatchCursors>;
  // Stores a counted batch in one write — its ID as the instance's last batch in its
  // epoch and in any epoch, its additions to the totals, its records, a step of the
  // sequence — only when the instance's last counted batch in the batch's epoch is still
  // `expectedLast` (undefined: none yet in that epoch). So the batch is either counted
  // and remembered or neither, and no two writers can both count one batch, whichever
  // epochs were counted in between. The config plays no part: a publish never refuses
  // a batch. Records older than the newest `keepRecords` are no longer needed and may
  // be dropped. A store that also keeps every record for good (a ledger) inserts them
  // idempotently by record_id: a batch resent after its cursor's retention is counted
  // again (Usage intake → Batch cursor retention), and a ledger refusing its records
  // would refuse the batch for ever.
  saveCountedBatch(
    counted: CountedBatch,
    expectedLast: BatchId | undefined,
    keepRecords: number,
  ): Promise<SaveCountedBatchResult>;

  // The totals at one snapshot: the sequence, the current config, `instance`'s last
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
  // changes `live` (or adds a live gateway) moves the sequence in the same write.
  saveGateway(record: GatewayRecord, expectedRevision: number | undefined): Promise<SaveGatewayResult>;
  // Forgets each gateway whose stored record is still at the given revision; returns the
  // instances forgotten. Forgetting a live gateway moves the sequence (once for
  // the call). Their last counted batches stay (dropBatchCursorsCountedBefore).
  forgetGateways(gateways: ForgetGateway[]): Promise<string[]>;
  // Drops every instance's last counted batch of each epoch counted before `cutoff`
  // (milliseconds since the epoch), and names the instances that lost one.
  dropBatchCursorsCountedBefore(cutoff: number): Promise<string[]>;

  // Calls listener with every change any process makes from now on — this one's
  // included — after the write it describes; returns the unsubscribe, which is safe to
  // call more than once. A listener hears changes in the order the store made them. The
  // store calls every listener whatever another one does; a listener that throws is
  // the store's to report (the memory store rethrows it from a microtask). A store whose
  // change channel can drop changes (a database's listening connection) announces a
  // `catch-up` to every listener each time the channel is back, so nothing it missed
  // stays missed; the in-memory store never drops one.
  subscribe(listener: StoreChangeListener): () => void;
}
