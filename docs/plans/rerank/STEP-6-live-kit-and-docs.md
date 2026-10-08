# Step 6 — live kit and docs

**Status:** not started

## Intent

- **One kit run checks a real reranker:** on vLLM or llama-server, beside the run's
  chat model.
- **The kit checks the wrong-endpoint answers live.**
- **An operator can serve Qwen3-Reranker behind kaiak from `DEPLOYMENT.md` alone.**

Live runs stay manual. Only `-self-test` runs in `scripts/check-gateway.sh`.

## Files likely touched

- `scripts/live/`:
  - **endpoints:** `apis.go` adds `rerank` for `vllm` and `llama-server`, and the
    other kinds check `endpoint_not_served`;
  - **reranker server:** `-rerank-base-url` and `-rerank-model` (env alike) add a
    reranker backend of the run's kind and a public model `live-rerank`;
  - **rerank checks:**
    - an answer with results, and the public model name;
    - a relevant document above an irrelevant one;
    - the usage log line exact, not estimated;
    - the documents cap refused before the backend;
    - an oversize document refused with a `400` the caller can read;
  - **the wrong-endpoint check:**
    - chat to `live-rerank` answers `upstream_endpoint_missing`, and rerank works
      right after;
    - on llama-server, rerank to the chat model, whose server is not started with
      `--reranking`;
  - **existing checks:** the wrong-path check updated for `vllm` (step 5);
  - **the self-test:** every new check against the fakes.
- `docs/testing/LIVE-BACKENDS.md`:
  - the reranker flags and checks;
  - a "Reranker on a server of its own" section, like the embeddings one, with both
    servers' start commands.
- `docs/DEPLOYMENT.md` (OVERVIEW decision 15):
  - serving Qwen3-Reranker on vLLM: `--runner pooling`, the `--hf_overrides` for the
    original checkpoint, the score template;
  - serving it on llama-server: `--embedding --pooling rank --reranking`, and `-ub`
    and `-b` at least a slot's context;
  - one backend per reranker server;
  - `max_rerank_documents`;
  - the upgrade note: upgrade gateways before setting `max_rerank_documents`;
  - the two places that still say a missing endpoint means an older server
    ("upgrade it"), now that it also means a model without the endpoint;
  - the wrong-`base_url` signal on `vllm` now being warnings.
- `README.md`, `docs/ARCHITECTURE.md`, `docs/architecture/gateway.html`: the endpoint
  lists.
- `scripts/live/config.go`, if it writes a config the new field belongs in.

## Decisions made during planning

- **The reranker backend takes the run's kind,** since only `vllm` and `llama-server`
  serve rerank. Other kinds skip the reranker flags, with the reason.
- **The relevance check is coarse.** A clearly relevant document must outrank a
  clearly irrelevant one. No score thresholds: they vary by model and template.
- **The kit's embeddings backend stays `openai-compatible`.** Typing it by kind is a
  separate change, made only if step 8 shows a need.

## Acceptance criteria

- `go -C scripts/live run . -self-test` passes, covering every new check.
- `scripts/check-gateway.sh` runs only the kit's lint and self-test, as before.
- Someone with no kaiak background can run a reranker check from
  `LIVE-BACKENDS.md`.
- **Phase 3 so far:** `scripts/check-all.sh` green and recorded.

## Result

(filled in when the step is done)
