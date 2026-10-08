# Step 22 — one endpoint table

**Status:** done (2026-10-08)

## Intent

What an endpoint is — route, metric name, GenAI operation, provider operation, error
shape, output-limit keys, whether it generates or counts — is one row in one table keyed
by `provider.Endpoint`. Today it is ~7 switches and ~6 equality checks over 6 server
files, joined to provider's own enum by `providerEndpoint`; adding an endpoint touches
~11 places in two packages.

## Findings

- server S3, provider F6 (independent F04): `endpointSpec{path, name, operation, api
  provider.Endpoint, body, counts, generates, outputLimitKeys, anthropicShape}`; the
  body endpoints *are* `provider.Endpoint` (the three model routes a small separate set);
  `providerEndpoint` and the switch functions (`path`, `name`, `operationName`,
  `counts`, `namesModel`, `errorShapeOf`, `outputLimitKeys`) become field reads;
  `errorShapeOf` and `answerModelEndpoint` share one "wants Anthropic" decision.
- server hint: the client route is the provider path with `/v1/` — state the path once.

## Files likely touched

- `gateway/internal/server/{pipeline,upstream,errors,params,inbound,inbound_messages,inbound_responses,models}.go`
  (and `attempts.go`/`relay.go` after step 10).
- `gateway/internal/provider/provider.go` (if the path is stated once there).

## Decisions made during planning

- Step 10's stage applicability reads the table's `body` field.
- Paths, metric labels, operation names and error shapes stay identical (no contract
  change).

## Removal checklist (clean at phase end)

- `git grep -nE 'func providerEndpoint|func \(e endpoint\) (path|name|operationName|counts|namesModel)|func errorShapeOf' gateway/` → none.

## Acceptance criteria

- Adding a body endpoint is one table row plus its format parser and the provider modules
  that serve it (show the list in the Result).
- `endpoints_test.go`, `api_test.go` and the request-line key test pass unchanged.
- `scripts/check-gateway.sh` green, or reds named with the step that clears them.

## Result

**What changed**

- **server: one row per endpoint** (`pipeline.go`).
  - `endpoint` is now a struct, one row per endpoint, and requests carry `*endpoint`.
    A request refused before it matched an endpoint has `nil`, the old `endpointNone`.
  - Fields: `name`, `operation`, `body`, `api provider.Endpoint`, `counts`,
    `outputLimitKeys` and `anthropicOnHeader`.
  - `bodyEndpoints` holds the seven body rows. Each row's identity is its `api`, the
    `provider.Endpoint` it serves. The server no longer has its own body-endpoint enum
    and no named variable per body row.
  - The three model routes are named rows: `endpointListModels`, `endpointGetModel`,
    `endpointModelProps`.
  - Gone: the `endpoint` int enum, `path`, `name`, `operationName`, `takesBody`,
    `counts`, `namesModel`, `providerEndpoint` (`attempts.go`), `errorShapeOf`
    (`errors.go`), the `outputLimitKeys` function (`params.go`) and
    `wantsAnthropicModels` (`models.go`).
  - What replaces them:
    - field reads: `.name`, `.operation`, `.body`, `.counts`, `.api`,
      `.outputLimitKeys`;
    - `route()`, which is `"/v1/" + api.Path()`;
    - one Anthropic decision, `answersAnthropic(r)`. Both `(*endpoint).errorShape(r)`
      and `answerModelEndpoint` read it.
  - `stageScope.covers` reads `ep.body`. So does the log line's `gen_ai.request.stream`
    condition, the `takesBody()` call step 10 left in `requestlog.go`.
  - `authorizeModel` skips `endpointListModels`, the one endpoint that names no model.
    Its doc comment now says so.
  - Helpers that only body requests reach now take a `provider.Endpoint` instead of
    the row: `serves`, `servingDeployments`, `MissingEndpoints.remember` and
    `exclude` (key `{backend, api}`), `sequences`.
  - The inbound parsers changed only where the old enum was compared:
    - `parseOwnedFields` switches on `rq.endpoint.api.Format()`;
    - embeddings: `rq.endpoint.api == provider.Embeddings`;
    - the Messages and Responses generating-vs-counting checks: `!rq.endpoint.counts`;
    - `EstimateInput` and `NewMeter` get `rq.endpoint.api`.

    Output-limit parsing is unchanged (step 23).
  - `outputLimitKeys` moved into the rows with the same accessor closures, now named
    `maxCompletionTokensKey`, `maxTokensKey` and `maxOutputTokensKey` (`params.go`).
    The comment explaining the keys moved with the field.
  - Refused requests have a `nil` endpoint, and two places read it: `observeRequest`'s
    `endpoint` label (`""`, as before) and the log line's `gen_ai.operation.name`
    (absent, as before). Both check for `nil`.
