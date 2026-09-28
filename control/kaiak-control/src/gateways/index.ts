// Gateway status and the live set (docs/specs/CONTROL-PROTOCOL.md, Gateway status):
// the latest status per instance with its receipt time, membership in the live set
// (joined by a status, left after a silence the expiry sweep finds), and the flag for
// two processes reporting under one instance name.

import { validateGatewayStatus } from "../messages/index.ts";
import type { GatewayStatus } from "../messages/index.ts";
import type { ControlPlaneStore, GatewayConflict, StoredGateway } from "../storage/index.ts";

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

export interface StatusError {
  // "status-invalid" (schema violation), a message rule code ("timestamp-invalid"),
  // or "instance-mismatch" (the body's instance is not the requester's).
  code: string;
  message: string;
  // The HTTP status an adapter answers with.
  status: 400;
}

export type StatusIntake =
  // joined: the status added the instance to the live set. conflictStarted: this
  // status raised the instance's conflict flag (it was not raised before).
  { ok: true; joined: boolean; conflictStarted: boolean } | { ok: false; error: StatusError };

// liveChanged: the live set gained or lost a member, so the live-gateway count changed.
export interface GatewaysChange {
  liveChanged: boolean;
}

export type GatewaysChangedListener = (change: GatewaysChange) => void;

// One run of the expiry sweep. trigger is what ran it: "schedule" for the core's timer,
// whatever the caller passed otherwise.
export type ExpirySweepRun =
  | {
      trigger: string;
      at: number;
      ok: true;
      expired: string[];
      forgotten: string[];
      // Instances whose last counted batch passed the batch cursor retention.
      batchCursorsDropped: string[];
    }
  | { trigger: string; at: number; ok: false; error: unknown };

export interface Gateways {
  // Takes one status from `instance` (the requester's checked instance ID).
  acceptStatus(instance: string, doc: unknown): Promise<StatusIntake>;
  // Every gateway the control plane remembers, live or recently expired, by instance.
  gateways(): Promise<GatewayView[]>;
  // The size of the live set.
  liveGateways(): Promise<number>;
  // Calls listener after every accepted status and every sweep that changed the live
  // set; returns the unsubscribe, which is safe to call more than once.
  onGatewaysChanged(listener: GatewaysChangedListener): () => void;
  // Drops gateways silent for the live timeout from the live set, forgets those
  // silent for the forget delay, and drops last counted batches past the batch cursor
  // retention. Resolves with the run (also given to onExpirySweep);
  // rejects if the store fails.
  expireSilentGateways(trigger: string): Promise<ExpirySweepRun & { ok: true }>;
  // Runs the sweep on a timer until stopped. Starting twice keeps one timer.
  startExpirySweep(): void;
  stopExpirySweep(): void;
}

export interface GatewaysOptions {
  store: ControlPlaneStore;
  // Milliseconds since the epoch.
  clock: () => number;
  // Silence after which a gateway leaves the live set.
  liveTimeoutMs: number;
  // Silence after which an expired gateway is forgotten.
  forgetAfterMs: number;
  // How long an instance's last counted batch is kept after it was counted, whether
  // or not the gateway is still remembered: a gateway resending a batch counted
  // within it gets it acknowledged without counting.
  batchCursorRetentionMs: number;
  // Milliseconds between scheduled sweeps.
  sweepIntervalMs: number;
  // Hears of every sweep run, whatever triggered it.
  onExpirySweep: (run: ExpirySweepRun) => void;
  // Called for each listener that throws; the status or the sweep stands either way.
  onListenerError: (error: unknown, change: GatewaysChange) => void;
}

const CONFLICT_REASON = "started-at-alternating";

