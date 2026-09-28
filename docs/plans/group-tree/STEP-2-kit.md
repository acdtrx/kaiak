# Step 2 — kaiak-control

**Status:** done (2026-09-27)

## Intent

The library implements the contract from step 1: validates group trees, resolves each
group's effective limits and allowed models, counts usage by the record's path, keys
totals by group, and refuses parent changes at publish.

## Files likely touched

- `control/kaiak-control/src/config/` — `types.ts` (Config v2, Group, ChildDefaults,
  Key.group), `semantic.ts` (tree rules and codes), `limits.ts`
  (`resolveScopeLimits` → a resolver of every group's effective limits along its
  `child_defaults`; `LimitScope` goes; `limitIdentity` unchanged), `index.ts`.
- `control/kaiak-control/src/messages/` — `types.ts` (UsageRecord `groups`,
  TotalsWindow `group`), `semantic.ts` if it names owners.
- `control/kaiak-control/src/usage/` — `aggregate.ts` (count toward each listed group
  the current config defines, plus global; model-set carry-over by (group, type)),
  `windows.ts`, `index.ts`.
- `control/kaiak-control/src/storage/types.ts`, `memory.ts` — `WindowKey`
  (`scope`/`id` → `group?`).
- `control/kaiak-control/src/config-versions/index.ts` — `group-parent-changed`
  against the previous published version.
- `control/kaiak-control/src/protocol/index.ts` — `PROTOCOL_VERSION = 2`.
- `control/kaiak-control/src/index.ts` — exports (resolver renamed; no `LimitScope`).
- Tests beside each; the fixture-parity tests (config, messages, resolution).

## Decisions for this step

- The resolver's output is the one the sample page and hosts read:
  `[{ group?: string, limits: Limit[], allowed_models?: string[] }]` with global
  first (no `group`), then every group; exact name and shape decided here and
  recorded in the result.
