// The control-plane core: one object holding the store, the gateway token and the
// clock, exposing the operations HTTP adapters and host apps call. It knows nothing
// of HTTP. Any number of cores may run over one store, in one process or many: the
// store decides what they must agree on and tells each core of every change
// (docs/specs/CONTROL-PROTOCOL.md, Control-plane processes).

import { createConfigVersions } from "../config-versions/index.ts";
import type { ConfigVersions } from "../config-versions/index.ts";
import { createGateways } from "../gateways/index.ts";
import type { ExpirySweepRun, Gateways, GatewaysChange } from "../gateways/index.ts";
import { checkGatewayRequest } from "../protocol/index.ts";
import type { GatewayRequestCheck, RequestHeaders } from "../protocol/index.ts";
import type { ControlPlaneStore, StoredConfig } from "../storage/index.ts";
import { createUsage } from "../usage/index.ts";
import type { Usage } from "../usage/index.ts";

// What a listener that threw was being told of.
export type ListenerEvent =
  | { type: "config-published"; published: StoredConfig }
  | { type: "totals-changed" }
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
  // Runs the expiry sweep on its timer. Call it before serving gateways (the Fastify
  // plugin does, when the app is ready); a second call while started does nothing.
  // Every core sweeps: its writes are conditional, so cores sweeping together agree.
  start(): Promise<void>;
  // Stops the sweep.
  stop(): Promise<void>;
}

const DEFAULT_CONFIG_HISTORY_SIZE = 100;
const DEFAULT_RECENT_RECORDS_SIZE = 100;
const DEFAULT_GATEWAY_LIVE_TIMEOUT_MS = 30_000;
const DEFAULT_GATEWAY_FORGET_AFTER_MS = 3_600_000;
const DEFAULT_BATCH_CURSOR_RETENTION_MS = 7 * 24 * 3_600_000;
const DEFAULT_EXPIRY_SWEEP_INTERVAL_MS = 5_000;

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
  } = options;
  if (token.length === 0) {
    throw Object.assign(new Error("the gateway token must not be empty"), { code: "token-missing" });
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
    onListenerError: (error) => onListenerError(error, { type: "totals-changed" }),
  });
  let started = false;
  return {
    publishConfig: configVersions.publishConfig,
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
    async start() {
      if (started) return;
      started = true;
      gateways.startExpirySweep();
    },
    async stop() {
      started = false;
      gateways.stopExpirySweep();
    },
  };
}

function rethrowLater(error: unknown): void {
  queueMicrotask(() => {
    throw error;
  });
}