export function createGateways(options: GatewaysOptions): Gateways {
  const { store, clock, onExpirySweep, onListenerError } = options;
  const { liveTimeoutMs, forgetAfterMs, batchCursorRetentionMs, sweepIntervalMs } = options;
  for (const [name, value] of Object.entries({ liveTimeoutMs, forgetAfterMs, batchCursorRetentionMs, sweepIntervalMs })) {
    if (!Number.isSafeInteger(value) || value <= 0) {
      throw Object.assign(new Error(`${name} must be a positive integer, got ${value}`), { code: "gateways-option-invalid" });
    }
  }

  const listeners = new Set<GatewaysChangedListener>();
  // Statuses and sweeps change gateway records one at a time, so a sweep never
  // overwrites a status that arrived while it read the records.
  let queue: Promise<unknown> = Promise.resolve();
  let sweepTimer: ReturnType<typeof setInterval> | undefined;

  const serialized = <T>(run: () => Promise<T>): Promise<T> => {
    const result = queue.then(run);
    queue = result.catch(() => {
      // The caller gets this change's failure from `result`; the queue only orders changes.
    });
    return result;
  };

  const notify = (change: GatewaysChange): void => {
    for (const listener of [...listeners]) {
      try {
        listener(change);
      } catch (error) {
        onListenerError(error, change);
      }
    }
  };

  const recordStatus = async (status: GatewayStatus): Promise<StatusIntake> => {
    const now = clock();
    const previous = await store.gateway(status.instance);
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
    const joined = previous?.live !== true;
    const gateway: StoredGateway = {
      instance: status.instance,
      status,
      receivedAt: now,
      live: true,
      ...(replacedStart !== undefined && { replacedStart }),
      ...(conflict !== undefined && { conflict }),
    };
    await store.saveGateway(gateway);
    notify({ liveChanged: joined });
    return { ok: true, joined, conflictStarted };
  };

  const sweep = (trigger: string): Promise<ExpirySweepRun & { ok: true }> =>
    serialized(async () => {
      const at = clock();
      try {
        const expired: string[] = [];
        const forgotten: string[] = [];
        for (const gateway of await store.gateways()) {
          const silence = at - gateway.receivedAt;
          if (silence >= forgetAfterMs) {
            forgotten.push(gateway.instance);
            if (gateway.live) expired.push(gateway.instance);
          } else if (gateway.live && silence >= liveTimeoutMs) {
            expired.push(gateway.instance);
            await store.saveGateway({ ...gateway, live: false });
          }
        }
        if (forgotten.length > 0) await store.deleteGateways(forgotten);
        const batchCursorsDropped = await store.dropBatchCursorsCountedBefore(at - batchCursorRetentionMs);
        const run = {
          trigger,
          at,
          ok: true as const,
          expired: expired.sort(),
          forgotten: forgotten.sort(),
          batchCursorsDropped: batchCursorsDropped.sort(),
        };
        onExpirySweep(run);
        if (expired.length > 0) notify({ liveChanged: true });
        return run;
      } catch (error) {
        onExpirySweep({ trigger, at, ok: false, error });
        throw error;
      }
    });

  return {
    async acceptStatus(instance, doc) {
      const validation = validateGatewayStatus(doc);
      if (!validation.ok) {
        const first = validation.issues[0];
        const code = first === undefined || first.code === "schema" ? "status-invalid" : first.code;
        return invalid(code, validation.issues.map((issue) => issue.message).join("; "));
      }
      const status = validation.message;
      if (status.instance !== instance) {
        return invalid("instance-mismatch", `the status's instance "${status.instance}" is not the requester's "${instance}"`);
      }
      return serialized(() => recordStatus(status));
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

    onGatewaysChanged(listener) {
      // A wrapper, so the same function subscribed twice is two subscriptions.
      const subscription: GatewaysChangedListener = (change) => listener(change);
      listeners.add(subscription);
      return () => {
        listeners.delete(subscription);
      };
    },

    expireSilentGateways: sweep,

    startExpirySweep() {
      if (sweepTimer !== undefined) return;
      sweepTimer = setInterval(() => {
        sweep("schedule").catch(() => {
          // The failure has gone to onExpirySweep; the next scheduled run tries again.
        });
      }, sweepIntervalMs);
    },

    stopExpirySweep() {
      clearInterval(sweepTimer);
      sweepTimer = undefined;
    },
  };
}

function invalid(code: string, message: string): StatusIntake {
  return { ok: false, error: { code, message, status: 400 } };
}
