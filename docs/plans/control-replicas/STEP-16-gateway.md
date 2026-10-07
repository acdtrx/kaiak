# Step 16 — round-3 gateway

**Status:** done (2026-10-07)

## Intent

Implement decisions 29 and 31–34 in the gateway, as step 15 wrote them into
`GATEWAY.md`, and fix the gateway-side round-3 findings
(`docs/reviews/2026-10-07/AUDIT-3.md`).

## Scope

- **Stale-totals signal (decision 29, 3H1):**
  - An acknowledged own batch keeps `UsageWaitingSince` set until totals covering it
    are applied.
  - Batches restored from another instance's spool do not hold it.
  - The outage rule is unchanged otherwise.
- **Malformed totals (decision 31):** a totals event that fails decoding ends the
  stream and is logged. `client_test.go`'s skip test is replaced.
- **Spool until covered (decision 32, 3H2, 3L4):**
  - A batch stays in the spool, marked acknowledged and never resent, until a
    completed `totals.json` save covers it.
  - On restart, own usage is rebuilt from the spooled batches beyond the saved
    `counted_through`.
  - `totals.json` is written in the background:
    - at an interval (pick one and state it; `SnapshotInterval` is the obvious
      candidate);
    - at shutdown;
    - never on the stream goroutine;
    - with current windows only.
  - The acknowledged list becomes part of the spool, and the spool's own bounds apply.
  - Bump the spool format, and `totals.json` if its shape changes, in `GATEWAY.md`
    and `DEPLOYMENT.md`.
  - Without a data directory, behaviour is unchanged.
- **Counters by scope (decision 33, 3M1):**
  - Hour and month counters are kept until their window ends, whatever the config. A
    recreated ID reuses its counters, and reservations stay on them.
  - Pushed windows whose window has passed are pruned.
  - This applies to file mode too.
- **Memory (decision 34, 3M2):**
  - The gateway's semantic check counts allocated counters, as step 15 did on the
    control side.
  - Minute buckets are allocated only for per-minute counters.
  - A changes-only `TakeTotals` touches only the listed windows, the per-minute
    shares (only when the live count changed), and the counters with own usage.
- **fakecontrol (3L8):** an ended window is not listed at "0".
- **Regression tests:** port [C]'s Go reproductions from the session scratchpad
  (`audit-c3/control_audit_c_test.go`, `limits_audit_c_test.go`), renamed to describe
  the behaviour. Write [G]'s (G3-M1, G3-L1) from AUDIT-3. Each fails before its fix;
  say so in Result. Add a benchmark or recorded measurement for `TakeTotals` and the
  `totals.json` write at 7k groups.

## Files likely touched

- `gateway/internal/limits/**`, `gateway/internal/control/**` (spool, usage, stream,
  client), `gateway/internal/config/semantic.go`, `gateway/cmd/kaiak/main.go`,
  `gateway/internal/fakecontrol/`, `gateway/e2e/**`.
- `docs/specs/GATEWAY.md` (format numbers), `docs/DEPLOYMENT.md` (format notes).

## Acceptance criteria

- `scripts/check-gateway.sh` passes (uncached).
- `scripts/check-all.sh` passes once, cross-half tests included.
- Results record the measurements before and after.

## Result

**Commits:**
- `f5cfcba`: the gateway (`cmd/kaiak`, `internal/{limits,control,config,fakecontrol}`, `e2e`);
- `e206388`: `GATEWAY.md` and `DEPLOYMENT.md`;
- this Result.

**What changed**

- **Stale-totals signal** (decision 29, 3H1):
  - `limits.Contact.UsageUncountedSince`, from `Client.UsageUncountedSince()`: the ack
    of the oldest acknowledged batch of this instance that applied totals have not
    covered. An ack never restarts it; totals covering the batch end it.
  - Another instance's batches are not remembered, so they never hold it.
  - The outage is logged with the reason `usage not shown counted`.
- **Malformed totals** (decision 31): a totals event that fails decoding ends the
  stream (`errMalformedTotals`), logged at error level by `config stream failed`. The
  reconnect's first totals are complete.
