# Step 13 — endpoints per model

**Status:** done — reviewed and committed 2026-10-11

## Intent

A model can declare which client API endpoints it serves, opt-in: an optional
`endpoints` list on the model in config. Absent, the model serves every endpoint its
deployments' backend types serve, as today. Present, the gateway refuses every other
endpoint with `400 endpoint_not_served` before routing, so no server sees it, and
`/v1/models` lists the declared endpoints.

The list only narrows. It closes, where it is set:
- the cost the live run found (`docs/BACKLOG.md`, Endpoints per model) — speech sent to
  a vLLM-Omni image model runs a whole image generation before its `400`;
- chat sent to a llama-server encoder reranker (`500`, a circuit failure) or to a
  causal one (`200` with nonsense);
- the neutral `upstream_endpoint_missing` round trips to vLLM rerankers, embedding
  models and chat models;
- `/v1/models` listing endpoints a loaded model does not serve.

Decided with the user (2026-10-11): opt-in, in this branch before the release, no
version bump (an optional additive field, `CONTROL-PROTOCOL.md` → Types bump no
version; it ships inside config format 6).

## Files likely touched

- `docs/specs/GATEWAY.md`:
  - Model metadata: the field and what it does;
  - Providers → Endpoint support: a model narrows its types' set;
  - Request pipeline stage 4: routing takes the declared set;
  - `/v1/models` and the Anthropic-shaped list: `endpoints` is the declared set;
  - the error table row for `endpoint_not_served`.

  Each dated 2026-10-11, with what was rejected:
  - a per-backend list (settled 2026-10-06 — a backend serves several models);
  - a list that widens what a type serves;
  - a default-deny list.
- `docs/specs/CONTROL-PROTOCOL.md`: the model's fields; the rule that every listed
  endpoint is served by at least one of the model's deployments' types; no version
  bump.
- `protocol/schema/config.schema.json`:
  - `models.*.endpoints`: an array of the gateway's endpoint names, unique, at least
    one;
  - the endpoint names as an enum.
- `protocol/fixtures/`:
  - a valid config with lists;
  - invalid cases: an unknown name, a duplicate, an empty list, an endpoint none of
    the model's deployment types serves.
- **Both halves refuse the same configs.** The "served by a deployment's type" rule
  needs the endpoint-support table on both sides. Make it shared data in `protocol/`
  (e.g. the backend-type fixtures gaining the endpoints each type serves) that both
  halves test against, so neither can drift. Choose the cleanest way and say why.
- `control/kaiak-control/`: the type, validation, the schema copy synced.
- The gateway:
  - `config/`: the field, the semantic check;
  - `server/`: the routing filter where `servingDeployments` / `serves` already
    narrow by type; `/v1/models` and the Anthropic list;
  - tests.
- `examples/config.json`:
  - `qwen-image` → `images_generations`;
  - `qwen3-tts` → `audio_speech`, `audio_speech_batch`;
  - the reranker → `rerank`;
  - `bge-m3` → `embeddings`;
  - others left open.
- `docs/DEPLOYMENT.md`:
  - set the list on every vLLM-Omni model, reranker and embedding model;
  - the speech-to-image warning now points to the list.
- `docs/BACKLOG.md`: "Endpoints per model" resolved and removed; "Omni chat models"
  stays.
- The live kit: `live-image` and `live-tts` declare their endpoints. The
  `speech-to-image` check then expects kaiak's `400 endpoint_not_served`, with no
  backend request. vLLM-Omni's own answer was recorded in step 12.

## Decisions made during planning

- **The refusal is the existing `400 endpoint_not_served`**, with the same wording and
  shape per API. The model already passed its access check, so naming the endpoint
  leaks nothing.
- **The endpoint-missing memory and readings stay** for models without a list. They
  cover a different case — a server that lacks an endpoint its type serves — so this
  is not a second safety net.

## Acceptance criteria

- **Component tests:**
  - the config: schema, the semantic rule, the shared fixtures on both halves;
  - routing refuses an undeclared endpoint before any backend request;
  - a declared endpoint routes as before;
  - `/v1/models` and the Anthropic list show the declared set;
  - an absent list changes nothing.
- **e2e:**
  - speech sent to an image model whose list is `images_generations` → `400
    endpoint_not_served`, no backend request;
  - chat sent to a model whose list is `rerank` → the same.
- **The example config passes on both halves.**
- `scripts/check-all.sh` green 3× in a row.

## Result

**The field.** `models.<name>.endpoints`: optional, the endpoint names `/v1/models`
lists (`$defs.endpoint`, an enum of eleven), unique, at least one. Absent: every
endpoint some deployment's type serves, unchanged. Present: narrows — any other
endpoint answers the existing `400 endpoint_not_served` (its code, status and shape
per API; a message of its own, below) at the `endpoint_support` stage, before limits and routing: no
backend request, no request slot, no usage record; a listed endpoint routes as
before among the deployments whose type serves it; `/v1/models`, `/v1/models/{id}`
and the Anthropic-shaped list (models whose endpoints include `messages`) show the
listed set. No version bump: format 6, protocol 6.

