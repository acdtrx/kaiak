# Step 5 — verify

**Status:** done (2026-10-06)

## Intent

Prove the phase: every review repro passes, e2e covers the two daily-impact fixes, the
suite is green three times.

## Files likely touched

- `gateway/e2e`: the exporter against a collector that redirects (nothing delivered
  elsewhere, counted failed); a token refusal blocked only by in-flight reservations
  answering the short `Retry-After`, then admitted.
- `docs/plans/audit-2026-10-05/OVERVIEW.md` verification status;
  `docs/reviews/2026-10-05/AUDIT.md` gets an implementation note per finding (fixed in
  which commit).

## Acceptance criteria

- `scripts/check-all.sh` ×3, Go test cache cleared before each, all green; recorded.

## Result

**What changed**

- `gateway/e2e/logexport_test.go` — `TestLogExportRedirectIsNotFollowed` (M1): the
  binary exports to a collector answering `307` with `Location` on a second
  `httptest` server, a text body carrying a marker and the credential, and
  `OTEL_EXPORTER_OTLP_HEADERS=x-api-key=<secret>`. The second server receives no
  request; `log export failing` reads exactly `collector answered 307 Temporary
  Redirect: redirects are not followed` with `http.response.status_code` 307, failed
  > 0, dropped 0; `kaiak_log_export_records_total{outcome="failed"}` covers the
  report, `exported` stays 0; stderr holds neither the secret, the body's text nor
  the second server's address.
- `gateway/e2e/keylimits_test.go` — `TestTokenRefusalBlockedByARunningRequestRetriesSoon`
  (M4): `eval` gets a 120 tokens/minute limit on `chat`; one streamed request is held
  by the fake backend (`Reply.Pace`, a channel) — its reservation (input estimate
  plus the 64 default output) fits once, not twice. The same request beside it is
  refused 429 `rate_limit_exceeded` with `Retry-After: 2` and
  `x-ratelimit-reset-tokens: 2s`; once the pace channel closes and the held request
  settles (its log line), the same request is admitted, its reset no longer `2s`.
  Deterministic: the only waits are the backend's arrival, the stream's end and log
  lines.
- `docs/reviews/2026-10-05/AUDIT.md` — Implementation section: each finding, its
  status, commits and regression tests.
- `OVERVIEW.md` — Verification status.

**Before the fixes** (both tests copied onto `2ab42a4`, the plan's base, `go test
-race -run …`):

```
--- FAIL: TestTokenRefusalBlockedByARunningRequestRetriesSoon (1.40s)
    refusal X-Ratelimit-Reset-Tokens "1m0s", want "2s": only a running request blocks
    refusal Retry-After "60", want "2": only a running request blocks
--- FAIL: TestLogExportRedirectIsNotFollowed (15.03s)
    gateway did not log the failure report within 15s
```

(the redirect was followed and the second server's `200` counted the batch as
exported, so no failure was ever reported). Both pass on this branch.

**Regression tests — all present and passing** (re-run together, `-race -count=1`):

- `otlplog`: `TestRedirectIsNotFollowed`, `TestRedirectToAPageAnsweringOKIsNotExported`,
  `TestCollectorTextIsNeverReported`, `TestTransportErrorTextIsNeverReported`,
  `TestUnreadableAnswerFailsUnretried`, `TestAnswersThatDeliver`,
  `TestLongRetryAfterFailsTheBatch`, `TestRetryAfterBelowTheBackoffWaitsTheBackoff`,
  `TestPanickingValuesAreRenderedAsSlogDoes`.
- `cmd/kaiak`: `TestSecondSignalCutsTheFinalLogFlush`.
- `netfail`: `TestClass`, `TestClassOfAMalformedHeaderCarriesNoBytes`.
- `provider`: `TestLogExportVariablesAreNeverSentAsACredential`,
  `TestBackendBytesNeverReachAProviderError`.
- `control`: `TestProtocolMismatchLogsNoHeaderValue`,
  `TestErrorCodesAreLoggedOnlyInTheirShape`, `TestTransportFailuresAreLoggedAsTheirClass`.
- `config`: `TestInvalidFixtures/api-key-env-otel-headers.json`,
  `…/api-key-env-otel-logs-endpoint.json`, `…/api-key-env-otel-logs-headers.json`.
- `limits`: `TestRefusalBlockedOnlyByRunningRequestsAnswersAShortRetry`,
  `TestRefusalBlockedBySettledUsageKeepsItsTime`, `TestCarryOverLogsUsedInDollars`,
  `TestGlobalLimitLinesCarryNoGroup`, `TestSlidingMinuteCopyDropsReservations`.
- `server`: `TestRequestLineKeysFollowTheFieldTable`.
- kaiak-control `src/config/config.test.ts`: "a backend's api_key_env cannot name a
  gateway OTEL_ variable" (and the three fixtures in "invalid config fixtures").
- `e2e`: `TestLogExportRedirectIsNotFollowed`,
  `TestTokenRefusalBlockedByARunningRequestRetriesSoon` (this step).

**Suite ×3** — `go -C gateway clean -testcache`, then `scripts/check-all.sh`, three
times in a row; every run exit 0, `all checks passed`:

| Run | Wall time | Gateway e2e | Cross-half e2e | `npm test` |
| --- | --- | --- | --- | --- |
| 1 | 164 s | 107.4 s | 48.9 s | 578 / 578 |
| 2 | 176 s | 110.9 s | 58.9 s | 578 / 578 |
| 3 | 166 s | 110.9 s | 48.8 s | 578 / 578 |

Each run: gofmt, vet, staticcheck clean; `go test -race` ok in all 18 gateway
packages (580 top-level Go tests, 40 of them e2e); live-test kit gofmt, vet,
staticcheck and self-test pass; `npm run lint` — `tsc` clean, `boundaries ok`;
cross-half e2e ok. No flaky test.
