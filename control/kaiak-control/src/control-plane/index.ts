// The control-plane core: one object holding the store, the gateway token and the
// clock, exposing the operations HTTP adapters and host apps call. It knows nothing
// of HTTP. Any number of cores may run over one store, in one process or many: the
// store decides what they must agree on and tells each core of every change
// (docs/specs/CONTROL-PROTOCOL.md, Control-plane processes).

import { createConfigPublishing } from "../config-publishing/index.ts";
import type { ConfigPublishing } from "../config-publishing/index.ts";
import { createGateways } from "../gateways/index.ts";
import type { ExpirySweepRun, Gateways, GatewaysChange } from "../gateways/index.ts";
import { checkGatewayRequest } from "../protocol/index.ts";
import type { GatewayRequestCheck, RequestHeaders } from "../protocol/index.ts";
import type { ControlPlaneStore, CurrentConfig, StoreChange } from "../storage/index.ts";
import { createUsage } from "../usage/index.ts";
import type { Usage } from "../usage/index.ts";

// What a listener that threw was being told of.
export type ListenerEvent =
  | { type: "config-published"; published: CurrentConfig }
  | { type: "totals-changed" }
  | { type: "gateways-changed"; change: GatewaysChange };

export type ListenerErrorHandler = (error: unknown, event: ListenerEvent) => void;

export interface ControlPlaneOptions {
  store: ControlPlaneStore;
  // The bearer token gateways present (KAIAK_CONTROL_TOKEN on the gateway side).
  token: string;
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
  // Milliseconds to wait before each new read of the current config after a read that
  // failed; one read more than there are delays. Default [100, 200, 400, 800, 1600].
  deliveryRetryDelaysMs?: readonly number[];
  // Hears of a config, totals or gateways listener that threw; the publish or the batch succeeds
  // regardless. Default: rethrow the error from a microtask, so a listener bug surfaces
  // as an uncaught exception the way a throwing event listener does. A host that would
  // rather keep running logs it here.
  onListenerError?: ListenerErrorHandler;
}

// Hears that the store's sequence went back under this core: the store was restored
// or failed over to a copy behind it (CONTROL-PROTOCOL.md, Config stream → Rollback).
export type RollbackListener = () => void;

// Hears that the current config could not be read after a change, every retry
// included: this core's streams may not have it (CONTROL-PROTOCOL.md, Config stream).
export type DeliveryFailedListener = (error: unknown) => void;

export interface ControlPlane
  extends Omit<ConfigPublishing, "takeChange">,
    Pick<Usage, "acceptUsageBatch" | "totals" | "recentRecords" | "onTotalsChanged">,
    Omit<Gateways, "takeChange"> {
  // Checks a gateway request's token, protocol version and instance ID.
  checkGatewayRequest(headers: RequestHeaders): GatewayRequestCheck;
  // Calls listener every time this core reads a store sequence below one it read
  // before; whatever was sent on a stream may then be ahead of the store, so an adapter
  // ends its streams and the gateways reconnect. Returns the unsubscribe, which is safe
  // to call more than once.
  onRollback(listener: RollbackListener): () => void;
  // Calls listener every time the current config could not be read after a change,
  // retries included; an adapter logs it and ends its streams, and the gateways
  // reconnect and read the current config on connect. Returns the unsubscribe, which is
  // safe to call more than once.
  onDeliveryFailed(listener: DeliveryFailedListener): () => void;
  // Runs the expiry sweep on its timer. Call it before serving gateways (the Fastify
  // plugin does, when the app is ready); a second call while started does nothing.
  // Every core sweeps: its writes are conditional, so cores sweeping together agree. A
  // core stopped before takes its store subscription again and catches up on what
  // changed meanwhile.
  start(): Promise<void>;
  // Stops the sweep and releases the store subscription: a stopped core hears of no
  // change.
  stop(): Promise<void>;
}

