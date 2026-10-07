# Step 19 — no data directory: the gateway, and counter lifetime

**Status:** done (2026-10-07)

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

**Commits:**
- `0a00dc6`: the gateway (`cmd/kaiak`, `internal/{control,limits,metrics,config,server}`,
  `e2e`), `scripts/live`, three stale comments on the control side;
- `148c587`: the metric rename in `GATEWAY.md` and `DEPLOYMENT.md`;
- this Result.

**Size:** `git diff --shortstat 5b32ee6 HEAD -- gateway`: 58 files, 635 insertions,
3 645 deletions (the whole change: 65 files, +650, −3 660).

**What changed**

- **Removed:**
  - `internal/state` (the data directory, its lock, versioned files);
  - `control/spooldisk.go`, `spoolmemory.go`, `spool.go` and the `batchStore`
    interface with its single remaining implementation;
  - `lastknowngood.go`, `TriggerLastKnownGood`, `Options.Dir`;
  - `RestoreSpooled`, `SpoolCovered`, the spool index (acknowledged sequences,
    `pruneAcknowledgedEpochs`), set-aside files and `rejectedKept`;
  - `limits/persist.go` (`totals.json`, `SaveShared`, `LoadShared`, `RestoreOwn`,
    `OwnBatch`), `limits/snapshot.go` (`limits.json`, `SnapshotInterval`);
  - `Totals.CountedThrough` and `CountedBatch` in the limiter, `countedThrough`,
    `totalsKnown` (now `firstClosed`), `countOf`, `window.settled`;
  - `main.go`: `KAIAK_DATA_DIR`, the restores, periodic and shutdown writes,
    `batchPositions`;
  - the `spool_unwritable` drop reason and `boundSealed`; the `last-known-good` config
    trigger label.
- **The in-memory queue** (`control/queue.go`): `queuedBatch` holds its records;
  `queueSealed` checks, numbers and queues sealed batches under `queueMu`;
  `boundQueued` bounds the queued bytes. The epoch is taken at `New`. An invalid record
  and a refused batch are dropped and logged. The acknowledged list is plain
  `ackedBatch { id, generation, ackedAt }`, forgotten once covered, at most 10 000.
- **Boot:** the stream's config, else the seed when the control plane is unavailable,
  else exit; the exit messages no longer name a last-known-good config.
- **Decision 36 (D-H3):** `counter.refs` counts the running requests holding a
  reservation on a counter (`Reserve` adds one per counter, `Settle` takes it back);
  `pruneRetainedLocked` drops a retained counter only with no reference and nothing in
  its current window.
