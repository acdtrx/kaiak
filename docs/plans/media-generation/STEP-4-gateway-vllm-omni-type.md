# Step 4 — gateway: the `vllm-omni` backend type

**Status:** not started

## Intent

A `vllm-omni` backend serves every endpoint `vllm` serves, with vLLM-Omni's answers
read correctly:
- its route-missing answer is the endpoint missing on every endpoint (no core
  endpoints);
- its model-mismatch answer is read by the wrong-model rule as step 1 settled;
- its probe reads its models list.

The media endpoints are added in steps 5–8.

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
- **A chat request to a `vllm-omni` deployment** answering vLLM's route-missing shape is
  `upstream_endpoint_missing`: neutral, remembered per deployment.
- **Phase 1 end:** `scripts/check-all.sh` green and recorded.

## Result

(filled in when the step is done)
