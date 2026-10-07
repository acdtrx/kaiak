# gateway-server — structure review

Scope: `gateway/internal/server`, 19 non-test files, 4,347 lines. All of them read in
full. Tests were read only where they show intent (`metrics_test.go`
`TestEveryErrorCodeHasItsClass`). History comes from `main` and `private` (`git log`,
`git show` of 2070fb1, d437d74, 5c2b27b).

## Module summaries

### `gateway/internal/server`

**What it does** (several separate jobs in one package):

- **HTTP lifecycle**: binds listeners, sets client deadlines, caps connections and adds `Connection: close` after early answers (`listener.go`). Runs the 3-phase drain and the in-flight count (`drain.go`). Serves the admin handler: probes and `/metrics` with an optional bearer token (`admin.go`).
- **API handler and pipeline**: the mux and method/URL refusals, the per-request struct, stage list and finishers, request IDs and `statusWriter` (`api.go`, `pipeline.go`).
- **Inbound stage**: reads the body against the gateway-wide body budget (`bodies.go`). Parses the owned fields for each API format (`inbound.go` OpenAI, `inbound_messages.go`, `inbound_responses.go`). Enforces the n/best_of/prompt-list caps. Refuses hosted tools, stored objects and stateful Responses.
- **Model params**: output-limit default, ceiling and context-length checks, and the thinking-budget fit (`params.go`).
- **Limits glue**: computes the reservation, `x-ratelimit-*` headers and the limit refusal answers (`limits.go`). Per-key concurrency (`keylimit.go`).
- **Attempt loop** (`upstream.go`): routing acquire, meter, provider send, the retry decision, the retry budget (`retrybudget.go`), memory of missing endpoints (`endpointmemory.go`), the 429 cooldown, the avoid set, and attempt classification for the circuit and metrics. Also settlement through the recorder.
- **Relay** (`upstream.go`): status, headers and events to the client; the TTFT and half-open signals; the gateway's own answer to a backend 5xx; reading backend error identifiers (`backend_errors.go`).
- **Model endpoints**: OpenAI- and Anthropic-shaped list, entry and props (`models.go`).
- **Errors**: the error envelopes (OpenAI or Anthropic) and every `apiError` constructor (`errors.go`). Some constructors live next to their concern (`limits.go`, `drain.go`).
- **Observation**: the request log line (`api.go` `logRequest`) and the ops-metrics feed with error classes (`metrics.go`).

**Exported surface used from outside** (only `gateway/cmd/kaiak`; e2e drives the binary):
`NewAPI`, `NewAdmin`, `Listen`/`Listener`, `ClientTimeouts`, `DefaultClientTimeouts`, `NewDrain`/`Drain` (`Run`, `Draining`), `DrainTimes`, `NewBodyBudget`, `DefaultBodyMemory`. Two exported names have no outside caller: `ClearBodyDeadline` is in-package only, and `BodyBudget.InUse` is used only by tests.

**Depends on**: accounting, auth, clip, config, limits, logattr, metrics, provider, routing.

**Domain concepts it encodes, and where**:

- **Endpoint** (API operation): the `endpoint` enum and its methods in `pipeline.go:21-127`. This mirrors `provider.Endpoint` for the 7 body endpoints (`upstream.go:564` `providerEndpoint`).
- **Error shape** (OpenAI or Anthropic envelope): `errors.go:43-110`. `models.go:177` decides it again.
- **API format**: borrowed from `provider.Format` (`inbound.go:79`).
- **Owned fields and output-limit keys**: `inboundFields` (`inbound.go:20`) and `outputLimitKeys` (`params.go:119`).
- **Retry reasons**: `upstream.go:22-32`, mirrored in `metrics/ops.go:100`.
- **Attempt outcomes, circuit classes and failure reasons**: `upstream.go:491` `classifyAttempt`.
- **Request error classes**: `metrics.go:76-177`.
- **Relay-end reasons**: `upstream.go:751`.
- **Drain phases**: `drain.go:16`.
- **Busy statuses (429/529)**: `upstream.go:302`, `errors.go:80`, `metrics.go:124`.
- **Hosted-tool allowlists and stateful/stored-object rules**: `inbound_messages.go`, `inbound_responses.go`.
- **Throttle cooldown**: `upstream.go:147`.
- **Retry budget**: `retrybudget.go`.
- **Missing-endpoint memory**: `endpointmemory.go`.
- **Body budget**: `bodies.go`.
- **Per-key in-flight count**: `keylimit.go`.

