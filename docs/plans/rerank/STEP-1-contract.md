# Step 1 — contract

**Status:** done — reviewed and committed 2026-10-08

## Intent

Settle what the plan builds in the documents that own it, before any code:
- the rerank endpoint, its cap and the new config field;
- versions, schema and fixtures;
- the wrong-endpoint rule.

Record the server behaviour each rule rests on: sources read, with versions and
dates.

## Files likely touched

- `docs/specs/GATEWAY.md`, each section dated 2026-10-08:
  - **endpoint list, client APIs and formats:** the rerank format, its owned fields
    and the fields that pass untouched (OVERVIEW decisions 9, 10);
  - **endpoint support table:** a `rerank` column — `vllm` and `llama-server` yes,
    the rest no. Record the sources: vLLM 0.30.0 and main (0.31.0), and the
    llama.cpp build read;
  - **base URLs and the path list:** `rerank`;
  - **error table:** the documents cap;
  - **batch caps:** `max_rerank_documents` (decision 3);
  - **input estimate:** the query repeated per document (decision 5);
  - **usage:** rerank counts `prompt_tokens` only (decision 11);
  - **relaying:** the answer is not translated (decision 12);
  - **observability:**
    - the `http.route` list;
    - `gen_ai.operation.name` `rerank`, with the amendment to the well-known-only
      rule (decision 6);
    - the usage-label cardinality: at most 4 operations;
  - **the wrong-endpoint rules** (decision 14):
    - "Wrong path to a host" and "An endpoint missing from a server": `vllm` has no
      core endpoints; llama-server's `501`s, each one decided;
    - the outcome-class table;
    - the endpoint memory per deployment;
    - why: vLLM creates routes from its model's tasks.
- `docs/specs/CONTROL-PROTOCOL.md`: the field bumps no version, and why (decision 16).
- `docs/kaiak.md`: v1 scope gains `/v1/rerank`.
- `protocol/schema/config.schema.json`: `global.max_rerank_documents`, with the same
  bounds as `max_embedding_inputs`, default 1000.
- `protocol/fixtures/`:
  - the config fixtures that carry `max_embedding_inputs` gain the new field
    (`valid/full.json`, `valid/at-bounds.json`, `resolved/full.json`, the
    config-event `valid/full.json`);
  - a new invalid case, `max-rerank-documents-zero`, with its entry in `cases.json`.
- `docs/BACKLOG.md`: an entry for `/v1/models` `endpoints` per loaded model, not per
  type. Revisit trigger: a client relies on `endpoints` to pick a model and is misled
  by a vLLM or llama-server model listing an endpoint its server does not serve.

## Decisions made during planning

- **Read the sources, not memory.** vLLM's rerank request and answer
  (`vllm/entrypoints/pooling/scoring/protocol.py`), its route setup
  (`launchers/api_server/routers.py`, `pooling/factories.py`), and llama-server's
  rerank handler, response format and error types (`tools/server/`).
  - Record the version or commit of each, as step 1 of the Messages plan did.
- **llama-server's `501`s:** list every one the server answers on an endpoint kaiak
  serves, and decide each:
  - endpoint missing — the server is not in the endpoint's mode;
  - the caller's — the request asks for something the model lacks, such as audio;
    relayed, not retried, neutral;
  - or left as a backend failure.

  The audio case is not a missing endpoint: remembering it would keep chat off a
  healthy backend.
- **Endpoint memory per deployment:**
  - Confirm that llama-server's router mode serves several models behind one
    `base_url`, each with its own flags.
  - Confirm that a newer server that gains an endpoint is still found again after
    the interval.

  If per deployment turns out wrong, say why here and keep per backend.

## Acceptance criteria

- Every rule above is in its owning doc, dated, with what it rests on.
- Schema and fixtures with the new field and the invalid case; no version moves.
- `scripts/check-all.sh` run and recorded. Expected reds:
  - both halves' tests over the fixtures that carry the new field, and
    `kaiak-control`'s schema-copy check: cleared by step 2 (`kaiak-control`) and
    step 3 (gateway).

## Result

### What changed