**The config rule, both halves.** `endpoint-not-served` — each entry must be served
by the type of at least one of the model's deployments; reported per entry at
`/models/<model>/endpoints/<i>`. Judged only when every deployment names a defined
backend (one on an unknown backend reports `deployment-backend-unknown` alone — one
defect, one code, as the tree rules do), pinned by a shared fixture.

**How the endpoint-support table is shared** (decided here; `GATEWAY.md` →
Providers → Endpoint support, `CONTROL-PROTOCOL.md` → Config → Endpoints per model).
`protocol/fixtures/backend-types/<type>.json` gains `endpoints`: the per-type file
already exists, both halves already enumerate it against their type lists (so a new
type cannot come without its endpoints), and it is "what both halves agree on about a
backend type". Three code copies are held to it, each by a test:
- the gateway's modules (`kinds[...].serves`) — the source of routing behaviour,
  unchanged — in `provider` `TestBackendTypeFixtures`;
- the gateway's config rule (`config/semantic.go` `endpointSupport`) in `config`
  `TestEndpointSupportIsTheSharedTable`: `config` cannot import `provider` (the
  reverse import exists), and it holds what its rules need about types already
  (`backendTypes`, `keyedBackendTypes`);
- `kaiak-control` (`config/endpoint-support.ts` `ENDPOINT_SUPPORT`) in
  `config.test.ts`.

`GATEWAY.md`'s table stays pinned to the modules (`TestEndpointSupportFollowsTheSpec`).
Rejected: the table in the config schema — JSON Schema cannot relate a model's list
to its deployments' types, so code would read it as data anyway; passing the
provider's table into `config.Parse` — every `Parse` caller (tests in five packages)
would change, and `config`'s own fixture tests could not import `provider`.

**Endpoint names have one home.** `config.EndpointName` (eleven constants, the
schema enum; `TestEndpointNamesAreTheSchemaEnum` in `config` and in `provider`).
`provider.Endpoint.Name()` returns it (the `endpoints` table row gains the name);
`server`'s body endpoint rows take their name from it (`newBodyEndpoint`), so the
`/v1/models` names, the log's `kaiak.endpoint` and the config's names cannot differ.