- **provider: one row per `Endpoint`** (`provider.go`).
  - `path()` and `Format()` were two switches. They are now one `endpoints` array of
    `{path, format}`, indexed by `Endpoint`.
  - `path()` is exported as `Path()`, because the server builds the client route from
    it. Callers renamed: 7 modules, `send.go`, and 3 test files (message and subtest
    names only).
- **Each path is stated once.** For all seven body endpoints, the client route was
  `/v1/` + the provider path. Nothing differed, so no route is kept explicitly.
  - The model routes keep their mux patterns in `api.go` (`/v1/models`,
    `/v1/models/{rest...}`), as before.
  - Their old `path()` strings (`/v1/models`, `/v1/models/{id}`,
    `/v1/models/{id}/props`) had no reader, so the model rows have no route.
- About 90 lines of switches went. Non-test Go: 172 lines added, 260 removed, over 22
  files.

**Before/after facts dump**

- A throwaway test, deleted afterwards, printed each endpoint's facts on the old code
  and again on the new:
  - name (metric label), route, operation, body, counts, names-model;
  - error shape without and with `anthropic-version`;
  - output-limit keys;
  - for body endpoints: provider endpoint, format, which of the 7 backend types serve
    it, and the `endpoint_not_served` message.
- Ran on all 11 endpoints, the none endpoint included. The `diff` shows one difference:
  the three model routes' `route` is now empty. It was `/v1/models`,
  `/v1/models/{id}` and `/v1/models/{id}/props`, strings no code read (see above).
- Every other fact is identical:
  - names: `chat_completions`, …, `responses_input_tokens`, `list_models`,
    `get_model`, `model_props`;
  - operations: `chat`, `text_completion`, `embeddings`, or none;
  - routes `/v1/<provider path>`;
  - error shapes: Anthropic on both Messages endpoints, and on list/get when the header
    is sent; OpenAI everywhere else, props included;
  - keys, served-by sets and messages.

**Adding a body endpoint now touches**

1. provider: an `Endpoint` constant and its `endpoints` row (path, format) in
   `provider.go`.
2. provider: the `…Endpoints` list of each module that serves it. Also the endpoint
   support table in `GATEWAY.md`, which `TestEndpointSupportFollowsTheSpec` pins.
3. server: one `bodyEndpoints` row (name, operation, counts, output-limit keys).
4. server: its format parser. For an existing format, only the checks specific to this
   endpoint, if it has any. For a new format, a parser and its case in
   `parseOwnedFields`.
5. accounting: only for a new format (`meter.go`, `estimate.go`), or when the input
   estimate is specific to this endpoint (`estimate.go`, `openai_usage.go`).

Nothing else in server changes. All of these read the row: the mux route, the 405
answers, the metric label, the `/v1/models` `endpoints` list, `gen_ai.operation.name`,
stage scope, the error shape (taken from the format), the provider and accounting
endpoint, and token-counting handling in limits and settlement.

**Decisions made during the step**

- **Reuse, not wrap.** A body endpoint's identity is its `provider.Endpoint` (`api`).
  The server keeps no mirror enum for body endpoints and no named variable per body
  row.
  - Requests carry `*endpoint` because the model routes need an identity
    (`answerModelEndpoint`, `authorizeModel`). Pointer equality on the three named rows
    gives that without a second enum.
  - A value row could not be compared anyway: it holds a slice.
- **The Anthropic shape is taken from the format for body endpoints.**
  `answersAnthropic`:
  - a body endpoint answers in Anthropic's shape when it speaks `FormatMessages`;
  - a model row does when it is `anthropicOnHeader` and the request carries
    `anthropic-version`.

  So the review's `anthropicShape` column exists only for the model rows. A new
  Messages-format endpoint gets the right shape without a flag.
