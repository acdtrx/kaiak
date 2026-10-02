# Step 2 — kaiak-control

**Status:** done (2026-10-02)

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

**What changed**

- `kaiak-control/src/config/types.ts`: `UsageUnit` gains `"tokens_cache_write"`
  (`PriceUnit` follows: every unit but `tokens_reasoning`); `format_version: 4`; the
  unit comment (the three input units add up to the prompt), the tier input size and
  the unpriced rule brought current.
- `kaiak-control/src/usage/aggregate.ts`: a record's tokens are `tokens_in +
  tokens_cached + tokens_cache_write + tokens_out`.
- Protocol version 4: `PROTOCOL_VERSION`, the status message's `protocol_version`,
  and the `kaiak-protocol` headers in the kaiak-control and sample tests (the
  mismatch cases now use 3 as the old version; `protocol.test.ts`'s list moves to
  `"3", "5", "4.0", " 4", "4, 4"`). Format 4 in `limits.test.ts` and the sample's
  `config-file.test.ts`.
- Tests: `usage.test.ts`'s record builder takes five units (`tokens_in`,
  `tokens_cached`, `tokens_cache_write`, `tokens_out`, `tokens_reasoning`; every
  existing call writes 0), and *input written to the cache counts toward a token
  limit beside the other input and the output* (3 plain + 1024 read + 1009 written +
  40 out, 12 of them reasoning → carol's hour window 2076). A config pricing the unit
  loads: covered by the shared fixture `config/valid/price-cache-write.json` (step 1),
  which `config.test.ts` validates with every valid fixture — no new test.
- `GUIDE.md`: header (format 4, protocol 4, 2026-10-02); §6 the record's units; §7
  `format_version: 4`; *Tiers* — written input priced with the rest, input size
  `tokens_in + tokens_cached + tokens_cache_write`; four price units, the unpriced
  rule for both cache units, a backend without the field records 0 written; LiteLLM —
  `cache_creation_input_token_cost` × 10⁶ → `tokens_cache_write`, its
  `_above_<N>k_tokens` variant → that tier, dropped from the "not priced" list.
- `examples/config.json`, `examples/local-config.json`: format 4 only — neither
  models an Azure gpt-5.6-or-later deployment (`config.json` prices gpt-4.1 and
  gpt-4.1-mini, `local-config.json` a fake backend).
- `sample/src/page/sections.ts`: a "Cache write" column beside "Cached" in the
  recent-usage table; `page.test.ts` gives alice's fixture record 512 written so the
  column shows a value no other column does, and matches it in place.
- Grep of `control/` for other unit lists (`tokens_cached`, `Cached`, `reasoning`):
  none beyond the sites step 1 named.

**Decisions made during the step**

- The builder's tuple follows the spec's unit order (written after cached), not key
  order.
- GUIDE, LiteLLM import: `cache_creation_input_token_cost_above_1hr` (the one-hour
  cache-write rate LiteLLM lists for some models) is named in the "not priced" list —
  writes are one unit (plan, Out of scope), and the field does not end in
  `_above_<N>k_tokens`, so the tier rule does not catch it.

**Suite (expected reds)** — `scripts/check-all.sh` stops at `go test`; the live-kit
stage and the cross-half e2e were run directly.

- `control npm test`: 574 tests, 574 pass. `npm run lint`: `tsc` clean,
  `boundaries ok`.
- Gateway: gofmt, vet, staticcheck pass; `go test -race` FAIL in `cmd/kaiak` (12
  top-level tests), `internal/config` (21: step 1's 20 plus `TestExampleConfigs`, the
  examples now at format 4), `internal/control` (33); every other package and `e2e`
  pass. Every failure is the gateway reading config format 3 (`/format_version: must
  be 3`), speaking protocol 3, or refusing the five-unit records, and the boots that
  follow from it (`TestRunExitsWithNoConfigAtBoot` on goroutines left by those, as in
  step 1). Cleared by step 3. Live-test kit: gofmt, vet, staticcheck and the
  self-test pass.
- Cross-half e2e: FAIL — the sample control plane rejects the e2e's format 3 config
  (`/format_version must be equal to constant`). Cleared by step 3 or 4.
