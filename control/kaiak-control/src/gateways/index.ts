// Gateway status and the live set (docs/specs/CONTROL-PROTOCOL.md, Gateway status):
// the latest status per instance with its receipt time, membership in the live set
// (joined by a status, left after a silence the expiry sweep finds), and the flag for
// two processes reporting under one instance name.

import { createListeners } from "../listeners/index.ts";
import { checkIntake, validateGatewayStatus } from "../messages/index.ts";
import type { GatewayStatus, IntakeError, IntakeRules } from "../messages/index.ts";
import type {
  ControlPlaneStore,
  ForgetGateway,
  GatewayConflict,
  GatewayRecord,
  StoreChange,
  StoredGateway,
} from "../storage/index.ts";

export type { GatewayConflict } from "../storage/index.ts";

// One gateway as a host shows it.
export interface GatewayView {
  instance: string;
  status: GatewayStatus;
  // When the latest status was received (milliseconds since the epoch).
  receivedAt: number;
  live: boolean;
  // Present while two processes appear to report under this instance name.
  conflict?: GatewayConflict;
}

export type StatusIntake =
  // joined: the status added the instance to the live set. conflictStarted: this
  // status raised the instance's conflict flag (it was not raised before). A refused
  // status's error code is "status-invalid" for a schema violation, a message rule
  // code ("timestamp-invalid"), or "instance-mismatch".
  { ok: true; joined: boolean; conflictStarted: boolean } | { ok: false; error: IntakeError };

// liveChanged: the live set gained or lost a member, so the live-gateway count changed.
export interface GatewaysChange {
  liveChanged: boolean;
}

export type GatewaysChangedListener = (change: GatewaysChange) => void;

// What one expiry of silent gateways changed, each list in instance order.
export interface GatewayExpiry {
  // Gateways that left the live set, the forgotten live ones included.
  expired: string[];
  forgotten: string[];
}

export interface Gateways {
  // Takes one status from `instance` (the requester's checked instance ID).
  acceptStatus(instance: string, doc: unknown): Promise<StatusIntake>;
  // Every gateway the control plane remembers, live or recently expired, by instance.
  gateways(): Promise<GatewayView[]>;
  // The size of the live set.
  liveGateways(): Promise<number>;
  // Calls listener after every gateway record any process wrote or forgot — every
  // accepted status, every sweep that changed a record — and on a store catch-up (as a
  // live-set change); returns the unsubscribe, which is safe to call more than once.
  onGatewaysChanged(listener: GatewaysChangedListener): () => void;
  // Takes one change the store announced (the core passes every one on).
  takeChange(change: StoreChange): void;
  // Drops gateways silent at `at` for the live timeout from the live set and forgets
  // those silent for the forget delay; rejects if the store fails.
  expireSilent(at: number): Promise<GatewayExpiry>;
}

export interface GatewaysOptions {
  store: ControlPlaneStore;
  // Milliseconds since the epoch.
  clock: () => number;
  // Silence after which a gateway leaves the live set.
  liveTimeoutMs: number;
  // Silence after which an expired gateway is forgotten.
  forgetAfterMs: number;
  // Called for each listener that throws; the status or the sweep stands either way.
  onListenerError: (error: unknown, change: GatewaysChange) => void;
}

const CONFLICT_REASON = "started-at-alternating";

const STATUS_INTAKE: IntakeRules<GatewayStatus> = {
  validate: validateGatewayStatus,
  schemaCode: "status-invalid",
  noun: "status",
  instanceOf: (status) => status.instance,
};