- **Spool until covered** (decision 32, 3H2, 3L4):
  - Spool **format 4**: the index holds `acknowledged`, each epoch's last
    acknowledged sequence.
  - An ack of this instance's batch writes the index (`batchStore.acknowledge`); the
    file stays. Another instance's batch is deleted on its ack.
  - A batch file is deleted once a `totals.json` write covering it has completed
    (`Client.SpoolCovered`, called by `saveShared`). The index forgets an epoch with
    nothing left.
  - At open, a batch at or below its epoch's acknowledged sequence comes back
    acknowledged: never sent again, waiting to be shown counted from the restart.
  - `Client.RestoreSpooled(counted)` hands over every kept batch the saved
    `counted_through` does not cover (this instance's), and every other instance's,
    and drops the acknowledged ones it covers. `Limiter.RestoreOwn` rebuilds their
    records into own usage, on global and every group of the record's path, in
    their own window when still current, under their generation.
  - `totals.json` **format 5**: `{ live_gateways, counted_through, windows[{group,
    type, window_start, used}] }`, the pushed bases of current windows only.
    `SaveShared` returns the written `counted_through`.
  - Written by `saveSharedPeriodically`: every 30 s (`limits.SnapshotInterval`) when
    totals were applied since the last write, and at shutdown. Never on the stream's
    goroutine.
  - Without a data directory: at most `ackedRemembered` (10 000) acknowledged
    batches, the oldest forgotten past that, keeping the wait's time; a covered one
    is forgotten at once.
  - **Removed:** `clearedAt` and the time bound; `RestoredGeneration`; the restored
    `uncounted` lump and `restoredUntagged`; the save on every push.
- **Counts by scope** (decision 33, 3M1):
  - The limiter keeps the hour and month counters of a scope a reload removes in
    `retained` while their current window holds own usage (settled or reserved). A
    group created again takes them back, and reservations stay on them.
  - `countOf` gives a retained counter for usage of a scope the config lacks (the
    rebuild, the file-mode snapshot).
  - `limits.json` saves retained counts, and the restore puts them back.
  - Pushed windows that ended are dropped once an hour (`prunePushedLocked`).
- **Memory and push cost** (decision 34, 3M2):
  - The gateway's semantic check is `counters-exceeded` (`countCounters`,
    `MaxCounters`), mirroring step 15's control side. **Removed:**
    `effective-limits-exceeded`, `MaxEffectiveLimits`, `countEffectiveLimits`.
  - Minute buckets live in `window.m`, allocated for sliding minutes only.
  - A changes-only `TakeTotals` re-shares per-minute counters only when the live
    count changed (`l.minute`), re-bases only the listed windows, and retires only the
    counters holding own usage (`l.owning`).
- **fakecontrol** (3L8): a window the script dropped is listed at "0" only while
  still in its window.
- **e2e:** the last-known-good boot step counts the batches the index does not name
  acknowledged. An acknowledged batch no write has covered yet stays spooled beside
  the unsent one.
- **Docs:**
  - `GATEWAY.md`: the outage reason and log table; `kaiak.usage.acknowledged_batches`;
    the totals-write trigger values.
  - `DEPLOYMENT.md`: `totals.json` format 5; spool format 4 with its flush note.

**Decisions made in this step**

- **The rebuilt usage includes the queued batches the saved totals do not cover**, as
  well as the acknowledged ones. Both are spend the restored bases lack. A queued
  batch the saved totals already cover (its ack was lost) is kept queued and resent,
  and its usage is not rebuilt.
- **A batch acknowledged after totals already covered it** (the ack was lost, then
  resent) is acknowledged covered: it does not hold the wait. That is the reason the
  sender keeps the `counted_through` it last saw.
- **`countedGeneration` reports only newly covered batches**, so a covered batch kept
  on disk until the next write is not reported again on every push. Retirement was
  idempotent anyway.
- **Index pruning** takes `persistMu`, then `mu`, as `persist` does. An ack records the
  index before its batch leaves the queue, so an epoch being acknowledged is never
  pruned.
- **A lost index** (another format, unreadable) makes the acknowledged batches it
  named plain queued ones. They are resent and acknowledged again without counting.
  `TestSpoolOfAnotherFormatStartsANewEpoch` now expects that duplicate.
- **`UsageUncountedSince` is a second field of `limits.Contact`**, with its own outage
  reason, rather than folded into `UsageWaitingSince`, so the log says which wait ran
  out.

**Tests deleted or rewritten** (each asserted removed behaviour):
- `TestAcknowledgedBatchesAreForgottenOnceTheirWindowsPassed` (the `clearedAt` bound)
  → `TestAcknowledgedBatchesAreBoundedWithoutADataDirectory`.
- `TestSharedStateSurvivesARestart`: rewritten for format 5 and `RestoreOwn`. The
  restored uncounted lump and its generation tag are gone.
