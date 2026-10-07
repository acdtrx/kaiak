# Step 19 — no data directory: the gateway, and counter lifetime

**Status:** not started

## Intent

Remove the data directory from the gateway as step 18's text states it (decision 35),
and fix D-H3 (decision 36). This is a removal step: nothing removed survives under
another name, and tests asserting removed behaviour are deleted.

## Scope

- **Remove:**
  - `KAIAK_DATA_DIR` and its loading;
  - the on-disk spool (`spooldisk.go`, the index, formats, restore, other-instance
    batches);
  - `lastknowngood.go` and the last-known-good boot;
  - `totals.json` (`limits/persist.go`) and the periodic writer;
  - `limits.json` (`limits/snapshot.go`, file mode);
  - `RestoreSpooled`, `RestoreOwn` and `SpoolCovered`;
  - `internal/state` if nothing else uses it;
  - the lock file;
  - `persistMu`;
  - data-directory metrics and log fields;
  - tests of all of it (`datadir_test.go`, `restart_test.go`, the data-directory steps
    in `e2e`).
- **Keep and simplify:**
  - the in-memory usage queue (`spoolmemory.go`) and the drain flush;
  - the acknowledged-batch memory for the stale-totals signal (at most 10 000);
  - a fresh batch epoch at every start;
  - the seed config.
  - `stateless_test.go` (cmd and e2e) becomes the only mode. Fold it into the ordinary
    tests where that reads better.
- **Decision 36 (D-H3):**
  - Counters carry a reference count of the running requests that hold a reservation
    on them, of any amount, zero included.
  - A retained counter is pruned only when it has no reference and no own usage in
    its current window.
  - The reference outlives a window roll-over.
  - A recreated scope takes back the same counter.
- **Regression tests:**
  - Port Codex's D-H3 reproductions from the session scratchpad
    (`audit-d4/limits_audit_d_test.go`: `TestAuditDRemovedScopeKeepsZeroAmountReservations`
    and `TestAuditDRemovedScopeReservationSurvivesWindowRollover`).
  - The rollover one used file-mode snapshot pruning. Re-express it without the
    snapshot, through whatever path prunes now.
  - Rename both to describe the behaviour. Each fails before its fix; say so in
    Result.
  - The data-directory reproductions (D-H1, D-H2, G4-M1) are not ported: their code
    is removed.
- **Images:** `scripts/smoke-images.sh` and the Dockerfile follow, if they mention a
  data directory.

## Files likely touched

`gateway/cmd/kaiak/**`, `gateway/internal/{control,limits,state,metrics,config}/**`,
`gateway/e2e/**`, `scripts/live/process.go`, `scripts/smoke-images.sh`,
`gateway/Dockerfile`, and the control sample's page comment if it names
last-known-good.

## Acceptance criteria

- `scripts/check-gateway.sh` (uncached) and `scripts/check-all.sh` pass once.
- Step 18's checklist greps clean in code, with the commands and their output recorded
  in Result.
- Record the line count removed.

## Result