export function createGateways(options: GatewaysOptions): Gateways {
  const { store, clock, onListenerError, liveTimeoutMs, forgetAfterMs } = options;
  for (const [name, value] of Object.entries({ liveTimeoutMs, forgetAfterMs })) {
    if (!Number.isSafeInteger(value) || value <= 0) {
      throw Object.assign(new Error(`${name} must be a positive integer, got ${value}`), { code: "gateways-option-invalid" });
    }
  }

  const listeners = createListeners<GatewaysChange>(onListenerError);

  // Every listener hears of every change to the gateway records, this process's or
  // another's; a listener's failure goes to onListenerError and never reaches the store.
  // A store catching up after its channel was down may have missed a live-set change.
  const takeChange = (storeChange: StoreChange): void => {
    if (storeChange.type !== "gateways-changed" && storeChange.type !== "catch-up") return;
    listeners.emit({ liveChanged: storeChange.type === "catch-up" || storeChange.liveChanged });
  };

  // A status is judged against the stored record and written only while that record is
  // still the one stored: a status or sweep of another process in between makes it
  // judged again against the record that won, so the live set and the conflict rule
  // hold across processes. Its receipt time is taken once: a stored record with a later
  // receipt time — on the first read or after a refused write — holds a newer status,
  // and this one is dropped: written late, it would replace the newer status and read
  // as its process coming back.
  const recordStatus = async (status: GatewayStatus): Promise<StatusIntake> => {
    const receivedAt = clock();
    let previous = await store.gateway(status.instance);
    for (;;) {
      if (previous && previous.receivedAt > receivedAt) return { ok: true, joined: false, conflictStarted: false };
      const judged = judgeStatus(status, previous, receivedAt);
      const written = await store.saveGateway(judged.record, previous?.revision);
      if (written.saved) return { ok: true, joined: judged.joined, conflictStarted: judged.conflictStarted };
      previous = written.current;
    }
  };

  const judgeStatus = (
    status: GatewayStatus,
    previous: StoredGateway | undefined,
    now: number,
  ): { record: GatewayRecord; joined: boolean; conflictStarted: boolean } => {
    let replacedStart = previous?.replacedStart;
    let conflict = previous?.conflict;
    if (conflict && now - conflict.detectedAt >= liveTimeoutMs) conflict = undefined;
    let conflictStarted = false;
    if (previous && previous.status.started_at !== status.started_at) {
      // A restart moves to a new start time once. A start time replaced within the live
      // timeout coming back means the replaced process is still reporting.
      const cameBack =
        replacedStart !== undefined &&
        replacedStart.startedAt === status.started_at &&
        now - replacedStart.lastSeenAt < liveTimeoutMs;
      if (cameBack) {
        conflictStarted = conflict === undefined;
        conflict = { reason: CONFLICT_REASON, detectedAt: now };
      }
      replacedStart = { startedAt: previous.status.started_at, lastSeenAt: previous.receivedAt };
    }
    const record: GatewayRecord = {
      instance: status.instance,
      status,
      receivedAt: now,
      live: true,
      ...(replacedStart !== undefined && { replacedStart }),
      ...(conflict !== undefined && { conflict }),
    };
    return { record, joined: previous?.live !== true, conflictStarted };
  };

  // Every process sweeps. Each expiry and each forget is conditional on the record the
  // sweep judged, so a status that arrived meanwhile (or another process's sweep)
  // wins, and a gateway is never dropped from the live set while it reports.
  const expireSilent = async (at: number): Promise<GatewayExpiry> => {
    const expired: string[] = [];
    const toForget: ForgetGateway[] = [];
    const liveForgotten = new Set<string>();
    for (const gateway of await store.gateways()) {
      const silence = at - gateway.receivedAt;
      if (silence >= forgetAfterMs) {
        toForget.push({ instance: gateway.instance, revision: gateway.revision });
        if (gateway.live) liveForgotten.add(gateway.instance);
      } else if (gateway.live && silence >= liveTimeoutMs) {
        const { revision, ...record } = gateway;
        const written = await store.saveGateway({ ...record, live: false }, revision);
        if (written.saved) expired.push(gateway.instance);
      }
    }
    // In instance order, so two sweeps never take a database's records in opposite
    // orders.
    toForget.sort((a, b) => (a.instance < b.instance ? -1 : a.instance > b.instance ? 1 : 0));
    const forgotten = toForget.length > 0 ? await store.forgetGateways(toForget) : [];
    for (const instance of forgotten) if (liveForgotten.has(instance)) expired.push(instance);
    return { expired: expired.sort(), forgotten: [...forgotten].sort() };
  };

  return {
    async acceptStatus(instance, doc) {
      const intake = checkIntake(STATUS_INTAKE, instance, doc);
      if (!intake.ok) return intake;
      return recordStatus(intake.message);
    },

    async gateways() {
      const stored = await store.gateways();
      return stored
        .sort((a, b) => (a.instance < b.instance ? -1 : a.instance > b.instance ? 1 : 0))
        .map(({ instance, status, receivedAt, live, conflict }) => ({
          instance,
          status,
          receivedAt,
          live,
          ...(conflict !== undefined && { conflict }),
        }));
    },

    async liveGateways() {
      return (await store.gateways()).filter((gateway) => gateway.live).length;
    },

    onGatewaysChanged: listeners.add,

    expireSilent,

    takeChange,
  };
}