## Findings

Ranked by payoff vs cost.

### S1 — One classification of an attempt's end; retry, cooldown, avoid scope and meter refusal derived from it

- **Kind**: cross-function
- **Where**:
  - `upstream.go:259` `retryReason`
  - `upstream.go:491` `classifyAttempt`, `:553` `errorEventOutcome`
  - `upstream.go:116-121` (cooldown switch in `sendAttempts`)
  - `upstream.go:223-240` (`meter.Refused` code list in `sendAttempt`)
  - `upstream.go:326` `avoidAfter`
  - `upstream.go:22-32` (retry constants)
  - `errors.go:271` `errUpstream`
  - `metrics.go:136` `errorCodeClass` (its `upstream_*` arm)
  - `metrics/ops.go:100` `retryReasons`
- **Now**: the same facts about one attempt are decoded from `rq.upstreamStatus`, `rq.upstreamErr`, the `provider.Code` and the error-event kind in 6 separate switches. `retryReason` returns exactly `classifyAttempt`'s outcome whenever that outcome is one of the eight retryable ones, and `""` otherwise. I checked this case by case:
  - Refusal → client_error / "".
  - nil → canceled / "".
  - ResponseTimeout → response_timeout / "".
  - Caller error event → client_error / "".
  - Busy event and 429/529 → rate_limited / rate_limited.
  - 5xx → server_error / server_error.
  - Default → unavailable / unavailable.

  The retry constants are even the same strings as `metrics.Attempt*` (`"unavailable"`, `"timeout"`, … `"endpoint_missing"`). Other rules repeat the same decoding:
  - The cooldown trigger (`busyStatus || ErrorEventBusy`) is "outcome == rate_limited, before relay".
  - `avoidAfter` keys the backend-wide avoid on three retry reasons.
  - `sendAttempt` has its own list of codes after which the meter is told `Refused`.
- **How it got here**: every new failure kind was added as one more arm in each switch:
  - 2070fb1 (`CodePathMissing`) touched 7 places in this package: `errUpstream`, `errorCodeClass`, the retry constant, the `sendAttempt` list, `retryReason`, `avoidAfter`, `classifyAttempt`. It also touched `metrics.AttemptPathMissing` and `retryReasons`.
  - 5c2b27b (`CodeEndpointMissing`) repeated the same pattern.
  - d437d74 (error-event kinds) added a kind dimension to 5 of these switches.
- **Proposed shape**:
  - `classifyAttempt` stays the one decoder and returns `metrics.AttemptOutcome`, `routing.Outcome` and the reason.
  - A small rule table keyed by outcome replaces the rest, e.g. `var attemptRules = map[metrics.AttemptOutcome]struct{ retry, backendWide, throttle bool }`.
  - `retryReason` becomes `string(outcome)` when `attemptRules[outcome].retry`.
  - `avoidAfter` checks `backendWide`; the cooldown checks `throttle`.
  - `kaiak_retries_total` keeps its label values: they are already the outcome names.
  - `metrics.CountRetry` takes an `AttemptOutcome`, and `retryReasons` becomes a subset of `attemptOutcomes` in metrics (see cross-module hints).
  - The `meter.Refused` list and `errUpstream` stay keyed by `provider.Code`, but in one table next to the outcome. Columns: outcome, refused-before-generation, and the client answer constructor. `classifyAttempt`'s provider-code arm then reads that row instead of its own switch.
- **Payoff**:
  - Removes `retryReason` and the 8 retry constants (about 60 lines incl. comments), the cooldown switch, and the hand-kept `retryReasons` list in metrics.
  - Adding a provider failure code goes from about 7 places in server plus 2 in metrics to 1 table row plus the outcome constant.
  - Retry, cooldown and circuit can no longer disagree about what a busy backend is.
- **Cost / risk**: about 150 lines changed in `upstream.go` and `errors.go`. The behavior is meant to be identical. Retry, circuit, cooldown and metrics tests cover it (`retry_test.go`, `circuit_test.go`, `cooldown_test.go`, `attempt_metrics_test.go`). Spec text names retry reasons and outcomes, and they stay the same. No protocol or `kaiak-control` contract is touched.
- **Confidence**: high for the `retryReason` = retryable-outcome identity (verified arm by arm). Medium on the exact table shape. Running the retry/circuit tests after a mechanical swap would raise it.

