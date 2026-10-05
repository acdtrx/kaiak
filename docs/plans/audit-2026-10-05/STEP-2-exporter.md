# Step 2 — exporter

**Status:** done (2026-10-05)

## Intent

Close the exporter's edges: M1, M2, M3, L1, L2 (with the exit report never
rate-limited), L3.

## Files likely touched

- `gateway/internal/otlplog/exporter.go`, `handler.go`; `gateway/cmd/kaiak/main.go`
  (L2: the signal watcher lives through the final flush).
- Tests: Codex's `otlplog/audit_b_test.go` and `cmd/kaiak/audit_b_test.go` ported;
  [X]'s repros (redirect to a login page answering 200, `Retry-After: 0`, a panicking
  `Error()`) added.

## Acceptance criteria

- Each ported repro fails before its fix (shown) and passes after.
- `scripts/check-gateway.sh` green; suite recorded (expected red: step 1's `OTEL_`
  fixtures until step 3).

## Result

**What changed**

- M1 — the exporter's client refuses redirects (`CheckRedirect` →
  `http.ErrUseLastResponse`); a `3xx` fails the batch at once, unretried, reported
  as `collector answered 307 Temporary Redirect: redirects are not followed` with
  its status.
- M2 — no collector text in a report: a failure status reads `collector answered
  <code> <text>` (Go's status text, local) and its body is no longer decoded; a
  partial success reads `collector rejected <n> records`. Transport errors are
  reported as `no answer from the collector: <class>` — the class from the new
  `internal/netfail` (timed out, cancelled, name not resolved, connection refused,
  host unreachable, connection closed, TLS failure, malformed response, else
  connection failed); the context cut stays `export cut short: <ctx error>`.
- M3 — `readExportResponse`: an empty (whitespace-only) body, or a JSON object
  whose `partialSuccess`, when present and not null, is an object with
  `rejectedLogRecords` an integer or decimal string (null = none) and
  `errorMessage` a string (null allowed); other members ignored. A read error on a
  `2xx`, or any other body, fails the batch unretried (`…, its answer unreadable:
  <class>` / `… with a body that is not an ExportLogsServiceResponse`).
- L1 — the wait is `max(Retry-After, backoff)`; `retryAfter` parses seconds as
  unsigned and saturates at the longest duration (an out-of-range value included)
  instead of reading as absent, so the deadline check fails the batch. The 24 h
  cap is gone.
- L2 — `cmd/kaiak`: the second-signal watch moved out of the background group; it
  runs from the drain's start until after the final flush (`endSignalWatch` in
  `run`'s first defer). `finishLogExport(e, by, hurry)` cuts the flush at its 1 s
  floor once `hurry` closes — before or during the flush; one already past the
  floor ends at once. The `logFlushBy` reset after the drain is gone (the hurry
  rule covers it). `Exporter.Close` writes its report through `writeReport`,
  bypassing the once-a-minute limit (`reportProblems` = the rate check +
  `writeReport`).
- L3 — `convert` recovers as `slog`'s `handleState.appendValue` does (checked
  against Go 1.27's `log/slog/handler.go`): `<nil>` when the value is a nil
  pointer, else `!PANIC: <value>`.
- `docs/ARCHITECTURE.md`: `netfail` listed; `otlplog` imports `netfail` (no longer
  `clip`).

**Decisions made during the step**

- **`internal/netfail` is a shared package**, not exporter-local: step 3 needs the
  same classification for the models probe, the request path and the control
  client (M5) — same purpose, so one module (CODING-RULES §2). Step 3 should use
  it, adding a class there if it needs one.
- A non-`2xx` body is still read (bounded, for connection reuse) and an oversized
  one still fails unretried, as before — `TestOversizedResponseFailsUnretried/503`
  unchanged; a read error on a non-`2xx` is ignored (the status decides).
- Negative `rejectedLogRecords` reads as none rejected (as before), not as an
  invalid answer.
- Existing tests updated to the new contract (each asserts the contract's rule,
  none weakened): `TestRetryAfterIsHonoured` (0 and a past date now wait the
  backoff), `TestNoRetryOnOtherStatuses` and `TestPartialSuccessIsReported` (no
  collector text), `TestPartialSuccessCountsRejectedAsFailed` ("not JSON" now
  fails the batch), `TestFlushEndsWithItsDeadlineAndCloseDrops` (two reports: the
  cut export's, then Close's drops), e2e `TestLogExportRefusedBatch` (the message
  is exactly `collector answered 400 Bad Request`, not the collector's text) and
  the comment of e2e `TestLogExportStalledCollectorAtExit`.

**Regression tests — before (the step's base, `7adfead`) → after**

Codex's ports (renamed):

- B2 → `otlplog` `TestRedirectIsNotFollowed` (307 and 308 to another host name):
  `the redirect's target was reached (x-api-key "test-secret")` → pass.
- B3 → `TestCollectorTextIsNeverReported`: `exception.message "collector
  answered 401 Unauthorized: invalid credential: Bearer test-secret"` and
  `"collector rejected 1 records: invalid credential: Bearer test-secret"` → pass.
- B5 → `TestUnreadableAnswerFailsUnretried/truncated_partial_success` (+ a login
  page, not JSON, `[]`, `null`, mistyped members — 9 cases): `counts
  {Exported:3 Failed:0 Dropped:0}, want {Exported:0 Failed:3 Dropped:0}` → pass.
- B7 → `TestLongRetryAfterFailsTheBatch` (`172800`, `99999999999999999999`):
  `2 requests (waits [359ms]), want 1` → pass.
- B6 → `cmd/kaiak` `TestSecondSignalCutsTheFinalLogFlush`: `the second signal did
  not cut the final log flush: run returned only once the collector answered`
  (2.01 s) → pass, run returns 1.00 s after the flush started.

The other reviewer's repros:

- 302 to a login page answering 200 → `TestRedirectToAPageAnsweringOKIsNotExported`:
  `counts {Exported:3 …}, want {… Failed:3 …}` → pass.
- `Retry-After: 0` / a past date → `TestRetryAfterBelowTheBackoffWaitsTheBackoff`:
  `wait 0 = 0s, want the backoff, within [250ms, 500ms]` (each of three waits) →
  pass.
- Typed-nil error, panicking `Error()`, panicking `MarshalJSON` →
  `TestPanickingValuesAreRenderedAsSlogDoes`: `panic: runtime error: invalid
  memory address or nil pointer dereference`, `panic: error text unavailable`,
  `panic: cannot marshal` (the test binary crashed) → pass (`<nil>`, `!PANIC: …`,
  equal to the JSON handler's line).
- Added for M2's transport half: `TestTransportErrorTextIsNeverReported` (a
  header line `Bearer test-secret`): `exception.message "net/http: HTTP/1.x
  transport connection broken: malformed MIME header: missing colon: \"Bearer
  test-secret\""` → pass (`no answer from the collector: malformed response`).
- `TestAnswersThatDeliver` (empty, `{}`, null members, unknown members) passed
  before and after: it pins what M3 must keep accepting.
- `netfail`: `TestClass`, `TestClassOfAMalformedHeaderCarriesNoBytes` (new).

**Suite (expected reds)** — test cache cleared; `scripts/check-all.sh` stopped
at `go test -race`, the remaining stages were run directly.

- Gateway: gofmt, vet, staticcheck pass; `go test -race ./...` FAIL in
  `internal/config` only — `TestInvalidFixtures/api-key-env-otel-headers.json`,
  `…/api-key-env-otel-logs-endpoint.json`, `…/api-key-env-otel-logs-headers.json`
  (step 1's fixtures; cleared by step 3). Every other package passes, `e2e`
  (103 s) and the new `netfail` included. Live-test kit: gofmt, vet,
  staticcheck, self-test pass.
- `control`: `npm test` 577 / 577 pass; `npm run lint`: `tsc` clean, `boundaries
  ok`.
- Cross-half e2e (`TestAcrossHalves`): pass (54 s).
