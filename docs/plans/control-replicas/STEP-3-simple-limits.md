# Step 3 — limits without model sets

**Status:** done (2026-10-06)

## Intent

Make limits per scope only, so config publishes and usage counting no longer depend
on each other. This is the contract and store half; steps 4–5 implement it in the
core and the gateway.

- **A limit is `{ type, value }`.**
  - The optional `models` goes, and with it the model-set identity.
  - A scope (global or a group) has at most one limit per type.
  - A limit's identity is (scope, type).
- **Usage counts by scope, whatever the config.** Every usage record counts toward
  the `tokens_per_hour` and `usd_per_month` window of every scope on its path:
  global, and each group in `groups`. That holds whether or not a limit of that type
  exists there now.
  - Totals list only the windows the latest config has a limit for.
  - A limit added mid-window therefore starts with the window's usage so far. A
    removed one simply stops being listed.
- **The model-set carry-over is gone:** an edited limit keeps its identity, so it
  keeps its spend. A publish no longer reads or writes windows.
- **The links between publishing and counting go:**
  - A publish is conditional on the latest config version only.
  - A batch is conditional on the instance's last batch only, not on the config
    version.
  - The totals read is the one place config and usage meet.

## Files likely touched

- **`docs/specs/CONTROL-PROTOCOL.md`:**
  - Config → The group tree: the Limits shape, identity, scopes "whose model set
    covers its model" (every limit of a scope now applies), `child_defaults` merging
    by type.
  - Semantic rules: `limit-model-unknown` goes; a repeated limit type in one scope
    is refused (the existing duplicate rule, now by type).
  - Budgets: windows keyed by (scope, type); counting by scope whatever the config;
    a limit added mid-window starts with the window's usage.
  - Model-set edits: removed; replaced by the rule that editing a limit keeps its
    window.
  - Totals → windows: no `models`.
  - Usage intake: counting is independent of the config version.
  - Config versions: a publish is conditional on the version alone.
  - All dated 2026-10-06, with what was rejected:
    - model sets (per-model caps): users set budgets per group; provider budgets are
      a separate plan;
    - limit IDs: unnecessary once a scope holds one limit per type;
    - an unconditioned carry-over.
- **`docs/specs/GATEWAY.md`:** Limits (scopes, identity, the gateway's counters and
  per-minute limits by scope and type, outage rule wording) and the file-mode snapshot.
- **`protocol/schema/config.schema.json`** (limit without `models`; still format 5,
  unreleased) and `totals.schema.json` (window without `models`); then
  `npm run sync-schemas`.
- **`protocol/fixtures/**`:**
  - limits lose `models` by a scripted transform, verified against `HEAD`;
  - fixtures that exist only for model sets are removed or rewritten (repeated type
    in one scope becomes the duplicate case);
  - totals fixtures lose window `models`.
- **`control/kaiak-control/src/storage/types.ts`, `memory.ts`,
  `src/store-contract/`:**
  - window keys (scope, type);
  - `publishConfig` without `carried` and without the sequence condition;
  - `saveCountedBatch` without `configVersion`;
  - contract tests updated: a publish racing batches never refused; a batch never
    refused for a publish.
- **`docs/BACKLOG.md`:** an entry for **provider budgets**: money limits on a set of
  backends (a provider account across regions), with deployments leaving routing when
  spent. Revisit trigger: the user's research and discussions on provider budgets
  (2026-10-06).

## Decisions made during planning

Settled with the user, 2026-10-06:
- Budgets are per group, not per model.
- Provider budgets (money only, on a set of backends) are a separate plan later.

Made while planning:
- **Config format stays 5 and protocol 5.** Both are unreleased, and the whole
  release bumps once.
- **Window rows grow per group on the path,** at most 8 groups plus global, × 2 types
  per record. They are bounded by the number of groups, not keys. Past windows are
  still dropped as today.

## Acceptance criteria

- Specs, schemas and fixtures carry the new limit shape and counting rule, dated.
  The fixture diff is exactly the transform plus the deliberate removals and
  rewrites, listed in Result.
- Store contract tests pass against the memory store, including publishes and
  batches racing without refusing each other.
- Suite run and recorded. Expected reds:
  - the core (step 4)
  - the gateway's config, limits and totals tests (step 5)
  - the cross-half e2e (step 5)

## Result

**What changed**

- `docs/specs/CONTROL-PROTOCOL.md` (dated 2026-10-06, with the rejected options: model
  sets, limit IDs):
  - totals window shape and identity are (group or global, type);
  - Config → The group tree: Limits is `{ type, value }`, one per type per scope, every
    limit of a scope applies; `child_defaults` merge by type; `limit-model-unknown`
    gone, `limit-duplicate` by type;
  - Usage intake: "Counted whatever the config" replaces "counted under the config in
    force"; Counted toward counts every scope on the path into its hour and month
    windows whether or not it has a limit; groups no longer defined still count
    (into unlisted windows); Totals list the current config's limits, so an edited
    limit keeps its spend and a limit added mid-window starts with the window's usage;
  - Budgets: "Editing a limit keeps its spend" replaces Model-set edits;
  - Config versions and Control-plane processes: publishes and batches never condition
    on each other; the outage rule's wording.
