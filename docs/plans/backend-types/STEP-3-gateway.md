# Step 3 — gateway

**Status:** not started

## Intent

The gateway loads the five types and routes each backend to its own module; the
standard service tier is forced by `openai` and `azure-openai` only.

## Files likely touched

- `gateway/internal/config/snapshot.go`: the `BackendType` constants.
- `gateway/internal/config/schema.go`: the type list; `api_key_env` required for
  `openai`, beside the `azure-openai` rule.
- `gateway/internal/provider/`:
  - `openai.go`, `vllm.go`, `llama_server.go`: new modules, each self-contained over
    the wire core (overview decision 6);
  - `openai_compatible.go`: drops `standardServiceTier`;
  - `azure_openai.go`: unchanged;
  - `provider.go`: the registry's switch names every type.
- `scripts/live/`: `config.go` emits the type of each `-kind`; `main.go` accepts
  `-kind llama-server`; the kit's self-test covers it if `check-gateway.sh` runs one
  per kind.

## Decisions made during planning

- The modules are separate types in the registry, even while `vllm` and
  `llama-server` behave as `openai-compatible`: a later fix changes one file.
- No module reads another's type or a flag; the registry's switch stays the one
  place a type is named (the 2026-09-29 core/module split holds).

## Acceptance criteria

- The gateway passes every shared config fixture with the codes `cases.json` names.
- **One table test runs every module through the same service-tier cases** against
  the fake backend's recorded body: chat without a tier, chat with `"priority"`,
  embeddings with and without one. `openai` and `azure-openai` send `"default"` on
  chat always and on other endpoints only when the client sent one; `openai-compatible`,
  `vllm` and `llama-server` send the client's body member untouched, or none. Removing
  the edit from any module fails it (closes the review's T1).
- Per module: URL, credential header (none without `api_key_env` where allowed) and
  probe against the fake backend.
- `scripts/check-gateway.sh` green, the live-test kit's self-test included. Suite
  recorded; expected red: whatever step 4 still owns (cross-half e2e configs, if not
  moved here).

## Result

(to be filled when the step is done)
