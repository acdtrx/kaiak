# Step 9 — broadcast-only gateway

**Status:** not started

## Intent

Implement step 7's contract in the gateway, removing every gateway-side item on the
removal checklist. This step ends phase 3: the whole suite green, and the removal
checklist clean across the repo.

## Files likely touched

- `gateway/internal/control/`:
  - **Boot:** from the stream's first `config` event; the snapshot fetch is removed.
  - **Config events:** every `config` event is applied, and one whose `config_hash`
    equals the running config's is skipped.
  - **Status:** reports `applied_config_hash` and the rejected hash.
  - **Totals:** applied from the stream only; no ordering by revision or epoch; no
    retired-store list.
  - **Acks:** they only drop the batch from the spool.
  - **Own usage:** retired only by a stream totals message whose `counted_through`
    covers it.
- `gateway/internal/limits/`:
  - Windows are applied to the running limits by (scope, type), whatever config.
  - The config-mismatch state, its grace, metric and log lines are removed.
  - The outage and "no totals yet" refusals stay.
  - Totals cache format bump.
- `gateway/internal/state/` (`last-known-good.json`): config plus hash, format bump.
- `gateway/internal/metrics/`: `kaiak_control_config_mismatch` removed.
- `gateway/internal/fakecontrol/`: speaks the new stream and ack.
- `gateway/e2e/` and the cross-half tests: version, resync and mismatch expectations
  removed. Add:
  - a restored store with an older config is applied;
  - a rejected config keeps budgets enforced from stream totals;
  - an ack carries no totals.
- The live kit, if it reads versions.

## Acceptance criteria

- `scripts/check-all.sh` green, including both cross-half tests: **phase 3 ends
  here**.
- Step 7's removal checklist greps clean across the whole repo, outside `docs/plans/`
  and `docs/reviews/`. Every grep and its output is recorded in Result.
- No test was weakened to pass. Each deleted test is listed in Result with the removed
  behaviour it asserted.

## Result

(filled in when the step is done)
