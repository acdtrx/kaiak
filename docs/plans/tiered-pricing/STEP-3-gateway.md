# Step 3 — gateway

**Status:** not started

## Intent

The gateway loads tiered prices and prices each record at the tier its input size
selects.

## Files likely touched

- `gateway/internal/config/document.go`, `snapshot.go`: `Price` holds its tiers.
- `gateway/internal/config/schema.go`: the tier shape and bounds, mirroring the JSON
  Schema.
- `gateway/internal/config/semantic.go`: the two new rules.
- `gateway/internal/accounting/accounting.go`: `Cost` picks the tier from
  `tokens_in + tokens_cached`, then prices as now. `PriceAt` (and so "priced") is
  unchanged.
- The protocol version constant; the last-known-good config file's format version in
  `gateway/internal/state` (or wherever it lives).
- Tests that build configs with prices (`limits_test.go`, `server_test.go`,
  `stateless_test.go`, …).

## Decisions made during planning

- Tier selection is one small function beside `PriceAt`, tested on its own.
- The sum stays in floating point and rounds once to nano-dollars, as settled
  2026-09-24.

## Acceptance criteria

- The gateway passes every shared config fixture with the codes `cases.json` names.
- `Cost` tests: input 0; exactly at a threshold (lower tier); one token above (upper
  tier); cached input counted toward the size; a one-tier entry equal to the old
  arithmetic; Qwen-style three tiers.
- A last-known-good config with the old format version is discarded with its log
  line.
- `scripts/check-gateway.sh` green. Suite recorded; expected red: cross-half e2e
  (step 4) if the sample configs are not yet moved.
