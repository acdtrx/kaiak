# Step 3 — the gateway at protocol 7

**Status:** done — reviewed and committed 2026-10-11

## Intent

Move the gateway to protocol 7 and take out what the new rules make unnecessary:
- the version and the two config rules;
- routing and refusals that no longer filter deployments by type;
- the endpoint memory;
- the endpoint-missing class: a route-missing answer on a listed endpoint becomes one
  reading on every type, `502 upstream_path_missing` at once, neutral and not retried
  (OVERVIEW decision 22).

## Files likely touched

- `gateway/internal/config/`:
  - `schema.go`: `protocol_version` in place of `format_version`; `endpoints`
    required; `FormatVersion` removed;
  - one version constant for the whole gateway, read by the config check and the
    control client (`control.ProtocolVersion` today) — its home is this step's call,
    without an import cycle;
  - `semantic.go`: the one-type rule; `endpoint-not-served` against the model's type;
  - `snapshot.go`: `Model.Admits` reads the list alone; the model's type if routing
    or the model lists need it.
- `gateway/internal/control/`: the version constant (as above).
- `gateway/internal/server/`:
  - `pipeline.go`: `findServingDeployments` becomes the list check alone;
  - `attempts.go`: the per-endpoint view of deployments (`servingDeployments`,
    `serves`) and the memory's `exclude` and `remember` removed; the model's
    deployments route as they are;
  - `endpointmemory.go` and its tests removed;
  - `models.go`: model entries show the list;
  - `errors.go`: `errEndpointNotServed` removed; the not-listed refusal stays;
  - `metrics.go`, the request line, `errorAnswers`: `upstream_endpoint_missing` gone;
  - tests: every test config builds models with a list (`server_test.go`'s model
    helper, `withAnthropicModels`, `withSpeechModels`, `withEndpointLists`); tests of
    removed behaviour deleted, not adapted into something else.
- `gateway/internal/provider/`:
  - `notfound.go`, `send.go`: one route-missing reading per type, whatever the
    endpoint; core endpoints and `vllm`'s no-core rule removed;
    `CodeEndpointMissing` removed;
  - `llama_server.go`, `vllm_omni.go`: the `501` and image `503` readings removed
    (plain `5xx` from now on);
  - `provider.go`: what the core-endpoint notion leaves behind.
- `gateway/internal/server/` and `gateway/internal/routing/`: the wrong path's outcome
  class (neutral) and retry rule (none — the rule refusing every deployment on that
  backend for the request goes).
- `gateway/cmd/kaiak/`: the memory's wiring (`Retain` on config apply) removed; tests'
  configs.
- `gateway/e2e/`: every config; tests of the endpoint memory or the endpoint-missing
  reading replaced by the one "not found" reading where a behaviour remains.
- `gateway/internal/fakebackend/`: its route-missing answers stay.
- `scripts/live/`: the configs it writes (`protocol_version: 7`, a list per model); its
  wrong-endpoint checks, now refusals by the list or the "not found" reading.

## Decisions made during planning

- **The "not found" answer errors out** (decision 22, settled with the user): the
  client gets `502 upstream_path_missing` at once; nothing is retried and the circuit
  does not count it; its message names both causes — a wrong `base_url`, or a list
  naming an endpoint the server does not serve. The backend's text is not relayed, as
  today.
- **Removal discipline:** removed code is gone — no renamed
  leftovers, no compatibility shims; a grep checklist recorded in the result
  (`EndpointMissing`, `endpoint_missing`, `MissingEndpoints`, `coreEndpoints`,
  `FormatVersion`, `format_version`, `servingDeployments`, `errEndpointNotServed`).

## Acceptance criteria

- **Component tests:**
  - the config rules and the version (missing list, two types, an unserved entry,
    version 6, a `format_version` member) against the shared fixtures;
  - an unlisted endpoint refused before limits, with the one message, on every body
    route (the route-by-route test from the phase A review, kept);
  - a route-missing answer on each type — `vllm`, `llama-server`, `vllm-omni`, the
    OpenAI and Anthropic types — answered `502 upstream_path_missing` at once, not
    retried, not counted by the circuit;
  - llama-server's `501` and vLLM-Omni's image `503` read as plain `5xx`, as any
    server error.
