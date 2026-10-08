# Step 8 — live run

**Status:** not started. Waits for the user to say the DGX is free.

## Intent

The real servers confirm what steps 1–6 built from their sources:
- rerank answers and usage;
- the wrong-endpoint answers;
- the oversize-document refusal;
- the `DEPLOYMENT.md` setup.

Then the branch merges.

## Files likely touched

- This file: the commands run, server versions, model files and the kit's output (no
  keys, no prompt text).
- Fixes for anything the run shows, each with its test, and the spec where a rule
  changes.
- `docs/plans/rerank/OVERVIEW.md`: verification status.

## Decisions made during planning

- **Models:**
  - Qwen3-Reranker-8B on vLLM, with the `DEPLOYMENT.md` flags;
  - a Qwen3-Reranker GGUF on llama-server: converted with llama.cpp's converter,
    which knows the model, or a published conversion.
- **Each server runs with the documented settings first.** Then llama-server runs once
  more with its default `-ub`, to record what an operator without the setting would
  see.
- **The user's DGX model is restored afterwards** (kaiak working style).

## Acceptance criteria

- The kit passes on `-kind vllm` and on `-kind llama-server`, each with the reranker
  and the wrong-endpoint checks.
- Each server's real answer matches the fake backend's shape. Otherwise the fake is
  fixed first.
- `scripts/check-all.sh` green after any fix.
- The branch rebases onto `main` and merges ff.

## Result

(filled in when the step is done)