- `docs/specs/GATEWAY.md`:
  - **Client API:** `POST /v1/rerank` in the endpoint list; four formats, Rerank its
    own (Jina's shape), with the OpenAI format rejected for it; rerank errors in the
    OpenAI shape; the documents cap in the `invalid_value` row; the
    `upstream_endpoint_missing` row widened (every endpoint on `vllm`, llama-server's
    `501`); owned fields for rerank (`stream` not owned: non-stream timers,
    `gen_ai.request.stream` false); `rerank` in the `/v1/models` example, and
    `endpoints` follows types, not the loaded model (backlog cross-reference).
  - **Request pipeline:** Rerank in stage 2; stage 4 leaves out deployments, not
    servers.
  - **Providers:** the `rerank` column (sources dated); `rerank` path and the routes
    each server also has; the model edit alone; "Rerank answers are relayed as the
    backend sends them"; Wrong path: `vllm` has no signature on requests; An
    endpoint missing from a server rewritten — `vllm` has no core endpoints (why:
    routes from tasks), llama-server's `501`s decided, the memory per deployment
    (why: router mode), retries go to the model's other deployments.
  - **Routing and reliability:** retry and outcome-class rows for the endpoint
    missing; a `5xx` row excludes llama-server's endpoint-missing `501`.
  - **Limits:** the rerank input estimate; no output limit for rerank;
    `max_rerank_documents` and no per-document size cap (batch caps).
  - **Accounting:** rerank usage (`prompt_tokens` → `tokens_in`); estimated output 0.
  - **Observability:** `/v1/rerank` in `http.route`; `rerank` in the usage labels;
    at most 4 operations; `gen_ai.operation.name` `rerank` with the dated amendment;
    the endpoint-missing warning's new message, once per interval and deployment;
    `server_error` outcome excludes the endpoint-missing `501`.
- `docs/specs/CONTROL-PROTOCOL.md`: Config → Top-level shape gains one dated sentence:
  adding `global.max_rerank_documents` bumps neither the format nor the protocol, for
  the reason types do not (it points to Backend types → Types bump no version).
  Config format and protocol stay 5 (decision 16, below).
- `docs/kaiak.md`: v1 scope gains `/v1/rerank` (vLLM and llama-server).
- `docs/BACKLOG.md`: new entry **`/v1/models` `endpoints` per loaded model**; the
  llama-server quirks entry (L3, client errors answered `500`) names the media case.
- `protocol/schema/config.schema.json`: `global.max_rerank_documents` (1 to 2^53 − 1,
  default 1000), beside `max_embedding_inputs`.
- `protocol/fixtures/`:
  - `max_rerank_documents` in `config/valid/full.json` (250),
    `config/valid/at-bounds.json` (2^53 − 1, as its neighbour),
    `config/resolved/full.json` and `messages/config-event/valid/full.json` (250);
  - new `config/invalid/max-rerank-documents-zero.json` and its `cases.json` entry;
  - `config_hash` recomputed (SHA-256 of `JSON.stringify(config)`) in
    `messages/config-event/valid/full.json` only, whose config changed; every other
    config-event fixture's hash still matches its config, as at `HEAD`.
- Not touched: `control/kaiak-control/schema/` (step 2 syncs it), `gateway/`,
  `control/`, `scripts/`.

### Sources read (2026-10-08)

- **vLLM** — `v0.30.0`, `v0.31.0` (latest release, 2026-10-05) and `main` at
  `bab0ac61f8dae8020df6725c90474bdd647f1c40` (2026-10-08):
  `vllm/entrypoints/pooling/scoring/{protocol,api_router,serving,typing}.py`,
  `pooling/{factories,utils}.py`, `pooling/base/protocol.py`,
  `launchers/api_server/routers.py`, `generate/api_router.py`,
  `cohere/api_router.py`, `serve/__init__.py`, `serve/engine/protocol.py`,
  `serve/exception_handling/error_response.py`, `renderers/params.py`,
  `vllm/tasks.py`. The scoring protocol, its router and the router setup are
  identical in 0.30.0, 0.31.0 and `main` (main adds only the dev-mode RL routers);
  `factories.py` differs by one constructor argument.
  For step 6 (not read in depth): `docs/models/pooling_models/scoring.md`,
  `examples/pooling/score/qwen3_reranker_online.py`,
  `examples/pooling/score/template/qwen3_reranker.jinja` (all present on `main`).
- **llama.cpp** — `master` at `71ad0590f4808b6202f9213d166913858c73b1bc`
  (2026-10-08, release **b11513**): `tools/server/{server.cpp, server-context.cpp,
  server-common.cpp, server-common.h, server-models.cpp, README.md}`. For the `501`
  list also tags `b9917` and `b11146` (`server.cpp`, `server-context.cpp`,
  `server-common.cpp`). Tag `b10802`'s files were not found under `tools/server/`.
- **OpenTelemetry** — `open-telemetry/semantic-conventions-genai` `main` at
  `06ec68e722c45a7218e23ea1bc1339fe4e21ecae` (2026-10-07),
  `docs/registry/attributes/gen-ai.md`; its `gen_ai.operation.name` values are
  unchanged from `4f85037` (the commit `GATEWAY.md` cites). In
  `open-telemetry/semantic-conventions` (`main` at `68e8918`, 2026-10-07) the
  `gen_ai.*` attributes are marked moved to that repository.
- **Cohere** — rerank API reference: "For optimal performance we recommend against
  sending more than 1,000 documents in a single request" (the default's reason).

### Findings that shaped the rules

- **vLLM rerank request:** `RerankRequest` = `query: ScoreInput` (a string or
  `{"content": [parts]}`), `documents: ScoreInput | list[ScoreInput]`,
  `top_n: int ≥ 0` (0 = all), plus the scoring common params (`instruction`,
  `chat_template_kwargs`, `max_tokens_per_query`, `max_tokens_per_doc`,
  `use_activation`) and pooling params (`model` — optional in vLLM —, `user`,
  `truncate_prompt_tokens`, `truncation_side`, `padding`, `request_id`, `priority`,
  `mm_processor_kwargs`, `cache_salt`). Unknown fields are accepted
  (`extra="allow"`, logged at debug), so `return_documents` and `stream` pass
  harmlessly. A `documents` that is not a list is wrapped as one document
  (`serving.py`) — the count rule.
- **vLLM rerank answer:** `RerankResponse {id, model, usage: {prompt_tokens,
  total_tokens}, results: [{index, document: {text, multi_modal}, relevance_score}]}`,
  sorted by score, cut to `top_n`; `prompt_tokens` sums every pair's prompt tokens
  (template included).
- **vLLM routes:** `/rerank`, `/v1/rerank` (logs once that it prefers `/rerank`),
  `/v2/rerank`, in the scoring router; the Cohere router is `/cohere/v2/chat` only,
  opt-in (`VLLM_ENABLE_COHERE_API`), not rerank.
- **vLLM creates routes from tasks** (0.30.0 = 0.31.0 = main): chat, completions,
  Responses, Messages (and Cohere chat) only if `"generate" in supported_tasks`;
  the embeddings router only if `"embed"`; the scoring router (score, rerank) if
  `enable_scoring_api`: pooling task `embed`/`token_embed` (bi-encoder: **an
  embedding model serves rerank too**, by similarity) or `classify` with
  `num_labels == 1` (a cross-encoder such as Qwen3-Reranker). The models list,
  tokenize and metrics routes are always there. A missing route is FastAPI's
  `{"detail": "Not Found"}`.
- **vLLM errors:** `VLLMValidationError` → `400 BadRequestError`; a pair over the
  context raises it from `_text_len_check`/`_token_len_check` ("This model's maximum
  context length is …") unless `truncate_prompt_tokens` is set. `NotImplementedError`
  → `501`, but the rerank/chat handlers raising it are guards unreachable when routes
  follow the tasks — no rule needed.
- **llama-server rerank:** routes `/rerank`, `/reranking`, `/v1/rerank`,
  `/v1/reranking`; `501` unless `--embeddings` and pooling `rank`; `query` must be a
  string (`400`), `documents` a non-empty string array (`400`; a TEI request uses
  `texts`); one task per document; the answer `{model, object: "list", usage:
  {prompt_tokens, total_tokens}, results: [{index, relevance_score}]}`, sorted,
  `top_n` applied, `model` echoed from the request (else its own name),
  `prompt_tokens` the sum of every task's `tokens_evaluated`.
- **llama-server pair size:** a pooling task that cannot split and exceeds
  `n_ubatch` → `ERROR_TYPE_SERVER` (`500`, "input (N tokens) is too large to
  process. increase the physical batch size"); over the slot context →
  `ERROR_TYPE_EXCEED_CONTEXT_SIZE` (`400`). With `--embeddings` the server sets
  `n_batch = n_ubatch` itself.
- **llama-server error mapping** (`server-common.cpp`): `not_supported_error` 501,
  `server_error` 500, `exceed_context_size_error` 400, `invalid_request_error` 400,
  `unavailable_error` 503. Exceptions (`server.cpp`, `ex_wrapper`):
  `std::invalid_argument` and JSON errors → 400, every other exception → 500.
- **llama-server `501`s** (b11513; b9917 and b11146 have the same set on the
  gateway's endpoints):

  | `501` | Route | Gateway endpoint? | Decision |
  |---|---|---|---|
  | `This server does not support embeddings. Start it with --embeddings` | `/v1/embeddings` (and `/embeddings`) | yes | endpoint missing |
  | `This server does not support reranking. Start it with --reranking` | `/v1/rerank` (and three aliases) | yes | endpoint missing |
  | metrics, slots, slot action / slot save, `POST /props`, infill, decision model (`/v1/systemone`) | their own routes | no | — |
  | `The current model does not support audio input.` | `/v1/audio/transcriptions` | no | — |
  | anything on chat, completions, Messages, count_tokens, Responses, input_tokens | — | — | none exists; a future one stays a backend `5xx` |

  **No `501` is the caller's on the gateway's endpoints.** The OVERVIEW's example (a
  chat request with audio to a model without audio answers `501`) does not hold: on
  chat, Messages and Responses an image, audio or video part a model has no
  projector for throws `std::runtime_error` (`… input is not supported - hint: …`)
  in `oaicompat_content_load_media`, outside the handler's own `try` — answered
  `500 server_error` by `ex_wrapper` (b9917, b11146, b11513 alike). It is one more
  of the client errors answered `500` already in `docs/BACKLOG.md` (L3); the entry
  now names it. Nothing in this plan changes that reading.
- **llama-server router mode** (`server.cpp`, `server-models.cpp`, README "Using
  multiple models"): one server, one base URL; every model endpoint is proxied to a
  child process per model, picked by the body's `model`; each child's arguments come
  from its preset (`--models-preset` INI section, `--models-dir`), so one child can
  run `--embeddings --pooling rank --reranking` beside a chat child. **Per deployment
  confirmed.** A server that gains an endpoint is found again: the memory is a
  timed entry, gone once the probe interval passes, and every deployment
  remembered means all are tried again — unchanged rules, now keyed by deployment.
- **OpenTelemetry:** `gen_ai.operation.name` well-known values: `chat`,
  `create_agent`, `create_memory`, `create_memory_store`, `delete_memory`,
  `delete_memory_store`, `embeddings`, `execute_tool`, `fetch_response`,
  `generate_content`, `invoke_agent`, `invoke_workflow`, `plan`, `retrieval`
  ("Retrieval operation such as OpenAI Search Vector Store API"), `search_memory`,
  `text_completion`, `update_memory`, `upsert_memory`. No rerank value. The rule: "If
  one of them applies, then the respective value MUST be used; otherwise, a custom
  value MAY be used."

### Deviations and refinements (please review)

1. **Decision 14, llama-server `501`s:** the audio premise is wrong (above) — no
   `501` is the caller's. Embeddings' `501` reads as the endpoint missing although
   embeddings is a core endpoint: the core rule is about the `404` signature; a
   `501 not_supported_error` names a mode, never a URL. Matched by status and
   `type` on embeddings and rerank only; the message is not read.
2. **Per deployment for the request's retries too:** the rule "every deployment of
   the model on that backend is refused for the request's retries" is gone for
   `endpoint_missing` (kept for `path_missing` and `auth_failed`); a retry goes to
   the model's other deployments, as any failover. Same reasoning as the memory.
3. **The warning line changes:** `the deployment's server does not serve an endpoint
   its type serves`, once per interval and deployment (was `the backend's server
   lacks an endpoint its type serves: an older version?`, per backend). The old
   text is pinned in `gateway/internal/server/attempts.go` and
   `gateway/e2e/messages_test.go` — step 5.
4. **vLLM serves rerank on embedding models** (bi-encoder scoring) — recorded in the
   spec (operations, cardinality); no rule changes.
5. **Decision 16 changed by the main session: no version bump.** Step 1 first
   bumped config format and protocol to 6, as planned, and flagged the conflict with
   `CONTROL-PROTOCOL.md` → "Types bump no version" (settled 2026-10-01) and with the
   precedent of `max_sequences_per_request` and `max_embedding_inputs`, added
   2026-09-25 (`b8fe967`, private history) with no bump. Ruling: follow the settled
   rule — an optional additive field keeps every current-format config valid, an
   older gateway rejects a config carrying it with a schema error the control plane
   sees like any rejection, and no protocol message changes. The bump was reverted
   (`protocol/` back to `HEAD`, then only the field edits re-applied), and
   `CONTROL-PROTOCOL.md` records the ruling in one dated sentence.
6. **Expected reds:** the cross-half e2e is **green** after step 1 — it builds its
   config in Go, not from the fixtures, and no version moves. It stays green unless
   step 2's sample config sets `max_rerank_documents` before step 3's gateway knows
   it.

### Notes for later steps

- **Step 3:** `endpointColumns` in `gateway/internal/provider/endpoints_test.go`
  ignores an unknown column, so the new `rerank` column is unchecked until step 3
  maps it. The estimate rules are the OpenAI body's; a `video_url` part is text by
  bytes there (as on chat today). The gateway accepts
  `config/invalid/max-rerank-documents-zero.json` as invalid today only because the
  field is unknown — once it knows the field, the `minimum: 1` check must refuse it.
- **Step 5:** `MissingEndpoints` keyed by backend → deployment (`exclude`,
  `remember`, `Retain`); the per-backend retry refusal for `endpoint_missing`; the
  `vllm` module's core endpoints (none); llama-server's `501` reading; the warning
  text.
- **Step 6:** `docs/DEPLOYMENT.md` :390 ("An older server version …") and :935 (the
  alert's "upgrade it") for the new causes of an endpoint missing.
- Left as is: `config/invalid/cases.json` → `model-defaults.json`'s reason says
  "(format 5)" — still true of the rule's origin, not a version check.

### Suite (2026-10-08, after the no-bump revision)

`scripts/check-all.sh` stops at the first failing stage, so the stages after the
gateway's `go test` were run on their own (same commands). Every red is a fixture
that now carries `global.max_rerank_documents`, which neither half knows yet
(`unknown field` / `must NOT have additional properties`), or the schema copy.

- **Gateway** (`check-gateway.sh`): gofmt, vet, staticcheck, telemetry boundary
  pass. `go test -race -count=1 ./...` fails 15 tests in three packages; the other
  17 packages pass, `e2e` included. **Cleared by step 3.**
  - `cmd/kaiak` (1): TestServingStatusCoversTheAppliedConfig.
  - `internal/config` (12): TestValidFixtures, TestResolvedFixtures,
    TestReaderKeepsItsSnapshotAcrossASwap, TestLoadAppliesAValidFile,
    TestLoadRejectsUnsetAPIKeyVariables, TestSchemaDefaultsAreResolved (the new
    default is unknown to the gateway), TestReliabilitySettings,
    TestExplicitValuesOverrideDefaults, TestModelResolution,
    TestKeysResolveToTheirGroup, TestWildcardExpandsToEveryModel,
    TestChildDefaultsMergeUnderEachChild.
  - `internal/control` (2): TestValidMessageFixtures,
    TestValidMessageFixturesRoundTrip (the config-event `full.json`).
  - TestInvalidFixtures passes, but for the wrong reason (see Notes, step 3).
- **Live-test kit** (`scripts/live`): gofmt, vet, staticcheck and `go run .
  -self-test` pass (33 checks per kind, every kind).
- **Control `npm test`** (no hang now; 11 s): 630 tests, 604 pass, 25 fail, 1
  skipped. **Cleared by step 2.**
  - `scripts/sync-schemas.test.ts` (1): the schema copy differs from
    `protocol/schema`.
  - Fixture checks (4): `at-bounds.json`, `full.json`, `valid/full.json` (config
    event), and the resolution fixture `full.json`.
  - Tests that publish `config/valid/full.json` and assert the publish succeeded
    (20): `control-plane.test.ts` 9, `usage-route.test.ts` 5, `status-totals.test.ts`
    4, sample `page/events.test.ts` 2.
- **Control `npm run lint`:** passes.
- **Cross-half e2e** (`go test -race -tags crosshalf -run '^TestAcrossHalves'
  -count=1 ./e2e`): passes.

Before the revision (format and protocol at 6) the reds were wider — every config
and status fixture — and control `npm test` hung in two `control-plane.test.ts`
tests waiting on a status the old protocol check refused; that no longer applies.
