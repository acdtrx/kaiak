// The config document as TypeScript sees it once it has passed the schema.
// protocol/schema/config.schema.json is the source of truth; these types follow it.

import type { PROTOCOL_VERSION } from "../protocol/index.ts";

// The backend type enum, as a value for apps that list or check types; a test pins it to
// the schema's enum.
export const BACKEND_TYPES = [
  "openai-compatible",
  "openai",
  "azure-openai",
  "vllm",
  "vllm-omni",
  "llama-server",
  "anthropic",
  "azure-anthropic",
] as const;

export type BackendType = (typeof BACKEND_TYPES)[number];

// The capabilities a model's metadata declares, every one required, as a value for apps
// that list or ask for them; a test pins it to the schema's required list.
export const MODEL_CAPABILITIES = ["streaming", "tools", "vision", "reasoning"] as const;

export type ModelCapability = (typeof MODEL_CAPABILITIES)[number];

// The client API endpoints by the names /v1/models lists them: what a model's endpoints
// list may hold, as a value for apps that list or offer them; a test pins it to the
// schema's endpoint enum.
export const MODEL_ENDPOINTS = [
  "chat_completions",
  "completions",
  "embeddings",
  "messages",
  "messages_count_tokens",
  "responses",
  "responses_input_tokens",
  "rerank",
  "audio_speech",
  "audio_speech_batch",
  "images_generations",
] as const;

export type ModelEndpoint = (typeof MODEL_ENDPOINTS)[number];

// The limit types, as a value the counter bound counts by; a test pins it to the
// schema's enum. Each counts one measure — requests, tokens, images generated, seconds of
// audio generated (audio_seconds_out) or processed (audio_seconds_in), or USD — per
// minute (each gateway's own), per UTC hour or per calendar month (counted by the
// control plane).
export const LIMIT_TYPES = [
  "requests_per_minute",
  "tokens_per_minute",
  "tokens_per_hour",
  "images_per_minute",
  "images_per_hour",
  "audio_seconds_out_per_minute",
  "audio_seconds_out_per_hour",
  "audio_seconds_in_per_minute",
  "audio_seconds_in_per_hour",
  "usd_per_month",
] as const;

export type LimitType = (typeof LIMIT_TYPES)[number];

// Usage units a usage record counts, every one on every record. tokens_in is plain
// input, tokens_cached input read from the cache, tokens_cache_write input written to it
// (the three add up to the backend's prompt tokens); tokens_out is all output including
// reasoning; tokens_reasoning is the reasoning share of tokens_out, recorded for
// visibility and never priced. images are images generated, audio_seconds_out seconds
// of audio generated and audio_seconds_in seconds of audio processed — beside the
// tokens, not inside them, and never priced.
export type UsageUnit =
  | "tokens_in"
  | "tokens_cached"
  | "tokens_cache_write"
  | "tokens_out"
  | "tokens_reasoning"
  | "images"
  | "audio_seconds_out"
  | "audio_seconds_in";

// Units a price entry may name: the token units but tokens_reasoning (inside tokens_out).
export type PriceUnit = "tokens_in" | "tokens_cached" | "tokens_cache_write" | "tokens_out";

// A scope's limit: at most one per type in a scope, so its type is its identity there
// (docs/specs/CONTROL-PROTOCOL.md, Config → The group tree).
export interface Limit {
  type: LimitType;
  value: number;
}

export interface Backend {
  type: BackendType;
  base_url: string;
  api_key_env?: string;
  connect_timeout_ms?: number;
  // Streams: until the first event. Non-stream: the whole response. Streams: the
  // longest gap between events after the first.
  first_event_timeout_ms?: number;
  response_timeout_ms?: number;
  stall_timeout_ms?: number;
  // Absent = no cap.
  max_in_flight?: number;
}

export interface Deployment {
  backend: string;
  model: string;
}

export interface ModelMetadata {
  context_length: number;
  capabilities: Record<ModelCapability, boolean>;
  reasoning_efforts?: string[];
}

// A price entry's tiers start at 0 and rise strictly; a record is priced whole at the
// last tier whose above_input_tokens is below its input (tokens_in + tokens_cached +
// tokens_cache_write).
export interface Price {
  effective_from: string;
  tiers: PriceTier[];
}

// Complete in itself: nothing is inherited from the tier below.
export interface PriceTier {
  above_input_tokens: number;
  // tokens_cached or tokens_cache_write left out is charged at this tier's tokens_in
  // price; tokens_in or tokens_out left out costs 0.
  usd_per_million: Partial<Record<PriceUnit, number>>;
}

// A model's queue, or global.queue; a model's override leaves out what it keeps.
export interface QueueSettings {
  size?: number;
  timeout_ms?: number;
}

export interface RetrySettings {
  max_attempts?: number;
}

export interface CircuitSettings {
  failure_threshold?: number;
  probe_interval_ms?: number;
}

export interface Model {
  // Identical instances: every deployment on a backend of the same type
  // (deployment-types-differ).
  deployments: Deployment[];
  // What the model serves, and all it serves: each entry one its backend type serves
  // (endpoint-not-served).
  endpoints: ModelEndpoint[];
  metadata: ModelMetadata;
  output_limit?: { default: number; ceiling: number };
  queue?: QueueSettings;
  retries?: Required<RetrySettings>;
  prices?: Price[];
}

// The allowed_models entry that stands alone for every model the config declares.
export const ALL_MODELS = "*";

// What a group gives each direct child unless the child's own entry overrides it.
export interface ChildDefaults {
  allowed_models?: string[];
  limits?: Limit[];
}

// A node of the group tree. No parent = a top-level group; global is the implicit
// root above every top-level group.
export interface Group {
  parent?: string;
  // For the control plane only; the gateway checks their shape and ignores them.
  labels?: Record<string, string>;
  allowed_models?: string[];
  limits?: Limit[];
  child_defaults?: ChildDefaults;
}

export interface Key {
  hash: string;
  // Any group, leaf or not.
  group: string;
  expires_at?: string;
  disabled?: boolean;
}

export interface Global {
  limits?: Limit[];
  max_request_body_bytes?: number;
  control_outage_grace_ms?: number;
  max_n?: number;
  max_sequences_per_request?: number;
  max_embedding_inputs?: number;
  max_rerank_documents?: number;
  max_images_per_request?: number;
  max_speech_batch_items?: number;
  max_concurrent_requests_per_key?: number;
  queue?: QueueSettings;
  retries?: RetrySettings;
  circuit?: CircuitSettings;
  metrics?: { key_id_label?: boolean; group_label?: boolean };
}

export interface Config {
  // The protocol version the document is written for, equal to Kaiak-Protocol's.
  protocol_version: typeof PROTOCOL_VERSION;
  global: Global;
  backends: Record<string, Backend>;
  models: Record<string, Model>;
  groups?: Record<string, Group>;
  keys: Record<string, Key>;
}
