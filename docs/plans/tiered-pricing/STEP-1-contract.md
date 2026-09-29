# Step 1 — contract

**Status:** done (2026-09-29)

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

## Result

**What changed**

- Specs: `CONTROL-PROTOCOL.md` — protocol version 3 (text, request-check table, the
  joined-header example); config `format_version` 3; Config → Prices (`{ effective_from,
  tiers }`) and a new *Tiered prices* bullet (bounds, input size, which tier applies,
  whole record at one tier, tiers complete in themselves, records and USD limits
  unchanged, the rejected base + long-context block), settled 2026-09-29; price-unit
  rules now "within a tier"; semantic rules `price-tier-first-not-zero` and
  `price-tiers-not-increasing` with their paths. `GATEWAY.md` — Accounting → Cost picks
  the tier from the input size; `last-known-good.json` format version 3 → 4 (config
  format 3 inside).
- Schemas: `config.schema.json` (`format_version` 3; price entry `{ effective_from,
  tiers }`, 1–8 tiers `{ above_input_tokens, usd_per_million }`, thresholds integers
  0..2^53 − 1), `status.schema.json` (`protocol_version` 3). kaiak-control's copy synced
  (`npm run sync-schemas`; the byte-for-byte test passes).
- Fixtures, migrated by a one-off script outside the repo: every price entry wrapped
  as one tier at 0, `format_version` 2 → 3, `protocol_version` 2 → 3 — config
  `valid/`, `invalid/`, `resolved/`, `messages/config-snapshot/`, `messages/status/`,
  `duplicate-members/`. Checked by a second scratch script: every fixture equals its
  `HEAD` version with exactly that transform (the two `cases.json` edits aside).
  Entries whose tier line would pass 100 columns are broken over lines; the one
  inline entry in `price-dates-equal.json` and `price-empty.json`'s one-line list
  were expanded by hand.
  - Renamed (the old-version case, as in group-tree): `config/invalid/format-version-1`
    → `format-version-2`, `messages/status/invalid/protocol-version-1` →
    `protocol-version-2`.
  - New valid: `price-tiers.json` (a one-tier entry, then a two-tier entry at
    272000), `price-brackets.json` (three Qwen-style brackets 0/32000/128000, plus a
    model with 8 tiers whose last threshold is 2^53 − 1 and whose tiers price different
    units — the upper bounds, and "each tier complete in itself").
  - New invalid, schema: `price-entry-usd-per-million`, `price-tiers-empty`,
    `price-tiers-too-many` (9), `price-tier-threshold-fractional`,
    `price-tier-threshold-negative`, `price-tier-threshold-above-safe`,
    `price-tier-threshold-missing`, `price-tier-prices-missing`. Semantic:
    `price-tier-first-not-zero`, `price-tiers-decreasing` and `price-tiers-equal`
    (`price-tiers-not-increasing`).
  - Existing `price-empty`, `price-negative`, `price-reasoning-unit`,
    `price-unknown-unit` keep their meaning, now inside tier 0.
- Checked outside both halves' suites (scratch, kaiak-control's ajv settings): valid
  fixtures and the `resolved/` configs pass the schema; every schema case fails it;
  every semantic case passes it; a scratch reference of the two tier rules reports
  exactly the `cases.json` code for the three semantic cases, at
  `/models/chat/prices/0/tiers/0/above_input_tokens` (first-not-zero) and
  `.../tiers/2/above_input_tokens` (not-increasing), and none for the valid fixtures
  or the other semantic cases.

**Decisions made during the step**

- `price-tier-threshold-negative` puts −1 on the second tier; it also breaks the
  increase, but the schema rejects it first, as with any schema case.
- `price-tier-threshold-above-safe` and `price-tier-prices-missing` /
  `price-tier-threshold-missing` added beyond the step's list: range and required
  fields are schema, one fixture each.
- The 8-tier upper bound lives in `price-brackets.json` rather than a third valid
  file, so the new valid fixtures stay the two the step names.

**Suite (expected reds)** — `scripts/check-all.sh` stops at `go test`; the other
stages were run directly.

- Gateway (`scripts/check-gateway.sh`): gofmt, vet, staticcheck pass; `go test -race
  ./...` FAIL in `cmd/kaiak`, `internal/config`, `internal/control` (63 top-level
  tests); every other package and `e2e` pass. Every failure is the gateway reading
  format 2 and the old price shape (`/format_version: must be 2`, `tiers: unknown
  field`) or speaking protocol 2 (`protocol version mismatch`), and the boots that
  follow from it. Cleared by step 3. Live-test kit: vet and self-test pass.
- `control npm test`: 554 tests, 548 pass, 6 fail — `price-tier-first-not-zero`,
  `price-tiers-decreasing`, `price-tiers-equal` (the rules are not in kaiak-control
  yet; cleared by step 2); `example configs` (`examples/config.json`,
  `local-config.json`) and sample `keygen` "the keys entry makes a valid config" (it
  reads `examples/config.json`) — `examples/` is still format 2, cleared by step 4.
  The schema-copy check passes. `npm run lint`: `tsc` clean, `boundaries ok`.
- Cross-half e2e: FAIL (`config snapshot: 503`: the sample control plane refuses its
  format 2 test config). Cleared by step 4.
