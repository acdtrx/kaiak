// The control-plane core: one object holding the store, the gateway token and the
// clock, exposing the operations HTTP adapters and host apps call. It knows nothing
// of HTTP. One core per store: start() takes the store's lease, so a second process
// on the same store refuses to start (docs/specs/CONTROL-PROTOCOL.md, Control-plane
// processes).

import { randomBytes } from "node:crypto";

import { createConfigVersions } from "../config-versions/index.ts";
import type { ConfigVersions } from "../config-versions/index.ts";
import { createGateways } from "../gateways/index.ts";
import type { ExpirySweepRun, Gateways, GatewaysChange } from "../gateways/index.ts";
import { checkGatewayRequest } from "../protocol/index.ts";
import type { GatewayRequestCheck, RequestHeaders } from "../protocol/index.ts";
import type { ControlPlaneStore, StoredConfig } from "../storage/index.ts";
import { createUsage } from "../usage/index.ts";
import type { LimitCarryOver, Usage } from "../usage/index.ts";

// What a listener that threw was being told of.
export type ListenerEvent =
  | { type: "config-published"; published: StoredConfig }
  | { type: "totals-changed" }
  | { type: "limit-carried-over"; carry: LimitCarryOver }
  | { type: "gateways-changed"; change: GatewaysChange };

export type ListenerErrorHandler = (error: unknown, event: ListenerEvent) => void;

export interface ControlPlaneOptions {
  store: ControlPlaneStore;
  // The bearer token gateways present (KAIAK_CONTROL_TOKEN on the gateway side).
  token: string;
  // How many recent config versions a stream can resume across. Default 100.
  configHistorySize?: number;
  // How many received usage records recentRecords keeps. Default 100.
  recentRecordsSize?: number;
  // Milliseconds since the epoch. Default Date.now.
  clock?: () => number;
  // Silence after which a gateway leaves the live set. Default 30000.
  gatewayLiveTimeoutMs?: number;
  // Silence after which an expired gateway is forgotten. Default 3600000 (an hour).
  gatewayForgetAfterMs?: number;
  // How long an instance's last counted batch is kept after it was counted, so a
  // gateway partitioned for longer than the forget delay that resends a counted batch
  // is not counted twice. Default 604800000 (7 days).
  batchCursorRetentionMs?: number;
  // Milliseconds between scheduled expiry sweeps (startExpirySweep). Default 5000.
  expirySweepIntervalMs?: number;
  // Hears of every expiry sweep run — its trigger, time and result — for the host to
  // log. Default: nothing.
  onExpirySweep?: (run: ExpirySweepRun) => void;
  // The totals revision's control-plane ID (32 lowercase hex digits), also the holder
  // of the store's lease. Default: random, new with every process — which is what
  // tells gateways the totals' order restarted.
  controlPlaneId?: string;
  // How long the store's lease lasts from each renewal; start() renews it every third
  // of this. Default 30000.
  storeLeaseTtlMs?: number;
  // Hears that the store's lease could not be renewed — another process took it (this
  // one stalled past the lease) or the store failed — while the core keeps running.
  // Default: rethrow from a microtask, so the process stops rather than write beside
  // another one.
  onStoreLeaseLost?: (error: unknown) => void;
  // Hears of every limit that kept its spend across a publish because only its model
  // set changed (docs/specs/CONTROL-PROTOCOL.md, Budgets → Model-set edits), for the
  // host to log; `ambiguous` when several limits it replaced matched. Default:
  // nothing.
  onLimitCarriedOver?: (carry: LimitCarryOver) => void;
  // Hears of a config, totals or gateways listener that threw; the publish or the batch succeeds
  // regardless. Default: rethrow the error from a microtask, so a listener bug surfaces
  // as an uncaught exception the way a throwing event listener does. A host that would
  // rather keep running logs it here.
  onListenerError?: ListenerErrorHandler;
}

export interface ControlPlane
  extends ConfigVersions,
    Pick<Usage, "acceptUsageBatch" | "totals" | "recentRecords" | "onTotalsChanged">,
    Gateways {
  // Checks a gateway request's token, protocol version and instance ID.
  checkGatewayRequest(headers: RequestHeaders): GatewayRequestCheck;
  // Takes the store's lease — rejecting with code "store-lease-held" when another
  // process holds it — then keeps it renewed and runs the expiry sweep. Call it before
  // serving gateways (the Fastify plugin does, when the app is ready); a second call
  // while started does nothing.
  start(): Promise<void>;
  // Stops the renewals and the sweep and gives the lease up.
  stop(): Promise<void>;
}

const DEFAULT_CONFIG_HISTORY_SIZE = 100;
const DEFAULT_RECENT_RECORDS_SIZE = 100;
const DEFAULT_GATEWAY_LIVE_TIMEOUT_MS = 30_000;
const DEFAULT_GATEWAY_FORGET_AFTER_MS = 3_600_000;
const DEFAULT_BATCH_CURSOR_RETENTION_MS = 7 * 24 * 3_600_000;
const DEFAULT_EXPIRY_SWEEP_INTERVAL_MS = 5_000;
const DEFAULT_STORE_LEASE_TTL_MS = 30_000;

