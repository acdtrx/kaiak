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

// The data of a config event: the control plane's current config and its content
// hash (lowercase hex SHA-256 of the config's JSON as sent; it identifies content and
// carries no order).
export interface ConfigEvent {
  config_hash: string;
  config: Config;
}

// Limit types whose windows the control plane counts; per-minute limits stay local.
export type TotalsLimitType = "tokens_per_hour" | "usd_per_month";

// One scope's current window of one type. group is absent for global.
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

// The data of a totals event, as one gateway gets it on its stream: the windows of a
// consistent snapshot — every batch counted at it inside them, counted_through (the
// recipient instance's last counted batch of each epoch) among them. The first on a
// stream lists every scope and type with usage; each later one only the windows that
// changed since. The gateway applies them to its counts by scope and type, whatever
// config it runs.
export interface Totals {
  live_gateways: number;
  counted_through: BatchPosition[];
  windows: TotalsWindow[];
}

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

// The answer to POST /v1/usage: the batch it acknowledges, counted now or before.
export interface UsageAck {
  batch: BatchId;
}

export type GatewayState = "ready" | "draining";

export interface ConfigRejection {
  config_hash: string;
  codes: string[];
}

// POST /v1/status.
export interface GatewayStatus {
  instance: string;
  protocol_version: 5;
  state: GatewayState;
  started_at: string;
  // The config_hash of the config in force; null before the first config from the
  // control plane is applied (and while the gateway runs its seed config).
  applied_config_hash: string | null;
  // The latest config the gateway received, when it rejected it; null once a later
  // one is applied.
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