const DEFAULT_RECENT_RECORDS_SIZE = 100;
const DEFAULT_GATEWAY_LIVE_TIMEOUT_MS = 30_000;
const DEFAULT_GATEWAY_FORGET_AFTER_MS = 3_600_000;
const DEFAULT_BATCH_CURSOR_RETENTION_MS = 7 * 24 * 3_600_000;
const DEFAULT_EXPIRY_SWEEP_INTERVAL_MS = 5_000;
const DEFAULT_DELIVERY_RETRY_DELAYS_MS = [100, 200, 400, 800, 1600];

export function createControlPlane(options: ControlPlaneOptions): ControlPlane {
  const {
    store,
    token,
    recentRecordsSize = DEFAULT_RECENT_RECORDS_SIZE,
    clock = Date.now,
    gatewayLiveTimeoutMs = DEFAULT_GATEWAY_LIVE_TIMEOUT_MS,
    gatewayForgetAfterMs = DEFAULT_GATEWAY_FORGET_AFTER_MS,
    batchCursorRetentionMs = DEFAULT_BATCH_CURSOR_RETENTION_MS,
    expirySweepIntervalMs = DEFAULT_EXPIRY_SWEEP_INTERVAL_MS,
    deliveryRetryDelaysMs = DEFAULT_DELIVERY_RETRY_DELAYS_MS,
    onExpirySweep = () => {},
    onListenerError = rethrowLater,
  } = options;
  if (token.length === 0) {
    throw Object.assign(new Error("the gateway token must not be empty"), { code: "token-missing" });
  }
  // The highest store sequence this core has read; a read below it is a rollback.
  let highestSequence = 0;
  const rollbackListeners = new Set<RollbackListener>();
  const observeSequence = (sequence: number): void => {
    if (sequence >= highestSequence) {
      highestSequence = sequence;
      return;
    }
    highestSequence = sequence;
    for (const listener of [...rollbackListeners]) listener();
  };
  const deliveryFailedListeners = new Set<DeliveryFailedListener>();
  const configPublishing = createConfigPublishing({
    store,
    clock,
    observeSequence,
    onListenerError: (error, published) => onListenerError(error, { type: "config-published", published }),
    retryDelaysMs: deliveryRetryDelaysMs,
    onDeliveryFailed: (error) => {
      for (const listener of [...deliveryFailedListeners]) listener(error);
    },
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
    observeSequence,
    onListenerError: (error) => onListenerError(error, { type: "totals-changed" }),
  });
  // The core's one store subscription: every change goes to each module, and its
  // sequence to the rollback check (a rollback shows on the first write after it,
  // whatever reads this core makes).
  const takeChange = (change: StoreChange): void => {
    observeSequence(change.sequence);
    configPublishing.takeChange(change);
    usage.takeChange(change);
    gateways.takeChange(change);
  };
  let unsubscribe: (() => void) | undefined = store.subscribe(takeChange);
  let started = false;
  return {
    publishConfig: configPublishing.publishConfig,
    currentConfig: configPublishing.currentConfig,
    onConfigPublished: configPublishing.onConfigPublished,
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
    onRollback(listener) {
      // A wrapper, so the same function subscribed twice is two subscriptions.
      const subscription: RollbackListener = () => listener();
      rollbackListeners.add(subscription);
      return () => {
        rollbackListeners.delete(subscription);
      };
    },
    onDeliveryFailed(listener) {
      // A wrapper, so the same function subscribed twice is two subscriptions.
      const subscription: DeliveryFailedListener = (error) => listener(error);
      deliveryFailedListeners.add(subscription);
      return () => {
        deliveryFailedListeners.delete(subscription);
      };
    },
    async start() {
      if (started) return;
      started = true;
      if (unsubscribe === undefined) {
        unsubscribe = store.subscribe(takeChange);
        takeChange({ type: "catch-up", sequence: highestSequence });
      }
      gateways.startExpirySweep();
    },
    async stop() {
      started = false;
      gateways.stopExpirySweep();
      unsubscribe?.();
      unsubscribe = undefined;
    },
  };
}

function rethrowLater(error: unknown): void {
  queueMicrotask(() => {
    throw error;
  });
}
