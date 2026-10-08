# Step 4 — e2e

**Status:** not started

## Intent

Rerank is proven end to end against the fakes:
- through a running gateway;
- across both halves, with a rerank request settling at the sample control plane.

## Files likely touched

- `gateway/internal/fakebackend/`:
  - the `rerank` path, answering vLLM's shape (`id`, `model`, `usage`, sorted
    `results` with `document`);
  - a llama-server-shaped answer (`object`, `usage`, `results` without `document`),
    selectable;
  - `OmitUsage` honoured, as for embeddings.
- `gateway/e2e/`:
  - rerank on a `vllm`-typed and a `llama-server`-typed backend: answer relayed with
    the public model name, usage record with `tokens_in` and its cost, and the log
    line's operation;
  - the cap refused before the backend;
  - usage missing, so estimated and flagged;
  - `endpoint_not_served` for a model with only `openai-compatible` deployments.
- `gateway/e2e/sample_test.go` (build tag `crosshalf`): a rerank request settles at
  the sample, and its totals and cost show there.

## Decisions made during planning

- **The fake's two answer shapes come from the servers' sources** recorded in step 1.
  No live captures: the DGX is busy, and step 8 checks the real shapes.

## Acceptance criteria

- The e2e tests above pass under `-race`, uncached (`scripts/check-gateway.sh`).
- The cross-half e2e passes.
- **Phase 1 end:** `scripts/check-all.sh` green and recorded, with no expected reds.

## Result

(filled in when the step is done)
