# Step 2 — money and routing correctness (F3, F5, F6, F7)

**Status:** done (2026-09-25) — phase 2 green

## Items

- **F3** the provider keeps the HTTP status it received even when reading the first
  body bytes fails: an error status (4xx/5xx) is classified and accounted as that
  status (no "unanswered" billing for an explicit refusal; retry decision by status —
  a 429 excludes that deployment); the client may get a sanitized gateway error.
  Regressions: `TestAuditErrorHeadersLostBeforeFirstBody`,
  `TestAuditTruncated429ChargesInput`; cover truncated 400/429/500 and a first-read
  timeout after error headers.
- **F5** the gateway keeps a small set of control-plane process IDs it has moved away
  from (bounded, e.g. the last 16) and ignores totals from them; acks from them still
  retire their batches (`counted_through`). Spec: the revision rule in
  CONTROL-PROTOCOL.md. Regression: `TestAuditDelayedPreviousProcessTotalsReapply` +
  an overlapping restart/ack/stream test with nonzero `counted_through`.
- **F6** admission decides USD applicability from the request's own snapshot (the one
  billing prices from). Regression: `TestAuditPriceChangeSkipsOldRequestMoneyCounter`.
- **F7** fixed windows get an incarnation identity; holds record it; a cleared
  incarnation's holds are never released against a later one, even if the timestamp
  repeats; an internal negative count is detected (logged, clamped at 0) instead of
  saturating. Regression: `TestAuditClockCorrectionReleasesOldHoldTwice`.

## Acceptance

Each regression fails on `0.4.0` and passes; specs dated 2026-09-25; phase 2 green.

## Result

