# Step 4 — e2e, sample and docs

**Status:** not started

## Intent

Close the phase: outcomes and refusals travel end to end through both halves, the
sample shows them, and operators and app authors know what changed.

## Files likely touched

- `gateway/e2e/` (cross-half, build tag `crosshalf`): a failed request and refused
  requests (an unknown key, a limit) reach the sample control plane's store with
  their `status`/`error_code` and counts.
- `control/sample/`: the status page shows `status`/`error_code` in its recent
  records and a small table of recent refusal counts (key, group, model, code,
  count, last time); escaping as for everything else on the page.
- `docs/DEPLOYMENT.md`: upgrading to protocol 4 — both halves together; flush the
  spool first when a data directory is used; an app's store must keep refusal
  counts.
- `README.md` if it describes what usage records hold.
- `docs/BACKLOG.md`: "Errors and refusals per team" — the control-plane part is
  built; keep only the metrics option (group labels on the gateway's error counters)
  with its revisit trigger.
- `docs/ARCHITECTURE.md` / `docs/architecture/` if they describe the usage flow's
  content.

## Decisions made during planning

- None beyond the overview.

## Acceptance criteria

- The cross-half scenario passes; the sample page renders the new data (a test, as
  for its other sections).
- **Phase end:** `scripts/check-all.sh` green 3× in a row, output recorded.

## Result

(to be filled when the step is done)
