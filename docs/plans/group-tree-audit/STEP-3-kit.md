# Step 3 — kaiak-control and the sample

**Status:** done (2026-09-27)

## Intent

The kit's share of the audit: linear carry-over and tree validation, the limits
bound, the deep-path counting test, and two small sample fixes.

## Files likely touched

- `control/kaiak-control/src/usage/aggregate.ts` — R1: identities computed once;
  dropped limits grouped in a `Map` by (group, type).
- `control/kaiak-control/src/config/tree.ts`, `semantic.ts` — R3: memoized ancestry
  (each group's outcome computed once), linear like the gateway's `tree()`; R2 rule
  `effective-limits-exceeded` (computed without materializing every child's limits).
- `control/kaiak-control/src/fastify/index.ts` — R6's stale comment (the record size
  with an 8-group path; the limit's headroom).
- `control/kaiak-control/src/usage/usage.test.ts` — T2: a 4-level path (team →
  project → env → workload, each with an hour and a month limit) counts toward all
  four and global; a middle group without limits is skipped harmlessly.
- `control/sample/src/keygen/index.ts` — refuse a `--group` that is not a valid ID
  before minting (export an `isGroupId`-style check from the kit, or reuse the ID
  checker).

## Acceptance criteria

- The audit's T2 mutation now fails a test (recorded, reverted).
- R1 measured: 20k children, model-set edit of a default — carry-over well under
  100 ms (was 25 s). R3: 20k chain and 20k cycle validate well under 1 s (were 9 s /
  18 s).
- Step-1 fixtures pass. `npm test` and `npm run lint` green.

## Result

- **R1** `usage/aggregate.ts` `carryOvers`: each identity computed once; dropped
  limits grouped in a `Map` by (group, type); a carry's `from` is that shared,
  read-only list. Same carries as before (all carry-over tests unchanged and green).
- **R3** `config/tree.ts`: `ancestryOf` replaced by `ancestries(groups)` — every
  group's outcome (`rooted` with its depth, `parent-unknown`, `cycle`,
  `below-broken`) settled once, walks stop at the first settled group (the gateway's
  `tree()` shape); `pathOf` gives a rooted group's path. `semantic.ts` reports from
  the map (same codes, paths and order: existing tree tests and fixtures pass);
  `resolveScopes` uses it too and refuses a group deeper than 8 as well as a broken one.
- **R2** `semantic.ts`: rule `effective-limits-exceeded` (constant
  `MAX_EFFECTIVE_LIMITS = 50_000`, named once), reported once at path `""` (message
  without the `path: ` prefix there). Counted without merging: per group its own
  limits plus its direct parent's `child_defaults.limits` identities none of its own
  has (one identity set per parent); a parent with no entry adds nothing; cycles and
  depth don't matter. New test in `config.test.ts` (at-bound fixture + a new child,
  an unknown-parent group without / with a limit, a self-cycle with a child) pins
  code, path `""` and the counting rule.
- **R6 comment** `fastify/index.ts`: a record with every field at its longest (8-group
  path of 128-character IDs, 512-character backend model name) is about 2.9 KB, a
  full batch about 0.69 of the 2 MiB limit; only six-byte-escaped backend model names
  (`<`, `>`, `&`) take it past (up to about 1.3×). Numbers recomputed with a throwaway
  script (2865 B / 5425 B per record → 0.683 / 1.293).
- **S6** `resolveScopes` comment: the order is `Object.entries`' — member order, but
  integer-like group IDs first in ascending numeric order.
- **T2** `usage.test.ts`: "a deep path counts toward every listed group with limits,
  and global" — team → project → region (no limits) → env → workload, each limited
  level with an hour and a month limit; asserts the exact windows (global + 4 × 2).
- **Keygen**: kit exports `isConfigId` (the config `id` definition; `createKey` uses
  it); `runKeygen` refuses an invalid `--group` before minting
  (`"<id>" is not a valid group ID`). Tests in `keys.test.ts` and `keygen.test.ts`.

### Mutation re-run (T2)

`batchAdditions` counting only the first and last listed groups: `npm test` → 1
failing, `aggregation › a deep path counts toward every listed group with limits, and
global`. Reverted.

### Measurements (throwaway script, deleted; same machine, old = `main` at 0.7.1)

| Scenario | Before | After |
|---|---|---|
| R1 `carryOvers`, 20k children, model-set edit of a default (20 000 carries) | 24 750 ms | 18.9 ms |
| R3 `validateConfig`, 20k chain (19 992 issues) | 11 367 ms | 62.3 ms |
| R3 `validateConfig`, 20k cycle (20 000 issues) | 20 598 ms | 39.9 ms |

### Suite

From `control/`: `npm test` — `tests 510, pass 510, fail 0` (the step-1 red
`effective-limits-exceeded.json` cleared); `npm run lint` — `tsc` clean,
`boundaries ok`.
