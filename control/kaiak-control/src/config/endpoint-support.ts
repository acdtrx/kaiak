// Which client API endpoints each backend type serves, as the endpoint-not-served rule
// judges a model's endpoints list against the model's one type (docs/specs/GATEWAY.md,
// Providers → Endpoint support), and as a value for apps that offer a model the
// endpoints its type serves. The gateway's modules decide it; this table, the gateway's
// and its config check's are each held to protocol/fixtures/backend-types/
// (endpoints).

import type { BackendType, ModelEndpoint } from "./types.ts";

export const ENDPOINT_SUPPORT: Readonly<Record<BackendType, readonly ModelEndpoint[]>> = {
  "openai-compatible": ["chat_completions", "completions", "embeddings"],
  openai: ["chat_completions", "completions", "embeddings", "responses", "responses_input_tokens"],
  "azure-openai": ["chat_completions", "completions", "embeddings", "responses"],
  vllm: ["chat_completions", "completions", "embeddings", "messages", "messages_count_tokens", "responses", "rerank"],
  "vllm-omni": ["audio_speech", "audio_speech_batch", "images_generations"],
  "llama-server": [
    "chat_completions",
    "completions",
    "embeddings",
    "messages",
    "messages_count_tokens",
    "responses",
    "responses_input_tokens",
    "rerank",
  ],
  anthropic: ["messages", "messages_count_tokens"],
  "azure-anthropic": ["messages", "messages_count_tokens"],
};
