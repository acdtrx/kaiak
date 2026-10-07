# Step 11 — one limit-type table in the gateway

**Status:** not started

## Intent

A limit type's window, measure and whether the control plane counts it are stated once,
in `config`, and read everywhere else. Today they are restated in ~10 places across 6
packages, and `limits.shape` silently treats any type it does not name as a USD-per-month
budget.

## Findings

- T1, config F2, routing-limits F9, observability F3 (the limit-type list only):
  - `config`: `LimitType → {Window: minute|hour|month, Measure: requests|tokens|cost,
    Counted bool}` (Counted = the control plane counts it), with `t.Window()`,
    `t.Measure()`, `t.Counted()` and an ordered `LimitTypes()`;
  - derived from it: the schema walker's enum and its "must be an integer" list
    (`Measure != Cost`); the per-minute set in `semantic.go` and `countCounters`' literal
    `2`; `limits.shape` (no fallthrough: an unknown type is a programming error), the
    `countedTypes` set and the four `kind != SlidingMinute` checks (they read `Counted`);
    `control/schema.go`'s totals enum and its window-start switch; `metrics/ops.go`'s
    `limitTypes`; `server/limits.go`'s `tokenWindow` and the "per minute" / "per month"
    wording in `errLimited`.

## Files likely touched

- `gateway/internal/config/{snapshot,schema,semantic}.go`.
- `gateway/internal/limits/{limits,shared,window}.go`.
- `gateway/internal/control/schema.go`.
- `gateway/internal/metrics/ops.go`.
- `gateway/internal/server/limits.go`.

## Decisions made during planning

- `config` owns the table: `LimitType` is a config concept, and every reader already
  imports `config`.
- `limits.Kind` stays (window mechanics); it is computed from `Window`.
- `metrics` reads `config.LimitTypes()` for its series; label values unchanged
  (decision 2 assigns the rest of the label lists to the OTel metrics plan).

## Removal checklist (clean at phase end)

- `git grep -nE 'countLimitTypes|countedTypes|totalsLimitTypes' gateway/` → none.
- `git grep -nE 'tokens_per_minute|tokens_per_hour|requests_per_minute|usd_per_month' gateway/internal --  ':!*_test.go'`
  → only the constants and the table in `config`.

## Acceptance criteria

- One table; a test asserts every `LimitType` constant has a row and the schema's enum
  equals `LimitTypes()`.
- No behaviour change: limits, config fixture and server limit tests pass unchanged.
- `scripts/check-gateway.sh` green, or reds named with the step that clears them.
