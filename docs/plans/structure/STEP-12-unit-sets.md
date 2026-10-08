# Step 12 — named usage-unit sets

**Status:** done (2026-10-08)

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

## Result

**What changed**

- `config/snapshot.go`, next to `Unit`: the named sets, each a slice (order matters
  where it is read), documented read-only.
  - `TokenUnits` (record order: in, cached, cache_write, out, reasoning),
    `PricedUnits` (in, cached, cache_write, out: the order `Cost` sums them),
    `InputUnits` (in, cached, cache_write), `CountedUnits` (in, cache_write, out).
  - `PriceFallback = map[Unit]Unit{UnitTokensCached: UnitTokensIn,
    UnitTokensCacheWrite: UnitTokensIn}`; a priced unit with neither a price nor a
    fallback costs 0 (tokens_in, tokens_out), as `cacheInputPrice` and `Cost` did.
- `accounting`:
  - `Cost` loops over `config.PricedUnits` in the old summation order
    (in, cached, cache_write, out), each term `float64(units[u]) * unitPrice(tier, u)`,
    then `× 1000`, then the same saturation and `math.Round`. The tier is picked by
    `units.Sum(config.InputUnits)`.
  - `cacheInputPrice` → `unitPrice`: own price, else the `PriceFallback` unit's price,
    else 0 (not recursive, as before).
  - `inputSize` is gone: `Units.Sum(set []config.Unit)` (saturating) replaces it.
  - `SaturatingAdd(a, b)` (exported, `meter.go` beside `Units`): the one saturating
    addition.
  - `tokenUnits(in, cached, cacheWrite, out, reasoning)` is gone. Readers build
    `Units{config.UnitTokensIn: …}` literals and pass them through
    `withEveryTokenUnit`, which adds every `config.TokenUnits` unit left out at 0
    (7 production sites: `meter.go` ×3, `openai_usage.go` ×2, `responses_usage.go`,
    `messages_usage.go`). The `Units` comment says "every token unit
    (config.TokenUnits)" instead of "all five".
- `limits`: `amountOf` is `rec.Units.Sum(config.CountedUnits)`; its reasoning (cache
  reads left out, reasoning inside tokens_out) moved to the `CountedUnits` comment.
  `window.go`'s `saturatingAdd` is gone, its 6 uses call `accounting.SaturatingAdd`;
  its "a wrapped count would read as room under every limit" reasoning moved to
  `add`'s comment.
- `server`: `inbound.go`'s `saturatingAdd` is gone (`checkLimits` calls
  `accounting.SaturatingAdd`). The request log's `gen_ai.usage.input_tokens` is
  `units.Sum(config.InputUnits)`.
- `config/schema.go`: `priceUnits` is gone; `usdPerMillion` reads `PricedUnits`.
  `control/schema.go`: `tokenUnits` is gone; `usageRecord` reads `config.TokenUnits`.