- **No `generates` field.** Its only readers today would be the Messages and Responses
  parsers' generating-vs-counting checks, which are exactly `!counts`.
  - Embeddings reads `stream` but generates nothing, so "generates" is not the same as
    "reads stream" either.
  - Step 23 adds the field if its shared prologue needs it.
- **`namesModel` is not a field.** `authorizeModel` compares against
  `endpointListModels`. A boolean set on 9 of 10 rows would be noise. The removal
  checklist's `namesModel` function is gone either way. Flagged, because the brief
  asked for a field read.
- **Provider's path and format are one row**, the "one provider descriptor for
  suffix/format" of provider F6 and independent F04. An out-of-range `Endpoint` now
  panics instead of returning `""`/OpenAI. Every value is a declared constant, so
  nothing can be out of range.
- **Test fixtures adapted, no assertion changed.**
  - A `bodyEndpoint(api)` helper was added to `server_test.go`.
  - The case tables in `limits_test.go` (`TestBatchSizeIsCappedPerRequest`) and
    `routing_test.go` (`TestEffectiveOutputLimit`,
    `TestInjectedOutputDefaultFitsTheContext`) now hold `*endpoint` built with it.
  - `endpointmemory_test.go` passes `provider.Responses`.
  - `metrics_test.go` passes `errEndpointNotServed` a row.
- **`TestParseOwnedFieldsKeepsTheRawBody` needed its fixture completed.** Its request
  set no endpoint. Under the old code, `endpointNone` worked by accident:
  - `providerEndpoint`'s default mapped it to chat, so the OpenAI parser ran;
  - `sequences` did not count it as chat, so the sequence cap never ran.

  Now it names chat. Its snapshot also gets `MaxSequencesPerRequest:
  DefaultMaxSequencesPerRequest`, because the zero cap refused one sequence. Its
  assertions are unchanged.

**Report vs code** (034329e; code after steps 9, 10 and 20)

- S3's sites had moved:
  - `providerEndpoint` was in `attempts.go`, after step 10's split, not
    `upstream.go`;
  - `bodyEndpoints` had gained `takesBody()`, which step 10's `stageScope.covers` reads;
  - the rest were in the reported files: `pipeline.go`, `errors.go`, `params.go`,
    `models.go`, and the inbound sites.
- `models.go` decided Anthropic in two places, as reported: `answerModelEndpoint`'s
  condition and `servesMessages`. The second is a serving check, not a shape
  decision. It now calls `serves(d, provider.Messages)`.
- Provider F6: "the upstream path is the client route minus `/v1/` for all seven"
  holds. The dump confirms it.
- The review's `path` for the model endpoints had no reader.

**Tests** (top-level from `go test -list`; in brackets, passes including subtests)

- `internal/server`: 188 (457) → 188 (457).
- `internal/provider`: 57 (204) → 57 (204).
- `endpoints_test.go`, `api_test.go` (including
  `TestRequestLineKeysFollowTheFieldTable`) and every other `server` test file not
  named above are unchanged.

**Removal checklist**

- `git grep --untracked -nP 'func providerEndpoint|func \(e endpoint\) (path|name|operationName|counts|namesModel)|func errorShapeOf' gateway/`
  → none.
- Wider, over `gateway`, `docs/specs`, `docs/ARCHITECTURE.md`, `docs/architecture`,
  `docs/TECH-STACK.md` and `README.md`, also → none:
  `git grep --untracked -nP 'providerEndpoint|errorShapeOf|wantsAnthropicModels|takesBody|namesModel|operationName\b|\bendpoint(None|ChatCompletions|Completions|Embeddings|Messages|MessagesCountTokens|Responses|ResponsesInputTokens)\b|outputLimitKeys\(|\.path\(\)'`.

**Suite**: `scripts/check-gateway.sh` passed (exit 0). No red carries over to a later
step.

```
==> gofmt / go vet / staticcheck 2026.2.1 (gateway)
==> go test -race -count=1 (gateway): ok cmd/kaiak, e2e (106s), accounting, auth, clip,
    config, control, limits, logattr, metrics, netfail, otlplog, provider, routing,
    schemacheck, server, sse
==> live-test kit: gofmt / vet / staticcheck; self-test passed for vllm, llama-server,
    openai, azure-openai, anthropic, azure-anthropic, vllm with two backends
gateway checks passed
```
