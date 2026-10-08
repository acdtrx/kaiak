# Step 9 — an attempt owns its result; one classification

**Status:** done (2026-10-08)

## Intent

An attempt's backend answer lives on the attempt, and one function decides what that
answer means. Today the status, error and error code sit on `request` (valid only by
call order), and six switches decode the same failure for retry, cooldown, avoid scope,
meter refusal, client answer and metrics. A new provider failure code touches ~9 places;
afterwards it is one table row.

## Findings

- server S2: `status`, `err`, `errorCode`, `errorType` move onto `attempt`;
  `rq.deployment` and `rq.meter` give way to `rq.lastAttempt()` (with a nil-safe
  accessor for the no-attempt log/metrics path); `classifyAttempt(at, relayEnd)`,
  `attemptOutcome(at)` take the attempt; the relay writes onto the last attempt.
  **Fixes the stale code:** a retried attempt's `kaiak.upstream.error.code` no longer
  reaches the log line of a request a later attempt answered.
- server S1, observability F2: `classifyAttempt` is the one decoder; a rule table keyed
  by `metrics.AttemptOutcome` gives `retry`, `backendWide` (avoid) and `throttle`
  (cooldown); `retryReason`, the eight `retry*` constants and the cooldown switch go;
  `metrics` declares `RetryableOutcomes` and `CountRetry` takes an `AttemptOutcome`;
  `metrics.retryReasons` goes. The `meter.Refused` code list and `errUpstream` sit in one
  table keyed by `provider.Code` next to the outcome.
- provider hints: "busy status (429/529)" is owned once (provider, beside
  `ErrorEventBusy`); `ErrorEventKind` → answer/outcome/label in one table instead of four
  server places.

## Files likely touched

- `gateway/internal/server/{upstream,pipeline,api,errors,metrics}.go`.
- `gateway/internal/metrics/ops.go` (`RetryableOutcomes`, `CountRetry`).
- `gateway/internal/provider` (busy status).

## Decisions made during planning

- `kaiak_retries_total`'s `reason` values stay exactly as they are: they already equal
  the outcome names.
- Before the swap, add the table test the observability report suggests:
  `retryReason(rq) == retryable(classifyAttempt(rq))` over the existing endpoint cases.
  It must pass on the old code, then the old function goes.

## Removal checklist (clean at phase end)

- `git grep -nE 'func retryReason|retryUnavailable|retryTimeout|retryServerError|retryRateLimited|retryAuthFailed|retryModelMissing|retryPathMissing|retryEndpointMissing|retryReasons' gateway/`
  → none.
- `git grep -nP 'rq\.(upstreamErr|upstreamStatus|upstreamErrorCode|upstreamErrorType|deployment|meter)\b' gateway/internal/server`
  → none.

## Acceptance criteria

- A regression test: first attempt ends on a busy error event, the retry answers `200`;
  the request line has no `kaiak.upstream.error.code`. It fails before the change.
- Retry, circuit, cooldown, attempt-metrics and endpoint tests pass with assertions
  unchanged.
- `scripts/check-gateway.sh` green, or reds named with the step that clears them.

## Result

**Bug proof (before the change)**

`TestRetriedAttemptsErrorCodeIsNotLogged` (`retry_test.go`): a Messages model with two
`vllm` deployments. The first stream opens with an `overloaded_error` error event, which
is busy. The retry on the second answers `200`. Run on the old code, it failed for the
reason the review gives: the per-attempt reset did not clear `upstreamErrorCode`.

```
--- FAIL: TestRetriedAttemptsErrorCodeIsNotLogged (0.01s)
    retry_test.go:167: kaiak.upstream.error.code = overloaded_error, want it absent
    retry_test.go:167: line: map[… http.response.status_code:200 kaiak.attempts:2
      kaiak.backend.id:vl-b … kaiak.tried:vl/msg-back:upstream_overloaded,vl-b/msg-back:200
      kaiak.upstream.error.code:overloaded_error …]
```

Its other assertions passed on the old code: the `kaiak.tried` value, no `error.type`,
message or type, and `kaiak_retries_total{…reason="rate_limited"} 1`. It passes now.

**What changed**

