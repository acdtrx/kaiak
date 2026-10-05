# Step 4 — limits and vocabulary

**Status:** not started

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

_Not started._
