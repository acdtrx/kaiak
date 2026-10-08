# Step 13 — counted units and window starts, both halves

**Status:** done (2026-10-08)

## Intent

What counts toward a limit is pinned across the two halves by a shared fixture, and the
control plane's store holds no limit-type knowledge. Today the counted-tokens rule is
written in Go and TS with no shared case (it changed by hand on both sides on
2026-10-05), every store hard-codes tokens_per_hour→hour and usd_per_month→month, and
the window identity is written five times.

## Findings

- T2: `protocol/fixtures/usage/` — cases of a usage record → counted amount per limit
  type (tokens, nano-USD), read by a Go test over `limits`' amount (and step 12's
  `CountedUnits`) and a TS test over `usage/aggregate.ts`.
- control-core F3: `type WindowStarts = Record<TotalsLimitType, number>` replaces
  `CurrentWindows {hourStart, monthStart}`; stores filter
  `total.windowStart === current[total.type]` and drop `< oldest[total.type]`;
  `windowStartFor` becomes indexing; `currentWindows`/`previousWindows` return the
  record; store-contract constants follow; the GUIDE's SQL sketch joins on
  `(type, window_start)`.
- control-core F7 (window keys), edges hint: `windowKeyOf` lives in `storage`
  (`aggregate.ts` imports it); one exported window identity from `messages`
  (`scopeTypeKey` or similar) for `messages/semantic.ts`, `fastify/totals-feed.ts` and
  the sample page.
- The sample page's counted-type `if` reads the library's counted set (`COUNTED_TYPES`
  or step 11's TS twin), so a new counted type does not touch the page.

## Files likely touched

- New `protocol/fixtures/usage/`; a Go test in `gateway/internal/limits`; a TS test in
  `kaiak-control/src/usage/`.
- `kaiak-control/src/{storage,usage,messages,fastify,store-contract}/`;
  `control/sample/src/page/sections.ts`.
- `control/kaiak-control/GUIDE.md` §5 (the store table and SQL; step 26 trims around it).
- Where `protocol/fixtures/` layout is described (`CONTROL-PROTOCOL.md` or
  `TECH-STACK.md`).

## Decisions made during planning

- The public store interface changes (decision 11); no external store exists yet.
- The fixture's numbers come from the spec's counting rule (`CONTROL-PROTOCOL.md`,
  Units), not from either implementation's output.

## Removal checklist (clean at phase end)

- `git grep -nE 'hourStart|monthStart|CurrentWindows' control/` → none.
- `git grep -n 'JSON.stringify(\[' control/` → only the one exported identity (and
  `windowKeyOf`).

## Acceptance criteria

- The fixture covers each counted type with plain, cached, cache-write, output and
  reasoning units present; both halves pass it; altering the rule in one half fails it
  (checked once by hand, noted in the Result).
- Store-contract tests pass on the memory store and the negative controls still fail as
  named.
- `scripts/check-all.sh` green, or reds named with the step that clears them.

## Result

**What changed**

- Counted-units fixture (T2):
  - `protocol/fixtures/usage/` (new), one file per case:
    `{ reason, units, cost_nano_usd, expected: { <limit type>: <decimal string> } }`.
    `expected` names every token and cost limit type: `tokens_per_minute`,
    `tokens_per_hour`, `usd_per_month`. `requests_per_minute` is left out: it counts
    the request, not the record.
  - Four cases, numbers from the spec's rule (CONTROL-PROTOCOL.md Counted toward,
    GATEWAY.md Limits → Settle), not from either implementation:
    - `every-unit.json`: in 1, cached 20, cache_write 300, out 4000, reasoning 500.
      Each unit has its own digit, so a wrongly counted unit shows in the sum.
      Tokens 4301, cost 678901234.
    - `cache-reads-only.json`: tokens 0, cost 1229.
    - `unpriced.json`: tokens 1750, cost 0.
    - `largest-record.json`: every unit and the cost at 2^53 − 1. Tokens
      27021597764222973 (past 2^53, exact).
  - Go: `limits/fixtures_test.go` `TestCountedUnitsFixtures`. It decodes each file
    strictly and checks:
    - `units` holds exactly `config.TokenUnits`;
    - `expected` names exactly the `config.LimitTypes()` whose `Measure()` is not
      requests;
    - per type, the real `amountOf(typ.Measure(), rec)` gives the expected amount.
  - TS: `usage/aggregate.test.ts`, "a record counts toward each counted type what the
    gateway's limits count (protocol/fixtures/usage)". It builds the record with
    test-support's `usageRecord`, which must pass `validateUsageRecord`. Then, per
    `COUNTED_TYPES`, the global addition from the real `batchAdditions` (in the
    record's own current window; absent = 0) equals the expected amount.
  - One-line comments on `amountOf` (Go) and `amountFor` (TS) name the fixture.
- Window starts keyed by limit type (control-core F3):
  - `storage/types.ts`: `WindowStarts = Record<TotalsLimitType, number>` replaces
    `CurrentWindows`. `totalsSnapshot(current)` and `dropPastWindowTotals(oldest)`
    take it, and their comments say `current[type]` / `oldest[type]`.
  - Memory store: `isCurrent` is `total.windowStart === current[total.type]`, and the
    drop is `total.windowStart < oldest[total.type]`.
  - `usage/windows.ts`:
    - `currentWindows` and `previousWindows` return the record;
    - `windowStartFor` is gone (indexing replaces it);
    - new `sameWindows(a, b)` (over `COUNTED_TYPES`) and `formatWindowStarts`
      (the `TotalsRead.windowStarts` record).
    - `recordWindowStart` indexes.
  - `usage/index.ts`:
    - `readTotals` re-reads when `!sameWindows(after, windows)`;
    - `listedWindows` indexes;
    - `dropPastWindows` keeps the windows it last dropped in (`prunedIn`) in place
      of the hour start. The behaviour is the same: a month boundary is always an
      hour boundary.
  - `store-contract`: `CURRENT`/`PREVIOUS` are `WindowStarts`, and `PREVIOUS.hourStart`
    is `PREVIOUS.tokens_per_hour`. `usage.test.ts`: `storedWindows` takes
    `WindowStarts`, and its 5 literals use the type keys (same numbers).
  - GUIDE.md §5:
    - the `totalsSnapshot` row says "each type's window starting at `current[type]`";
    - the `dropPastWindowTotals` row says "Windows starting before `oldest[type]`";
    - the SQL sketch joins on the pairs:
      `join unnest($types::text[], $starts::bigint[]) as current (type, window_start) using (type, window_start)`.
- One window identity (control-core F7, edges hint):
  - `storage/window-key.ts` (new) holds `windowKeyOf`, exported from `storage`.
    `aggregate.ts` imports it, and `memory.ts`'s copy (`windowKey`) is gone.
  - `messages/totals.ts` (new) holds:
    - `COUNTED_TYPES` (moved from `aggregate.ts`);
    - `isCountedType(type: LimitType): type is TotalsLimitType`;
    - `scopeTypeKey(group, type)`.

    All three are exported from `messages`; `isCountedType` and `scopeTypeKey` are
    also exported from the package entry.
  - Users: `semantic.ts`'s `checkTotals`; `totals-feed.ts` (its exported
    `windowIdentity` is gone); `gateway-stream.ts`; the sample page (its `limitKey`
    is gone); the GUIDE §9 snippet.
