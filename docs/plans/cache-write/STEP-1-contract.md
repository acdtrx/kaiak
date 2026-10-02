# Step 1 — contract

**Status:** done (2026-10-02)

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

**What changed**

- Specs: `CONTROL-PROTOCOL.md` — protocol version 4 (text, request-check table, the
  joined-header example); config `format_version` 4; Units and price units: the
  fifth unit `tokens_cache_write` (`prompt_tokens_details.cache_write_tokens`),
  `tokens_in` now prompt minus read and written, the three input units add up to
  the prompt, `usd_per_million` accepts the unit, unpriced → the tier's `tokens_in`,
  and a *`tokens_cache_write`* bullet (the Azure evidence, one unit for every
  backend, rejected: one unit per cache lifetime), settled 2026-10-02; Tiered
  prices: input size `tokens_in + tokens_cached + tokens_cache_write` (written
  tokens count: they are input), "the whole record" names written input, the
  "records do not change" note reworded to "tiers add nothing to records"; Usage
  records: five units, zeros included, the example record; Totals `used` and Usage
  intake → Counted toward: the token sum gains the unit. `GATEWAY.md` — Limits →
  Settle: the actual-tokens sum; Accounting: the units list, the meter mapping and
  the clamping order (cached ≤ prompt, then written ≤ prompt − cached; missing = 0;
  every backend type), Estimation (estimated input is all `tokens_in`, the other
  input units and reasoning 0), Cost's tier input size; Metrics:
  `kaiak_usage_tokens_total` five units, cardinality 6 → 7 per record label set
  (worked example 18,000 → 21,000 and 216 → 252); the request log line gains
  `tokens_cache_write`; usage spool format 3; `last-known-good.json` format 5 (config
  format 4 inside). `totals.json` and `limits.json` untouched.
- Schemas: `usage-record.schema.json` (`tokens_cache_write` required in `units`;
  description), `config.schema.json` (`format_version` 4; the unit in a tier's
  `usd_per_million`; the prices, tiers and unpriced-rule descriptions),
  `totals.schema.json` (the `used` description's sum), `status.schema.json`
  (`protocol_version` 4). kaiak-control's copy synced (`npm run sync-schemas`; the
  byte-for-byte test passes).
- Fixtures, migrated by a one-off script outside the repo: every usage record's
  `units` gains `"tokens_cache_write": 0` just before `tokens_cached` (543 records:
  42 pretty-printed, 501 in single-line batches — every fixture writes units in key
  order, as the gateway's encoder does, and `tokens_cache_write` sorts before
  `tokens_cached`), `format_version` 3 → 4, `protocol_version` 3 → 4 — config
  `valid/`, `invalid/`, `resolved/`, `duplicate-members/`, `messages/config-snapshot/`,
  `messages/status/`, `messages/usage-record/`, `messages/usage-batch/` (252 files).
  Checked by a second scratch script (order-preserving parse, so key order and
  repeated members count): every fixture equals its `HEAD` version under exactly
  that transform, the `cases.json` edits below aside; the line diff is one-for-one
  apart from the 42 inserted unit lines.
  - Renamed (the old-version case, as in tiered-pricing):
    `config/invalid/format-version-2` → `format-version-3` (value 3),
    `messages/status/invalid/protocol-version-2` → `protocol-version-3` (value 3).
  - `cases.json` reasons: "format_version must be 4", "the protocol version is 4",
    "price units are tokens_in, tokens_cached, tokens_cache_write, tokens_out",
    "the five token units are always present" (`units-missing-reasoning`).
  - New valid: `config/valid/price-cache-write.json` (an Azure gpt-5.6 model: a
    first entry without a write price — the unpriced rule — then a two-tier entry
    pricing `tokens_cache_write` at 1.25× input in both tiers),
    `messages/usage-record/valid/cache-write.json` (the Azure first call: 3 in,
    2033 written, 0 cached).
  - New invalid, schema: `config/invalid/price-cache-write-negative.json`,
    `messages/usage-record/invalid/units-missing-cache-write.json`, each with its
    `cases.json` entry.
  - `units-unknown` keeps its meaning (a unit outside the five), now beside
    `tokens_cache_write: 0`.

**Unit-sum and unit-list sites** (grep of both halves for `tokens_cached` /
`cached_tokens` / the unit constants beside `tokens_in` / `tokens_out`, at `HEAD`):

