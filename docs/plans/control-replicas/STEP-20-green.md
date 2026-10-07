# Step 20 — no data directory: phase end

**Status:** done (2026-10-07)

## Intent

Prove the removal is complete, account for AUDIT-4, and end phase 7 and the plan
green.

## Scope

- Run step 18's removal checklist over the whole repo outside `docs/plans/` and
  `docs/reviews/`. Fix anything left.
- Repeat the earlier phases' removal greps (steps 14, 16, 17); they stay clean.
- Fill in AUDIT-4's Outcome: D-H3 fixed (with its commit), and the rest dissolved by
  decision 35.
- Mark the OVERVIEW's Phase 7 box.

## Acceptance criteria

- The checklists grep clean, with the commands and their output in Result.
- `scripts/check-all.sh` green **three times in a row**; record the times and counts.
- No orphaned test, sample or gateway process left.
- **Phase 7 and the plan end here.**

## Result

**Commits:**
- `485bd56`: the memory-bound wording in `GATEWAY.md`, and AUDIT-4's Outcome;
- this Result, with the OVERVIEW's Phase 7 box.

**What changed**

- **`GATEWAY.md`** (Usage batches in memory): `KAIAK_USAGE_MEMORY_BYTES` bounds "the
  encoded size of the queued batches' records". The code counts `queuedBytes`
  (`control/queue.go`), so "queued and sealed records" said more than it does. The
  other mentions (`ARCHITECTURE.md`, `DEPLOYMENT.md`, `control-plane.html`) already
  said encoded records.
- **AUDIT-4 Outcome:**
  - D-H3 fixed in `0a00dc6` (decision 36, two regression tests);
  - D-H1, D-H2, G4-M1 and the wording findings dissolved by decision 35;
  - G4-L3's bound accepted.
- **Metric rename, checked across the repo:**
  - `kaiak_usage_queue_batches` and `kaiak_usage_queue_records` are in code, tests,
    `GATEWAY.md`, `DEPLOYMENT.md` and the architecture pages (14 hits);
  - the old names appear only in the migration note.
- Nothing else was left to fix: every checklist below was already clean apart from its
  allowed exceptions.

**Removal checklists** (`git grep -n -E <pattern> -- . ':!docs/plans' ':!docs/reviews'`;
`-i` where marked):

Step 18 (data directory) and the metric rename:

```
KAIAK_DATA_DIR|DataDir|dataDir
  docs/DEPLOYMENT.md:949, 954     the migration note (allowed)
  docs/specs/GATEWAY.md:1835      the dated Rejected line (allowed)
(-i) data director|data dir\b                                       (none)
(-i) last-known-good|lastKnownGood|lastknowngood
  docs/DEPLOYMENT.md:953          the migration note (allowed)
  docs/specs/GATEWAY.md:1835      the Rejected line (allowed)
totals\.json|limits\.json|last-known-good\.json
  docs/DEPLOYMENT.md:953          the migration note (allowed)
  protocol/fixtures/config/invalid/cases.json:286   "key-with-limits.json", a fixture name
spooldisk|batchStore|usage-spool|usage-rejected|rejectedKept|index\.json|usage-batch-[^i]
  docs/DEPLOYMENT.md:952          the migration note's file list (allowed)
RestoreSpooled|RestoreOwn|SpoolCovered|saveShared|SaveShared|LoadShared|
saveSharedPeriodically|persistMu|WriteVersioned|internal/state|SnapshotInterval|
SaveSnapshot|LoadSnapshot                                            (none)
volumeClaimTemplates|StatefulSet|\bPVC\b|\bemptyDir\b               (none)
(-i) restart keeps the last totals|crash rebuild|rebuilt own usage|rebuilds? (its|own) usage   (none)
(-i) flush (it )?before upgrading|data[- ]file format                (none)
kaiak_usage_spool|UsageSpoolDepth|spool_unwritable|DroppedSpoolFull
  docs/DEPLOYMENT.md:956-957      the migration note naming the rename (allowed)
(-i -w) spool|spooled|spools|spooling
  docs/DEPLOYMENT.md:950, 952     the migration note (allowed)
  docs/specs/GATEWAY.md:1835, 1838   the dated Rejected line (allowed)
```

Step 14:

```
highestSequence|observeSequence|CurrentConfig\b                       (none)
change\.sequence|saved: true; sequence                               (none)
onRollback|[Rr]ollback
  docs/specs/CONTROL-PROTOCOL.md:192, docs/architecture/control-plane.html:279
    dated Rejected lines (allowed)
lastSent|\bdelivered\b                                              (none)
limitedOf|limitedWindowsOf|LimitedWindow|counted but not listed|only the windows the   (none)
counted_through": ?null|CountedThrough == nil|\*BatchPosition|BatchPosition \| null
  protocol/fixtures/messages/totals/invalid/counted-through-null.json   the invalid
    fixture refusing it (allowed)
snapshot\.(config|liveGateways|last|sequence)                         (none)
JSON\.stringify\((current|published|entry)\.config\)                   (none)
noteAcked|ackedGens                                                  (none)
Totals size bound                                                    (none)
config-versions|snapshot fetch|for another config|resumes from|kaiak\.config\.version   (none)
store's sequence|totals sequence|one sequence
  CONTROL-PROTOCOL.md:1071 (a dated Rejected line); the rest the token estimate's
    "input one sequence sees" (GATEWAY.md:1263, 1317; estimate_test.go:13;
    server/limits_test.go:217; params.go:95)
```

Steps 16 and 17:

```
restoredGeneration|RestoredGeneration|clearedAt|restoredUntagged|totals event ignored|
MaxEffectiveLimits|CodeEffectiveLimitsExceeded|countEffectiveLimits|
effective-limits-exceeded|base_window_start|"uncounted"
  docs/DEPLOYMENT.md:947   the migration note naming the replaced rule (allowed)
restored lump|uncounted usage restored|written whenever totals are applied|drops the batch from
  (none)
existed only|it existed|existed because|needed only while            (none)
```

The format-4 `totals.json` content in `shared_test.go`, an allowed hit in steps 16–17,
went with the file in step 19.

**Suite** (2026-10-07, after `485bd56`): `scripts/check-all.sh` three times in a row,
all green.

| Run | Ended (UTC) | Total | Gateway e2e | Control `npm test` | Lint | Cross-half |
|---|---|---|---|---|---|---|
| 1 | 14:31:56 | 191 s | 108.8 s | 614: 613 pass, 1 skipped | ok | 65.4 s |
| 2 | 14:35:01 | 185 s | 103.2 s | 614: 613 pass, 1 skipped | ok | 65.4 s |
| 3 | 14:38:10 | 189 s | 107.2 s | 614: 613 pass, 1 skipped | ok | 65.4 s |

- The skip is the memory store's catch-up contract test (its channel cannot drop a
  change); the lossy channel runs that test.
- No `node --test`, sample or `kaiak` process is left; the run logs are deleted.
- **Phase 7 and the plan end here.**
