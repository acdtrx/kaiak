# Step 1 — contract

**Status:** not started

## Intent

Settle the `tokens_cache_write` unit in the contract both halves test against: what
it counts, how it is priced, how it counts toward tiers and limits, and the version
bumps. Every later step implements what this step writes.

## Files likely touched

- `docs/specs/CONTROL-PROTOCOL.md`:
  - Config → Units and price units: the fifth unit, its source, `tokens_in` now
    minus both cached and written; `usd_per_million` accepts it; unpriced →
    the tier's `tokens_in` (settled 2026-10-02);
  - Tiered prices: input size = `tokens_in + tokens_cached + tokens_cache_write`;
    the "records do not change" note no longer holds as written — reword;
  - Usage records: five token units, zeros included; the example record;
  - Totals and Usage intake: the token sum gains the unit;
  - protocol version 4, config `format_version` 4.
- `docs/specs/GATEWAY.md`:
  - Accounting: the meter mapping (`cache_write_tokens` → `tokens_cache_write`, the
    clamping order), Cost's tier input size, Usage units list;
  - Limits: the actual-tokens sum;
  - Metrics: `kaiak_usage_tokens_total` covers five units, the cardinality formula
    (6 → 7 per record label set);
  - the request log line's fields;
  - Usage spool format 3, `last-known-good.json` format 5.
- `protocol/schema/usage-record.schema.json`: `tokens_cache_write` required in `units`.
- `protocol/schema/config.schema.json`: `format_version` 4; `tokens_cache_write` in a
  tier's `usd_per_million`; descriptions of prices, tiers and the unpriced rule.
- `protocol/schema/totals.schema.json`: the `used` description's token sum.
- `protocol/schema/status.schema.json` and other carriers of `protocol_version`: 4.
- Then `npm run sync-schemas` from `control/`.
- `protocol/fixtures/**`, by a one-off script outside the repo:
  - every usage record gains `"tokens_cache_write": 0`;
  - config `format_version` 3 → 4; `protocol_version` 3 → 4;
  - the old-version cases renamed (`format-version-2` → `format-version-3`,
    `protocol-version-2` → `protocol-version-3`), as in tiered-pricing.
- New fixtures:
  - valid config: a tier pricing `tokens_cache_write` (in `price-tiers.json` or a new
    `price-cache-write.json`);
  - valid usage record with nonzero writes;
  - invalid usage record: `units-missing-cache-write`; invalid config: a negative
    `tokens_cache_write` price if `price-negative` doesn't already cover any unit.

## Decisions made during planning

- Before editing, grep both halves for every sum or list of token units
  (`tokens_cached` beside `tokens_in` / `tokens_out`) and list each site in this file's
  Result: steps 2 and 3 own them. Known now: gateway `limits.go` (actual tokens),
  `accounting.go` (`inputSize`, `Cost`), `meter.go`; kaiak-control `aggregate.ts`,
  `types.ts`; sample `page/sections.ts`; live kit `checks.go`.
- `units-unknown` keeps its meaning: a unit outside the five is refused.

## Acceptance criteria

- The specs state the unit, mapping, clamping, pricing, input size, limit sum,
  metrics, log line and versions, each with its settled date.
- Schema copy in sync (`npm test`'s byte-for-byte check passes).
- The fixture diff is exactly the scripted transform plus the new fixtures (checked by
  a second scratch script against `HEAD`).
- Suite run and recorded. Expected reds: kaiak-control fixture and version tests
  (step 2), gateway fixture and version tests (step 3), the cross-half e2e (step 3
  or 4).

## Result

_Not started._
