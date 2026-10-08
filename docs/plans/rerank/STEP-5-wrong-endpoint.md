# Step 5 — wrong-endpoint answers

**Status:** not started

## Intent

A request for an endpoint the self-hosted server's loaded model does not serve no
longer counts as the deployment failing. Two examples:
- chat sent to a reranker or embedding model on vLLM;
- rerank sent to a llama-server not started with `--reranking`.

Such a request answers `502 upstream_endpoint_missing`, is retried on other
deployments, and is neutral for the circuit, as step 1 settled (OVERVIEW decision 14).
The deployment keeps serving the endpoints it has.

## Files likely touched

- `gateway/internal/provider/`:
  - `vllm`: no core endpoints. Its route-missing answer reads as the endpoint missing
    everywhere.
  - `llama-server`: its `501`s as step 1 decided each one.
  - `send.go`: the `501` reading, which today looks only at `404` and `405`.
  - Whatever helper only `vllm` used for core endpoints becomes unused and is removed.
- `gateway/internal/server/`:
  - the endpoint memory and the per-request refusal held per deployment, if step 1
    confirmed it (`endpointmemory.go`, `attempts.go`).
- Tests beside each change:
  - module tests: each signature on each endpoint;
  - server tests: memory per deployment; a newer server found again after the
    interval.
- `gateway/e2e/`:
  - a chat request to a reranker deployment answering vLLM's route-missing shape is
    neutral, and the same deployment serves rerank right after with its circuit
    closed;
  - a `vllm` backend with a wrong `base_url` gives the endpoint-missing answer and
    warning, and its circuit does not open.

## Decisions made during planning

- **No new mechanism.** The existing endpoint-missing outcome, its warning and its
  memory carry the fix. Nothing new probes the backend to tell a wrong `base_url` from
  a model without the endpoint (AGENTS.md → Debugging, no stacked safety nets).
- **The `openai-compatible` reading stays as it is** (decision 14).

## Acceptance criteria

- The tests above pass.
- No test that asserted `upstream_path_missing` for a `vllm` backend is deleted
  silently. Each one is either rewritten to the settled rule, with the rule cited, or
  kept where the rule still holds.
- **Phase 2 end:** `scripts/check-all.sh` green and recorded.

## Result

(filled in when the step is done)