- S2: the attempt owns its result.
  - `attempt` gains `status`, `err`, `errorCode` and `errorType`.
  - `retryReason` is now a `metrics.AttemptOutcome`.
  - `attempt.outcome` is gone. `attemptOutcome(at)` derives it from `at.status` /
    `at.failure`, which do not change after the retry decision.
  - `request` loses `deployment`, `meter`, `upstreamErr`, `upstreamStatus`,
    `upstreamErrorCode` and `upstreamErrorType`, along with the copy/reset lines in the
    loop.
  - `sendAttempt(ctx, rq, at, …)` and `upstreamFailure(ctx, at, err)` write onto the
    attempt. So do `relayResponse`, which takes `rq.lastAttempt()`, and
    `answerBackendFault(at, resp)`.
  - `classifyAttempt(at, relayEnd)` and `attemptOutcome(at)` take the attempt.
    `releaseAttempt` passes `rq.relayEnd`, which is `""` until the relay.
  - The log line and the metrics read `rq.answeringAttempt()`. This nil-safe accessor
    returns the last attempt, or an empty `attempt` for a request that made none.
- S1 / observability F2: one decoder plus rule tables (`upstream.go`).
  - `classifyAttempt` is the one decoder. The loop classifies once (`classifyAttempt(at, "")`)
    and derives the rest:
    - retry is `retryable(outcome)`, read from `metrics.RetryableOutcomes`;
    - the cooldown comes from `attemptRules[outcome].throttle`, using the response's
      headers when there is a response and the default wait otherwise;
    - `avoidAfter` reads `attemptRules[at.retryReason].backendWide`.
  - Gone: `retryReason`, the eight `retry*` constants, the cooldown switch,
    `busyStatus` and `errorEventOutcome`.
  - `failureRules`, keyed by `provider.Code`, has these columns: outcome, circuit class,
    `refused` (replaces `sendAttempt`'s `meter.Refused` code list) and `answer`
    (replaces `errUpstream`, which is gone; rows built with `upstreamAnswer` in
    `errors.go`). A code without a rule is `CodeUnavailable`, as before.
  - `errorEventRules`, keyed by `provider.ErrorEventKind`, holds the same `failureRule`
    plus `class`, the error class of a stream the event ended. `failureRuleOf(perr)`
    picks the right row.
- `metrics`:
  - `RetryableOutcomes` is declared next to `attemptOutcomes`.
  - `CountRetry(model, backend string, outcome AttemptOutcome)` panics on an outcome
    that is not retryable.
  - `retryReasons` is gone. `prepareSeries` pre-creates from `RetryableOutcomes`.
- Provider hints:
  - `provider.StatusOverloaded` (529) and `provider.BusyStatus(status)` now sit beside
    `ErrorEventBusy`.
  - Uses:
    - `classifyAttempt`'s status arm uses `BusyStatus`;
    - `relayedStatusClass` uses `BusyStatus`;
    - `anthropicErrorType` and `answerBackendFault` use `StatusOverloaded`.
  - Server's `statusOverloaded` is gone.
  - `ErrorEventKind` is now mapped in one table (`errorEventRules`): outcome, circuit
    class, answer and error class. Before, four switches did this:
    - `errUpstream`;
    - `errorClass`;
    - the cooldown;
    - `errorEventOutcome`.

    What reads a kind now is a lookup (`failureRuleOf`, `classifyAttempt`'s
    incomplete arm, `errorClass`). The cooldown reads the outcome.
- `GATEWAY.md`, the request line rows:
  - `kaiak.upstream.error.*` are the last attempt's (settled 2026-10-08, decision 10);
  - `kaiak.upstream.error.code`'s "when" column also names a first event that was an
    error event, which the code already logged.

**Decisions made during the step**

- **`retry` is not a column of `attemptRules`.** It is read from
  `metrics.RetryableOutcomes`, which the step requires, so the retryable set is listed
  once. `attemptRules` holds only `backendWide` and `throttle`.
- **The equivalence test** `retryReason(rq) == retryable(classifyAttempt(rq))` was
  written first as `TestRetryReasonIsTheRetryableOutcome`. It ran 21 cases on the old
  code and passed:
  - refusal, canceled, gateway fault;
  - every `provider.Code` plus an unknown one;
  - the three event kinds;
  - statuses 200, 400, 404, 429, 500, 503, 529.

  Each case asserted the outcome, the retry reason and the equivalence. Once
  `retryReason` was gone, it was **rewritten to pin the rule table** as
  `TestAttemptRules`, with the same 21 cases. Each case asserts the outcome, the retry
  decision (`retriedFor`), `backendWide` and `throttle`.
- **The pre-relay failure reason** is the error text unless the rule is neutral. This
  equals the old arms one by one: neutral arms gave `""`, and failure and
  response-timeout arms gave the text.
- **Mid-stream error events:** an `errorEventRules` row keeps its outcome only when its
  circuit class is neutral (busy, caller). The backend failing is `broke_off`, as
  before.
- **`at.failure` is set for every attempt**, the answering one included. `triedAttempts`
  no longer falls back to `rq.failure`. For the answering attempt the two were the same
  whenever its status was 0.
- **`relayedStatusClass`** uses `BusyStatus`. Only relayed statuses below 500 reach it:
  every 5xx is answered by the gateway with `rq.failure` set. So 529 cannot get there,
  and behaviour does not change.
- **Not done here:** `errorCodeClass`'s `upstream_*` arm (S6, step 10). The tables
  carry no error class for code rules yet.
- **White-box tests adapted, assertions unchanged**
  (`TestRefusalAndEndpointMissingClassification`,
  `TestErrorEventsAndOverloadClassification`):
  - `rq` → `at`;
  - `retryReason(rq)` → `retriedFor(at)`, a test helper: the outcome when retryable;
  - `retry*` constants → the `metrics.Attempt*` constant of the same string;
  - the mid-stream request carries its attempt.
- **`TestOpsMetrics` gains two assertions:**
  - `CountRetry` panics on a non-retryable outcome;
  - every `RetryableOutcomes` value is an attempt outcome.

**Report vs code**

- Every location cited in server S1/S2, observability F2 and T3 matched the code at
  this step's start. Steps 1–8 had not touched these lines:
  - `upstream.go:20-32` constants, `:107-108` copy/reset, `:116-121` cooldown,
    `:223-240` refused list, `:259` `retryReason`, `:326` `avoidAfter`, `:491`
    `classifyAttempt`, `:553` `errorEventOutcome`, `:594-596` and `:639-641`;
  - `errors.go:271`;
  - `api.go:321-325`;
  - `metrics/ops.go:100`.
- Provider hint, "busy status in four places":
  - `server/metrics.go:124` checked only 429, not 529, though only statuses below 500
    reach it.
  - `server/errors.go:80` (`anthropicErrorType`) is not a busy predicate: it maps 429
    and 529 to different Anthropic types. It now shares only `StatusOverloaded`.
  - `provider/stream_end.go:129` is a comment. The kind mapping there is by Anthropic
    error type, not status.

**Behaviour check**

- `kaiak_retries_total`: the pre-created series of the test config (73 lines: every
  model × backend × reason, plus the counted retry) were captured from the old code's
  scrape and from the new code's, using the same scenario. They are byte-identical. The
  metric's help text is unchanged.
- Retry, circuit, cooldown, attempt-metrics and endpoint tests pass with their
  assertions unchanged.

**Tests** (before → after)

| Package | `go test -list` | `-v` `=== RUN` |
|---|---|---|
| `internal/server` | 185 → 187 | 433 → 456 |
| `internal/metrics` | 12 → 12 | 12 → 12 |
| `internal/provider` | 54 → 54 | 201 → 201 |

- New: `TestAttemptRules` (21 subtests) and `TestRetriedAttemptsErrorCodeIsNotLogged`.
- No test was deleted.

**Removal checklist**

- `git grep -nE 'func retryReason|retryUnavailable|retryTimeout|retryServerError|retryRateLimited|retryAuthFailed|retryModelMissing|retryPathMissing|retryEndpointMissing|retryReasons' gateway/`
  → none.
- `git grep -nP 'rq\.(upstreamErr|upstreamStatus|upstreamErrorCode|upstreamErrorType|deployment|meter)\b' gateway/internal/server`
  → none.
- `busyStatus`, `statusOverloaded`, `errUpstream(`, `errorEventOutcome` and
  `at.outcome` are gone from `gateway/`, `docs/specs` and `ARCHITECTURE.md`.

**Suite**: `scripts/check-gateway.sh` passes. No red carries over to a later step.

```
==> gofmt / go vet / staticcheck 2026.2.1 (gateway)
==> go test -race -count=1 (gateway): ok cmd/kaiak, e2e (106s), accounting, auth, clip,
    config, control, limits, logattr, metrics, netfail, otlplog, provider, routing,
    schemacheck, server, sse
==> gofmt / go vet / staticcheck (live-test kit)
==> live-test kit self-test: passed for vllm, llama-server, openai, azure-openai,
    anthropic, azure-anthropic, vllm with two backends
gateway checks passed
```
