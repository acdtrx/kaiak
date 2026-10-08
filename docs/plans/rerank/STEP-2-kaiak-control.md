# Step 2 — kaiak-control

**Status:** done — reviewed and committed 2026-10-08

## Intent

`kaiak-control` reads and validates `global.max_rerank_documents`. The sample
control plane's example config shows a reranker.

## Files likely touched

- `control/kaiak-control/schema/config.schema.json`, synced from `protocol/schema/`
  (`control/scripts/sync-schemas.ts`).
- `control/kaiak-control/src/config/types.ts`: `max_rerank_documents?`, beside
  `max_embedding_inputs`.
- `examples/config.json`:
  - a reranker backend (`vllm`) and model (`qwen3-reranker-8b`), with no output limit
    and no prices, the way `bge-m3` is set up.
- `control/sample/` tests and the GUIDE, where they name the endpoint list or the
  global settings.

## Decisions made during planning

- **`backend-verify` does not change.** It reads models lists and llama-server's
  `/props` and probes no endpoint. A reranker on vLLM looks like any model there.

## Acceptance criteria

- `kaiak-control` passes the step 1 fixtures, the new invalid case included.
- `npm test` and `npm run lint` are green in `control/`.
- `scripts/check-all.sh` run and recorded. Expected reds: the gateway's tests over
  the fixtures that carry the new field, cleared by step 3.

## Result

### What changed

- `control/kaiak-control/schema/config.schema.json`: synced from `protocol/schema/`
  (`npm run sync-schemas` in `control/`) — the one difference was
  `global.max_rerank_documents`.
- `control/kaiak-control/src/config/types.ts`: `Global.max_rerank_documents?: number`
  after `max_embedding_inputs`. No comment: the `Global` fields carry none, their
  meanings live in the schema's descriptions.
- `examples/config.json`:
  - backend `vllm-rerank` (`vllm`, `http://vllm-rerank.internal:8000/v1`, no
    `api_key_env`, so the README's run commands name no new variable);
  - model `qwen3-reranker-8b` on it (`Qwen/Qwen3-Reranker-8B`), metadata as
    `bge-m3`'s with `context_length` 32768 (the model card's 32k), no
    `output_limit`, no `prices`;
  - added beside `bge-m3` in each `allowed_models` list that names it (`search-rag`,
    `search-rag-dev`, `users` → `child_defaults`);
  - `global.max_rerank_documents` not set (the gateway learns it in step 3).
- `README.md` → Quick start step 3: the example's hosts include "a vLLM reranker host".

Nothing else in `control/` names `max_embedding_inputs`: validation is the schema
(bounds and the zero case come from it), `kaiak-control` resolves no global caps, and
neither the sample's page nor `GUIDE.md` shows the global settings or lists the
endpoints, so neither changed. No test of its own exists for `max_embedding_inputs`
in `control/`; the shared fixtures cover both fields (`valid/full.json`,
`valid/at-bounds.json`, `invalid/max-rerank-documents-zero.json`, the resolution and
config-event fixtures), and `examples/config.json` runs in both halves' example
checks and in `keygen.test.ts`.

### Suite (2026-10-08)

`scripts/check-all.sh` stops at the gateway's `go test`, so the later stages were run
on their own with the script's commands.

- **Gateway** (`check-gateway.sh`): gofmt, vet, staticcheck, telemetry boundary
  pass. `go test -race -count=1 ./...` fails the 15 tests step 1 named, every one
  `/global/max_rerank_documents: unknown field [schema]` (or, for
  TestSchemaDefaultsAreResolved, the default the gateway does not resolve yet);
  every other package passes, `e2e` and `TestExampleConfigs` (the example with the
  reranker) included. **Cleared by step 3.**
  - `cmd/kaiak` (1): TestServingStatusCoversTheAppliedConfig.
  - `internal/config` (12): TestValidFixtures (`at-bounds.json`, `full.json`),
    TestResolvedFixtures, TestReaderKeepsItsSnapshotAcrossASwap,
    TestLoadAppliesAValidFile, TestLoadRejectsUnsetAPIKeyVariables,
    TestSchemaDefaultsAreResolved, TestReliabilitySettings,
    TestExplicitValuesOverrideDefaults, TestModelResolution,
    TestKeysResolveToTheirGroup, TestWildcardExpandsToEveryModel,
    TestChildDefaultsMergeUnderEachChild.
  - `internal/control` (2): TestValidMessageFixtures,
    TestValidMessageFixturesRoundTrip (the config-event `full.json`).
- **Live-test kit** (`scripts/live`): gofmt, vet, staticcheck and `go run .
  -self-test` pass (33 checks per kind, every kind).
- **Control `npm test`:** green — 630 tests, 629 pass, 0 fail, 1 skipped (the store
  contract's reconnect case, skipped as before). The 25 reds step 1 left are gone.
- **Control `npm run lint`:** passes (`tsc`, boundaries ok).
- **Cross-half e2e** (`go test -race -tags crosshalf -run '^TestAcrossHalves'
  -count=1 ./e2e`): passes.
