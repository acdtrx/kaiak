# Step 1 — the contract

**Status:** done (2026-09-27)

## Intent

Settle the group tree in the contract before either half changes: the specs, the JSON
Schemas and the shared fixtures, protocol version 2. Everything later steps implement
is decided here (`OVERVIEW.md`, Decisions).

## Files likely touched

- `docs/specs/CONTROL-PROTOCOL.md` — Config (owners and limits → groups; semantic
  rules; top-level shape; format_version 2), Messages (totals window `group`;
  usage record `groups`; protocol version 2), Usage records, Usage intake (Counted
  toward; owners the config no longer defines → groups), Budgets (model-set edits by
  group identity), Control-plane processes unchanged. Add `group-parent-changed`
  (control plane only, Config versions).
- `docs/specs/GATEWAY.md` — Limits (scopes = path), Client API refusal wording
  (`group limit`), Accounting (record `groups`), Observability (usage labels,
  `group_label`, rejection scope kinds, log fields, cardinality), Configuration
  sources (data-directory format versions bumped).
- `docs/kaiak.md` — Domain model: Key, Workload, Team, User → Group; Limits scopes.
- `protocol/schema/config.schema.json` (`format_version` 2, `groups`, `group`,
  `child_defaults`, `labels`, key `group`; remove `teams`, `workloads`, `users`,
  `global.default_user`, `global.metrics.user_label` → `group_label`),
  `usage-record.schema.json` (`groups`), `totals.schema.json` / `common.schema.json`
  (window `group`), and the protocol version wherever the schemas or fixtures carry
  it (status `protocol_version`).
- `protocol/fixtures/config/{valid,invalid}/` and `cases.json`;
  `protocol/fixtures/messages/*/` (usage-record, usage-batch, usage-ack, totals,
  status version); `protocol/fixtures/duplicate-members/` where they embed owners.
- A resolution fixture set pinning the effective limits and allowed models per group
  (e.g. `protocol/fixtures/config/resolved/` — input config + expected
  `{ group → { allowed_models | "all", limits } }`), for both halves to check.
- `control/kaiak-control/schema/` — `npm run sync-schemas`.

## Decisions for this step

- Labels: at most 16 per group; keys `^[a-z][a-z0-9_.-]{0,62}$`; values strings of
  1–256 printable characters. Shape only — no meaning in either half.
- `groups` optional (omitted = none); a key's `group` required; `child_defaults`
  allowed on any group (on a leaf it simply has no effect — not an error).
- Record `groups`: 1–8 IDs, unique (schema `uniqueItems`), top first.
- Fixtures keep the one-defect-per-invalid-file discipline; every new rule code gets
  at least one invalid fixture; `group-parent-changed` is kit-only, tested in step 2
  (it needs two versions).

## Acceptance criteria

- Specs describe only the new model (no mention of teams/workloads/users as config
  entities except in the "today's model maps as" example).
- Schemas and fixtures updated; `npm run sync-schemas` leaves the kit copy identical.
- Suite run and recorded: expected reds are the gateway's and the kit's fixture
  and type tests (cleared by steps 2 and 4), named in the result.

## Result

**What changed**

- Specs: `CONTROL-PROTOCOL.md` — protocol version 2; Config → *The group tree*
  (shape, keys, scopes, limit identity, allowed models, `child_defaults`, immutable
  parents, labels, resolution fixtures); semantic rules; Config versions →
  `group-parent-changed`; totals window `group`; usage record `groups`; Usage intake
  (counted toward the listed groups the current config defines + global); Budgets.
  `GATEWAY.md` — scopes = path + global, refusal text `group limit` / `global limit`,
  counter identity, record's scopes, data-directory format versions, usage labels
  `group` / `root_group`, `group_label`, rejection scope kinds, log fields
  (`group`, `limit_scope` `global`|`group`), cardinality example. `docs/kaiak.md` —
  domain model: Key, Group, Global, Limits.
- Schemas: `config.schema.json` (format 2, `groups`, `group`, `labels`, key
  `group` required; `teams`, `workloads`, `users`, `global.default_user` gone;
  `group_label`), `usage-record.schema.json` (`groups`: 1–8 IDs, unique),
  `totals.schema.json` (window `group?`, no `scope`/`id`), `status.schema.json`
  (`protocol_version` 2). Kit copy synced (`npm run sync-schemas`; identical).
