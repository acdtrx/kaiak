// The storage interface: a database implements it in the real control plane; the
// in-memory store is the reference implementation. Every method is async so a
// database fits without change, and every call settles — resolves or rejects; a
// database store bounds every statement (a statement timeout), since a core's config
// deliveries run one at a time and one call that never returns holds the rest.
//
// The store is where control-plane processes agree (docs/specs/CONTROL-PROTOCOL.md,
// Control-plane processes): any number of cores may run over one store, and what they
// must agree on — the current config, the counted batches, the live set — is decided
// here, by conditional writes and consistent reads, and announced to every core
// through subscribe(). The store keeps no order of its own: a core orders what it
// sends on a stream by when it issued each read (CONTROL-PROTOCOL.md, Config stream →
// Order). A store meant for several processes
// holds each guarantee below across them; the in-memory store holds them across the
// cores of one process. The contract tests (`kaiak-control/store-contract`) check
// them against any store.

import type { BatchId, GatewayStatus, TotalsLimitType, UsageRecord } from "../messages/index.ts";

// A published config, as the store keeps it and gives it back: its JSON text, its
// content hash and when it was published.
export interface ConfigEntry {
  // The config's JSON text exactly as the control plane sends it in a `config` event.
  // The store keeps the text and returns it unchanged — a database keeps it as text,
  // never as a JSON type that may reorder members — so `hash` stays the hash of what
  // goes out.
  text: string;
  // The config_hash (docs/specs/CONTROL-PROTOCOL.md, Current config): lowercase hex
  // SHA-256 of `text`. It identifies content and carries no order.
  hash: string;
  // When it was published, by the control plane's clock (milliseconds since the epoch).
  publishedAt: number;
}

// One scope's window for one limit type: the scope — a group, absent for global — the
// type, and the window's start. Usage counts toward every scope on a record's path
// whatever the config's limits (CONTROL-PROTOCOL.md, Usage intake → Counted toward),
// and totals list every window with usage.
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

// One window start per counted limit type (milliseconds since the epoch): the windows
// current at one instant, or the oldest ones still needed.
export type WindowStarts = Record<TotalsLimitType, number>;

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

// One epoch's last counted batch of an instance, and when it was counted: the batch
// cursor. Each epoch keeps its own for the batch cursor retention, so a batch of an
// earlier epoch is still known as counted after its gateway moved to a new epoch
// (docs/specs/CONTROL-PROTOCOL.md, Usage intake).
export interface BatchCursor {
  batch: BatchId;
  // When it was counted, by the control plane's clock (milliseconds since the epoch).
  countedAt: number;
}

// What the conditional batch write found: the batch saved, or — when the instance's
// last counted batch in the batch's epoch was no longer the expected one — nothing
// written, and the instance's cursors now (lastBatches), for the caller to decide
// again.
export type SaveCountedBatchResult = { saved: true } | { saved: false; cursors: BatchCursor[] };

// What the conditional publish found: saved, or nothing written and the current config
// the store holds now.
export type PublishConfigResult = { saved: true } | { saved: false; current: ConfigEntry | undefined };

// The totals at one store snapshot (CONTROL-PROTOCOL.md, Messages → Totals: consistent
// snapshot): both members read together, so the windows hold exactly the batches the
// cursors name as counted, whichever process counted them. One snapshot serves every
// stream a core holds; each stream takes its own instance's cursors from it.
export interface TotalsSnapshot {
  // The stored totals of the current windows asked for, every scope and type with
  // usage.
  windows: WindowTotal[];
  // Every instance's last counted batch of each epoch still kept (in no particular
  // order).
  cursors: BatchId[];
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
  // forgotten and recreated included, and across a restore of the store (the
  // allocator moved past every value issued before it — CONTROL-PROTOCOL.md,
  // Control-plane processes → Restoring the store): a conditional write naming a
  // revision can only match the record it was computed from (the in-memory store
  // numbers every gateway write of the store).
  revision: number;
}

// What a conditional gateway write found: saved with the record's new revision, or
// nothing written and the record the store holds now.
export type SaveGatewayResult =
  | { saved: true; revision: number }
  | { saved: false; current: StoredGateway | undefined };

