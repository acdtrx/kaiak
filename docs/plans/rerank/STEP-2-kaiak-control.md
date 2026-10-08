# Step 2 — kaiak-control

**Status:** not started

## Intent

`kaiak-control` reads and validates config format 6 and speaks protocol 6. The sample
control plane's example config shows a reranker.

## Files likely touched

- `control/kaiak-control/schema/config.schema.json`, synced from `protocol/schema/`
  (`control/scripts/sync-schemas.ts`).
- `control/kaiak-control/src/config/types.ts`: `max_rerank_documents?`, beside
  `max_embedding_inputs`.
- The protocol and format version constants, and their tests.
- `examples/config.json`:
  - a reranker backend (`vllm`) and model (`qwen3-reranker-8b`), with no output limit
    and no prices, the way `bge-m3` is set up;
  - the config moves to format 6.
- `control/sample/` tests and the GUIDE, where they name the endpoint list or a
  format version.

## Decisions made during planning

- **`backend-verify` does not change.** It reads models lists and llama-server's
  `/props` and probes no endpoint. A reranker on vLLM looks like any model there.

## Acceptance criteria

- `kaiak-control` passes the step 1 fixtures, the new invalid case included.
- `npm test` and `npm run lint` are green in `control/`.
- `scripts/check-all.sh` run and recorded. Expected reds:
  - the gateway's fixture and version tests;
  - the cross-half e2e.

  Step 3 clears both.

## Result

(filled in when the step is done)
