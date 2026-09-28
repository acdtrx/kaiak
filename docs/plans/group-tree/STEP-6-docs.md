# Step 6 — docs and proof

**Status:** done (2026-09-27)

## Intent

Every doc that explains the system describes the group tree; the proof runs.

## Files likely touched

- `docs/ARCHITECTURE.md` — package descriptions naming owners, the data flow.
- `docs/DEPLOYMENT.md` — config for many hosts (examples), metrics cardinality
  (`group`, `root_group`, `group_label`), alerts naming labels.
- `docs/architecture/gateway.html`, `docs/architecture/control-plane.html` —
  limits sections, budgets diagram wording, any owner naming.
- `control/kaiak-control/GUIDE.md` — config building (groups, keys by group), the
  totals-vs-limits snippet (the new resolver), store `WindowKey` notes, protocol 2.
- `README.md`, `docs/testing/LIVE-BACKENDS.md`, `docs/TECH-STACK.md` where they name
  owners.
- `docs/BACKLOG.md` — new entry: cut-across memberships (a group counting toward a
  second parent), revisit trigger: a team needs a hard cap on a project across its
  environments *and* team-wide environment budgets at once.

## Acceptance criteria

- `grep` for `team`, `workload`, `default_user`, `user_label` across docs, code and
  examples finds only the new meaning (e.g. a `team` label in an example) or history
  in `docs/plans/` and `docs/reviews/`.
- `scripts/check-all.sh` green three times in a row; outputs recorded.
- Images built on the `dev` context and smoke-tested (`scripts/build-images.sh`,
  `scripts/smoke-images.sh`); not pushed unless the user asks.
- OVERVIEW's verification status filled in.

## Result

**Docs changed**

- `docs/ARCHITECTURE.md` — `config` (snapshot resolves each group's path, allowed
  models, effective limits), `auth` (key → key ID, group and path), `limits`
  (counters per limit of global and each group; a request checks global and its
  path), `accounting` (records name the group path), kit `config` (`resolveScopes`),
  `config-versions` (refuses a changed parent), `usage` (windows of global and each
  path group the current config defines), sample `page` (group tree, totals per
  scope, usage by path) and `keygen` (`{ hash, group }`). Mermaid unchanged (no
  shape change).
- `docs/DEPLOYMENT.md` — record-size note, month-reset example, admin-port
  exposure, cardinality (`group`, `root_group`, `group_label`; 216 series with
  `group_label` off, as `GATEWAY.md`'s worked example), request log `group`,
  global-limit alert text (`scope_kind="group"`, `limit_id`), sample page and data
  directory contents, sample image keygen `--group` (was still `--user`).
- `docs/architecture/gateway.html` — package map label and table (auth, limits,
  config, accounting), pipeline "1 · Auth" label and stage list (Auth, Limits),
  Limits intro rewritten for the group tree (scopes = global + path, allowed models,
  `child_defaults`, refusal text), totals caption, records carry `groups`, usage
  metric labels and switches, seam ⑥/⑦ wording, backlog snapshot table (fair
  dispatch across keys and groups; new cut-across row; date 2026-09-27), footer.
- `docs/architecture/control-plane.html` — "Who owns what" (config, usage, keys),
  `Kaiak-Protocol: 2` (callout and endpoints figure), config-push caption
  (`group-parent-changed`, control plane only), a "Totals per scope" bullet, a new
  figure "Budgets on the group tree" (a record's path counted toward global and each
  group; a project limit shared by two envs), internals label `resolveScopes`,
  footer.
- `control/kaiak-control/GUIDE.md` — header (format 2, protocol 2), window identity
  `(group, type, models, windowStart)` and the SQL sketch's key, record `groups`,
  config top level and a group-tree summary, publish flow (`group-parent-changed`),
  keys by `group`, the state table (`resolveScopes`), the totals-vs-limits snippet
  (type-checked against the real exports in a scratch file under `tsc` with the
  project's settings, then removed), three new "look reasonable and are wrong" items
  (moving a group, meanings for levels, adding children's windows to a parent's).
- `docs/TECH-STACK.md` — aggregation "per group and global".
- `docs/BACKLOG.md` — new Limits entry **Cut-across memberships** with its revisit
  trigger (ruled 2026-09-27: tree only); fair-dispatch entry and prompt-logging entry
  reworded from owners/team to keys/groups.
- `README.md`, `docs/testing/LIVE-BACKENDS.md`, `docs/kaiak.md` — already on the
  group tree (steps 1, 5); nothing stale.
- `gateway/internal/server/routing_test.go` — test renamed
  `TestModelListDependsOnTheKeyGroup` (was `…KeyOwner`).

**Specs vs implementation**: no inconsistency found; nothing changed in the specs.

**Grep** (`team`, `workload`, `default_user`, `user_label`, `key-owner-unknown`,
`resolveScopeLimits`, `Kaiak-Protocol: 1`, `format_version": 1`, outside
`docs/plans/`, `docs/reviews/`): every remaining hit is the new meaning — group IDs
and `kind` labels in fixtures, examples and tests (`team1`, `t`/`w`, `research`
"team", `eval` "workload"), level names in prose examples (team → project → env →
workload), Kubernetes "workload kind", "cloud workload identity", "another team's
cluster"; or the old format on purpose — `format-version-1.json`, the
`owner-field.json` / `global-default-user.json` / `key-user-field.json` invalid
fixtures, `cases.json`'s reason, the discard tests' previous-format files
(`seed_test.go` last-known-good v2, limits/usage previous-version comments),
`snapshot_test.go`'s truncated-JSON case, and keygen's refused `--workload`.
Browser check: both pages opened at 1100×900 in Chrome; a script compared every SVG
text's box with its enclosing rect and the viewBox — no overflow; the new tree
figure and the package map screenshotted and read.

**Suite** — `GOFLAGS=-count=1 scripts/check-all.sh` three times in a row, each exit 0:

| Run | Duration | gateway e2e | control `npm test` | cross-half e2e | last line |
|---|---|---|---|---|---|
| 1 | 144 s | `ok kaiak/e2e 92.314s` | 504 / 504 pass | `ok kaiak/e2e 43.876s` | `all checks passed` |
| 2 | 154 s | `ok kaiak/e2e 91.607s` | 504 / 504 pass | `ok kaiak/e2e 53.758s` | `all checks passed` |
| 3 | 142 s | `ok kaiak/e2e 91.463s` | 504 / 504 pass | `ok kaiak/e2e 43.892s` | `all checks passed` |

Every run also: every gateway package `ok`, live-kit `self-test passed for vllm,
openai, azure-openai, vllm with two backends`, `gateway checks passed`, `boundaries
ok`. No flakes; no stray processes afterwards.

**Images** (context `dev`, builder `dev`; built from this step's working tree before
its commit, so tagged `-dirty` — the uncommitted diff was docs and one test name):

- `scripts/build-images.sh`: `registry.example.com/kaiak/kaiak:0.6.1-5-gb07ca5f-dirty`
  (18.9 MB) and `…/kaiak-sample:0.6.1-5-gb07ca5f-dirty` (373 MB).
- `scripts/smoke-images.sh --gateway … --sample …`: file-mode gateway ready, chat
  `200`, `kaiak_build_info{version="0.6.1-5-gb07ca5f-dirty"}`, clean stop; sample +
  control-plane-mode gateway (keygen `--group demo-app`) ready, chat `200`, `usage
  flushed` on stop; `smoke passed`, cleanup done. Not pushed.