- Fixtures: every config fixture rewritten to format 2 (teams → top-level groups,
  workloads → their children; users → top-level groups when the old default user
  restricted nothing, else children of a `users` group whose `child_defaults` hold
  the old default user); message fixtures on `groups`, window `group`, protocol 2;
  duplicate-members fixtures likewise (the snapshot case's path is now
  `/config/keys/k-me/group`).
  - Config valid: new `group-tree.json` (team → project → env → workload, labels, a
    key on a non-leaf group, a leaf's ineffective `child_defaults`, a group with no
    restriction); `user-overrides.json` → `users-child-defaults.json`;
    `at-bounds.json` gains an 8-level chain and 16 labels at the bounds (a 63-char
    key, 256-char values incl. 256 astral characters — pins code-point counting).
  - Config invalid, new: `group-cycle`, `group-cycle-self`, `group-depth-exceeded`,
    `allowed-models-wildcard-mixed-child-defaults`,
    `limit-model-unknown-child-defaults`, `limit-duplicate-child-defaults`
    (semantic); `groups-as-array`, `group-unknown-field`, `group-id-invalid`,
    `child-defaults-unknown-field`, `labels-too-many`, `label-key-uppercase`,
    `label-key-too-long`, `label-value-empty`, `label-value-too-long`,
    `label-value-control-char`, `label-value-number`, `key-user-field`,
    `global-default-user` (schema). Renamed: `key-user-unknown` →
    `key-group-unknown`, `workload-team-unknown` → `group-parent-unknown`,
    `key-no-owner` → `key-no-group`, `allowed-model-unknown-{default-user,workload}`
    → `-{child-defaults,group}`, `limit-duplicate-user-override` →
    `limit-duplicate-group`, `user-label-string` → `group-label-string`,
    `format-version-2` → `format-version-1`. Removed: `key-both-owners`,
    `key-workload-unknown`, `allowed-model-unknown-user`, `default-user-missing`,
    `workload-missing-allowed-models`, `workload-missing-team`.
  - Messages: usage-record valid `group-path`, `one-group`, `groups-8`; invalid
    `groups-empty`, `groups-too-deep`, `groups-repeated`, `groups-id-invalid`,
    `owner-field` (removed `owner-both`, `owner-workload-without-team`); totals
    invalid `group-invalid`, `window-scope-field` (removed `global-with-id`,
    `scope-unknown`, `team-without-id`); status `protocol-version-2` →
    `protocol-version-1`; usage-batch `mixed-owners` → `mixed-groups`.
  - Resolution fixtures, new kind: `protocol/fixtures/config/resolved/*.json` —
    `{ reason, config, expected: { groups: { <id>: { path, allowed_models, limits }
    } } }` (format in `CONTROL-PROTOCOL.md`, The group tree → Resolution
    fixtures): `allowed-models`, `limits`, `group-tree`, `users-child-defaults`,
    `full`.
- Checked outside both halves' suites: every schema and fixture parses; a scratch
  ajv run (the kit's validator settings) confirmed valid fixtures pass, invalid
  schema fixtures fail, semantic fixtures pass the schema, and a scratch reference
  of the group rules and resolution reproduced every semantic code and every
  `resolved` expectation.

**Decisions made during the step**

- `global` stays required (it may be `{}`); it has no allowed-models list — the
  root restricts nothing, as decision 5 needs.
- `allowed_models: []` keeps its meaning: that level allows no model.
- Label values: `^[^\u0000-\u001F\u007F-\u009F]{1,256}$` — 1–256 code points, no
  C0/C1 control characters or DEL; spaces and any other Unicode allowed. Code points,
  not bytes or UTF-16 units: ajv's `u` flag and Go's runes count the same.
- One defect per fixture with a tree: `group-cycle` is reported for each group on
  the cycle (codes are deduplicated by both halves); depth is judged only where
  parents reach a top-level group, so a group under an unknown parent or a cycle
  reports that rule alone.
- `group-parent-changed` compares with the current published version only: a group
  deleted in one publish may come back under another parent later (a move is a
  delete and a create).
- Resolution order is pinned so both halves compare exactly: limits in merge order
  (parent's `child_defaults.limits`, each replaced in place, then the group's other
  limits); allowed models sorted by code point, or `"all"`.
- Usage metrics carry `group` and `root_group` only — intermediate levels are not
  labels (a path of any length would widen the label set); `root_group` is the
  key's group itself when that is top-level.
- Data-directory versions: `limits.json` 1 → 2, `totals.json` 1 → 2, usage spool
  1 → 2, `last-known-good.json` 2 → 3.
- The record's `groups` is the path in the request's config snapshot.

**Suite (expected reds)**

- `go -C gateway test ./...`: FAIL in `cmd/kaiak` (12 top-level tests),
  `internal/config` (17: `TestValidFixtures`, `TestInvalidFixtures` and the
  loader/snapshot tests that read the shared fixtures), `internal/control` (34:
  the message fixtures and every test serving a fixture config or speaking
  protocol 1); all other packages and `e2e` pass (their configs are built in Go).
  `TestExampleConfigs` still passes — `examples/` is format 1 like the gateway's
  code; it turns red with step 4 and green with step 5. Cleared by step 4.
- `control npm test`: 484 tests, 308 pass, 175 fail, 1 cancelled — kaiak-control
  `config` (39), `fastify` (35), `usage` (27), `config-versions` (16),
  `status-totals` (8), `messages` (7), `duplicate-members` (7), `control-plane`
  (6), `limits` (2), `usage-route` (1); sample `page` (10), `config-file` (8),
  `app` (5), `events` (2), `main` (2), `keygen` (1). The schema-drift check passes.
  Cleared by step 2 (kit), step 3 (sample), step 5 (the kit's example-configs test
  over `examples/`).

