// The control-protocol messages as TypeScript sees them once they have passed their
// schemas. protocol/schema/ is the source of truth; these types follow it.

import type { Config, UsageUnit } from "../config/index.ts";

export interface UsageRecord {
  record_id: string;
  request_id: string;
  gateway_instance: string;
  key_id: string;
  // The key's path in the config the request ran under: 1 to 8 group IDs, top-level
  // first, the key's own group last.
  groups: string[];
  // The public model name.
  model: string;
  deployment: { backend: string; model: string };
  units: Record<UsageUnit, number>;
  // Billionths of a US dollar; at most 2^53 - 1, so a number holds it exactly.
  cost_nano_usd: number;
  estimated: boolean;
  partial: boolean;
  gateway_time: string;
}

// GET /v1/config, and the data of a config event.
export interface ConfigSnapshot {
  // The store's config epoch the version counts in.
  config_epoch: string;
  version: number;
  config: Config;
}

// Limit types whose windows the control plane counts; per-minute limits stay local.
export type TotalsLimitType = "tokens_per_hour" | "usd_per_month";

// One limit's current window, by its scope and type. group is absent for a global
// limit.
export interface TotalsWindow {
  group?: string;
  type: TotalsLimitType;
  window_start: string;
  // Decimal digits: tokens, or nano-USD for usd_per_month. A string because a month of
  // nano-USD can pass 2^53; read it with BigInt.
  used: string;
}

// A batch within its instance's epochs.
export interface BatchPosition {
  epoch: string;
  sequence: number;
}

// The data of a totals event, and part of every usage ack, as one gateway gets it: a
// consistent snapshot whose windows include every batch counted at its revision —
// counted_through, the recipient instance's last counted batch, among them.
export interface Totals {
  // The store's totals sequence at the snapshot: an integer from 0 that grows with
  // every change to the totals, whichever process made it. Within one config epoch a
  // gateway applies a message only when its revision is higher than the last applied.
  revision: number;
  // With config_version, the config the totals were computed under (the store's
  // epoch and the version in it): a gateway applies them only to that config.
  config_epoch: string;
  config_version: number;
  live_gateways: number;
  counted_through: BatchPosition | null;
  windows: TotalsWindow[];
}

// The data of a resync event: an empty object.
export type Resync = Record<string, never>;

export interface BatchId {
  instance: string;
  epoch: string;
  sequence: number;
}

// POST /v1/usage.
export interface UsageBatch {
  batch: BatchId;
  records: UsageRecord[];
}

// The answer to POST /v1/usage.
export interface UsageAck {
  batch: BatchId;
  totals: Totals;
}

export type GatewayState = "starting" | "ready" | "draining";

export interface ConfigRejection {
  version: number;
  codes: string[];
}

// POST /v1/status.
export interface GatewayStatus {
  instance: string;
  protocol_version: 5;
  state: GatewayState;
  started_at: string;
  // The config in force: its version and the store epoch it counts in, both null
  // before the first apply (and while the gateway runs its seed config).
  applied_config_version: number | null;
  applied_config_epoch: string | null;
  // The latest config the gateway received, when it rejected it; null once a later
  // one is applied. Its version may be below the applied one (a restarted control
  // plane counts from 1 again).
  last_rejection: ConfigRejection | null;
  // Every backend of the applied config, plus any backend a reload dropped while it
  // still has requests in flight; keyed by backend ID.
  backends: Record<string, BackendStatus>;
  // Every model of the applied config, keyed by public model name.
  models: Record<string, ModelStatus>;
}

export interface BackendStatus {
  in_flight: number;
  // The configured cap (fleet-wide, not the gateway's share); absent = no cap.
  max_in_flight?: number;
  // The applied config's deployments on this backend, keyed by the model name on the
  // backend.
  deployments: Record<string, DeploymentStatus>;
}

// half_open: a probe succeeded since the circuit opened; the next request is its trial.
// A half-open circuit keeps its opening time.
export type DeploymentStatus =
  | { circuit: "closed"; opened_at?: never }
  | { circuit: "open"; opened_at: string }
  | { circuit: "half_open"; opened_at: string };

export interface ModelStatus {
  queued: number;
}
