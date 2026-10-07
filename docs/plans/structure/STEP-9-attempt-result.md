# Step 9 — an attempt owns its result; one classification

**Status:** not started

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
- `git grep -nE 'rq\.(upstreamErr|upstreamStatus|upstreamErrorCode|upstreamErrorType|deployment|meter)\b' gateway/internal/server`
  → none.

## Acceptance criteria

- A regression test: first attempt ends on a busy error event, the retry answers `200`;
  the request line has no `kaiak.upstream.error.code`. It fails before the change.
- Retry, circuit, cooldown, attempt-metrics and endpoint tests pass with assertions
  unchanged.
- `scripts/check-gateway.sh` green, or reds named with the step that clears them.