- `TestCountEffectiveLimits` and `TestEffectiveLimitsExceededAtRoot` →
  `TestCountCounters` and `TestCountersExceededAtRoot`.
- `TestTotalsEventsReachTheConsumer` lost its malformed event, which was skipped
  (removed behaviour; now `TestMalformedTotalsEndTheStream`).
- `TestUsageBatchesSealAtTheSizeLimit` asserted an acknowledged batch leaves the
  spool. It now asserts the batches stay acknowledged until a covering write.
- `TestSharedStateOfAnotherVersionIsDiscarded`: the old file is format 4, and format 5
  is wanted.

**Regression tests** (each failed before its fix; [C]'s originals and [G]'s were run
against the pre-fix gateway at `5be2b2c` in a scratch worktree, since deleted):

| Test | Finding | Before the fix |
|---|---|---|
| `TestACrashRestoresTheSpooledSpendIntoTheLimits` (control) and `TestACrashKeepsTheSpendSettledAfterTheLastWrite` (limits) | 3H2 | [C] `TestAuditCCrashRestoresSpooledSpendIntoLimits`: "restart admitted a priced request although its restored spool already spends the full budget" |
| `TestMalformedTotalsEndTheStream` | 3H1 ([C] C5, [K] K3-M1) | [C] `TestAuditCMalformedTotalsCannotContinueWithDeltas`: "accepted a changes-only totals event after losing its preceding delta" |
| `TestAnAcknowledgedBatchWaitsToBeShownCounted` (control) and `TestUsageNotShownCountedPastTheGraceIsAnOutage` (limits) | 3H1 ([G] G3-M1) | [G]'s `TestReview3AckedButNeverShownCountedIsNotWaiting`: "an acknowledged batch no totals covered is not waiting" |
| `TestARecreatedGroupKeepsItsSpend` (both modes) | 3M1 ([C] C3) | [C] `TestAuditCRecreatedGroupKeepsOwnSpend`: spent budget 0, want 1 USD, in both modes |
| `TestEndedPushedWindowsAreDropped` | 3M1 ([C] C8) | [C] `TestAuditCExpiredPushedScopesAreReclaimed`: "expired deleted group's pushed window retained" |
| `TestAcknowledgedBatchesAreBoundedWithoutADataDirectory` | 3L4 ([G] G3-L1) | [G]'s `TestReview3AckedListGrowsForPricedBatches`: 10 001 kept, at 3.99 s under `-race` |
| `TestCountersExceededAtRoot` and the shared fixture `counters-exceeded.json` | 3M2 ([C] C6) | the fixture was step 15's named red: the gateway counted effective limits |
| `TestAcknowledgedBatchesSurviveARestartUntilCovered` | decision 32 (the index and restart) | new behaviour; no "before" |

**Measurements** at 7 000 groups (`internal/limits/scale_test.go`: one group per key,
two windows each; `go test -bench`, Apple M5 Max):

| | Before (`5be2b2c`) | After |
|---|---|---|
| changes-only `TakeTotals` (one window listed) | 1.49 ms | 0.21 µs |
| one `totals.json` write | 15.2 ms, on every push, on the stream's goroutine | 11.9–13.5 ms, every 30 s at most, in the background; 1.28 MiB |
| counter heap | 29.7 MiB | 7.6 MiB |

**Removal checklist (gateway side):** `git grep -n -E "restoredGeneration|RestoredGeneration|clearedAt|restoredUntagged|totals event ignored|MaxEffectiveLimits|CodeEffectiveLimitsExceeded|countEffectiveLimits|effective-limits-exceeded|base_window_start|\"uncounted\"" -- . ':!docs/plans' ':!docs/reviews'`
returns only `gateway/internal/limits/shared_test.go:956-957`. That is the format-4
file the discard test writes, which exists to be refused. A docs grep for the old
restore wording (`restored lump`, `uncounted usage restored`, `written whenever totals
are applied`, an ack deleting the batch) is empty.

**Suite** (2026-10-07):
- `scripts/check-gateway.sh`: green (uncached, race), gateway e2e 108.9 s.
- `scripts/check-all.sh`: green in 199 s. Gateway e2e 112.4 s; control `npm test` 614,
  613 pass, 1 skipped (the memory store's catch-up contract test); lint ok; cross-half
  69.1 s.
- Step 15's expected red (`TestInvalidFixtures/counters-exceeded.json`) is cleared.
- No test, sample or `kaiak` process is left.

**Left for step 17:** none from this step beyond its brief.

