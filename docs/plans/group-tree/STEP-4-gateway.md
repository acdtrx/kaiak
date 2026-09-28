# Step 4 — the gateway

**Status:** done (2026-09-27)

## Intent

The gateway implements the contract: decodes and validates the group tree, resolves
each key to its path, enforces limits on every group of the path, writes usage
records with the path, matches totals by group, and labels metrics and logs by group.

## Files likely touched

- `gateway/internal/config/` — `document.go` (v2 document), `schema.go` (walker),
  `semantic.go` (tree rules, codes), `snapshot.go` (Group with parent pointer and
  depth, effective allowed models intersected down the path, effective limits with
  `child_defaults`; `Key.Group`; `GroupLabel` replaces `UserLabel`), `errors.go`.
- `gateway/internal/auth/` — identity = key ID + the key's group (path available).
- `gateway/internal/limits/` — `Scope` becomes global | group; counters keyed by
  (group ID or global, type, model set); the path's counters per request;
  `headers.go` (rejection carries the group ID for the log only); `persist.go`
  (file-mode snapshot format bump); totals matching by group.
- `gateway/internal/accounting/` — `UsageRecord.Groups` replaces `Owner`.
- `gateway/internal/control/` — `messages.go`, `schema.go`, `decode.go` (record
  `groups`, totals `group`), protocol version 2, last-known-good / totals cache /
  spool format bumps.
- `gateway/internal/metrics/` — `usage.go` labels `group`, `root_group`, `key_id`,
  `model`, `status`; `ops.go` rejection scope kinds `global`, `group`.
- `gateway/internal/server/` — `limits.go` refusal text (`group limit`), `api.go`
  log fields (`group` replaces `team`/`workload`/`user`; `limit_scope`,
  `limit_id`), `upstream.go`.
- `gateway/cmd/kaiak/` — anything wiring owners (seed-config checks, data-dir files).
- Unit tests across these packages (28 test files name owners today).

## Decisions for this step

- A counter's identity for a group is the group ID (unique across the tree); a
  config reload that keeps a group and its limit keeps the counter (as today).
- A request walks at most 8 groups + global; per-request limit work grows with
  the path, bounded by the depth rule.

## Acceptance criteria

- Every step-1 fixture passes with the right codes (config, messages, resolution,
  duplicate members).
- Tests: a request refused by an env-level limit while its project has room; two
  envs sharing a project limit; allowed models intersected down a path and "no
  restriction anywhere = every model"; `child_defaults` override and inheritance;
  refusal text `group limit` with no ID; log fields; usage labels with
  `group_label` on and off; data-dir files of the old version discarded and logged.
- `scripts/check-gateway.sh` green except the e2e package, whose reds are named
  (cleared by step 5).

## Result

**What changed, per package**

- `config`: format 2 document (`groups`, key `group`, `child_defaults`, `labels`,
  `global.metrics.group_label`; `teams`/`workloads`/`users`/`global.default_user`
  gone; `FormatVersion = 2`). Schema walker mirrors the JSON Schema, labels included
  (at most 16; key pattern; values 1–256 code points counted as runes, no C0/C1/DEL).
  Semantic rules: `key-group-unknown`, `group-parent-unknown`, `group-cycle`
  (each group on the cycle), `group-depth-exceeded` (each group past level 8, judged
  only where parents reach a top-level group), `allowed-model-unknown` /
  `allowed-models-wildcard-mixed` / `limit-model-unknown` / `limit-duplicate` on
  groups and `child_defaults`; `workload-team-unknown` and `key-owner-unknown` gone.
  The tree walk follows each group's parents once (memoized), so a long invalid chain
  costs linear time. Snapshot: `Groups map[string]*Group`; `Group{ID, Parent, Path,
  PathIDs, AllowedModels, Limits}` plus `Root()`; `Key.Group`; `GroupLabel` replaces
  `UserLabel`; `MaxGroupDepth = 8`.
- `auth`: `Identity{KeyID, Group}` (the group carries its path); model access from
  the group's effective set.
- `limits`: `Scope` is `global` | `group`; counter identity `(group, type, model
  set)` with `""` = global (group IDs are unique across the tree and never empty);
  counters per group (`byGroup`), a request's counters = global + every group of
  `Subject.Groups` (the path). `PushedWindow`, `CounterUsage`, `limits.json` and
  `totals.json` name a limit by `group` (omitted for global); `limits.json` 1→2,
  `totals.json` 1→2. `Rejection{Scope, ID}` keeps the group ID for the log only.
  Log attributes on limiter warnings: `scope` (kind) and `group`.
