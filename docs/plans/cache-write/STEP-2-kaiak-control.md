# Step 2 — kaiak-control

**Status:** not started

## Intent

`kaiak-control` accepts records with the new unit, counts it toward token limits,
validates prices that name it, and its GUIDE tells app authors how to price it.

## Files likely touched

- `control/kaiak-control/src/config/types.ts`: `UsageUnit` gains
  `"tokens_cache_write"`; the price-units type accepts it; `format_version: 4`.
- `control/kaiak-control/src/usage/aggregate.ts`: a record's tokens include
  `tokens_cache_write`.
- The protocol version constant (`src/protocol/index.ts`) and tests that send the
  `kaiak-protocol` header (mismatch cases move to 3 as the old version).
- `control/kaiak-control/GUIDE.md`:
  - §7 Prices: the unit, the unpriced rule;
  - the LiteLLM paragraph: `cache_creation_input_token_cost` × 10⁶ →
    `tokens_cache_write`, and its `_above_<N>k_tokens` variant → that tier; drop it
    from the "not priced" list;
  - the header's format / protocol versions.
- `examples/config.json`, `examples/local-config.json`: format 4 (both halves' example
  tests read them); price `tokens_cache_write` where an example models an Azure
  gpt-5.6-or-later deployment.
- `control/sample/src/page/sections.ts`: a "Cache write" column in the recent-usage
  table, beside "Cached".

## Decisions made during planning

- No new semantic rule: the unit is a plain price unit, and the schema covers it.

## Acceptance criteria

- kaiak-control passes every shared fixture with the codes `cases.json` names.
- Tests: a record's token sum counts written tokens toward a `tokens_per_hour` limit;
  a config pricing the unit loads.
- `npm test` and `npm run lint` from `control/` green. Suite recorded; expected reds:
  gateway (step 3), including `TestExampleConfigs`; the cross-half e2e.

## Result

_Not started._