- Sample page (edges F3's counted-type `if`): `limitRow` tests
  `!isCountedType(limit.type)` and only then looks up the used window by
  `scopeTypeKey(group, limit.type)`. It takes `group` and the used map, because the
  identity takes a counted type. The rendered output is unchanged.
- `messages.test.ts`: "COUNTED_TYPES is the totals schema's window type enum". This
  is the TS twin of step 11's `TestTotalsWindowTypesAreTheCountedLimitTypes`, so the
  library's counted set is pinned to the protocol.
- Docs:
  - `CONTROL-PROTOCOL.md`, Counted toward: a "Counted-units fixtures" sub-bullet
    (settled 2026-10-08) with the file format and what each half checks;
  - `TECH-STACK.md` and `ARCHITECTURE.md`: `protocol/` lists the fixture.

**Decisions made during the step**

- **`tokens_per_minute` is in the fixture.** The gateway's limiter counts it with the
  same `amountOf` (the measure is tokens), and GATEWAY.md Settle states one rule for
  every token limit. The TS test reads only the counted types, and the Go test reads
  all three.
- **Through `batchAdditions`, not a newly exported `amountFor`.** The TS test calls the
  real public function of `aggregate.ts` and reads global's addition, so nothing is
  exported only for a test. A zero amount gives no addition, which the test reads as 0.
- **Amounts in `expected` are decimal strings** (as `used` on the wire), so the
  past-2^53 case is exact in both parsers. Record fields stay JSON numbers, as in a
  usage record.
- **One fixture directory without `cases.json`.** Each file carries its own `reason`,
  as `config/resolved/` does. The format is documented in CONTROL-PROTOCOL.md beside
  the Resolution fixtures' precedent, not in a README.
- **The identity takes a counted type** (`scopeTypeKey(group, type: TotalsLimitType)`).
  That is why the page checks `isCountedType` before the lookup. A per-minute limit has
  no totals window to look up.
- **`COUNTED_TYPES` lives in `messages`, beside `TotalsLimitType`.** It is the totals
  schema's window enum, now pinned by a test, and `usage` imports it. The package entry
  exports `isCountedType` (what the page needs to narrow) and not the list. YAGNI: no
  host reads the list.
- **`formatWindowStarts` is written per type, not built from `COUNTED_TYPES`.**
  `Record<TotalsLimitType, …>` makes the compiler require every key, with no cast.
  `usage/windows.ts` is the one place that knows each type's window (current,
  previous, format).
