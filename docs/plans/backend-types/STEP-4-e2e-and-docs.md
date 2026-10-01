# Step 4 — e2e and docs

**Status:** not started

## Intent

Close phase 1: the types work end to end through both halves, and everything an
operator reads names them.

## Files likely touched

- `gateway/e2e/`: a scenario routing to backends of several types through one
  gateway (the fake backend serves both URL layouts).
- `examples/*.json`: the self-hosted backends take their server's type.
- `docs/DEPLOYMENT.md`: choosing a type; which types force the standard tier (and
  that an OpenAI deployment kept as `openai-compatible` runs on the project's own
  tier).
- `README.md`: config snippets and any mention of the types.
- `docs/ARCHITECTURE.md` and `docs/architecture/`: the provider modules, if they list
  them.
- `docs/testing/LIVE-BACKENDS.md`: `-kind llama-server`.
- A repo-wide grep for prose saying `openai-compatible` covers vLLM, llama-server or
  OpenAI.

## Decisions made during planning

- None beyond the overview.

## Acceptance criteria

- The e2e scenario passes: requests to an `openai`, a `vllm`, a `llama-server` and
  an `azure-openai` backend each succeed, and only the `openai` and `azure-openai`
  requests reach the backend with the forced tier.
- Both example configs validate in both halves.
- The grep finds nothing stale.
- **Phase end:** `scripts/check-all.sh` green (3× in a row, as before a merge), output
  recorded.

## Result

(to be filled when the step is done)
