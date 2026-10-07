# Step 21 — round-5 fixes

**Status:** done (2026-10-07)

## Intent

Fix the three findings of the fifth, Codex-only review
(`docs/reviews/2026-10-07/AUDIT-5-independent.md`), then end the plan green. This
step is **phase 8** and ends green.

## Scope

- **E-M1:** dropped and removed usage payloads stay reachable.
  - Where: `gateway/internal/control/queue.go:66` and `usage.go:372`.
  - Advancing a slice head leaves the removed element's records in the backing
    array, so a batch dropped under the memory bound stays in memory.
  - Fix: zero each removed element before advancing, and release empty backing
    arrays. The sender keeps its own reference to the outstanding batch until its
    send finishes.
- **E-L1:** in file mode, a deleted group's retained counters are pruned only on a
  reload.
  - Fix: prune through ordinary limiter activity, without a config change. For
    example, prune a retained counter when its last reservation settles, and at
    window boundaries, keeping decision 36's conditions.
- **E-L2:** `docs/specs/CONTROL-PROTOCOL.md:513` still names the file-mode snapshot.
  Remove it, and grep for any other live mention.
- **Regression tests:** port Codex's two reproductions from the session scratchpad
  (`audit-e5/control_audit_e_test.go`, `limits_audit_e_test.go`), renamed to describe
  the behaviour. Each fails before its fix.
- **AUDIT-5 outcome:** add an Outcome section at the end of
  `AUDIT-5-independent.md`, marking each finding fixed with its commit.

## Acceptance criteria

- `scripts/check-all.sh` green **three times in a row**. **Phase 8 and the plan end
  here.**

## Result

**Commits:**
- `4dc87df`: the fixes and their tests;
- `cd669f7`: a spec reflow;
- this Result, with AUDIT-5's Outcome.

**What changed**

- **E-M1** (`gateway/internal/control/queue.go`, `usage.go`):
  - `dropFirst` clears the removed slot before advancing the slice, and gives back a
    nil slice when it empties, so the backing array lets go of the batch.
  - It is used for the sealed batches (`queueSealed`) and the queue's head
    (`dropHeadLocked`, on an ack or a refusal).
  - `boundQueued` already used `slices.Delete`, which clears the tail. What kept a
    dropped record alive was its original slice in the sealed array.
  - The acknowledged list holds IDs only, no records.
- **E-L1** (`gateway/internal/limits/limits.go`):
  - `expireRetainedLocked` prunes the retained counts once an hour, from `sync`,
    which every entry point calls (`Reserve`, `Usage`, `Outage`, `TakeTotals`).
  - A retained count's window ends on an hour boundary, so it goes within the hour
    after, with no reload and no totals.
  - Decision 36's conditions are unchanged: no reference, and nothing in the current
    window.
  - `GATEWAY.md` (A count outlives its scope's config) says "at the latest within the
    hour after, on whatever next uses the limits".
- **E-L2:** `CONTROL-PROTOCOL.md`'s limit identity paragraph no longer names a
  file-mode snapshot. A repo-wide grep finds no other live mention; the one left is
  the dated Rejected line in `GATEWAY.md`.

**Regression tests** (each fails with its fix reverted, and passes with it):

| Test | Finding | With the fix reverted |
|---|---|---|
| `TestRecordsDroppedAtTheBoundLeaveMemory` (`control/memory_test.go`; [E] `TestAuditEMemoryBoundReleasesDroppedPayloads`) | E-M1, sealed batches | "a record dropped at the bound is still held after a collection" |
| `TestARemovedHeadBatchLeavesMemory` (new) | E-M1, the queue's head | "an acknowledged batch's record is still held by the queue after a collection" |
| `TestADeletedGroupsCountersEndWithTheirWindowsWithoutAReload` (`limits/limits_test.go`; [E] `TestAuditEFileModePrunesExpiredRemovedCounters`) | E-L1 | "2 counters of the deleted group kept after their windows passed" |

**Suite** (2026-10-07): `scripts/check-all.sh` three times in a row, all green.

| Run | Ended (UTC) | Total | Gateway e2e | Control `npm test` | Lint | Cross-half |
|---|---|---|---|---|---|---|
| 1 | 14:58:19 | 191 s | 107.6 s | 614: 613 pass, 1 skipped | ok | 65.4 s |
| 2 | 15:01:24 | 185 s | 103.6 s | 614: 613 pass, 1 skipped | ok | 65.3 s |
| 3 | 15:04:35 | 191 s | 109.4 s | 614: 613 pass, 1 skipped | ok | 65.8 s |

- The skipped test is the memory store's catch-up contract test (the lossy channel runs
  it).
- No test, sample or `kaiak` process is left, and the run logs are deleted.
- **Phase 8 and the plan end here.**
