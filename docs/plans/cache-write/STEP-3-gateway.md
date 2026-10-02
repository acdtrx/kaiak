# Step 3 — gateway

**Status:** done (2026-10-02)

## Intent

The gateway meters written tokens from the backend's usage report, prices them,
counts them toward tiers and token limits, and reports them in records, metrics and
logs.

## Files likely touched

- `gateway/internal/config/snapshot.go`: `UnitTokensCacheWrite`; the price-unit set.
- `gateway/internal/config/schema.go`: `FormatVersion` 4; the unit accepted in a
  tier's `usd_per_million`.
- `gateway/internal/accounting/meter.go`: `usageReport` reads
  `prompt_tokens_details.cache_write_tokens`; `parseUsage` maps and clamps it
  (cached ≤ prompt, then written ≤ prompt − cached; `tokens_in` is the rest);
  `tokenUnits` carries five units.
- `gateway/internal/accounting/accounting.go`: `inputSize` adds the unit; `Cost`
  prices it, unpriced → the tier's `tokens_in` (one helper for both fallbacks if
  they share the rule's purpose — CODING-RULES §2).
- `gateway/internal/limits/limits.go`: the actual-tokens sum adds the unit.
- `gateway/internal/metrics`: the usage token counter emits the fifth unit.
- The request log line (wherever it writes `tokens_cached`).
- `gateway/internal/fakebackend/fakebackend.go`: reports `cache_write_tokens` when the
  test sets it, in both body and stream usage.