- `group-parent-changed` compares with the current published version at publish time
  (in the totals' turn, where publishes run); a group absent from the previous
  version is new, whatever its parent.

## Acceptance criteria

- Every step-1 fixture passes with the right codes, the resolution fixtures included.
- Tests: counting a record whose last group was deleted (surviving ancestors +
  global); a publish moving a group refused with `group-parent-changed`, a
  delete-then-recreate across two publishes accepted; model-set carry-over by group.
- `npm test -w kaiak-control` and `npm run lint` green; the sample's reds named
  (cleared by step 3).

## Result

**What changed** (`control/kaiak-control/src/`)

- `config/types.ts` — Config format 2: `groups` (`Group`: `parent?`, `labels?`,
  `allowed_models?`, `limits?`, `child_defaults?: ChildDefaults`), `Key.group`,
  `global.metrics.group_label`; `Team`, `Workload`, `User`, `global.default_user`
  gone.
- `config/tree.ts` (new) — `ancestryOf(groups, id)`: where a group's parents lead
  (`rooted` with its path, `parent-unknown`, or `cycle` with `onCycle`);
  `MAX_GROUP_DEPTH = 8`. Shared by the semantic rules and the resolver.
- `config/semantic.ts` — codes `key-group-unknown`, `group-parent-unknown`,
  `group-cycle`, `group-depth-exceeded` (`key-owner-unknown`,
  `workload-team-unknown` gone); `allowed-model-unknown`,
  `allowed-models-wildcard-mixed`, `limit-model-unknown`, `limit-duplicate` run
  over each group's lists and each `child_defaults`' lists. Reporting follows the
  spec: an unknown parent only on the group naming it, `group-cycle` on each group
  on the cycle, depth only where parents reach a top-level group (a group below an
  unknown parent or a cycle reports nothing of its own); depth is reported on every
  group below level 8.
- `config/limits.ts` — the resolver (below) replaces `resolveScopeLimits` /
  `LimitScope` / `ScopeLimits`; `mergeUserLimits` → `mergeLimits`;
  `limitIdentity`, `limitCovers` unchanged.
- `messages/types.ts` — `UsageRecord.groups: string[]` (no `UsageOwner`),
  `TotalsWindow.group?` (no `scope`/`id`, no `TotalsScope`),
  `GatewayStatus.protocol_version: 2`. `messages/semantic.ts` —
  `totals-window-duplicate` keyed by (group or global, type, model set).
- `usage/aggregate.ts` — `CountedLimit.group?`; `CountedLimits` is `{ all, global,
  byGroup }`; a record counts toward global plus every group in its `groups` the
  current config defines (others skipped); model-set carry-over matched by
  (group or global, type). `usage/index.ts` — totals windows and
  `LimitCarryOver` carry `group?` instead of `scope`/`id`.
- `storage/types.ts`, `storage/memory.ts` — `WindowKey` `scope`/`id` → `group?`.
- `config-versions/index.ts` — `group-parent-changed` (below).
- `protocol/index.ts` — `PROTOCOL_VERSION = 2`.
- `index.ts` / `config/index.ts` — export `resolveScopes` and type
  `ResolvedScope` (plus the new config types via `export type *`).
- Tests: every kit test on the new model (group paths in records, `group` windows,
  protocol header `2`, fixture `mixed-groups.json`); new — the resolution-fixture
  test (`config/limits.test.ts`, every `protocol/fixtures/config/resolved/*.json`
  matched exactly: path, allowed models or `"all"`, limits), tree-rule reporting
  (below an unknown parent / a cycle, depth on levels 9 and 10), a record whose last
  group was deleted (counts toward `research` and global only), model-set
  carry-over of a `child_defaults` limit per group (carol carries her spend, bob is
  reported with nothing to carry, alice's override untouched), and
  `group-parent-changed` (move refused and the current version stays; top-level ↔
  child both refused; delete then recreate under another parent accepted;
  unchanged parents with other edits accepted; a new group named `constructor` is
  new).

**The resolver**

```ts
resolveScopes(config: Config): ResolvedScope[]

interface ResolvedScope {
  group?: string;            // absent for global
  path: string[];            // top-level first, the group last; [] for global
  limits: Limit[];           // effective, in merge order, each as written in config
  allowed_models?: string[]; // across the whole path, sorted by code point; absent = every model
}
```

Global first (`{ path: [], limits: global.limits ?? [] }`), then every group in
config order, a group with no limits still listed. Called only on a valid config;
a group whose parents do not reach a top-level group throws `{ code:
"config-invalid" }`.

**`group-parent-changed`**: `publishOne` (already serialized per publish, and run
by the core inside `Usage.publishing`, the totals' turn) validates the document,
reads the current published version and compares: every group of the new config
that the current version defines (own property) must have the same `parent`
(both absent counts as the same). Each change is an issue `{ code:
"group-parent-changed", path: "/groups/<id>/parent", message }`, returned as
`{ ok: false, issues }` like a validation failure — no version is assigned. Groups
absent from the current version are new whatever their parent; the first publish
has nothing to compare.

**Decisions made during the step**

- Name `resolveScopes` / `ResolvedScope`: the spec's word for global and each group
  a request passes is *scope*. The entry carries `path` as well as the planned
  `{ group?, limits, allowed_models? }` — hosts (the sample page) and the fixture
  test need it, and it saves each of them re-deriving the tree.
- `allowed_models` keeps the config's snake_case (it is the config's notion, and
  the fixture's field); code-point order via UTF-8 byte comparison (`Buffer.compare`),
  which equals code-point order — `Array.prototype.sort` compares UTF-16 units.
- `ancestryOf` lives in `config/tree.ts`, one walk used by both validation and
  resolution, so they cannot disagree on what a path is.
- `group-parent-changed` checks with `Object.hasOwn`: group IDs such as
  `constructor` are valid, and a plain index would find `Object.prototype`'s.
- Contract: no fix needed — every fixture (valid, invalid with its exact code,
  resolved, messages, duplicate members) passes as step 1 wrote it.

**Suite**

- `npm test -w kaiak-control`: `# tests 454 · pass 452 · fail 2` — the two reds are
  `example configs` › `config.json`, `local-config.json`: `examples/` is still
  format 1. Expected, named in step 1's result; cleared by step 5 (examples
  rewritten). Every other kit test is green, the resolution fixtures included.
- `npm run lint`: `tsc` fails with 18 errors, all in `sample/` (0 in
  `kaiak-control`) — `sample/src/page/sections.ts` (imports `resolveScopeLimits`;
  reads `teams`, `workloads`, `users`, `key.workload`, `key.user`, window
  `scope`/`id`, record `owner`) and `sample/src/page/page.test.ts` (key `user`,
  `format_version: 1`, window `scope`). The boundary check, run on its own
  (`node scripts/check-boundaries.ts`): `boundaries ok`. Cleared by step 3.
- `npm test` (workspace): `# tests 483 · pass 474 · fail 9` — the kit's 2 example
  configs above, and the sample's: `app.test.ts`, `page/events.test.ts`,
  `page/page.test.ts` fail to load (`sections.ts` imports `resolveScopeLimits`),
  `main` tests 102–104 (the server does not start, same import), and keygen's
  "the keys entry makes a valid config when pasted in" (writes a `workload` key).
  Cleared by step 3.
- `kaiak-control/GUIDE.md` still shows `resolveScopeLimits` and the owner model;
  it is step 6's (`STEP-6-docs.md`).