- **Metric rename** (the coordinator's addition, decided by the user):
  `kaiak_usage_spool_batches` → `kaiak_usage_queue_batches`,
  `kaiak_usage_spool_records` → `kaiak_usage_queue_records`;
  `UsageObserver.UsageSpoolDepth` → `UsageQueueDepth`; the metric struct fields; the
  `GATEWAY.md` table and alert note, `DEPLOYMENT.md`'s alert and its migration note.
- **Tests:**
  - deleted with their behaviour: `cmd/kaiak/datadir_test.go`,
    `control/restart_test.go`; the last-known-good, spool-file, spool-restart,
    spool-format, other-instance-spool, unwritable-spool and refused-batch-file tests
    (`client_test`, `seed_test`, `usage_test`, `memory_test`); the limits snapshot and
    `totals.json` tests and benchmark; the e2e restart-from-disk steps
    (`TestRestartWithTheControlPlaneDownKeepsASpentBudget`, the sample e2e's outage
    restart, the control e2e's kill-and-restart and last-known-good boot);
  - rewritten for the one mode: the in-memory tests lose "without a data directory";
    the control e2e's lost ack is resent within the process; the file-mode e2e restart
    now asserts the budget counts from zero; `TestPushCountingTheOutstandingBatch`
    waits for the ack and asserts nothing newly counted (it raced the ack before, and
    a covered batch is no longer remembered).
- **Regression tests (D-H3)** in `limits_test.go`, ported from [D]'s
  `limits_audit_d_test.go`:
  - `TestARunningRequestHoldsTheCountsOfADeletedGroup` (both modes; [D]
    `TestAuditDRemovedScopeKeepsZeroAmountReservations`);
  - `TestARunningRequestHoldsADeletedGroupsCountsAcrossTheHour` ([D]
    `…ReservationSurvivesWindowRollover`, re-expressed: the pruning reload is a config
    swap in the new hour instead of the removed snapshot write).
  - Both fail with the fix reverted in place (`if c.w.used == 0`): "re-created group's
    month 0 nano-USD, want the 1 USD its running request settled", "request admitted
    against the re-created group's spent budget" (both modes), "re-created group's
    hour 0 tokens, want the 100 settled in the new hour". Both pass with it.
  - Not ported: D-H1, D-H2 and G4-M1, whose code is removed.

**Decisions made in this step**

- **The batch store interface is removed, not kept with one implementation:** the
  queue holds the records itself, so there is nothing to abstract.
- **`queueMu` serializes sealing**, so the record checks run outside `u.mu`, off the
  lock `Record` takes on the request path. The memory bound counts the queued bytes;
  a sealed batch is checked and queued in the same step, so no sealed bytes wait
  uncounted beyond one seal. `GATEWAY.md:2078` says "queued and sealed records";
  accurate enough, since sealed ones are queued at once, and left as written.
- **The start logs `usage batches kept in memory until acknowledged`** with the epoch,
  as the in-memory mode did.

**Removal checklist** (`git grep -n -E <pattern> -- . ':!docs/plans' ':!docs/reviews'`):

```
KAIAK_DATA_DIR|DataDir|dataDir|data director|data dir
  docs/DEPLOYMENT.md:949, 954    the migration note (allowed)
  docs/specs/GATEWAY.md:1835     the dated Rejected line (allowed)
last-known-good|lastKnownGood|LastKnownGood|lastknowngood
  docs/DEPLOYMENT.md:953         the migration note (allowed)
  docs/specs/GATEWAY.md:1835     the Rejected line (allowed)
totals\.json|limits\.json|last-known-good\.json
  docs/DEPLOYMENT.md:953         the migration note (allowed)
  protocol/fixtures/config/invalid/cases.json:286   "key-with-limits.json", a fixture name
spooldisk|batchStore|usage-spool|usage-rejected|rejectedKept
  docs/DEPLOYMENT.md:952         the migration note's file list (allowed)
RestoreSpooled|RestoreOwn|SpoolCovered|saveShared|SaveShared|LoadShared|
saveSharedPeriodically|persistMu|WriteVersioned|internal/state|SnapshotInterval|
SaveSnapshot|LoadSnapshot                                           (none)
volumeClaimTemplates|StatefulSet|\bPVC\b|\bemptyDir\b              (none)
kaiak_usage_spool_batches|kaiak_usage_spool_records|UsageSpoolDepth|spool_unwritable|DroppedSpoolFull
  docs/DEPLOYMENT.md:956-957     the migration note naming the rename (allowed)
spool (whole word, case-insensitive: git grep -i -w -E "spool|spooled|spools")
  docs/DEPLOYMENT.md:950, 952    the migration note: flush the old spool, its files
  docs/specs/GATEWAY.md:1835, 1838   the dated Rejected line on the data directory
```

`usage-batch-invalid` (an error code) matches `usage-batch-` and is unrelated; the
case-insensitive `emptydir` matched only the test helper `wantEmptyDir`.

**Suite** (2026-10-07):
- `scripts/check-gateway.sh` (uncached, race): green; gateway e2e 106.0 s; live-kit
  self-test passed.
- `scripts/check-all.sh`: green in 189 s — gateway e2e 105.5 s; control `npm test` 614:
  613 pass, 1 skipped (the memory store's catch-up contract test); lint ok;
  cross-half 65.7 s.
- No test, sample or `kaiak` process left; scratch logs deleted.
