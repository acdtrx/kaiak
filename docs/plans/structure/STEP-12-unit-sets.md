# Step 12 — named usage-unit sets

**Status:** not started

## Intent

The four usage-unit groupings are named once beside `config.Unit`, and the record's
units are built by name, not by position. Today each set is a literal list or sum where
it is used (7 gateway places), and the five-`int64` `tokenUnits` constructor compiles
when `cached` and `cacheWrite` are swapped.

## Findings

- T2, observability F1:
  - in `config`: `TokenUnits` (record order), `PricedUnits`, `InputUnits` (sum = the
    backend's prompt; picks the tier; is `gen_ai.usage.input_tokens`), `CountedUnits`
    (what token limits and totals count), and the price fallback as data
    (`PriceFallback = map[Unit]Unit{Cached: In, CacheWrite: In}`);
  - `accounting.Cost` loops over `PricedUnits` in today's summation order (identical
    rounding); `inputSize`, `limits.amountOf`, the request log line,
    `config/schema.go`'s price units and `control/schema.go`'s token units read the sets;
  - `tokenUnits(...)` goes: readers build `Units{config.UnitTokensIn: …}` (or a
    named-field helper).
- control-main hint: `control/schema.go`'s hand list of token units reads `TokenUnits`.
- observability hint: saturating addition written three times (`accounting.inputSize`,
  `limits/window.go`, `server/inbound.go`) — one helper where the sums now loop.

## Files likely touched

- `gateway/internal/config/{snapshot,schema}.go`.
- `gateway/internal/accounting/{accounting,meter,*_usage}.go` and tests.
- `gateway/internal/limits/limits.go`, `server/api.go` (or `requestlog.go` after step 10),
  `control/schema.go`.

## Decisions made during planning

- **Priced and recorded stay distinct sets** (the independent review's warning):
  `tokens_reasoning` is recorded but not priced on its own, so no code iterates "all
  units" where it means "priced units".
- `CountedUnits` lives in `config` beside the others: it is also the control plane's
  totals unit (step 13 pins it across halves).

## Removal checklist (clean at phase end)

- `git grep -n 'tokenUnits(' gateway/` → none.

## Acceptance criteria

- Cost, limits and the log line give identical numbers (accounting, limits, server and
  e2e tests unchanged); a test that each set is a subset of `TokenUnits` and that every
  priced unit has a fallback or a price field.
- `scripts/check-gateway.sh` green, or reds named with the step that clears them.
