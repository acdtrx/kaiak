# Step 3 — gateway

**Status:** done (2026-10-01)

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
  `openai-without-api-key-env.json` is rejected for the missing `api_key_env`, not
  as an unknown type: a gateway test asserts the reason (the fixture test only
  checks that the config is rejected; found in step 1).
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

- **Config.** `snapshot.go`: `BackendOpenAI`, `BackendVLLM`, `BackendLlamaServer`
  beside the two existing constants. `schema.go`: the type list names the five (the
  schema's enum order); the `api_key_env` rule covers `openai` and `azure-openai`,
  its message naming the type (`openai backends need api_key_env`). New
  `TestBackendsRequiringAPIKeyEnv` (`fixtures_test.go`): the `openai` and `azure`
  without-key fixtures are each refused with exactly one issue, at the backend's
  path, saying it needs `api_key_env`.
- **Provider.** New `openai.go` (forces the standard tier), `vllm.go`,
  `llama_server.go` (pass the client's tier untouched, add none) — each its own
  `url`, `header`, `Send`, `probe` over the wire core, the shape of
  `openai_compatible.go`; `missingModelCodes` `model_not_found` on all three.
  `openai_compatible.go` calls `passthroughBody(req)` with no tier edit; its comment
  says what it is for (servers without a type of their own). `azure_openai.go`
  unchanged. The registry's switch names the five. Comments: the wire core's module
  list, `standardServiceTier` scoped to the tier-billing modules, the `Provider`
  doc's "the OpenAI-format modules", the fake backend's layout notes.
- **Tests.** `modules_test.go`: `TestServiceTierByModule` — the five modules × four
  cases (chat without a tier, chat with `"priority"`, embeddings without one,
  embeddings with `"flex"`), sent through the registry to the fake backend and read
  from its recorded body; `TestModuleURLCredentialAndProbe` — per module, the three
  endpoints' paths, the credential header (and the other module's header absent),
  the probe's `GET <prefix>models` with the same header and what it reports served;
  without `api_key_env` for `vllm`, `llama-server`, `openai-compatible`: no
  credential header at all. Both share one `moduleCases` table.
- **Server tests updated to the contract**: five exact-body expectations in
  `server/upstream_test.go` and `server/routing_test.go` carried
  `"service_tier":"default"` on the test gateway's `openai-compatible` backend; they
  now expect the client's body with only the owned edits (the bodies they had before
  the 2026-09-29 tier rule). The tier itself is asserted in the provider table.
- **Live-test kit.** `-kind llama-server` added (no default key, `max_tokens` for the
  ceiling check, as vLLM); each kind's backend gets the type of the same name;
  `-kind openai` without a key variable is refused up front, as Azure's is. The
  `-embeddings-base-url` server stays `openai-compatible` (its server is unknown).
  The self-test runs every kind in `allKinds`, so `llama-server` is covered with no
  change to `check-gateway.sh`.

Decisions made during the step:

- The new modules' `probe` and `header` comments name their server's own flags
  (`--api-key`, `--served-model-name`, `--alias`); the code is identical to
  `openai-compatible`'s by design (overview decision 6).
- The existing compat/azure probe and URL tests are kept: they cover failure cases
  the new table does not (a failing models list, a refused connection).
- The kit's embeddings server keeps `openai-compatible`: `-embeddings-base-url` says
  "a separate openai-compatible server", and nothing tells its kind.

Mutation checks (each reverted after):

- `openai.go` without the tier edit: `TestServiceTierByModule/openai/*` fail on
  chat without a tier, chat with priority, embeddings with flex.
- `azure_openai.go` without it: the same three `azure-openai` cases fail.
- The edit added to `vllm.go`, `llama_server.go`, `openai_compatible.go` (each
  alone): that module's chat ×2 and embeddings-with-flex cases fail.
- `vllm.go` sending `Api-Key`, `llama_server.go` with a wrong URL join,
  `openai.go` probing the wrong path: `TestModuleURLCredentialAndProbe` fails for
  that module.
- `openai` dropped from the schema's type list: `TestInvalidFixtures` still passes
  (rejected, as an unknown type) while `TestBackendsRequiringAPIKeyEnv` fails
  (two issues, the first at `/backends/openai/type`).

Suite (`scripts/check-all.sh`, 2026-10-01): **green, exit 0.** gofmt, vet,
staticcheck clean (gateway and kit); `go test -race ./...` every package ok;
live-test kit self-test passed for vllm, llama-server, openai, azure-openai (13
passed each) and vllm with two backends (16 passed); control `npm test` 565 passed,
0 failed; `npm run lint` clean, boundaries ok; cross-half e2e ok. No expected reds:
step 4 still owns the e2e scenario across types, examples and docs.
