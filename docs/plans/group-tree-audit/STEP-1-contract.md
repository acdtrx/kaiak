# Step 1 — contract

**Status:** done (2026-09-27)

## Intent

Write the audit's decisions into the contract before either half changes.

## Files likely touched

- `docs/specs/CONTROL-PROTOCOL.md` — Config → The group tree: the effective-limits
  bound (decision 3) and its code; `child_defaults` is a default, not a ceiling
  (decision 4); a re-created ID resumes its window's spend (decision 1, also Usage
  intake / Budgets where windows are keyed); "config order" → object-key order where
  it is claimed (S6); Budgets → Model-set edits: each side carries against the config
  it last held (decision 5).
- `docs/specs/GATEWAY.md` — Observability: `group` → `key_group` (decision 2), the
  cardinality text: `root_group` bounds series only when top-level groups are few (S5).
- `protocol/fixtures/config/invalid/` + `cases.json` — `effective-limits-exceeded`
  (one limit over the bound, reached through `child_defaults` × children, not one
  giant list); `protocol/fixtures/config/valid/` — exactly at the bound (keep the file
  small: few defaults × many children); T7: `child_defaults.allowed_models: []` in a
  resolution fixture, a 10-level chain invalid fixture (levels 9 and 10 flagged).
- `control/kaiak-control/schema/` unchanged unless the schema changes (it doesn't).

## Acceptance criteria

- Specs state all five decisions, dated `(settled 2026-09-27)`, replacing any text
  that said otherwise.
- New fixtures valid JSON; suites run; expected reds named (both halves' fixture
  tests: the new rule; cleared by steps 2 and 3).

## Result

- `docs/specs/CONTROL-PROTOCOL.md`:
  - The group tree: `child_defaults` is a default, not a ceiling (decision 4);
    effective limits bounded at 50 000 (decision 3); "Parents never change" points
    to the ID-reuse rule.
  - Semantic rules: `effective-limits-exceeded` — reported once at the root (path
    `""`); counted from each group's direct parent, so made whatever the other tree
    rules find (a group with an unknown parent counts its own limits alone).
  - Usage intake → Totals: a group ID used again resumes its window's spend
    (decision 1; rejected alternative: dropping a deleted group's windows).
  - Budgets → Model-set edits: each side carries against the config it last held
    (decision 5).
- `docs/specs/GATEWAY.md`: usage label `group` → `key_group` everywhere (labels,
  empty-label note, switches, the log field's cross-reference), with the reason
  (decision 2); cardinality — `root_group` bounds series only when top-level groups
  are few (S5); config reload — a re-created ID gets its window back from the pushed
  totals in control-plane mode, starts empty in file mode (the counter was dropped
  with the delete); model-set edits — predecessors come from the config the gateway
  applied before.
- `protocol/schema/config.schema.json`: `group_label`'s description names
  `key_group` (description only); `npm run sync-schemas` run.
- S6: neither spec claims an order for resolution; the only "config order" claim
  is the kit's `resolveScopes` comment (`control/kaiak-control/src/config/limits.ts`)
  — step 3 corrects it to object-key order.
- Fixtures:
  - `valid/effective-limits-at-bound.json` — exactly 50 000: 50 global + 50 of
    `users`' own + 499 children × 100 `child_defaults` limits (5 models give 128
    identities); `u001` overrides one default (still 100). ~35 KB.
  - `invalid/effective-limits-exceeded.json` — the same, `u002` adds one limit of
    its own: 50 001.
  - `invalid/group-depth-exceeded-two-levels.json` — a 10-level chain (the
    `cases.json` format pins the code only; the l9/l10 issue paths stay pinned by
    each half's unit test).
  - `resolved/allowed-models.json` — `child_defaults.allowed_models: []`: the
    child gets no model, a child with its own list gets that list.
  - Counts checked with a throwaway counter (own ∪ parent defaults by identity,
    plus global) and the kit's `resolveScopes`: 50 000 and 50 001; deleted.
- Suites: `go -C gateway test ./internal/config/...` — FAIL
  `TestInvalidFixtures/effective-limits-exceeded.json` only; `npm test -w
  kaiak-control` — 1 failing, `invalid config fixtures › effective-limits-exceeded.json`
  only. Expected reds: the new rule, cleared by step 2 (gateway) and step 3 (kit).
