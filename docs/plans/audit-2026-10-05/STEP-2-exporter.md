# Step 2 — exporter

**Status:** not started

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

_Not started._
