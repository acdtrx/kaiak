# Step 5 — gateway

**Status:** done (2026-10-06)

## Intent

Order totals by the store's sequence within the config epoch, and remove what
existed only to tell control-plane processes apart. Bring the gateway's limits to
step 3's shape: no model sets, a limit identified by its scope and type.

## Files likely touched

- `gateway/internal/control/messages.go`: `Revision` is the integer. Totals apply
  only when their `config_epoch` is the running config's, and then only with a higher
  sequence (decision 10 as changed in step 1).
- `gateway/internal/control/client.go`: the retired-process list and its log lines
  go. A totals message from an older sequence is ignored, with a debug line if one
  exists today.
- `gateway/internal/control/schema.go`: the totals schema.
- `gateway/internal/fakecontrol/`: speaks the new revision; it can stand in for two
  processes over one sequence where the tests need it.
- `gateway/internal/config/` and `gateway/internal/limits/`: limits without `models`;
  identity (scope, type); one limit per type per scope (config refuses a repeat);
  `child_defaults.limits` merged by type; totals windows without `models`; the
  file-mode usage snapshot and the totals cache keyed by the new identity (their
  format versions bump if their shape changes).
- Tests: ordering within an epoch; another epoch's totals not applied; an older
  sequence from any connection ignored (push read before ack, ack before push);
  `counted_through` handling unchanged; limits by scope and type, a scope's usage
  counted whatever the model; an edited limit value keeping its window.

## Decisions made during planning

- **`counted_through`'s rule is unchanged.** Any totals message, newer or not, stops
  counting the batches it covers as the gateway's own. It never depended on which
  process sent it.
- **The data directory's totals cache** holds no revision, but its counters are
  keyed by limit identity, which step 3 changes: its format bumps with the snapshot's
  if the stored shape changes.

## Acceptance criteria

- `scripts/check-gateway.sh` green.
- `scripts/check-all.sh` green, the cross-half e2e included: **phase 1 ends here**,
  committed.

## Result

**What changed** (`b44b13b`)

- **Totals ordering** (`internal/control/`):
  - `Revision` is the integer.
  - `takeTotals` applies totals only when their `config_epoch` is the epoch of the
    config in force (`appliedEpoch`), and then only with a higher revision; the first
    in an epoch whatever its revision. The "from another config epoch" line logs
    `kaiak.totals.config_epoch` and `kaiak.config.epoch`, as the spec's table says.
  - `counted_through` counts whatever the epoch, unchanged.
  - The retired-process list and its two log lines are gone.
  - The totals schema takes an integer `revision` and windows without `models`. The
    window identity rule is (group, type).
- **The fake control plane:** an integer revision, one sequence per config epoch.
  `Restart` starts a new epoch at 0; `Revision()` returns the sequence.
- **Limits without model sets** (`internal/config/`, `internal/limits/`):
  - The limit schema has no `models`; `limit-model-unknown` is gone; `limit-duplicate`
    is by type.
  - `child_defaults` limits merge by type, and the effective-limits count follows.
  - `config.Limit` has no `Models` or `Covers`.
  - Counters are keyed by (group, type). The reload's carry-over (`carryOver`,
    `dropBucket`, its log line) is replaced by `newCounter`: a limit the old config
    lacked starts empty.
  - Every limit of a scope applies to every request; USD limits still only to priced
    ones.
  - The per-minute share warning checks every model's default output.
  - `PushedWindow`, the snapshot and the totals cache lose `models`. `limits.json` and
    `totals.json` are format 3.
- **Tests:**
  - Ordering within an epoch.
  - `TestTotalsFollowTheRunningConfigEpoch`, replacing the replaced-process test:
    another epoch's totals wait for its config, a delayed answer from the old store
    is ignored, and `counted_through` counts across.
  - Every limit of a scope counting every model.
  - An edited limit value keeping its window, in file and control-plane mode.
  - Merging by type.
  - The global-scope log line through the share warning.
  - The fixture totals pushed in the running config's epoch.
- **e2e:** every limit counts every model of its scope, so the checks that spend a
  limit got keys of their own.
  - `testConfig` gains the group `metered` (2 requests a minute, key `k-rpm`) and the
    group `budgeted` (0.0001 USD a month, key `k-budget`); global and `eval` carry no
    limits.
  - The group-tree test's limits are one per type: env 2 a minute; project 4 a minute
    and the budget. Its refusals are recounted (2).
  - The cross-half config's research budget counts only priced models, with "chat"
    unpriced there so its total is exact. `eval` has 1000 a minute.
  - The seed config drops the budgeted group.
- **Live kit:** a second key and group, `live-metered`, holds the 1-request-a-minute
  limit the rate-limit checks spend (chat and Messages). The runbook says so.
  `-self-test` passes for every kind.

**Deviation:** totals of another config epoch are compared against the epoch of the
config *applied*, not the latest taken. A rejected config's epoch therefore never lets
its totals apply, which the spec's "the config epoch the gateway runs" means.

**Spec/code:** no disagreement found. The log lines and fields the gateway emits match
`GATEWAY.md` (no `kaiak.limit.models`, `kaiak.totals.control_plane` or carry-over line
remains).

**Suite** (2026-10-06): `scripts/check-all.sh` passed in full. That covers gofmt, vet,
staticcheck, `go test -race` with the gateway e2e, the live kit's lint and self-test
(all seven configurations), control `npm test` (593 pass, 0 fail) and lint, and the
cross-half e2e. **Phase 1 ends green.**