### S2 — Per-attempt state lives on `request` and is valid only by call order

- **Kind**: cross-function
- **Where**:
  - `pipeline.go:180-181, 199-210`: `rq.deployment`, `rq.meter`, `upstreamErr`, `upstreamStatus`, `upstreamErrorCode/Type`.
  - `upstream.go:107-108` (copy and reset), `:422` `releaseAttempt` → `classifyAttempt(rq)`, `:341` `attemptOutcome(rq, …)`.
  - `api.go:323` (`triedAttempts` falls back to `rq` for the answering attempt).
- **Now**: `attempt` holds slot, deployment, meter, failure, held response, retry reason and outcome. The backend's answer for that same attempt (status, provider error, error code and type) sits on `request`. `rq.deployment` and `rq.meter` duplicate `lastAttempt()`'s fields (copied at `upstream.go:107`). `releaseAttempt(at)` classifies `rq`, not `at`. This is correct only because it is always called on the current attempt, and the comment has to say so. The reset at `:108` clears `upstreamErr` and `upstreamStatus` but not `upstreamErrorCode/Type`. As a result, an error-event code from a retried attempt survives onto the log line of a request a later attempt answered 200 (see Bugs).
- **How it got here**: the request struct predates retries (8bd91c9). The `attempt` struct grew alongside it, and the upstream fields stayed where the single-attempt code had put them.
- **Proposed shape**:
  - Move `status`, `err`, `errorCode` and `errorType` onto `attempt`.
  - Drop `rq.deployment` and `rq.meter` in favor of `rq.lastAttempt()`. A nil-safe accessor is needed for the log/metrics code that runs when no attempt was made.
  - `classifyAttempt(at, relayEnd)`, `attemptOutcome(at)` and `retryReason(at)` take the attempt.
  - The relay writes onto `rq.lastAttempt()`.
  - Request-level fields keep only what is the request's own: `relayEnd`, `firstContent`, `abort`, `failure`.
- **Payoff**:
  - Removes 6 request fields, the copy/reset lines and the "current attempt only" convention.
  - Makes the stale-code bug impossible by construction.
  - Pairs with S1: the classification gets a self-contained input.
- **Cost / risk**: mechanical, about 60 touched lines in `upstream.go`, `api.go` and `metrics.go`. Behavior is unchanged except the stale-code fix. Tests are black-box over HTTP, so they should not need changes.
- **Confidence**: high.

### S3 — The endpoint as one descriptor table instead of about 12 switches and predicates

- **Kind**: cross-function, with a cross-module mirror
- **Where** — switches and predicates over `endpoint`:
  - `pipeline.go:38` `path`, `:65` `name`, `:94` `operationName`, `:108` `bodyEndpoints`, `:119` `counts`, `:125` `namesModel`
  - `upstream.go:564` `providerEndpoint`
  - `errors.go:95` `errorShapeOf`
  - `params.go:119` `outputLimitKeys`
  - `inbound.go:251,271` (chat/completions in `sequences`), `inbound.go:109` (embeddings)
  - `inbound_messages.go:17,35`, `inbound_responses.go:19` (generating vs token-counting)
  - `models.go:177` (Anthropic shape decided again), `models.go:145`
- **Now**: what an endpoint is (route, metric name, GenAI operation, provider operation, counts-only, error shape, output-limit keys, whether it streams and generates) is scattered across 7 switches and about 6 equality checks in 6 files. For the 7 body endpoints, `endpoint` is a 1:1 mirror of `provider.Endpoint`, joined by the `providerEndpoint` switch.
- **How it got here**: chat/completions/embeddings came first. Messages (91e2c1e) and Responses (7d23023) each added 2 endpoints by extending every switch and adding `== endpointMessages` / `== endpointResponses` checks for the generating-vs-counting split.
- **Proposed shape**: one table, e.g.

  ```go
  type endpointSpec struct {
      path, name, operation string
      api      provider.Endpoint // body endpoints
      body     bool
      counts   bool              // token counting
      generates bool             // reads stream + output-limit keys
      outputLimitKeys []string   // first = injection key
      anthropicShape bool        // errors (model endpoints: by header)
  }
  ```

  The current `endpoint` int indexes it, and the methods become field reads. `errorShapeOf` and `answerModelEndpoint` share one "wants Anthropic" decision. The mux loop, `servedEndpoints` and the log line keep iterating `bodyEndpoints`.