export function createControlPlane(options: ControlPlaneOptions): ControlPlane {
  const {
    store,
    token,
    configHistorySize = DEFAULT_CONFIG_HISTORY_SIZE,
    recentRecordsSize = DEFAULT_RECENT_RECORDS_SIZE,
    clock = Date.now,
    gatewayLiveTimeoutMs = DEFAULT_GATEWAY_LIVE_TIMEOUT_MS,
    gatewayForgetAfterMs = DEFAULT_GATEWAY_FORGET_AFTER_MS,
    batchCursorRetentionMs = DEFAULT_BATCH_CURSOR_RETENTION_MS,
    expirySweepIntervalMs = DEFAULT_EXPIRY_SWEEP_INTERVAL_MS,
    onExpirySweep = () => {},
    onListenerError = rethrowLater,
    controlPlaneId = randomBytes(16).toString("hex"),
    storeLeaseTtlMs = DEFAULT_STORE_LEASE_TTL_MS,
    onStoreLeaseLost = rethrowLater,
    onLimitCarriedOver = () => {},
  } = options;
  if (token.length === 0) {
    throw Object.assign(new Error("the gateway token must not be empty"), { code: "token-missing" });
  }
  if (!Number.isSafeInteger(storeLeaseTtlMs) || storeLeaseTtlMs < 3) {
    throw Object.assign(new Error(`storeLeaseTtlMs must be an integer of at least 3, got ${storeLeaseTtlMs}`), {
      code: "store-lease-ttl-invalid",
    });
  }
  const configVersions = createConfigVersions({
    store,
    historySize: configHistorySize,
    clock,
    onListenerError: (error, published) => onListenerError(error, { type: "config-published", published }),
  });
  const gateways = createGateways({
    store,
    clock,
    liveTimeoutMs: gatewayLiveTimeoutMs,
    forgetAfterMs: gatewayForgetAfterMs,
    batchCursorRetentionMs,
    sweepIntervalMs: expirySweepIntervalMs,
    onExpirySweep,
    onListenerError: (error, change) => onListenerError(error, { type: "gateways-changed", change }),
  });
  const usage = createUsage({
    store,
    clock,
    recentRecordsSize,
    liveGateways: gateways.liveGateways,
    onListenerError: (error, carry) =>
      onListenerError(error, carry ? { type: "limit-carried-over", carry } : { type: "totals-changed" }),
    controlPlaneId,
    onLimitCarriedOver,
  });

  // The lease: taken by start(), renewed on a timer, given up by stop() once no renewal
  // is in flight (a renewal landing after the release would take the lease again).
  let starting: Promise<void> | undefined;
  let renewal: ReturnType<typeof setInterval> | undefined;
  let renewing: Promise<void> = Promise.resolve();
  const takeLease = async (): Promise<void> => {
    const now = clock();
    const result = await store.acquireLease(controlPlaneId, now, now + storeLeaseTtlMs);
    if (result.ok) return;
    const { holder, expiresAt } = result.lease;
    throw Object.assign(
      new Error(
        `another control-plane process holds this store (holder ${holder}, lease until ` +
          `${new Date(expiresAt).toISOString()}): run one control-plane process per store`,
      ),
      { code: "store-lease-held" },
    );
  };
  const start = (): Promise<void> => {
    starting ??= takeLease().then(
      () => {
        renewal = setInterval(() => {
          renewing = takeLease().catch(onStoreLeaseLost);
        }, Math.floor(storeLeaseTtlMs / 3));
        gateways.startExpirySweep();
      },
      (error: unknown) => {
        starting = undefined;
        throw error;
      },
    );
    return starting;
  };
  const stop = async (): Promise<void> => {
    if (starting === undefined) return;
    // A start still taking the lease finishes first, so its timers are stopped too.
    await starting.catch(() => {
      // A start that failed holds nothing to give up; its caller has the error.
    });
    starting = undefined;
    gateways.stopExpirySweep();
    clearInterval(renewal);
    renewal = undefined;
    await renewing;
    await store.releaseLease(controlPlaneId);
  };
  // A publish and a live-set change alter the totals too, so they move the revision on.
  // Subscribed here, first, so the revision has moved before any stream pushes for
  // the change.
  configVersions.onConfigPublished(() => usage.totalsChanged());
  gateways.onGatewaysChanged((change) => {
    if (change.liveChanged) usage.totalsChanged();
  });
  return {
    // Every publish runs in the totals' turn (Usage.publishing).
    publishConfig: (doc) => usage.publishing((beforeSave) => configVersions.publishConfig(doc, beforeSave)),
    currentConfig: configVersions.currentConfig,
    configEpoch: configVersions.configEpoch,
    configsSince: configVersions.configsSince,
    onConfigPublished: configVersions.onConfigPublished,
    acceptUsageBatch: usage.acceptUsageBatch,
    totals: usage.totals,
    recentRecords: usage.recentRecords,
    onTotalsChanged: usage.onTotalsChanged,
    acceptStatus: gateways.acceptStatus,
    gateways: gateways.gateways,
    liveGateways: gateways.liveGateways,
    onGatewaysChanged: gateways.onGatewaysChanged,
    expireSilentGateways: gateways.expireSilentGateways,
    startExpirySweep: gateways.startExpirySweep,
    stopExpirySweep: gateways.stopExpirySweep,
    checkGatewayRequest: (headers) => checkGatewayRequest(headers, token),
    start,
    stop,
  };
}

function rethrowLater(error: unknown): void {
  queueMicrotask(() => {
    throw error;
  });
}
