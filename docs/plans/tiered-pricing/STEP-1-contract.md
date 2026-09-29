# Step 1 — contract

**Status:** not started

## Intent

Settle the tiered price shape in the contract that both halves test against. Every
later step implements what this step writes.

## Files likely touched

- `docs/specs/CONTROL-PROTOCOL.md`:
  - Config → Prices: the tiers shape, tier selection, input size, bounds (settled
    2026-09-29, with what was rejected);
  - Units and price units: the rules now apply per tier;
  - Semantic rules: `price-tier-first-not-zero`, `price-tiers-not-increasing`;
  - config `format_version` 3 and protocol version 3.
- `docs/specs/GATEWAY.md`:
  - Accounting → Cost: pick the tier from the input size, then price as before;
  - the data directory's last-known-good config format version.
- `protocol/schema/config.schema.json`: price entry `{ effective_from, tiers }`; tier
  `{ above_input_tokens, usd_per_million }`; 1–8 tiers; integer thresholds from 0 to
  2^53 − 1; `format_version` 3. Then `npm run sync-schemas` from `control/`.
- Other schemas and fixtures that carry the protocol version (status
  `protocol_version`, …).
- `protocol/fixtures/config/**`: every fixture moved to the new shape (one-off script,
  not committed); new valid fixtures (`price-tiers.json` with 272k, `price-brackets.json`
  with three tiers); new invalid fixtures with `cases.json` entries (tiers empty,
  first not 0, not increasing, equal thresholds, 9 tiers, fractional and negative
  thresholds, entry-level `usd_per_million`).

## Decisions made during planning

- Semantic rule paths point at the offending tier's `above_input_tokens`
  (`/models/<m>/prices/<i>/tiers/<j>/above_input_tokens`).
- `price-reasoning-unit`, `price-unknown-unit`, `price-negative`, `price-empty` keep
  their meaning, now inside a tier; their fixtures move with the shape.

## Acceptance criteria

- The specs describe the shape, selection rule, bounds, rule codes and versions, with
  settled dates.
- Schema copy in sync (`npm test`'s byte-for-byte check passes).
- The fixture diff is the plain wrapping plus the new fixtures, nothing else.
- Suite run and recorded. Expected reds: gateway and kaiak-control fixture and
  version tests (cleared by steps 3 and 2), the cross-half e2e (step 4).
