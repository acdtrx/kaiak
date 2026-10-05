# Step 4 — limits and vocabulary

**Status:** done (2026-10-05)

## Intent

M4 (short `Retry-After` when only in-flight reservations block), L4, L5, L6.

## Files likely touched

- `gateway/internal/limits/window.go` (in-flight tracking for minute windows,
  `waitFor`, `resetIn`), `limits.go` (L4 carry-over line), `shared.go` (L5).
- `gateway/internal/server/api.go` (L5 request line, L6 status code), `headers.go`.
- Tests: Codex's `limits/audit_b_test.go` ported; [L]'s repro (refused, then admitted
  within the short wait) as a test; the request-line key test updated for L5/L6.
- Live-test kit and docs if they read `kaiak.limit.id` or the 499 status.

## Acceptance criteria

- Ported repros fail before, pass after; a minute-window and an hour-window refusal
  blocked only by in-flight reservations answer the short wait; one blocked by settled
  usage still answers the slot expiry.
- `scripts/check-all.sh` green; suite recorded.

## Result

**What changed**

- M4 — `limits/window.go`: minute windows keep the part of each bucket held by
  unsettled reservations (`nHeld`, beside `n`), as hour windows keep `held`;
  `reserve`/`release`/`keep`/`clampNegative` maintain it, `inFlight(now)` reads it for
  both kinds, and `copySettled` now drops a minute window's in-flight reservations
  too (the spec's "copied … without its in-flight reservations" held only for hour
  and month windows). `limits.go`: `counter.blockedByRunning` — a token limit that
  would admit the request without its in-flight reservations (the per-minute
  share's empty-window rule included); `Reserve` counts `inFlightRetry` (2 s) for
  such a limit instead of `waitFor`, and `headersFor` reports reset `2s` for it on
  the refusal only. A limit blocked by settled usage keeps `waitFor` (slot expiry /
  window end) — unchanged.
- L4 — the carry-over line writes `kaiak.limit.used` through `limits.LogValue`
  (dollars for USD limits); the request line's `limitAttrs` uses the same function.
- L5 — `kaiak.limit.id` is gone: the request line logs `kaiak.limit.group`, absent
  for a global limit; the four operational lines (carry-over, negative clamp, small
  share, pushed window ahead) build scope and group with `identityAttrs`, which
  omits the group for a global limit instead of logging `""`. `Rejection.ID` renamed
  `Rejection.Group`.
- L6 — `server/api.go` `logRequest`: no `http.response.status_code` when the request
  failed with `client_closed` (`codeClientClosed`, new constant in `errors.go`); the
  request metric still records 499 (`status_class="4xx"`).
- Tests: `TestRequestLineKeysFollowTheFieldTable` — group refusal pins
  `kaiak.limit.group`, a new global-refusal case (no group key), a new
  client-left-before-any-answer case (no status code); `visibility_test.go`,
  `queue_test.go`, `retry_test.go` and the e2e tests (`grouptree`, `media`,
  `logexport`) read `kaiak.limit.group` / assert the status code absent.
  `TestRetryAfterIsTheLongestWaitAmongRefusingLimits` now settles its two requests,
  so its hour limit is blocked by settled usage and still answers the window end —
  the assertion unchanged. Live-test kit: reads neither key nor 499 (its status
  check is on successful requests only) — no change; docs were updated in step 1.

**Decisions**

- The short retry applies to token limits only: a request limit's in-flight hold is
  kept at settlement, so running requests never free it.
- "Blocked only by requests still running" is computed per counter as "settled usage
  + need fits" (`usedAt − inFlight`); a counter blocked by settled usage keeps the
  existing slot-expiry walk, which still counts its in-flight buckets as staying
  until they expire (conservative, as before).
- `Retry-After` stays the longest wait among refusing limits, so a request limit
  refusing too still wins with its slot expiry.
- L6's condition is the request's failure code (`client_closed`), not the recorded
  status 499 — a relayed backend status is a response sent.

**Regression tests — before → after**

New in `limits`: `TestRefusalBlockedOnlyByRunningRequestsAnswersAShortRetry`
(minute: 500 000 tpm, 7 × 64 000 running, 8th refused, then admitted after the first
settles small; hour: 1 000 000 tph, 9 × 104 000, 10th), `TestRefusalBlockedBySettledUsageKeepsItsTime`
(guard — passes before and after), `TestCarryOverLogsUsedInDollars` (Codex's B8,
ported), `TestGlobalLimitLinesCarryNoGroup`, `TestSlidingMinuteCopyDropsReservations`.
Before the fix:

```
--- FAIL: TestRefusalBlockedOnlyByRunningRequestsAnswersAShortRetry/minute
    retry after 1m0s, want 2s: only running requests block
    tokens headers on the refusal &{Limit:500000 Remaining:52000 Reset:1m0s}, want reset 2s
--- FAIL: TestRefusalBlockedOnlyByRunningRequestsAnswersAShortRetry/hour
    retry after 55m0s, want 2s: only running requests block
    tokens headers on the refusal &{Limit:1000000 Remaining:64000 Reset:55m0s}, want reset 2s
--- FAIL: TestCarryOverLogsUsedInDollars
    $2 of carried spend logged as kaiak.limit.used=2e+09, want 2
--- FAIL: TestGlobalLimitLinesCarryNoGroup
    global limit's line: scope global, kaiak.limit.group "" present true; want global and no group
--- FAIL: TestSlidingMinuteCopyDropsReservations
    copy counts 500, want the 300 settled
```

`server` before the fix (updated expectations): `TestRequestLineKeysFollowTheFieldTable`
(`limit_refusal`, `global_limit_refusal`, `client_left_before_any_answer`),
`TestLeavingTheQueueReleasesTheReservation`,
`TestRetryQueuesForACappedBackend/client_gone_while_the_retry_waits`,
`TestLimitRefusalsLogTheLimit`, `TestBudgetUnavailableLogsTheLimit` — FAIL
(`kaiak.limit.id` present, `kaiak.limit.group` missing, status code 499 present).
After: all PASS.

**Suite** — `scripts/check-all.sh`: all checks passed. Gateway gofmt, vet,
staticcheck, `go test -race` (every package, `e2e` included); live-test kit gofmt,
vet, staticcheck, self-test; control `npm test` 578 / 578, `npm run lint` (`tsc`,
`boundaries ok`); cross-half e2e ok.