- Tests:
  - `config/schema_test.go`: `TestUnitSets` — `TokenUnits` is exactly the package's
    `Unit` constants (go/parser, as step 11's row test); every other set is a subset
    without duplicates; `PricedUnits`, `InputUnits`, `CountedUnits` and
    `PriceFallback` equal what the spec says (reasoning not priced; input size; what
    token limits and totals count — CONTROL-PROTOCOL.md Totals → `used`, GATEWAY.md
    Limits → Settle; the fallback rule); every fallback is a priced unit to a priced
    unit that does not fall back itself. `TestPricedUnitsAreTheSchemaPriceUnits` —
    every priced unit, and only those, is a `usd_per_million` field of
    `config.schema.json`. The go/parser walk of `TestEveryLimitTypeHasARow` moved into
    a `constants(t, typeName)` helper both use; its assertions are unchanged.
  - `control/fixtures_test.go`: `TestRecordUnitsAreTheTokenUnits` —
    `usage-record.schema.json`'s `units.required` is `config.TokenUnits` in order, and
    its unit fields are the same set.
  - Checked by mutation, then reverted: dropping `tokens_reasoning` from
    `TokenUnits`, adding `tokens_cached` to `CountedUnits` and adding a
    `tokens_out → tokens_reasoning` fallback failed `TestUnitSets` and
    `TestRecordUnitsAreTheTokenUnits` with messages naming each.
  - Existing tests: `tokenUnits(…)` calls became `Units{…}` literals with the same
    numbers (zeros left out). Where the result is compared with `maps.Equal`
    (`expect`, the usage-mapping tables in `meter_test.go`,
    `messages_usage_test.go`, `responses_usage_test.go`) the literal goes through
    `withEveryTokenUnit`, so they still assert every token unit is present. Cost
    inputs are plain literals (a missing unit reads 0). `TestInputSize` keeps its
    name and assertions; its subject is `Units.Sum(config.InputUnits)`. No
    assertion changed.

**Decisions made during the step**

- **A fill helper, not a named-field struct.** `withEveryTokenUnit(Units{…})` reads
  `config.TokenUnits`, so a new token unit needs no edit there; a five-field struct
  would be one more hand list of the units. The keyed literal makes a swapped
  cached/cache-write visible at the call site.
- **`SaturatingAdd` and `Units.Sum` live in `accounting`.** It owns amounts
  (`Units`, `MaxAmount`); `limits` and `server` already import it. `config` is the
  wrong home for arithmetic, and a new package for a five-line function is not
  worth it.
- **All three saturating additions were the same operation** (a + b for
  non-negative a and b, capped at `math.MaxInt64`; `limits` documented "non-negative
  b" but its counts are non-negative too — `clampNegative` guards that). All three are
  now the one helper. `server.saturatingMul` stays: one definition, one file.
- **Sets are exported vars, not functions** like `LimitTypes()` (which derives from
  the table): `Cost` reads `PricedUnits` per record, and a copy per call buys nothing
  when the comment says read-only.
- **The request log's per-record merge loop stays** (`for unit, n := range
  rec.Units`): it means "every recorded unit", not "priced". Likewise
  `clampToProtocol` and `metrics/usage.go`'s tokens-by-unit loop: recorded meaning,
  and metrics are out of scope.
- **Log line value:** `input_tokens` was plain `+`, now saturating. Identical for
  every reachable value: each record's units are at most 2^53 − 1 after
  `clampToProtocol`, and a request has at most `MaxAttemptsCeiling` (10) records.
- **Identical cost rounding, checked beyond the tests.** A throwaway test (deleted)
  compared the old `Cost` body with the new one on 3,000,000 random records (random
  units up to 2^53, random one- and two-tier prices with units left out) on arm64
  and on amd64 (`GOARCH=amd64` under Rosetta): 0 differences on both. Sensitivity
  check: a variant that rounds each product separately (`float64(x*y)`) differed
  from the old code in 45,785 of the 3M cases on arm64.
- **Finding, not fixed (behaviour change, out of scope):** that sensitivity check
  shows the Go compiler fuses `x*y + z` into FMA on arm64 but not on amd64
  (GOAMD64=v1), so `Cost` can differ in the last bit of the float sum between an
  arm64 and an amd64 build — 1 nano-USD at a rounding boundary for realistic
  amounts. Both the old and new code have it. Making cost architecture-independent
  would mean an explicit `float64(...)` per product, which changes arm64 results;
  worth a backlog entry if cross-arch agreement with `kaiak-control`'s re-pricing
  matters.

**Report vs code** (034329e; code at 101a2a7)

- `tokenUnits` at `accounting/meter.go:15`, 7 production sites as reported;
  `inputSize` at `accounting.go:205`; `Cost`'s sum at `:237-240`; `cacheInputPrice`
  at `:251`.
- `amountOf`'s list at `limits/limits.go:608` (report 635).
- The log line is in `server/requestlog.go:106-108` (step 10 moved it out of
  `api.go:244-250`).
- `priceUnits` at `config/schema.go:103-105` (report 109-111); `control/schema.go`'s
  `tokenUnits` at `:30-32` (report 33-35).
- `saturatingAdd` at `limits/window.go:202` (report 191) and `server/inbound.go:309`
  (report 306/314: 306 is `saturatingMul`).

**Tests** (`go test -count=1 -v`, `--- PASS`, before → after)

| Package | before | after |
|---|---|---|
| `internal/config` | 206 | 208 (+`TestUnitSets`, +`TestPricedUnitsAreTheSchemaPriceUnits`) |
| `internal/accounting` | 122 | 122 |
| `internal/limits` | 80 | 80 |
| `internal/control` | 238 | 239 (+`TestRecordUnitsAreTheTokenUnits`) |
| `internal/server` | 456 | 456 |

- The passing-test name lists differ only by the three added tests. None failed.

**Removal checklist** (step 12)

- `git grep -n 'tokenUnits(' gateway/` → none. Also `inputSize`, `cacheInputPrice`,
  `priceUnits`, `saturatingAdd` → none in `gateway/` or `scripts/`.
- The `UnitTokens*` constants left outside `config/snapshot.go` are per-unit reads
  (one unit's amount or its mapping from a backend field), none a set.

**Suite**: `scripts/check-gateway.sh` passed (exit 0) on the final code; only this step
file was edited after it started. The control half (`npm test`, cross-half e2e) was not
run: this step touches no `control/` code or shared fixture (it only reads
`protocol/schema/` in two new Go tests).

```
==> gofmt / go vet / staticcheck 2026.2.1 (gateway)
==> go test -race -count=1 (gateway): ok cmd/kaiak, e2e (106s), accounting, auth, clip,
    config, control, limits, logattr, metrics, netfail, otlplog, provider, routing,
    schemacheck, server, sse
==> live-test kit: gofmt / vet / staticcheck; self-test passed for vllm, llama-server,
    openai, azure-openai, anthropic, azure-anthropic, vllm with two backends
gateway checks passed
```