// A gateway to forget, with the revision of the record the sweep judged.
export interface ForgetGateway {
  instance: string;
  revision: number;
}

// What any process changed in the store, as every subscriber hears of it.
export type StoreChange =
  // A config was published and is the current one.
  | { type: "config-published"; hash: string }
  // A usage batch was counted.
  | { type: "batch-counted"; instance: string }
  // A gateway record was written or forgotten; liveChanged when the live set gained or
  // lost a member.
  | { type: "gateways-changed"; liveChanged: boolean }
  // The store's change channel may have missed changes (it reconnected): anything may
  // have changed, and every subscriber reads what it depends on again.
  | { type: "catch-up" };

export type StoreChangeListener = (change: StoreChange) => void;

export interface ControlPlaneStore {
  // The current config, or undefined before the first publish.
  currentConfig(): Promise<ConfigEntry | undefined>;
  // Replaces the current config with entry, only when the current config is still the
  // one whose hash is expectedHash (undefined: none published yet) — the config the
  // publish was checked against. Otherwise nothing changes, and the answer names the
  // current config. The store keeps no earlier configs. A publish depends on no usage:
  // counted batches never refuse it, and it never refuses them.
  publishConfig(entry: ConfigEntry, expectedHash: string | undefined): Promise<PublishConfigResult>;

  // An instance's batch cursors: its last counted batch of each epoch still kept, one
  // per epoch, in no particular order; none before its first batch.
  lastBatches(instance: string): Promise<BatchCursor[]>;
  // Stores a counted batch in one write — its ID as the instance's last batch in its
  // epoch, its additions to the totals, its records — only when the instance's last counted batch in the batch's epoch is still
  // `expectedLast` (undefined: none yet in that epoch). So the batch is either counted
  // and remembered or neither, and no two writers can both count one batch, whichever
  // epochs were counted in between. The config plays no part: a publish never refuses
  // a batch. Records older than the newest `keepRecords` are no longer needed and may
  // be dropped. A store that also keeps every record for good (a ledger) inserts them
  // idempotently by record_id: a batch resent after its cursor's retention is counted
  // again (CONTROL-PROTOCOL.md, Status intake → Batch cursor retention), and a ledger refusing its records
  // would refuse the batch for ever.
  saveCountedBatch(
    counted: CountedBatch,
    expectedLast: BatchId | undefined,
    keepRecords: number,
  ): Promise<SaveCountedBatchResult>;

  // The totals at one snapshot: the stored totals of the current windows (each
  // window whose start is `current[type]`) and every instance's last counted batch of
  // each epoch, both as of one point between writes.
  totalsSnapshot(current: WindowStarts): Promise<TotalsSnapshot>;
  // Totals of windows before `oldest` (each window starting before `oldest[type]`) are
  // no longer needed and may be dropped. The caller passes the previous windows: late
  // records still count there. Dropping changes no current window and is not
  // announced.
  dropPastWindowTotals(oldest: WindowStarts): Promise<void>;

  // The newest received records, newest first, at most `limit`: the reverse of the
  // order they were saved, a batch's records saved in batch order, so records with the
  // same receipt time keep a defined order.
  recentRecords(limit: number): Promise<ReceivedRecord[]>;

  // One gateway's record, or undefined before its first status or once forgotten.
  gateway(instance: string): Promise<StoredGateway | undefined>;
  // Every gateway record, in no particular order.
  gateways(): Promise<StoredGateway[]>;
  // Writes a gateway's record, replacing the previous one, only when the stored record
  // is still the one at `expectedRevision` (undefined: none stored).
  saveGateway(record: GatewayRecord, expectedRevision: number | undefined): Promise<SaveGatewayResult>;
  // Forgets each gateway whose stored record is still at the given revision; returns the
  // instances forgotten, announced as one change. Their last counted batches stay
  // (dropBatchCursorsCountedBefore).
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
  // stays missed — after the reconnect, so a read the listener makes then sees every
  // change made while the channel was down; the in-memory store never drops one. A read
  // a listener makes after hearing of a change sees that change: the current config
  // after config-published, the gateway records after gateways-changed, the totals
  // after batch-counted (a database store reads them from its primary, never from a
  // replica the notification may be ahead of).
  subscribe(listener: StoreChangeListener): () => void;
}