- `accounting`: `UsageRecord.Groups` (JSON `groups`, the path, top first) replaces
  `Owner`; `Request.Groups`.
- `control`: `ProtocolVersion = 2`; record schema `groups` (1–8 unique IDs); totals
  window `group?` (no `scope`/`id`), window identity for `totals-window-duplicate` by
  group; `last-known-good.json` 2→3; usage spool 1→2.
- `fakecontrol` (updated minimally, as `internal/control`'s tests import it): speaks
  protocol `2` (a `protocolVersion` constant). Nothing else changed there.
- `metrics`: usage labels `group`, `root_group`, `key_id`, `model`, `status` —
  `group` = last of the record's path, `root_group` = first; `group_label` off leaves
  `group` empty, `root_group` stays. Rejection scope kinds `global`, `group` (8
  series at 0 from startup).
- `server`: limiter subject from `identity.Group.PathIDs`; record `Groups` from the
  same path (the request's snapshot); refusal text says `group limit` / `global
  limit` (the scope kind, never an ID or label); request log field `group` (replacing
  `team`/`workload`/`user`); `limit_scope` `global`|`group`, `limit_id` the group ID
  or `global`.
- `cmd/kaiak`: totals conversion by `group`. Seed and data-dir behaviour need no
  change (format checks live in the packages above).

**Decisions made during the step**

- Effective allowed models are computed incrementally: a group's set is its parent's
  set narrowed by its own level (own list, else the parent's `child_defaults` list;
  `["*"]` or none = no narrowing). One shared every-model set per snapshot is the
  root. `ModelSet.All()` now means "no level restricted it".
- `Limit.Models` stays sorted in the snapshot (identity ignores order); the
  resolution-fixture test sorts each expected limit's models before comparing, and
  compares list order, values and types exactly.
- Counters exist only for groups with limits; a group with none adds nothing to a
  request's counter list.

**Tests added**

- `config`: `TestResolvedFixtures` (every `protocol/fixtures/config/resolved/*.json`:
  group set, path, allowed models or `"all"`, effective limits in order),
  `TestGroupTreeIssues` (cycle per member, orphan, depth; nothing below a cycle or an
  unknown parent), `TestChildDefaultsMergeUnderEachChild`,
  `TestChildOverrideMatchesOnTypeAndModelSet`, key → group → path checks.
- `limits`: `TestEveryGroupOnThePathIsEnforced` (env limit refuses while the project
  has room; two envs share the project's limit; a child's own limit replaces the
  default), `TestEveryScopeIsEnforced` over group/child_defaults/override scopes,
  `TestSharedStateOfAnotherVersionIsDiscarded` (totals.json v1), snapshot v1
  discarded and logged (`found_version=1`).
- `control`: `TestLastKnownGoodOfAnotherFormatIsDiscarded` (v2 file → seed boot,
  logged); spool discard test now writes the previous format.
- `metrics`: usage labels with `group`/`root_group`, intermediate levels absent,
  `group_label` on/off.
- `server`: `TestModelAccessIntersectsDownThePath` (listing and requests; no
  restriction anywhere = every model), `TestGroupLabelSwitchedOff`; refusal text
  `group limit` without ID or label, log fields `group`, `limit_scope`/`limit_id`,
  labels never in a log line or refusal (`TestLimitRefusalsLogTheLimit`).

**Suite**

- `go -C gateway test ./internal/... ./cmd/...`: all packages `ok` except
  `internal/config` — `FAIL TestExampleConfigs` only (`examples/` is still format 1;
  cleared by step 5). Every config, message, duplicate-member and resolution fixture
  passes.
- `scripts/check-gateway.sh`: stops at `go vet` — `kaiak/e2e` does not compile with
  the `crosshalf` tag (`e2e/sample_test.go:678: w.Scope undefined`). Run by hand
  past that point:
  - gofmt clean; `go vet -tags crosshalf` and staticcheck 2026.2.1 on every package
    but `e2e`: clean.
  - `go test -race ./...`: every package `ok` except `internal/config`
    (`TestExampleConfigs`) and `e2e` (26 tests fail: their configs are format 1, the
    gateway exits at startup with `config rejected`).
  - live-test kit: gofmt, vet, staticcheck clean; `go run . -self-test` fails — its
    generated config is format 1 (`config rejected: /format_version: must be 2`).
- Expected reds, all cleared by step 5: `internal/config` `TestExampleConfigs`;
  the `gateway/e2e` package (compile error under `crosshalf`, 26 failing tests
  without it); the live kit's self-test.

**Contract issues found**

- None requiring a change. Note only: resolution fixtures write limit `models` as in
  config (`["b", "a"]`); the gateway compares them order-ignored, as limit identity
  does.
