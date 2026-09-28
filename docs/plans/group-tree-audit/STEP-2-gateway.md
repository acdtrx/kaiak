# Step 2 — gateway

**Status:** done (2026-09-27)

## Intent

The gateway's share of the audit: linear reloads, the limits bound, the metric
label, and the missing tests.

## Files likely touched

- `gateway/internal/limits/limits.go` — R1: in `sync`, index the dropped counters by
  (group, type) once after the exact matches; `carryOver` reads its bucket. Same
  outcome rules (most used wins, taken whole or copied, ambiguity warned).
- `gateway/internal/config/snapshot.go` — R4: `mergeLimits` with precomputed
  identities and a map; `semantic.go` — R2 rule `effective-limits-exceeded`.
- `gateway/internal/metrics/usage.go` (+ tests, e2e assertions that name the label) —
  S4 `key_group`.
- Tests:
  - T1 — `limits`: tree team → env → workload with limits on each; reserve on the
    path; swap to a config without `workload`; settle N tokens; team and env windows
    hold N. Shared-mode variant.
  - T3/T4 — `server`: group-scoped token refusal (full window and oversize request)
    and group `budget_unavailable`: body says `group limit`, carries no group ID or
    label; a global refusal says `global limit`.
  - T5 — `limits`: groups A and B each with `tokens_per_hour` on `["m1"]`, A with
    400 used; reload dropping A's limit and changing B's set to `["m1","m2"]`; B
    starts at 0.
  - T6 — `control`: literal version-1 spool index and one `usage-batch-*.json` both
    discarded, no record without `groups` sent.
  - R1/R4 regression: a test with a large tree asserting the reload's work is linear
    by counting comparisons or bounding time generously is flaky — prefer a
    benchmark (`BenchmarkReloadCarryOver`) plus the measured numbers in the result.

## Acceptance criteria

- The audit's mutations for T1, T3, T4, T5, T6 re-run: each now fails a test
  (record which), then reverted.
- R1 measured: model-set edit of a default with 40k children — the reload's lock
  time well under 100 ms (was 36 s). R4: 10k × 10k merge well under 1 s (was 9.9 s).
- Step-1 fixtures pass. `scripts/check-gateway.sh` green.

## Result

- **R1** `limits.go` `sync`: after the exact matches, the dropped counters are
  indexed once by (group, type); `carryOver` takes its bucket as the predecessors.
  Outcome rules unchanged (most used wins, taken whole if unclaimed else copied,
  pushed base carried, several predecessors warned); the existing carry-over tests
  pass unchanged. Kept as found: a copy made from a predecessor another new limit
  already claimed reads the pushed base under the claimer's (new) key, as before.
- **R4** `snapshot.go` `mergeLimits`: the overrides' identities computed once into
  a map (first override of an identity wins, as before); each default looks up its
  own.
- **R2** `semantic.go`: `effective-limits-exceeded` (code in `errors.go`, bound
  `MaxEffectiveLimits = 50_000` beside `MaxGroupDepth`), reported once at path `""`.
  `countEffectiveLimits` counts without building: global + per group its own limits
  plus its direct parent's `child_defaults.limits`, minus each own limit a default
  takes (first own limit of a default's identity — what `mergeLimits` does); the
  parent's default identities are built once per parent; an unknown parent counts
  own limits alone. Tests: `TestCountEffectiveLimits` (override, extra, orphan,
  grandchild), `TestEffectiveLimitsExceededAtRoot` (at the bound accepted, +1 one
  issue at `""`); the step-1 fixtures pass.
- **S4** usage label `group` → `key_group` (`metrics/usage.go`, its comments with
  the reason, `Snapshot.GroupLabel` comment); assertions in `metrics_test.go`,
  `server/metrics_test.go` (the switched-off check now looks for `key_group=`),
  `retry_test.go`, `e2e/e2e_test.go`, `e2e/grouptree_test.go`. `group_label`
  semantics unchanged.
- **Tests**
  - T1 `limits` `TestSettlementReachesAncestorsOfADeletedGroup` (file and shared):
    team → env → workload, reserve 5000, reload without `workload` (the limiter
    syncs: the reservation still held on team and env), settle 180 → team and env
    hold 180.
  - T3/T4 `server` `TestRefusalsNameTheScopeKindOnly`: group token limit spent,
    group request too large, group `budget_unavailable`, and the same three for
    global; each body names the scope kind and never `research`, `eval` or the
    group's label value.
  - T5 `limits` `TestCarryOverStaysWithinItsGroup`: team's `["m1"]` limit with 400
    used is dropped while ann's changes to `["m1","m2"]` → ann at 0.
  - T6 `control` `TestFormatOneSpoolIsDiscardedWithItsBatches`: literal version-1
    index and a `usage-batch-*.json` whose record has `owner`; both discarded
    (two discard log lines), the batch file gone, the first batch sent is a new
    epoch with only the new record, which carries `groups`.
- **Mutations re-run** (each reverted; `git diff` clean of it):
  - T1 — `Settle` returns early when a held counter is no longer the live one:
    `TestSettlementReachesAncestorsOfADeletedGroup/{file,shared}` fail (team and
    env stay at 5000).
  - T3 — group ID added to the tokens, too-large and `budget_unavailable`
    messages: `TestRefusalsNameTheScopeKindOnly/group_tokens_spent`,
    `/group_request_too_large`, `/group_budget_unavailable` fail (body names
    `research`), plus the global too-large and budget cases.
  - T4 — the requests message forced to "group": `/global_requests` fails;
    too-large and `budget_unavailable` forced to the group scope:
    `/global_request_too_large`, `/global_budget_unavailable` fail.
  - T5 — the drop buckets keyed by type only (the group condition dropped):
    `TestCarryOverStaysWithinItsGroup` fails (ann 400).
  - T6 — `spoolFormat` back to 1: `TestFormatOneSpoolIsDiscardedWithItsBatches`
    fails (the old batch is sent and breaks the protocol: no `groups`).
- **Measurements** (throwaway tests on this machine; before = the step-1 commit's
  code, same harness):
  - R1, a `users` group with N children, `child_defaults` model-set edit, the
    reload's `sync` under the lock: 10k children 1.92 s → 6–11 ms; 40k children
    35.4 s → 45–65 ms (allocation-bound, linear: 20k 33 ms, 40k 65 ms in one run).
    Kept `BenchmarkReloadCarryOver` (40k children): ~37 ms/op.
  - R4, `mergeLimits` 10k defaults × 10k own limits (no shared identity, the
    old worst case): 6.1 s → 1.3–1.6 ms.
  - The bound in action: the throwaway reload test at 80k children was refused by
    `effective-limits-exceeded`.
- **Suite**: `scripts/check-gateway.sh` — gofmt, vet, staticcheck clean; race tests
  all `ok` (`e2e` 92 s, `config`, `control`, `limits`, `metrics`, `server`, …);
  live-test kit self-test 13/13/13/16 passed; "gateway checks passed".
