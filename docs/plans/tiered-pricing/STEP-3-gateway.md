# Step 3 — gateway

**Status:** done (2026-09-29)

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
- `scripts/live/config.go`: the live-test kit builds configs in Go, and
  `check-gateway.sh` self-tests it (moved here from step 4).
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

## Result

**What changed**

- `internal/config`: `Price` is `{ EffectiveFrom, Tiers []PriceTier }`; `PriceTier` is
  `{ AboveInputTokens int64, USDPerMillion map[Unit]float64 }` (`snapshot.go`, and
  `priceDoc` / `priceTierDoc` in `document.go`). `schema.go`: `FormatVersion` 3; a price
  entry is `{ effective_from, tiers }`, 1–8 tiers (`maxPriceTiers`), each
  `{ above_input_tokens: integer 0..2^53 − 1, usd_per_million }` with the unit rules
  unchanged; an entry-level `usd_per_million` is an unknown field. `semantic.go`:
  `price-tier-first-not-zero` and `price-tiers-not-increasing` (codes in `errors.go`) at
  `/models/<m>/prices/<i>/tiers/<j>/above_input_tokens`, checked for every entry before
  its date rules, as kaiak-control does.
- `internal/accounting`: `tierFor(price, input)` beside `PriceAt` (the last tier whose
  threshold is strictly below the input, else the first) and `inputSize(units)`
  (`tokens_in + tokens_cached`, saturating at the int64 bound); `Cost` prices the whole
  record at that tier, otherwise exactly as before. `PriceAt`, and so "priced", is
  unchanged.
- Versions: `control.ProtocolVersion` 3 (and the test control plane's
  `fakecontrol` header); `last-known-good.json` format version 4.
- `scripts/live/config.go`: format 3, prices as one tier at 0.
- Tests: `accounting_test.go` — `TestTierFor` (0, 1, exactly at 32000 and 128000 →
  the tier below, one above → the tier, MaxInt64; a one-tier entry at every size),
  `TestInputSize` (sum; no overflow on huge units), `TestCostPicksTheTierByInputSize`
  (input 0; exactly at 272000 → lower; 272001 → upper, whole record; cached input
  counted toward the size; upper tier without `tokens_cached` → that tier's
  `tokens_in`; Qwen-style three brackets), `TestEstimatedRecordPicksTheTierByItsEstimatedInput`
  (through `Recorder.Settle`), `TestOneTierPricesAsTheFlatArithmetic`.
  `snapshot_test.go` — `TestPriceTiersResolve` (the new valid fixtures, 2^53 − 1
  exact, a tier with one unit), `TestPriceTierIssues` (both rules, their paths, several
  in one document and across entries). The shared fixtures run as before
  (`TestValidFixtures`, `TestInvalidFixtures`, `TestExampleConfigs`).
  `TestLastKnownGoodOfAnotherFormatIsDiscarded` now writes a format-3 file holding a
  format-2 config with the old price shape and checks the log names
  `found_version=3`. Every other test config moved to format 3 and the tier shape
  (`cmd/kaiak`, `auth`, `config`, `limits`, `server`, `e2e`), and the protocol
  tests to 3 (mismatch cases now use 2). No assertion was weakened.

**Decisions made during the step**

- `tierFor` and `inputSize` are unexported: only `Cost` uses them.
- `inputSize` saturates rather than trusting the meter: parsed usage makes
  `tokens_in + tokens_cached` the backend's `prompt_tokens`, but units are clamped to
  2^53 − 1 only after pricing.
- The e2e helper `testConfig` in `gateway/e2e/e2e_test.go` is also the base of the
  cross-half config (`crossHalfConfig`), so migrating it (needed for the gateway e2e to
  load) moved the cross-half e2e config too: step 4's "cross-half e2e configs" item is
  done, and the cross-half e2e is already green. The two-tier e2e scenario stays in
  step 4.
- `fakecontrol`'s package comment named protocol version 1; the version is dropped
  from the comment (the constant holds it).

**Suite** — `scripts/check-all.sh`: all checks passed.

- Gateway (`scripts/check-gateway.sh`): gofmt, vet, staticcheck pass; `go test -race
  ./...` every package ok (481 top-level tests, 0 failing); live-test kit vet,
  staticcheck and self-test pass (vllm, openai, azure-openai, vllm with two backends).
- `control npm test`: 558 tests, 558 pass. `npm run lint`: `tsc` clean,
  `boundaries ok`.
- Cross-half e2e: ok.
- No expected reds remain; step 4 still owns the two-tier e2e, the docs and the
  repo-wide grep for the old shape.