- **F3** `provider.openAIFormat.Send`: an error answer (status ≥ 400) whose first
  body read fails (cut, or the first-event/response timeout after the headers) is
  returned as the response — status and allowlisted headers intact, the break as its
  first `Next` — instead of `upstream_unavailable`. The `404` model check no longer
  turns a read error into a failure (the body replays it). The server then settles
  it as answered (`meter.Answered`: no units, no cost, not estimated), retries and
  classifies by status (429 → `rate_limited`, deployment refused; 5xx →
  `server_error`, circuit failure; other 4xx → not retried, neutral). A 5xx goes
  through `answerBackendFault` as before; a 4xx whose first `Next` is a break gets
  the same answer (`upstream_error` under the backend's status + `Retry-After`).
  A 2xx broken before its first event stays `upstream_unavailable`. Fake backend:
  `Reply.CutBeforeBody`, `Reply.StallBeforeBody`. Spec: GATEWAY.md → Providers:
  upstream failures.
- **F5** `control.Client` keeps `retired` (last 16 control-plane process IDs moved
  away from); `takeTotals` ignores totals from one (logged, info) but still hands
  over its `Counted` (acked batch + `counted_through`). Spec: CONTROL-PROTOCOL.md →
  Totals → revision (replaced processes). No wire change, no kit change.
- **F6** `limits.Subject.Priced`: the server computes it from the request's own
  snapshot at `rq.start` (`accounting.PriceAt`, the entry `accounting.Cost` uses);
  `Limiter.applicable` no longer reads the live config's prices. Spec: GATEWAY.md →
  Unpriced models (edge case).
- **F7** fixed windows carry an `incarnation`, bumped by `window.clear` (roll and
  `setBase` take-back); a hold records it, and `release`/`keep` act only in the same
  incarnation. `window.clampNegative` + `Limiter.checkCountLocked` after every
  release/keep/counted: a negative count is logged (error, `limit counter went
  negative: clamped to 0`) and clamped. Spec: GATEWAY.md → Control-plane mode →
  Windows (incarnations).
- Regression tests (each confirmed failing on the unfixed code first):
  - `provider.TestErrorAnswerBrokenBeforeItsBodyKeepsItsStatus` (from
    `TestAuditErrorHeadersLostBeforeFirstBody`; 400/404/429/500 cut, 429 stream cut,
    429 response timeout, 503 first-event timeout) — failed: `upstream_unavailable`
    / `upstream_response_timeout` / `upstream_timeout`;
  - `server.TestErrorAnswerBrokenBeforeItsBodyIsAnsweredByItsStatus` (from
    `TestAuditTruncated429ChargesInput`; 400/429/500 cut, 429 timed out after
    headers; record zero units/cost, client status, Retry-After, attempt outcome) —
    failed: `status 502, want 429` etc.;
  - three new cases in `server.TestRetrySucceedsOnTheOtherDeployment` (429 cut,
    500 cut, 503 timed out after headers: retried by status, one record) — failed:
    2 records / `504`;
  - `control.TestTotalsFromAReplacedControlPlaneAreIgnored` (from
    `TestAuditDelayedPreviousProcessTotalsReapply`; A→B→A with nonzero
    `counted_through` on every message, B's later ack, the 16-entry bound) — failed:
    `[{100 1} {200 1} {150 2} {170 2} {260 3}]`;
  - `limits.TestBillabilityComesFromTheRequestsOwnSnapshot` (from
    `TestAuditPriceChangeSkipsOldRequestMoneyCounter`; priced→free before admission
    and after reservation, free→priced) — failed: `admitted, want refused`;
  - `limits.TestHoldOfAClearedWindowIsNeverReleasedAfterAClockCorrection` (from
    `TestAuditClockCorrectionReleasesOldHoldTwice`) — failed:
    `used 9223372036854775807`; plus `limits.TestNegativeCountIsClampedNotSaturated`.
- Test helper change (not weakened): the limits tests' subjects carry
  `Priced: true` for m1, and `on(model)` sets it (m1 priced, m2 not), matching the
  test config.
- `gateway/internal/_auditrepro/` removed: F1, F2, F4 were converted in step 1
  (`TestAuditDuplicateConfigBypassesReservedSecret`, `TestAuditRepeatedModelExpansion`,
  `TestAuditBatchMultiplicityExceedsMaxN`), the other five here.
- Suite: `GOFLAGS=-count=1 scripts/check-all.sh` → `all checks passed` (gateway race
  tests incl. e2e, control 472 tests + lint, cross-half e2e).

## Decisions made during the step

- F3's client answer: a broken 4xx gets the gateway's `upstream_error` (type
  `server_error`, message "The model backend failed to process the request.") under
  the backend's status, with its `Retry-After`/`Retry-After-Ms` — the same answer as
  a 5xx, no new error code. The refusal is known, its text is not.
- F3 scope: statuses ≥ 400 only; a 3xx/2xx broken before its first byte keeps the
  "connection lost" treatment.
- F3: a stream request's first-event timeout after error headers counts as the
  status's answer too (the timeout is not charged as sent-unanswered).
- F5: the bound is 16; ignoring is logged at info (a rare event worth seeing).
  `counted_through` from a retired process is still honoured — its process counted
  those batches before it was replaced.
- F6: `Subject.Priced` (a bool the caller computes) rather than passing prices into
  the limiter; the limits themselves still come from the live config.
- F7: the incarnation replaces the start timestamp in holds (the start check became
  redundant); `copySettled` needs no bump (holds stay on the original counter).

## Decisions for the user to confirm

- The broken-4xx client answer (`upstream_error` under the backend's status) rather
  than a synthesized OpenAI-shaped error of the 4xx's own kind (e.g. a
  `rate_limit_exceeded` for a 429).
- Retired control-plane IDs are in memory only: a gateway restart forgets them (a
  delayed old-process ack right after a gateway restart is not a case — its
  connections died with it).
- Negative counters log at error level (they indicate a bug); no metric added.

## User confirmation

All open "Decisions for the user to confirm" in this step were confirmed by the user on
2026-09-25, as implemented.
