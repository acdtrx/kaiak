# Step 4 — gateway: the `vllm-omni` backend type

**Status:** not started

## Intent

The `vllm-omni` backend type exists: the config accepts it, its module builds URLs and
credentials, and its probe reads its models list. It claims the media endpoints only
(OVERVIEW decision 8), which steps 5–8 add, so until then it serves nothing reachable.
A request for any other endpoint to a model whose deployments are all on `vllm-omni` is
kaiak's `400 endpoint_not_served`. Its three media endpoints are core endpoints (a
route-missing `404` is a wrong `base_url`), and its wrong-endpoint and wrong-model
readings follow step 1's spec (the image route's `503 No diffusion stage…` is the
endpoint missing; image generation's `400 Model mismatch` is a missing model).

## Files likely touched

- `gateway/internal/provider/vllm_omni.go` (new), `provider.go` (the `kinds` map, its
  GenAI provider name), and the endpoint-support table test.
- `gateway/internal/config/` — the type.
- The fake backend: plays a `vllm-omni` server (its route-missing and model-mismatch
  answers).
- Tests: the module's URLs, credential, probe, and the wrong-endpoint and wrong-model
  signatures on each endpoint.

## Decisions made during planning

- **Share by purpose, not by shape** (CODING-RULES §2). `vllm-omni` is vLLM's server
  with more routes, so the module may share vLLM's signatures where they are the same
  server code. Where vLLM-Omni answers differently (step 1), it has its own.

## Acceptance criteria

- **Module tests:** each signature on each endpoint.
- **A chat, completions, embeddings, Messages, Responses or rerank request** to a model
  whose deployments are all on `vllm-omni` answers `400 endpoint_not_served`, before
  any backend.
- **Phase 1 end:** `scripts/check-all.sh` green and recorded.

## Result

(filled in when the step is done)