- **Payoff**:
  - Adding a body endpoint (image/audio is in the backlog, and Messages/Responses each needed this twice) goes from 7–9 places in server to 1 row, plus the format parser.
  - Removes `providerEndpoint` and 6 switch functions, about 90 lines → about 35.
- **Cost / risk**: low to moderate; about 150 lines touched across 6 files. No contract change: paths, metric labels and error shapes stay identical. `endpoints_test.go` and `api_test.go` cover the routes.
- **Confidence**: medium-high. The payoff is real but realised only when an endpoint is added; S4 benefits from it now.

### S4 — Owned fields: read the output-limit keys from the endpoint's list; one parse prologue

- **Kind**: cross-function
- **Where**:
  - `inbound.go:20-36` (`MaxTokens`, `MaxCompletionTokens`, `MaxOutputTokens`)
  - `inbound.go:94-104`, `inbound_messages.go:16-28, 42`, `inbound_responses.go:18-30, 40`
  - `params.go:106-132` (`outputLimitKey.value` closures)
- **Now**: each output-limit key is known in three places: a field on `inboundFields`, a parse line in its format's parser, and an accessor closure in `outputLimitKeys`. All three parsers repeat the same prologue and epilogue:
  - `if generating { readModelAndStream; read <key> } else { readModel }`
  - `Sequences = 1`
  - `rq.input = accounting.EstimateInput(...)`

  The OpenAI parser also type-checks `max_tokens` and `max_completion_tokens` on embeddings, and `max_completion_tokens` on completions. Neither is in that endpoint's `outputLimitKeys`, so the gateway validates a key it then ignores.