- **Gateway e2e and the cross-half e2e** green at protocol 7.
- `scripts/check-all.sh` green — **phase 1 ends green.**

## Result

### What changed

- `internal/config`:
  - `ProtocolVersion = 7` (`schema.go`) is the gateway's one version: the walker
    requires `protocol_version` to equal it, and the control client sends it in
    `Kaiak-Protocol` and in its status. `FormatVersion` and the document's
    `format_version` are gone;
  - `endpoints` required in the walker;
  - `semantic.go`: `backendType` replaces `endpoints` — `deployment-types-differ` once
    per model at `/models/<m>/deployments/<i>/backend` of the first deployment whose
    type differs from the first one's (the list then not judged), otherwise
    `endpoint-not-served` per entry the one type does not serve; a model on an unknown
    backend reports `deployment-backend-unknown` alone. New code constant
    `CodeDeploymentTypesDiffer`;
  - `Model.Admits` reads the list alone.
- `internal/control`: `ProtocolVersion` removed; the header value and the status read
  `config.ProtocolVersion`. `internal/fakecontrol` speaks `7`.
- `internal/provider`:
  - `wireCall.core`, `openai_rules.go` (`openAICore`) and `CodeEndpointMissing` gone;
  - the core reads a 404 alone (missing model first, then the module's unknown-path
    signature → `CodePathMissing`); vLLM's `405 {"detail":"Method Not Allowed"}` is
    the `vllm` module's own `answerReading` (status 405 → `CodePathMissing`) — every
    other type relays a 405;
  - `llama_server.go`'s 501 reading (`readings`, `modeMissing`) and `vllm_omni.go`'s
    503 reading (`noDiffusionStage`) removed: plain 5xx now;
  - comments pointing at the removed spec sections now point at "A path the server
    does not have".
- `internal/metrics`: `AttemptEndpointMissing` gone; `AttemptPathMissing` is neutral
  and not in `RetryableOutcomes` (six retry outcomes, thirteen attempt outcomes).
- `internal/server`:
  - `endpointmemory.go` and its tests deleted; `attempts` loses `missing` and `logger`,
    `send` the remember-and-warn branch; `NewAPI` and `newPipeline` lose the memory
    (and the now-unused logger parameter of `newPipeline`);
  - the `endpoint_support` stage is now `endpoint_list` (`checkEndpointListed`, the
    list check alone); `request.serving`, `servingDeployments` and `serves` removed —
    the attempt loop routes `rq.snapshot.Models[rq.model]` as it is;
  - `errEndpointNotServed` removed; `errEndpointNotListed` is the one refusal;
  - `failureRules`: `upstream_path_missing` neutral, refused (no usage), the spec's
    message naming both causes; `attemptRules` loses the path's backend-wide rule;
  - model entries show the list sorted; the Anthropic-shaped list is the models whose
    list names `messages`;
  - the `endpoint` row's `name` field removed (its only uses were the removed warning
    and the old entry derivation).