- Gateway (step 3):
  - `internal/config/snapshot.go:76-85` — the `Unit` constants and their comment;
    `:216`, `:225-226` — tier comments naming the input size and the unpriced rule.
  - `internal/config/schema.go:34` — `FormatVersion = 3`; `:103` — `priceUnits`.
  - `internal/accounting/meter.go:12-23` — `Units` comment ("all four token units")
    and `tokenUnits(in, cached, out, reasoning)`; its callers `:186`, `:190`, `:203`
    (estimates), `:259` (embeddings), `:269` (the report); `:234-241` —
    `usageReport` reads only `cached_tokens`; `:245` — the `parseUsage` comment.
  - `internal/accounting/accounting.go:211-214` — `inputSize`; `:225-247` — `Cost`
    and its unpriced-`tokens_cached` fallback.
  - `internal/limits/limits.go:557` — the actual-tokens sum.
  - `internal/server/api.go:232` — the request log line's unit fields.
  - `internal/control/schema.go:33` — the record check's unit list;
    `internal/control/messages.go:83` — the `Used` comment's sum.
  - `internal/control/control.go:18` — `ProtocolVersion = 3`;
    `internal/control/lastknowngood.go:16` — `lastKnownGoodFormat = 4`;
    `internal/control/spooldisk.go:28` — `spoolFormat = 2`.
  - `internal/fakebackend/fakebackend.go:532` — `prompt_tokens_details` reports
    only `cached_tokens`.
  - `internal/metrics/usage.go:82` ranges over the record's units — no list to
    edit, but its test should name the fifth unit; `internal/server/metrics.go:45`
    reads `tokens_out` only (decode rate) — unaffected.
  - `scripts/live/checks.go:400` (the "no input" check), `:407-408` (the usage line);
    `scripts/live/config.go:118` — `format_version` 3.
  - Tests that build records or units: `internal/accounting/accounting_test.go`,
    `internal/metrics/metrics_test.go`, `internal/config/snapshot_test.go`,
    `internal/server/{metrics,accounting,server}_test.go`,
    `internal/control/usage_test.go`, `e2e/e2e_test.go`.
- kaiak-control and sample (step 2):
  - `kaiak-control/src/config/types.ts:12-18` — `UsageUnit`, `PriceUnit` and their
    comment; `:60`, `:69` — tier comments (input size, unpriced rule); `:139` —
    `format_version: 3`.
  - `kaiak-control/src/usage/aggregate.ts:89-90` — a record's token sum.
  - `kaiak-control/src/protocol/index.ts:11` — `PROTOCOL_VERSION = 3`.
  - `kaiak-control/src/usage/usage.test.ts` — its record builder (no
    `tokens_cache_write`).
  - `sample/src/page/sections.ts:304`, `:320-322` — the recent-usage table.
  - `examples/config.json` (`:2` format 3; prices `:82`, `:101`),
    `examples/local-config.json:2`.

**Decisions made during the step**

- Key order: `tokens_cache_write` goes first in every fixture's `units`, since
  every fixture writes units in key order (the order Go's encoder writes a map) and
  it sorts before `tokens_cached`. The spec's example record follows.
- `units-missing-cache-write` is a copy of `units-missing-reasoning`'s record with
  reasoning present and `tokens_cache_write` left out.
- The negative-price case got its own fixture: `price-negative` covers `tokens_in`
  only, and each price unit has its own `minimum` in the schema.
- `price-cache-write.json` keeps a first entry without a write price, so the valid
  set holds the unpriced case beside the priced one.
- The 2026-10-01 *Backend types* decision named "format 3 and protocol 3" as the
  versions that stayed; reworded to "types bump no version", the decision it
  records, so it no longer names a current version.
- The *Estimation* bullet now says the estimated input is all `tokens_in` (cached,
  written and reasoning 0) — what the meter already does, stated for decision 9.

**Suite (expected reds)** — `scripts/check-all.sh` stops at `go test`; the other
stages were run directly.

- Gateway (`scripts/check-gateway.sh` stages): gofmt, vet, staticcheck pass;
  `go test -race ./...` FAIL in `cmd/kaiak` (12 top-level tests),
  `internal/config` (20), `internal/control` (33); every other package and `e2e`
  pass. Every failure is the gateway reading config format 3
  (`/format_version: must be 3`), speaking protocol 3, or refusing the five-unit
  records (`TestValidMessageFixtures`, `TestValidMessageFixturesRoundTrip`:
  `rejected: [schema]` on the usage-record, usage-batch, status and config-snapshot
  fixtures), and the boots that follow from it.
  `TestRunExitsWithNoConfigAtBoot` fails on goroutines left by the earlier boot
  tests that never reached serving; it passes run alone. Cleared by step 3.
  Live-test kit: gofmt, vet, staticcheck and the self-test pass.
- `control npm test`: 573 tests, 542 pass, 31 fail — 28 in
  `kaiak-control/src/usage/usage.test.ts` (its record builder has no
  `tokens_cache_write`: `/records/0/units must have required property
  'tokens_cache_write'`), `example configs` (`examples/config.json`,
  `local-config.json`: format 3) and sample `keygen` "the keys entry makes a valid
  config when pasted in" (it reads `examples/config.json`). Cleared by step 2. Every
  shared fixture passes, the new ones and the renamed old-version cases included;
  the schema-copy check passes. `npm run lint`: `tsc` clean, `boundaries ok`.
- Cross-half e2e: FAIL (`config snapshot: 503`: the sample control plane refuses
  its format 3 test config). Cleared by step 3 or 4.
