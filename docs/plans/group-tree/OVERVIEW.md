# Plan: the group tree

## Goal

Replace the fixed owner model — `global > team > workload` for workload keys,
`global > user` for personal keys, `global.default_user` as the users' template — with
one generic **tree of groups** of any depth, so a deployment can shape it as
team → project → env → workload, team → env → project → workload, or anything else,
while today's model stays expressible on the new structure.

## Scope

- **Config** (`format_version` 2): `groups` replaces `teams`, `workloads`, `users` and
  `global.default_user`. A key names one `group`. `global` stays the implicit root
  (`global.limits`).
- **Group**: `{ parent?, labels?, allowed_models?, limits?, child_defaults? }`, keyed by
  group ID (the existing ID pattern). No `parent` = a top-level group.
- **Protocol** (version 2): usage records carry `groups` (the key's path, top first)
  instead of `owner`; totals windows name `group` (absent = global) instead of
  `scope` + `id`.
- Both halves (gateway, `kaiak-control`), the sample control plane, keygen, fixtures,
  examples, the e2e suites, the live kit's generated configs, and every doc that
  describes the owner model.

## Out of scope

- Cut-across memberships (a group counting toward a second parent) — backlog entry
  with its trigger (Phase 2).
- User management, group ownership and who may mint keys where: control-plane
  concerns, not in the config (as team membership is today).
- Moving a group to another parent: not a supported operation (decision below).
- Any compatibility with format 1 / protocol 1 (Feature-Building Mode).

## Decisions (settled 2026-09-27 with the user)

1. **Plain single-parent tree**, depth at most 8 (a top-level group has depth 1). The
   order of levels is free per branch; the gateway gives levels no meaning.
2. **Scopes of a request** = the key's group and all its ancestors, plus global. A
   request must pass every limit on them; its usage counts toward each. The same
   mechanism as today, with a path of any length.
3. **Parents are immutable.** A group never changes parent; a move is delete + create.
   `kaiak-control` refuses a publish that gives an existing group another parent
   than the previous published version did (`group-parent-changed`). The gateway
   does not check it (it is stateless and may boot with no previous config).
4. **Labels** (`labels`: string → string) replace any `kind` field. Read only by the
   control plane (UI, reports); the gateway validates their shape and ignores them.
5. **`allowed_models` intersects down the path.** A group's effective list at its
   level is its own `allowed_models`, else its parent's `child_defaults.allowed_models`,
   else no restriction at that level. A key may use a model only if every restricting
   level on its path allows it. **No restriction anywhere on the path = every model**
   (user's choice). `["*"]` is an explicit "no restriction".
6. **`child_defaults`** (`{ allowed_models?, limits? }`) apply to each **direct child**
   with today's default-user rules: a child's `allowed_models` replaces the default
   list; a child's limit replaces the default limit with the same type and model
   set; default limits it does not override still apply. `global.default_user`
   becomes `child_defaults` on a `users` group.
7. **Usage records carry the path** (`groups`, top first, 1–8 IDs). The control plane
   counts a record toward every listed group the current config still defines, plus
   global — so usage settled just before a delete still counts toward the ancestors
   that remain, and records stay readable after reorganizations.
8. **Limit identity** = (group, type, model set); global has no group. Model-set
   carry-over and totals matching keep working by that identity.
9. **Client-facing refusals** say `group limit` or `global limit` — never a group ID,
   never a label (user's choice). Operators get the group ID in the log line
   (`limit_scope` `global`|`group`, `limit_id`).
10. **Usage metrics** are labelled `group` (the key's group) and `root_group` (its
    top-level ancestor) plus `key_id`, `model`, `status`. `global.metrics.group_label`
    replaces `user_label` (off: `group` left empty, `root_group` kept — bounded
    series). `kaiak_limit_rejections_total` scope kinds become `global`, `group`.
11. **Versions**: config `format_version` 2, protocol version 2; data-directory files
    whose content changes (last-known-good config, totals cache, usage spool,
    file-mode limits snapshot) bump their format versions — a gateway discards other
    versions and says so, as the rules already provide.
12. **New semantic rule codes** (both halves): `group-parent-unknown`, `group-cycle`,
    `group-depth-exceeded`, `key-group-unknown` (replaces `key-owner-unknown`);
    `workload-team-unknown` goes. `limit-duplicate` covers each group's `limits` and
    each `child_defaults.limits`. `allowed-model-unknown`, `limit-model-unknown`
    apply to groups and `child_defaults`. Control plane only: `group-parent-changed`.

## Risks

- **Breadth**: every layer touches the owner model (gateway config, auth, limits,
  accounting, metrics, server messages, control messages; kit validation,
  aggregation, API; sample; ~200 fixtures; 28 gateway test files). Mitigation: the
  contract lands first with fixtures both halves must pass; each half then turns
  green against it.
- **Resolution drift between halves**: `child_defaults` and path resolution are
  implemented twice. Mitigation: fixtures that pin the resolved limits per group
  (a `resolved` fixture kind or cases in `cases.json`) run in both halves.
- **Metrics cardinality**: `group` per key group can grow with the tree;
  `group_label` off is the bound (DEPLOYMENT.md's cardinality section redone).

## Phases and steps

- **Phase 1 — the group tree, end to end** (steps 1–5). Green at the end.
  1. `STEP-1-contract.md` — specs, schemas, fixtures, protocol version 2.
  2. `STEP-2-kit.md` — `kaiak-control`: validation, resolution, aggregation, API,
     `group-parent-changed`.
  3. `STEP-3-sample.md` — sample control plane: keygen `--group`, status page on
     the tree.
  4. `STEP-4-gateway.md` — gateway: config, auth, limits, accounting, metrics,
     messages, data-directory versions.
  5. `STEP-5-e2e.md` — gateway e2e and cross-half e2e, examples, live kit configs,
     smoke script; `scripts/check-all.sh` green.
- **Phase 2 — docs and proof** (step 6).
  6. `STEP-6-docs.md` — architecture docs and pages, DEPLOYMENT, README, GUIDE,
     backlog entry; check-all 3× green; images built and smoke-tested.

Expected reds inside Phase 1: after step 1 both halves fail the new fixtures
(cleared by steps 2 and 4); after step 2 the sample fails (step 3); gateway e2e and
the cross-half e2e fail until step 5.

## Verification

- Fixture parity: every valid/invalid config and message fixture passes in both
  halves with the same codes, including the resolution fixtures.
- Today's model on the new structure: `examples/config.json` rewritten to teams →
  projects → envs → workloads plus a `users` branch; e2e shows a request refused by
  an env limit while its project has room, and by a project limit shared by two
  envs.
- A record for a group deleted mid-flight counts toward its surviving ancestors
  (kit test) and the gateway's counters settle it (gateway test).
- `scripts/check-all.sh` green 3×; images built and smoke-tested.

**Verification status** (2026-09-27):

- [x] Fixture parity, resolution fixtures included — both halves (STEP-2, STEP-4).
- [x] Today's model on the new structure: `examples/config.json` rewritten; e2e env
  limit refusing while the project has room, project limit shared by two envs
  (STEP-5, `TestGroupTreeEndToEnd`).
- [x] Record for a deleted group counts toward surviving ancestors — kit test
  (STEP-2). The gateway test was missing at 0.7.0 (found by the 2026-09-27 review,
  T1); now covered by `TestSettlementReachesAncestorsOfADeletedGroup`
  (group-tree-audit step 2).
- [x] `scripts/check-all.sh` green 3× in a row (STEP-6 result).
- [x] Images built on `dev` and smoke-tested, not pushed (STEP-6 result).
- [x] Docs on the group tree; backlog entry for cut-across memberships (STEP-6).

## Git

- Anchor tag `0.6.1` on `main` before step 1 (the world the plan starts from).
- Branch `group-tree`, worktree `.claude/worktrees/group-tree`; commit per step;
  rebase onto `main` and fast-forward merge after Phase 2.
