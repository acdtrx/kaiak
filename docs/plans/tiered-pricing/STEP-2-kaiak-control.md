# Step 2 — kaiak-control

**Status:** done (2026-09-29)

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
- `examples/config.json`, `examples/local-config.json`: moved to format 3 here, not
  in step 4 — kaiak-control's example-configs test and the sample's keygen test read
  them (found in step 1). The gateway's `TestExampleConfigs` reads them too; it stays
  red with the rest of the gateway until step 3.
- `control/kaiak-control/GUIDE.md` → §7 Prices: the tiers shape; the LiteLLM mapping
  gains `_above_<N>k_tokens` → a tier at N×1000 (no longer "not priced").

## Decisions made during planning

- No helper to build tiers: an app writes the document, as for every other config
  field.

## Acceptance criteria

- kaiak-control passes every shared config fixture with the codes `cases.json` names.
- Unit tests for both rules, including their paths.
- `npm test` and `npm run lint` from `control/`: kaiak-control and sample green.
  Suite recorded; expected reds: gateway, including `TestExampleConfigs` (step 3);
  cross-half e2e (step 4) unless it already passes once both halves read format 3.

## Result

**What changed**

- `kaiak-control/src/config/types.ts`: `Price` is `{ effective_from, tiers }`;
  `PriceTier` is `{ above_input_tokens, usd_per_million }`; `format_version: 3`.
- `kaiak-control/src/config/semantic.ts`: `price-tier-first-not-zero` and
  `price-tiers-not-increasing`, at `/models/<m>/prices/<i>/tiers/<j>/above_input_tokens`,
  checked for every entry before its date rules.
- Protocol version 3: `PROTOCOL_VERSION`, the status message's `protocol_version`, and
  the `kaiak-protocol` headers in the kaiak-control and sample tests (the mismatch cases
  now use 2 as the old version).
- Tests: `config.test.ts` → *price tier rules* (tiers that pass; first not zero in a
  later entry; equal and decreasing thresholds, each reported at its tier; tier and
  date rules together, with their order and paths). Format 3 in `limits.test.ts` and
  the sample's `config-file.test.ts`. The sample builds no prices of its own: its
  configs come from the shared fixtures and `examples/`.
- `examples/config.json`, `examples/local-config.json`: format 3, each price entry
  wrapped as one tier at 0 (neither prices a model with long-context rates).
- `GUIDE.md` §7: format 3; a *Tiers* bullet (bounds, which tier applies, input size,
  one tier for most models, tiers complete in themselves); the cached-input rule per
  tier; the LiteLLM paragraph maps `_above_<N>k_tokens` fields to the tier at N × 1000.

**Decisions made during the step**

- The tier rules run for an entry whose date is invalid too: the date check returns
  early, so the tier check comes first.
- GUIDE, LiteLLM import: a unit the list gives no long-context rate for is copied from
  the tier at 0 into the upper tier, since tiers inherit nothing (a missing
  `tokens_out` would make long-context output free).

**Suite (expected reds)** — `scripts/check-all.sh` stops at `go test`; control was run
directly.

- `control npm test`: 558 tests, 558 pass. `npm run lint`: `tsc` clean,
  `boundaries ok`.
- Gateway: gofmt, vet, staticcheck pass; `go test -race` FAIL in `cmd/kaiak`,
  `internal/config` (now including `TestExampleConfigs`), `internal/control` — 64
  top-level tests, the gateway still reading format 2 and speaking protocol 2. Cleared
  by step 3.
- Cross-half e2e: not reached (check-all stops at the gateway); expected red until
  the gateway reads format 3 (step 3) and the e2e config moves (step 4).