- **`windowKeyOf` has its own file in `storage`** (`window-key.ts`). It is a function
  over `WindowKey`, and `types.ts` holds only types.

**Report vs code** (034329e line numbers; code at 335bfb1)

- control-core F3: as reported. `CurrentWindows` was at `storage/types.ts:53-56`,
  `isCurrent` at `memory.ts:44-45`, the drop at `:109-113`, `windowStartFor` at
  `windows.ts:19-21`, and the constants at `store-contract/index.ts:56-57`.
  `usage/index.ts`'s uses were at 138-164 and 231-234.
- F7 window keys: `memory.ts`'s copy was at `:187-189`; `semantic.ts:42` and
  `totals-feed.ts:58-60` as reported.
  - The sample page's copy was at `sections.ts:250-252`, not 257-259 (step 8
    shortened the section).
  - The fifth `JSON.stringify([` was GUIDE §9's snippet (two lines), which the
    removal grep over `control/` also matches.
- Cross-module hint: the counted-tokens rule is at `limits.go:601-606` (step 12 made
  it `Units.Sum(config.CountedUnits)`) and at `aggregate.ts:46-50`, as reported.
- **No disagreement between the halves and the spec.** Both give the spec's numbers
  for every case, the 2^53 case included (Go `Units.Sum` saturates only at
  `MaxInt64`).

**By-hand break checks** (each reverted by copying the saved file back)

- TS `amountFor` adding `tokens_cached` → the fixture test fails
  (`cache-reads-only.json, tokens_per_hour`: actual 8192n, expected 0n).
- Go `amountOf` summing `config.TokenUnits` → `TestCountedUnitsFixtures` fails on
  three files: `every-unit.json` counts 4821 (want 4301), `cache-reads-only.json`
  8192 (want 0), and `largest-record.json`. Each file fails for both
  `tokens_per_minute` and `tokens_per_hour`.

**Tests** (before → after)

- TS `npm test` (control): 615 → 617 tests (614 → 616 pass, 1 skipped as before).
  The two new tests are the fixture test and the `COUNTED_TYPES` schema pin. No
  existing assertion changed: `usage.test.ts`'s five window literals and the
  store-contract constants are renamed keys with the same numbers. The four
  negative-control broken stores still fail as named (`negative-control.test.ts`
  green).
- Go `internal/limits` (`go test -count=1 -v`, `--- PASS`): 80 → 85
  (+`TestCountedUnitsFixtures` and its 4 file subtests).

**Removal checklist** (step 13)

- `hourStart|monthStart|CurrentWindows` in `control/` (tracked and new files) → none.
- `JSON.stringify([` in `control/` → `messages/totals.ts` (`scopeTypeKey`) and
  `storage/window-key.ts` (`windowKeyOf`) only.
- Also gone: `windowIdentity`, `limitKey`, `windowStartFor`, `prunedHourStart`.

**Suite**: `scripts/check-all.sh` passed (exit 0) on the final code. Only this step
file was edited after it started.

```
==> gofmt / go vet / staticcheck 2026.2.1 (gateway)
==> go test -race -count=1 (gateway): ok cmd/kaiak, e2e (105s), accounting, auth, clip,
    config, control, limits, logattr, metrics, netfail, otlplog, provider, routing,
    schemacheck, server, sse
==> live-test kit: gofmt / vet / staticcheck; self-test passed for vllm, llama-server,
    openai, azure-openai, anthropic, azure-anthropic, vllm with two backends
gateway checks passed
==> npm test (control): 617 tests, 616 pass, 0 fail, 1 skipped
    (the four negative controls: torn snapshot, reconnect without catch-up, catch-up
    to one subscriber, early announcement: each fails the contract tests as named)
==> npm run lint (control): boundaries ok
==> cross-half e2e (sample control plane + two gateways; two cores over one store)
ok  	kaiak/e2e	65.582s
all checks passed
```