- `docs/specs/GATEWAY.md`: every limit of a scope applies; config reload by (group,
  type), a new limit starting empty in file mode and from the totals in control-plane
  mode; the gateway's model-set carry-over section removed; `limits.json` and
  `totals.json` format 3 (both stored `models`); the outage rule and the per-minute
  share warning reworded; the `kaiak.limit.models` and carry-over log fields removed.
- Schemas: `models` removed from the config limit and the totals window; the copy in
  `control/kaiak-control/schema/` synced. Config format stays 5, protocol 5.
- Fixtures (29 files by the scripted transform, checked against the parsed originals
  with `models` removed from `limits` and `windows` only), plus by hand:
  - deleted: `config/valid/limits-same-type-different-model-sets.json`,
    `config/invalid/limit-duplicate-reordered-models.json`, `limit-model-unknown.json`,
    `limit-model-unknown-child-defaults.json`, `limit-models-empty.json`,
    `messages/totals/invalid/window-models-duplicate.json`;
  - renamed: `limit-duplicate-all-models.json` → `limit-duplicate.json`;
    `limit-models-wildcard.json` → `limit-models.json` (now a limit naming a real
    model: the member itself is refused); `messages/totals/invalid/models-empty.json`
    → `window-models.json` (kept with `models`: the member is refused);
  - rewritten: `config/resolved/limits.json` (merging by type);
    `config/valid/effective-limits-at-bound.json` and
    `config/invalid/effective-limits-exceeded.json` regenerated at one limit per type
    (4 global + 4 on `users` + 4 defaults × 12 498 children = 50 000; one more
    top-level group with one limit makes 50 001; about 450 KB each);
  - `cases.json` reasons updated.
- Store: `WindowKey` without `models`; `publishConfig(entry, expectedVersion, keep)`
  (no carry, no sequence condition; still moves the sequence);
  `saveCountedBatch` without `configVersion`; the memory store and the contract tests
  follow — a new test races publishes and batches and checks neither refuses the other,
  and the window test checks scopes and types name windows apart.
- `examples/config.json`: the two USD limits lose their model lists (unpriced vLLM
  models cost nothing, so the budgets count the same spend).
- `docs/BACKLOG.md`: **Provider budgets** added under Limits; the carry-over imprecision
  entry removed (the carry-over is gone).

**Things the plan did not foresee**

- **File mode differs on a limit added mid-window:** the gateway counts only configured
  limits, so in file mode a new limit starts empty; in control-plane mode it starts
  from the totals, which count every scope. Recorded in GATEWAY.md's config reload
  rule; not worth a change (file mode has one gateway and no ledger).
- **Both data-directory limit files stored `models`**, so `limits.json` and
  `totals.json` move to format 3 (step 5 implements).
- **The effective-limits bound (50 000) still matters**: groups are unbounded, so 4
  limits per scope can still reach it; only the fixtures changed.
- **Deleted groups now keep counting** into windows no limit lists (they used to be
  skipped); harmless, pruned with past windows.
- **Per-model per-minute caps are gone** (a `tokens_per_minute` on one scarce model),
  as decided; the per-minute share warning now names a model the scope may use.
- **Left for step 4:** kaiak-control's config module still carries model-set helpers
  — `Limit.models` in `types.ts`, `limitIdentity` with models, `limitCovers`,
  `limit-model-unknown` in `semantic.ts`, and their tests. Valid configs never reach
  them now (the schema refuses `models`); they go with the usage module that uses
  `limitCovers`. The GUIDE's `onLimitCarriedOver` text goes in step 4/6.

**Suite** (2026-10-06)

- Store and contract tests: 25 pass, 0 fail. kaiak-control config tests (shared
  fixtures): 172 pass, 0 fail.
- Control `npm test`: 591 tests, 437 pass, 153 fail — **expected, cleared by step 4**:
  config-versions 21, control-plane 12, fastify 43 + status-totals 16 + usage-route 6,
  gateways 7, usage 30, sample app 5, config-file 8, main 2, page events 2, page 1.
- `npm run lint`: 53 TypeScript errors, all in the core modules and their tests
  (usage, control-plane, gateways, config-versions, fastify, aggregate) — **expected,
  step 4**; none in storage or store-contract. Boundary lint not reached by `tsc`'s
  failure; it passed in step 2 and no imports changed.
- Gateway `go test ./...`: `internal/config` (`TestInvalidFixtures/limit-models.json`,
  `TestChildOverrideMatchesOnTypeAndModelSet`, `TestLimitCovers`) and
  `internal/control` (step 1's five totals tests) — **expected, step 5**. Every other
  package passes.
- Cross-half e2e: fails (the sample's core no longer builds) — **expected until step
  5**.