- **How it got here**: chat had two keys, and Messages and Responses each added a field plus a parser branch (91e2c1e, 7d23023) instead of generalising.
- **Proposed shape**:
  - `inboundFields.OutputLimits []*int64`, aligned with `outputLimitKeys(ep)` (S3's `outputLimitKeys` field).
  - `parseOwnedFields` does the common part once: decode, `readModel`, stream and the output-limit keys when the endpoint generates, then the format-specific checks (n/best_of/inputs/stream_options; tools/files/thinking; stateful/tools/items), then `EstimateInput`.
  - `applyModelParams` iterates names and values directly; the closures go.
- **Payoff**:
  - Removes 3 struct fields, the accessor closures and about 20 lines of repeated prologue.
  - A new output-limit key or generating endpoint touches 1 place instead of 3.
- **Cost / risk**: about 60 lines. One behavior change to decide: embeddings and completions would stop type-checking keys the gateway never uses. Those keys pass to the backend either way, as passthrough requires. Covered by `server_test.go`, `messages_test.go`, `responses_test.go`. No spec contract except that the owned-field list in GATEWAY.md might mention the embeddings keys; check before changing.
- **Confidence**: high on the shape; medium on whether the incidental type checks are relied on.

### S5 — JSON walkers in the inbound rules repeat one prologue 10 times

- **Kind**: cross-function
- **Where**:
  - List walkers: `inbound.go:145` `refuseHostedTools`, `inbound_messages.go:143` `refuseStoredFiles`, `:160` `refuseStoredFileBlocks`, `inbound_responses.go:162` `refuseResponsesInputItems`, `:202` `refuseStoredFileParts`.
  - Object-with-repeats checks: `inbound.go:118`, `inbound_messages.go:118, 177`, `inbound_responses.go:99, 134`.
- **Now**: the list prologue (`var xs []json.RawMessage; if !startsWith(raw,'[') || Unmarshal…; for i, x := range xs { at := fmt.Sprintf(...); members, repeats, ok := decodeMembersRepeats(x); if !ok {continue}; if len(repeats)>0 {return errDuplicateMember(at+"."+repeats[0])} …`) appears 5 times. The object check (decode members, refuse the first repeat with a dotted path) appears 5 more times. The copies have already drifted:
  - `refuseStoredFiles` ignores repeats on message objects (`inbound_messages.go:149`, `members, _, ok`), while every sibling refuses them.
  - `refuseHostedTools` refuses non-objects; the others skip them.
- **How it got here**: the Messages and Responses inbound rules were written per format, then extended past the top level (5c2b27b).
- **Proposed shape**: two helpers.
  - `eachObject(raw, param, func(at string, m map[string]json.RawMessage) *apiError) *apiError`: list → objects, repeats refused, non-objects skipped. It takes a strict flag or a variant for the tools list.
  - `objectMembers(raw, at) (map, *apiError, ok)`.
- **Payoff**: about 50 lines removed across the two files. The drift (the message-repeat gap) is fixed by construction.
- **Cost / risk**: small and local. Inbound tests (`inbound_review_test.go`, `messages_test.go`, `responses_test.go`) pin the param paths in error answers.
- **Confidence**: high.

### S6 — The error class decided by the constructor, not by a list of code strings

- **Kind**: cross-function
- **Where**: `metrics.go:136-177` `errorCodeClass`, `errors.go` (every `err*` constructor), `limits.go:86,117`, `drain.go:183,191`.
- **Now**: each constructor sets a code string, and `errorCodeClass` maps the string back to a `metrics.ErrorClass` through a 40-line switch whose default is `internal`. Adding an answer means: the constructor, the switch, the test map in `metrics_test.go:528`, and the GATEWAY.md table. The test checks that the table and the switch agree. Codes that come from outside (`provider.RefusalError.Code`, e.g. `price_option_unsupported`; `string(provider.Code)` in `errUpstream`) are mapped by string as well.
- **Proposed shape**: `apiError` carries `class metrics.ErrorClass`, set where the error is built. `errRefused` classes every provider refusal as `invalid_request`. The upstream answers take their class from S1's table row. `errorCodeClass` goes away. The spec-parity test changes to collecting the `(code, class)` pairs the constructors produce (a table-driven list of constructors in the test). The parity with GATEWAY.md stays testable, at the cost of one test rewrite.
- **Payoff**: removes the string list and its silent `internal` fallback. A new error answer touches the constructor and the spec table, not three places. A provider refusal code can no longer fall into `internal`.
- **Cost / risk**: about 35 constructors gain a field, and the test needs restructuring. The payoff is modest because the current test already catches drift against the spec.
- **Confidence**: medium. Worth it mainly if done together with S1.

### S7 — The attempts stage as a struct, not an 8-parameter function threading its collaborators

- **Kind**: in-function, cross-function
- **Where**: `pipeline.go:253` `newPipeline` (8 params), `upstream.go:71` `sendAttempts` (8 params), `:208` `sendAttempt` (`missing` and `logger` passed through for one warning), `:440,:464` (`recorder` passed into `settleAttempt` and `endAttempt`), `api.go:43` `NewAPI` (9 params).
- **Now**: router, recorder, providers, retry budget, missing endpoints and logger are created or captured in `newPipeline` and passed down through every call of the attempt loop.
- **Proposed shape**: `type attempts struct{ router, recorder, providers, budget, missing, logger }` with `run(ctx, rq)` as the stage, and `send`, `settle`, `end` as methods. `newPipeline` builds it once.
- **Payoff**: parameter lists drop to `(ctx, rq)`. The next attempt-loop collaborator touches 2 places instead of 4–5 signatures.
- **Cost / risk**: small and mechanical. Best done together with S1/S2.
- **Confidence**: high, but low payoff alone.

### S8 — Stage applicability declared once instead of guards in every stage

- **Kind**: cross-function
- **Where**: `pipeline.go:253-270`. Guards in `inbound.go:44`, `params.go:26`, `limits.go:27`, `upstream.go:73`, `pipeline.go:290,297`. The `models` terminal stage (`models.go:176`) also runs as a no-op after every body request.
- **Now**: one stage list serves two request kinds: body endpoints, and model endpoints. Every body stage begins `if !rq.endpoint.takesBody() { return nil }`. `answerModelEndpoint` is reached by every successful body request and does nothing there.
- **Proposed shape**: `stage{name, run, bodyOnly bool}`, or a `models` flag, checked once in `API.serve`. This is still one pipeline: model endpoints pass admission, auth, key concurrency and model access, as now. This does not create a side door.
- **Payoff**: 5 guards removed, and the two request kinds become visible in the stage list.
- **Cost / risk**: tiny.
- **Confidence**: high, but low payoff.

### S9 — Split `upstream.go` (attempt loop | relay) and move the log line out of `api.go`

- **Kind**: cross-function (file-level)
- **Where**: `upstream.go` (783 lines, the most-churned file: 31 commits). It holds the attempt loop, retry policy, cooldown parsing, classification, settlement, and the relay with its backend-fault answer. `api.go` holds the handler/mux and the 150-line request log line (`logRequest`, `methodAttrs`, `providerName`, `limitAttrs`, `triedAttempts`).
- **Proposed shape**:
  - `attempts.go`: the loop, retry policy, classification, settlement; with S1, also the rule table.
  - `relay.go`: `relayResponse`, `peekedResponse`, `answerBackendFault`, relay-end reasons.
  - `requestlog.go`: the log line.
- **Payoff**: separates the two hot jobs that change for different reasons (retry/circuit rules vs relay behavior). No code removed.
- **Cost / risk**: a pure move.
- **Confidence**: high, but low payoff. Do it only in the same change as S1/S2.

Considered and left as is:

- **Extracting `listener.go` and `drain.go` into their own package.** They are a separable HTTP-lifecycle job. Their coupling to the pipeline is `admit`, `errShuttingDown`, `errConfigNotLoaded`, and `cutOff`/`errDrainCut`, which `upstream.go` uses. Extracting them would export those names for a mostly navigational gain.
- **`servingDeployments` + `missingEndpoints.exclude`.** These are two model-view filters, but with different semantics: static 400 vs dynamic fallback to all. Folding the dynamic filter into `routing.Avoid` would break `Avoid.firstAttempt()`.
- **`limits.go`, `keylimit.go`, `bodies.go`, `retrybudget.go`, `admin.go`.** Clean.

## Cross-module hints

- **Retry reasons kept in two lists**: `metrics/ops.go:100` `retryReasons` is a hand-kept string list equal to server's retry constants (`server/upstream.go:22`) and a subset of `attemptOutcomes`. `CountRetry(model, backend, reason string)` could take `AttemptOutcome` (S1).
- **`server.endpoint` mirrors `provider.Endpoint`**: they match 1:1 for body endpoints (`provider/provider.go:44`). Paths are kept twice: `/v1/chat/completions` in `server/pipeline.go:38` and `chat/completions` in `provider/provider.go:58`. Each provider module lists its endpoints (`provider/*.go` `…Endpoints`). Adding an endpoint touches provider (const, `path`, `Format`, each module's list), server (S3) and accounting (`meter.go:97`, `estimate.go:205` switch on `Format`).
- **Busy status (429/529) is known in several places**: `server/upstream.go:302` `busyStatus`, `server/errors.go:80` `anthropicErrorType`, `server/metrics.go:124`, and in provider's error-event kinds (`provider/stream_end.go:129`). The provider package could own "is this status busy" next to `ErrorEventBusy`.
- **`provider.Code` values double as client error codes**: `string(code)` in `server/errors.go:276-287`, and the strings are matched again in `server/metrics.go:156-166`. The provider's code vocabulary has effectively become part of the Client API error table.
- **Stored-file references get two error codes**: a `file_id` is answered `stored_object_unsupported` on Messages (`errors.go:419`) but `stateful_responses_unsupported` on Responses (`errors.go:412`). This is a contract (spec) choice, but it looks like arrival-order drift. Worth a look from whoever owns GATEWAY.md's Client API table.

## Bugs noticed in passing

- **Stale upstream error code on the log line.** `upstreamFailure` sets `rq.upstreamErrorCode` from an error event (`upstream.go:594-596`). The per-attempt reset at `upstream.go:108` does not clear it, and `relayResponse` overwrites it only for status ≥ 400 (`:639-641`). A request whose first attempt hit a busy/failed error event and whose retry succeeded logs `kaiak.upstream.error.code` from the failed attempt next to status 200 and no `kaiak.upstream.error.message`. Log-only. Fixed by S2. Confidence medium-high, from code reading; not reproduced.
- **Repeated members on Messages message objects are not refused.** `inbound_messages.go:149` discards `decodeMembersRepeats`' repeats for message objects, while blocks, sources, Responses items and parts are all refused on repeats. A message naming `content` twice could let the gateway's stored-file walk read a different occurrence than the backend. Low confidence that it matters: it depends on which occurrence the backend's JSON parser keeps. S5 would close it.