**Files.**
- `protocol/schema/config.schema.json`: `$defs.endpoint`, `model.endpoints`; the
  stale `type` description ("vllm-omni: vLLM's endpoints plus speech and image
  generation") corrected to "speech and image generation only". Copy synced
  (`control/kaiak-control/schema/`).
- `protocol/fixtures/backend-types/*.json`: `endpoints` per type.
- `protocol/fixtures/config/valid/model-endpoints.json` (lists on an embedding
  model, a reranker on vllm + llama-server, a speech and an image model, a model on
  anthropic + llama-server listing endpoints only one of its types serves each);
  `invalid/`: `endpoints-unknown-name`, `-duplicate`, `-empty` (schema),
  `endpoints-not-served` (semantic, `endpoint-not-served`: chat on vllm-omni, speech on
  vllm), `endpoints-deployment-backend-unknown` (semantic, `deployment-backend-unknown`
  alone); `cases.json`.
- Gateway `config/`: `snapshot.go` (`EndpointName`, `Model.Endpoints`,
  `Model.Admits`), `document.go`, `schema.go` (walker enum), `semantic.go` (rule and
  table), `errors.go` (`CodeEndpointNotServed`); tests in `schema_test.go`,
  `snapshot_test.go` (`TestModelEndpointsResolve`, `TestEndpointNotServedIssues`),
  `fixtures_test.go`.
- `provider/provider.go` (`Endpoint.Name`); `backendtypes_test.go` (fixture
  `endpoints` against the modules; names against the schema enum).
- `server/`: `attempts.go` `servingDeployments` refuses a model whose list leaves
  the endpoint out (the stage-2 refusal and routing's set both come from it);
  `models.go` `servesEndpoint` (`servedEndpoints`, `servesMessages`); `pipeline.go`
  (`newBodyEndpoint`); `errors.go`, comments. Tests: `endpoints_test.go`
  `TestEndpointsListRefusesTheOthers` (speech and the batch to an image model,
  embeddings to a listed chat model, Messages to a listed model with an anthropic
  deployment — each refused in its shape, nothing sent, nothing settled; images and
  chat routed, chat only to the deployment whose type serves it) and
  `TestEndpointsListShowsInTheModelLists` (entries, the list, the Anthropic list and
  entry; a model without a list unchanged). A mutation (the list check removed)
  fails them.
- e2e: `speech_test.go` — `painter` lists `images_generations`: speech and a speech
  batch to it `400 endpoint_not_served`, no backend request; its entry lists that
  alone (`tts`, no list, still all three). `rerank_test.go` — `rerank-llama` lists
  `rerank`: chat and embeddings to it `400 endpoint_not_served`, no backend request,
  entry `["rerank"]`; the vllm reranker (no list) keeps testing the endpoint-missing
  reading.
- `control/kaiak-control`: `types.ts` (`MODEL_ENDPOINTS`, `ModelEndpoint`,
  `Model.endpoints`), exported from `config/index.ts` and the package entry, as
  `MODEL_CAPABILITIES` is, for a form's choices; `endpoint-support.ts`;
  `semantic.ts` (`endpoint-not-served`); `config.test.ts` (enum pin, shared table,
  three rule cases); a comment in `backend-verify.test.ts`. The sample page shows no
  model endpoints: unchanged.
- `examples/config.json`: `qwen-image` `images_generations`; `qwen3-tts`
  `audio_speech`, `audio_speech_batch`; `qwen3-reranker-8b` `rerank`; `bge-m3`
  `embeddings`.
- Live kit: `live-tts` and `live-image` declare their lists (`config.go`); `models`
  checks each media model lists its own; `speech-to-image` and `image-to-speech` are
  refusal checks (`400 endpoint_not_served`, the log line showing nothing sent) —
  `image-to-speech` changed too, since `live-tts`'s list leaves image generation out:
  the kit no longer reaches vLLM-Omni's `503` reading, which the component and e2e
  tests keep (models without a list). vLLM-Omni's own answers stay recorded in the
  docs (`GATEWAY.md`, `LIVE-BACKENDS.md`), not in a check.
- Docs: `GATEWAY.md` (Client APIs, errors row, `/v1/models`, Anthropic list, Model
  metadata → **Endpoints per model** with the three rejections — a per-backend list,
  a list that widens, default-deny — pipeline stages 2 and 4, Endpoint support →
  narrowing and the shared table, the vLLM-Omni speech-on-image reading);
  `CONTROL-PROTOCOL.md` (fields, no bump, Endpoints per model, `endpoint-not-served`);
  `BACKEND-VERIFY.md`, `TECH-STACK.md` (the fixture's new member); `ARCHITECTURE.md`
  (the name is `provider`'s); `DEPLOYMENT.md` (Clients, Config for many hosts — set
  it on every vLLM-Omni model, reranker and embedding model, how it shows in
  `/v1/models` — Rerankers, Speech and images, next release's notes); `README.md`,
  `kaiak-control/GUIDE.md`; `LIVE-BACKENDS.md`; `BACKLOG.md` — "Endpoints per model"
  removed, "Omni chat models" kept.

**The list refusal's wording** (the main session's follow-up at review, 2026-10-11,
changing the planned "same wording"): the code, status and shape stay, the message
says why — "The model is not served on /v1/audio/speech: its configured endpoints do
not include this API." when the model's list leaves the endpoint out; "…: none of
its backends serves this API." as before when no deployment's type serves it. The
list is checked first (`server/pipeline.go` `findServingDeployments`;
`errors.go` `errEndpointNotListed` beside `errEndpointNotServed`, listed in
`metrics_test.go`'s `errorAnswers` with its class). Tests assert each whole message: the server's
`TestEndpointsListRefusesTheOthers` (OpenAI and Anthropic shapes) and
`TestRoutingFollowsEndpointSupport`, the e2e speech refusals (both reasons) and the
listed reranker. `GATEWAY.md` → the errors row (both messages, dated) and Model
metadata → Endpoints per model. No doc quoted the message elsewhere
(`DEPLOYMENT.md` none).

**Suite** (2026-10-11, the worktree uncommitted on `ba66d04`):
- `scripts/check-gateway.sh` green: gofmt, vet, staticcheck 2026.2.1 on go1.27.1,
  the telemetry boundary, `go test -race -count=1 ./...` (every package and
  `gateway/e2e`), the live kit's lint and self-test (all kinds, 34 passed / 2
  skipped on the two-backend run; the vllm run's `models`, `image-to-speech` and
  `speech-to-image` pass as refusals, "none sent").
- `scripts/check-all.sh` **three times in a row, all green**: each run the gateway
  checks above, control `npm test` (666 tests: 665 pass, 0 fail, 1 skipped — the
  store contract's conditional reconnect case, as before), `npm run lint`
  (`tsc` + boundaries ok) and the cross-half e2e (`ok kaiak/e2e` 76 s, 75 s, 76 s).
- After the list refusal's own wording: `scripts/check-gateway.sh` green, then
  `scripts/check-all.sh` once more, green (control 666 tests: 665 pass, 0 fail, 1
  skipped; lint ok; cross-half e2e `ok kaiak/e2e` 75 s). The first gateway run after
  the change caught the new constructor missing from `errorAnswers`
  (`TestEveryErrorCodeHasItsClass`); fixed as above.
- No expected reds: phase 5 ends green with this step.

**Green 3× in a row on the final code (5c1ca6e, 2026-10-11):** the run after the
wording change, then two more by the main session — each `all checks passed`; control
666 tests, 665 pass, 0 fail, 1 skipped; gateway `kaiak/e2e` 126.5 s and 122.6 s;
cross-half e2e `ok`.
