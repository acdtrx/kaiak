# Step 2 — kaiak-control

**Status:** not started

## Intent

`kaiak-control` validates and publishes configs with tiered prices, and its GUIDE
tells app authors how to write them.

## Files likely touched

- `control/kaiak-control/src/config/types.ts`: `Price` becomes
  `{ effective_from, tiers: PriceTier[] }`; `PriceTier` is
  `{ above_input_tokens, usd_per_million }`.
- `control/kaiak-control/src/config/semantic.ts`: the two new rules beside
  `price-dates-not-increasing`.
- The config format version and protocol version constants.
- `control/sample`: its configs and anything that builds a price.
- `control/kaiak-control/GUIDE.md` → §7 Prices: the tiers shape; the LiteLLM mapping
  gains `_above_<N>k_tokens` → a tier at N×1000 (no longer "not priced").

## Decisions made during planning

- No helper to build tiers: an app writes the document, as for every other config
  field.

## Acceptance criteria

- kaiak-control passes every shared config fixture with the codes `cases.json` names.
- Unit tests for both rules, including their paths.
- `npm test` and `npm run lint` from `control/`: kaiak-control and sample green.
  Suite recorded; expected reds: gateway (step 3), cross-half e2e (step 4).
