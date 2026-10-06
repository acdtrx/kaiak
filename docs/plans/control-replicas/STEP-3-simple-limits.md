# Step 3 — limits without model sets

**Status:** not started

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

(filled in when the step is done)