- Versions: `control.ProtocolVersion` 4 (and `fakecontrol`'s header);
  `lastKnownGoodFormat` 5; `spoolFormat` 3.
- `scripts/live/config.go` (format 4), `scripts/live/checks.go` (print the unit in
  the usage check; count it in the "no input" check).
- Tests that build configs or records (moved to format 4 and five units).

## Decisions made during planning

- The meter stays provider-blind: every backend type reads the same OpenAI usage
  object; a backend that omits the field reports 0.

## Acceptance criteria

- The gateway passes every shared fixture with the codes `cases.json` names.
- Meter tests: writes present and absent; stream (usage chunk) and non-stream; cached
  + written above prompt clamped in order; embeddings ignore it.
- Cost tests: writes at their price; unpriced → the tier's `tokens_in`; written
  tokens push the input size over a tier threshold.
- Limits test: written tokens count toward a token limit's actual usage.
- Metrics test: `kaiak_usage_tokens_total{unit="tokens_cache_write"}`.
- A spool or last-known-good file of the old format is discarded with its log line.
- `scripts/check-gateway.sh` green (incl. the live kit's self-test). Suite recorded;
  expected red: cross-half e2e if its configs still need moving (step 4).

## Result

**What changed**

- `internal/config`: `UnitTokensCacheWrite` (`snapshot.go`; the unit, `Price.Tiers` and
  `PriceTier.USDPerMillion` comments name it and the unpriced rule); `schema.go`:
  `FormatVersion` 4, `priceUnits` gains the unit (a tier's `usd_per_million` accepts
  it, at least 0).
- `internal/accounting/meter.go`: `usageReport` reads
  `prompt_tokens_details.cache_write_tokens`; `parseUsage` clamps cached to
  `[0, prompt]` first, then written to `[0, prompt − cached]`, and `tokens_in` is
  the rest; `tokenUnits(in, cached, cacheWrite, out, reasoning)` — every record
  carries the five units; estimates and zero records write 0; embeddings count
  `prompt_tokens` only.
- `internal/accounting/accounting.go`: `inputSize` sums the three input units,
  saturating; `Cost` prices `tokens_cache_write`; `cacheInputPrice(tier, unit)` is
  the one fallback for both cache units (own price, else the tier's `tokens_in`).
- `internal/limits/limits.go`: `amountOf` sums `tokens_in + tokens_cached +
  tokens_cache_write + tokens_out` (saturating); the `Settle` comment names it.
- `internal/control`: the record check's unit list (`schema.go`), the `Used` comment
  (`messages.go`); `ProtocolVersion` 4, `lastKnownGoodFormat` 5, `spoolFormat` 3;
  `fakecontrol`'s protocol header 4.
- `internal/server/api.go`: the request log line gains `tokens_cache_write` (after
  `tokens_cached`). Metrics needed no code change: the usage sink ranges over the
  record's units.
- `internal/fakebackend`: `Usage.CacheWriteTokens`, reported as
  `prompt_tokens_details.cache_write_tokens` beside `cached_tokens` — one
  `usageBody` serves the body and the stream's usage chunk.
- `scripts/live`: config format 4; the usage check counts written tokens toward "no
  input tokens" and prints `cache write`.
- Tests:
  - `meter_test.go` — `TestUsageMapping` gains the Azure first call (3 / 0 / 2033),
    read and written together, written without a cached count, written above the
    prompt, cached + written above the prompt (cached clamped first, written to the
    rest), cached above the prompt (nothing written), a negative written count, and
    embeddings ignoring it; `TestStreamUsageCarriesInputWrittenToTheCache`. Absent
    writes: every existing case now expects 0 written.
  - `accounting_test.go` — `TestCostOfInputWrittenToTheCache` (own price; unpriced →
    `tokens_in`; neither cache unit priced; unpriced input → 0; priced at 0);
    `TestCostPicksTheTierByInputSize` gains written tokens pushing the input over
    272k (upper tier, write price), read + written exactly at the threshold (lower),
    an upper tier without a write price (its `tokens_in`); `TestInputSize` sums three
    units and saturates on written; `TestOneTierPricesAsTheFlatArithmetic` prices the
    unit; the record encoding names it.
  - `limits_test.go` — `TestSettlementCountsInputWrittenToTheCache` (3 + 1024 read +
    1009 written + 40 out = 2076 on per-minute and per-hour counters); the `record`
    helper takes five units.
  - `metrics_test.go` — `kaiak_usage_tokens_total{…,unit="tokens_cache_write"}`;
    server `TestOpsAndUsageMetricsMoveWithRequests` has the backend report 20 written
    and checks the series and the record.
  - server `TestInputWrittenToTheCacheIsRecordedAndLogged` (non-stream and stream,
    through the fake backend: units, cost at the input price, the log line); the
    exact-usage test's log check names `tokens_cache_write`; the drain and queue
    token sums add the unit.
  - `control` — `TestOlderFormatSpoolIsDiscardedWithItsBatches` (was
    `TestFormatOneSpoolIsDiscardedWithItsBatches`): formats 1 and 2, each an index
    and a batch of that format's record shape, discarded with their log lines
    (`found_version`), only the new record counted, and it carries
    `tokens_cache_write`; `TestLastKnownGoodOfAnotherFormatIsDiscarded` writes a
    format-4 file holding a format-3 config (`found_version=4`);
    `TestSpoolOfAnotherFormatStartsANewEpoch` follows `spoolFormat`.
  - Every test config to format 4 (`cmd/kaiak`, `auth`, `config`, `limits`, `server`,
    `e2e`), records to five units, protocol 4 (mismatch case: 3 as the old
    version). No assertion was weakened.

**Decisions made during the step**

- One helper for both unpriced fallbacks (`cacheInputPrice`): the spec states them as
  one rule with one purpose (input read from or written to the cache never priced
  below plain input), so CODING-RULES §2 favours sharing.
- `inputSize` and `amountOf` loop over a unit list instead of nesting saturating
  adds — the same arithmetic, readable with three and four terms.
- The format-1 spool test became a table over formats 1 and 2 rather than a second
  near-copy; its name changed to match.
- The gateway e2e and cross-half configs share `testConfig` (as in tiered-pricing),
  so moving it to format 4 made the cross-half e2e green here. The e2e scenarios with
  writes stay in step 4.

**Suite** — `scripts/check-all.sh` (Go test cache cleared first): all checks passed.

- Gateway (`scripts/check-gateway.sh`): gofmt, vet, staticcheck pass; `go test -race
  ./...` every package ok (510 top-level tests, 0 failing); live-test kit gofmt, vet,
  staticcheck and self-test pass (vllm, llama-server, openai, azure-openai, vllm with
  two backends; 13, 13, 13, 13 and 16 checks, all passing; the usage lines print
  `cache write 0`).
- `control npm test`: 574 tests, 574 pass. `npm run lint`: `tsc` clean,
  `boundaries ok`.
- Cross-half e2e: ok.
- No expected reds remain; step 4 owns the e2e scenarios with writes, the operator
  docs and the repo-wide grep.