- `cmd/kaiak`: the memory's wiring (`missingEndpoints`, its `Retain` on apply) removed.
- Tests:
  - central model helpers: `server_test.go`'s `endpointsOf`/`everyEndpoint` (every
    endpoint a type serves) used by the test config and `withSpeechModels`,
    `withMessagesModels`, `withResponsesModels`, `newRetryGateway`; rerankers list
    `rerank`; `gateway/e2e`'s `everyEndpoint(typ)` reads
    `protocol/fixtures/backend-types/<type>.json` and every e2e config uses it;
  - config: `TestBackendTypeIssues` (one type: each unserved entry; types differing
    once, list not judged; unknown backend alone), `TestBackendTypeFixturePaths` (the
    shared fixtures report at the contract's paths), `TestProtocolVersionIsTheSchemasConst`
    (config and status schemas' const is `ProtocolVersion`); the shared fixtures
    (missing list, two types, unserved entries, version 6, a `format_version` member)
    run through `TestInvalidFixtures`;
  - `TestEndpointsListIsJudgedOnEveryRoute` keeps its checks; each route's model is on
    `llama-server` (or `vllm-omni` for the media routes) and lists another endpoint of
    that type alone (`withListedOn`), so the refusal comes from the list only;
    `TestEndpointsListFollowsTheAppliedConfig` runs on `llama-server`, its "no list"
    rounds now the type's every endpoint;
  - the "not found" reading: provider `TestPathMissingAnswersByModule` (every type,
    every endpoint it serves; 405 read on `vllm` alone; 501/503 relayed) and server
    `TestPathMissingAnswersAtOnce` (vllm 404 and 405, vllm-omni, llama-server, openai,
    azure-openai, anthropic, azure-anthropic, openai-compatible: `502
    upstream_path_missing` with the message, one request in all across two
    deployments, the circuit closed at threshold 1, `path_missing` counted, no retry
    series, no usage), `TestServersOwnAnswersAreServerErrors` (llama-server 501 on
    embeddings and rerank, vllm-omni's image 503: retried as `server_error`, both
    circuits open, the backend's status answered `upstream_error`);
    `TestImagesNoDiffusionStageIsAServerError` replaces the endpoint-missing images
    test;
  - deleted as removed behaviour: `endpointmemory_test.go` (8 tests),
    `cmd/kaiak/graph_test.go` (the memory forgotten on apply),
    `TestRoutingFollowsEndpointSupport`, `TestVLLMOmniServesNoOtherEndpoint`,
    `TestModelEntriesListServedEndpoints`, `TestModelEntriesListRerank`,
    `TestUnknownPathRefusesTheBackendForTheRequest`,
    `TestVLLMRouteMissingIsTheEndpointMissing` and `TestUnknownPathIsTheDeploymentsFailure`
    (replaced by `TestPathMissingAnswersAtOnce`), the retried "no endpoint at the
    host's base_url" case; `TestOutcomeClassification`'s wrong-path case and
    `TestAttemptRules`' path case now neutral and not retried.
- `gateway/e2e`: every config at `protocol_version: 7` with lists on one type;
  `TestWrongBaseURLOnVLLM` and rerank's "chat to the reranker" now expect `502
  upstream_path_missing`, neutral, counted `path_missing` (no warning); Messages' and
  Responses' "pair" scenario (`pairPathMissing`): a request routed to the server
  lacking the endpoint is answered `upstream_path_missing`, not retried, its circuit
  closed; the speech refusals carry the one message; `TestSeriesStartAtZero`'s retry
  reasons are the six; the cross-half test sends `config.ProtocolVersion`, and its
  `agent` model lists llama-server's endpoints, its reranker `rerank`.
- `scripts/live`: configs at protocol 7; chat models list the kind's endpoints but
  embeddings and rerank (`chatEndpoints`), the embeddings model `embeddings`, the
  reranker `rerank`; `models` checks every model's list; `messages-models` expects
  the chat models alone; the wrong-endpoint checks (`chat-to-reranker` on both kinds,
  `rerank-to-chat`) are now the list's refusal — `400 endpoint_not_served`, the one
  message, never sent; the endpoint-missing warning helper and `servedAfter` removed.

### Decisions made here

- **The version's home is `config`**: `control` imports `config`, so
  `config.ProtocolVersion` is readable by both without a cycle; the fake control plane
  keeps its own literal (it stands in for the other half and imports nothing).
- **vLLM's 405 is a module reading**, not a core rule: the core reads 404 for every
  module; only `vllm`'s signature includes the 405, so every other type relays it.
- **`provider.Serves` and the modules' endpoint tables stay**: no production caller
  remains (config validation holds lists to the type), but the spec has the modules
  tested against the shared table, and the server tests build lists from them.
  Reversed at review (Review follow-up, below).
- **The stage is renamed** `endpoint_list` (`checkEndpointListed`): it no longer finds
  deployments.
- **The live kit's wrong-endpoint checks are list refusals on both kinds**: with
  correct lists a wrong-endpoint request never reaches a server. The kit no longer
  drives a real server's route-missing answer; the reading is covered by the component
  and e2e tests against the recorded signatures.
- **e2e "pair"** keeps one deployment on a server lacking the endpoint — a valid
  config (one type) that is not identical instances — to show the reading is not
  retried.

### Removal checklist (`git grep -i` over `gateway/` and `scripts/`)

`EndpointMissing` 0 · `endpoint_missing` 0 · `upstream_endpoint_missing` 0 ·
`MissingEndpoints` 0 · `endpointmemory` 0 · `coreEndpoints` 0 · "core endpoint" 0 ·
`FormatVersion` 0 · `format_version` 0 · `servingDeployments` 0 ·
`errEndpointNotServed` 0 · "none of its backends serves" 0 · `Retain(` 6, all the
provider registry's connection-pool `Retain` (its definition, its call on config
apply in `cmd/kaiak`, and its tests) — unrelated to the endpoint memory. Also 0:
`openAICore`, `endpointMissing`, `noDiffusionStage`, `modeMissing`,
`missingEndpoints`, `kaiak.endpoint`, `endpoint_support`, `rq.serving`,
`control.ProtocolVersion`, "An endpoint missing from a server", "Wrong path to a
host".

### Suite

`scripts/check-all.sh` **fully green, twice** (the second run after a comment, a
wrapped test line and the schema-const test were added) — phase 1 ends green:

- gateway: gofmt, vet and staticcheck 2026.2.1 (crosshalf tag included), the
  telemetry boundary, `go test -race -count=1 ./...` — every package `ok`, `e2e` in
  122 s;
- live-test kit: gofmt, vet, staticcheck; self-test passed for vllm (59 passed),
  llama-server (38), openai (22), azure-openai (21), anthropic (16),
  azure-anthropic (16) and vllm with two backends (34), no failure;
- control: `npm test` 672 tests, 671 pass, 0 fail, 1 skipped (the store contract's
  reconnect test, as before); `npm run lint` — `tsc` clean, "boundaries ok";
- cross-half e2e (`-tags crosshalf`, race): `ok` in 75 s — the sample takes the
  gateway's `Kaiak-Protocol` 7 and the protocol-7 config.

`e2e` `TestSeriesStartAtZero` passed in both runs. During the step it failed once,
deterministically, from this step's own change: it listed `path_missing` among the
`kaiak_retries_total` reasons, which is no longer a retry outcome — fixed by listing
the six. The flake step 1 recorded did not recur, so it has no root cause found here.

### Review follow-up

The gateway keeps one endpoint-support table (the fewest lines):

- **Removed:**
  - the modules' endpoint tables: `openAIEndpoints`, `azureOpenAIEndpoints`,
    `vLLMEndpoints`, `vLLMOmniEndpoints`, `llamaServerEndpoints`,
    `openAICompatibleEndpoints`, `anthropicEndpoints`, `azureAnthropicEndpoints`;
  - `backendKind.serves` and `provider.Serves`;
  - provider's `TestEndpointSupportFollowsTheSpec` with its `endpointColumns`;
  - the module-against-fixture comparison in provider's `TestBackendTypeFixtures`,
    which keeps its models-list checks.
- **The one table** is `config.endpointSupport`. It sits in `snapshot.go` beside the
  `BackendType` and `EndpointName` constants, and is read through the method
  `BackendType.Serves(EndpointName)`. Config validation (`endpoint-not-served`)
  uses it, and the tests in provider, server and e2e build their lists from it.
- **Its checks:**
  - `TestEndpointSupportIsTheSharedTable` (against the fixtures, every type);
  - `TestEndpointSupportFollowsTheSpec`, moved to `config` (against GATEWAY.md's
    table, every type a row, every endpoint name in a column);
  - provider's `TestEndpointNamesAreTheSchemaEnum` still gives every
    `provider.Endpoint` its config name, in the schema's order.
- **`docs/specs/GATEWAY.md`, Providers → Endpoint support:**
  - "by its module" and "in its module" are gone;
  - "The table is shared data" now says each half holds one table that its config
    validation reads, both are tested against the fixtures, and the gateway's
    against the spec's table;
  - a type gaining an endpoint changes its fixture and both halves' tables in one
    change, and its module when the module needs code for it.

Suite: `scripts/check-all.sh` passed in full.

- gateway: every package `ok` under `-race`; `e2e` took 122 s;
- live-test kit: lint and self-test passed for every kind;
- control: 672 tests, 671 pass, 1 skipped; lint clean;
- cross-half e2e: `ok` in 76 s.

**Phase 1 ends green** (main session's run after the review follow-up, 2026-10-11):
`scripts/check-all.sh` — `all checks passed`; control 672 tests, 671 pass, 1
skipped; gateway `kaiak/e2e` and the cross-half e2e (`ok kaiak/e2e` 75.6 s) green;
the live kit's self-test green for every kind.
