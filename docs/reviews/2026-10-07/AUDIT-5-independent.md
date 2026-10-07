# Audit E — removal of gateway disk state

## Verdict

**One Medium and two Low findings.** The memory bound can discard records without
releasing their memory; deleted-scope counters can outlive their windows in file
mode; one contract sentence still describes the removed snapshot. No additional
usage double-counting, budget under-enforcement, outage-clock defect, reference
imbalance, race or deadlock was confirmed in the reviewed paths.

This was a focused source review of the supplied copy, which has no git history.
Production code and existing tests were left unchanged. Two failing Go reproductions
were added.

## Medium

### E-M1 — Dropped usage payloads remain reachable outside the memory accounting

**Frequency: occasional** — a traffic burst accumulates sealed batches, particularly
while delivery is unavailable and the configured memory bound starts dropping them.

- **Location:** `gateway/internal/control/queue.go:66`; the equivalent head removal
  also occurs at `gateway/internal/control/usage.go:372`.
- **Mechanism:** advancing `u.sealed = u.sealed[1:]` leaves the removed element's
  `records` reference in the backing array. The surviving slice, even when empty,
  keeps that allocation and its references reachable. `boundQueued` clears its own
  deleted queue elements correctly, but their original records remain reachable
  through the sealed array. Consequently, queued bytes and dropped-record metrics
  say the bound was restored while the discarded payloads still occupy heap. Head
  removal from `u.queue` similarly retains acknowledged/refused payloads. Further
  appends can eventually replace those arrays, but without them retention persists.
- **Impact:** the drop policy does not reliably relieve memory pressure; a burst's
  discarded records can remain resident beyond the configured bound. This raises
  OOM risk and therefore the risk of losing the outstanding usage too. The test
  confirms retention, not an actual OOM.
- **Reproduction:** `gateway/internal/control/audit_e_test.go:21`,
  `TestAuditEMemoryBoundReleasesDroppedPayloads`. Queue 63 valid one-record batches
  with a one-record byte bound; only the outstanding batch survives logically.
  A weak pointer into the second record remains live after GC, proving a strong
  reference remains despite its drop.
- **Fix sketch:** zero each removed sealed/queued element before advancing the
  slice; release empty backing arrays where appropriate. Keep the sender's local
  outstanding-batch reference until its send finishes.

## Low

### E-L1 — File-mode retained counters are never expired without another reload

**Frequency: occasional** — deleting a used group, then continuing to run the same
file-mode configuration beyond its hour/month windows.

- **Location:** `gateway/internal/limits/limits.go:249`, `:306`, `:565`.
- **Mechanism:** retained counters are pruned on config synchronization and on
  `TakeTotals`. With an unchanged snapshot, `sync` returns before pruning; file
  mode never receives totals. `Settle` correctly releases `refs` but performs no
  pruning. Requests and metrics reads therefore leave expired removed counters
  resident indefinitely, until another reload or process exit.
- **Impact:** unnecessary retained memory and disagreement with the contract that
  retained counts end with their windows. This is a cleanup-trigger omission,
  **not a leaked reference or a demonstrated enforcement error**. Continued reloads
  do prune older counters, so this is not unbounded growth under every workload.
- **Reproduction:** `gateway/internal/limits/audit_e_test.go:5`,
  `TestAuditEFileModePrunesExpiredRemovedCounters`: reserve zero tokens, delete the
  group, settle usage, advance beyond both windows, then invoke ordinary request
  and observation paths. Two counters remain, both with zero references. The
  existing `TestARecreatedGroupKeepsItsSpend` calls the private pruning helper
  directly, masking the missing production trigger.
- **Fix sketch:** trigger expiry through ordinary limiter activity even without a
  config change, amortized at window boundaries; remove an eligible retained counter
  when its last reservation settles. Preserve the `refs == 0` and no-current-own-usage
  conditions.

### E-L2 — Protocol spec still claims a file-mode snapshot exists

**Frequency: occasional** — consulting the limits contract.

- **Location:** `docs/specs/CONTROL-PROTOCOL.md:513`.
- **Mechanism:** “file-mode snapshot all key limits by it” describes a removed
  persistence mechanism, contradicting the explicit empty-on-start contract.
- **Fix sketch:** remove the snapshot from this identity sentence. No runtime
  reproduction applies; confirmed by reading and repository-wide search.

## Checked and found sound

- `queueMu` serializes the sealer and flush, preserving sequence/generation order
  while record validation runs outside the request-path mutex. Encoded queued-byte
  arithmetic and protection of the outstanding batch are correct, apart from E-M1's
  physical retention.
- One sender retries the same ID; ack validation checks the whole ID. Permanent
  batch refusals are restricted to the specified status/code combinations, dropped
  and logged; authentication/version problems keep retrying.
- Queue-to-acknowledged movement is atomic under `mu`. Totals can cover a batch
  before its ack; acks alone never retire limiter usage. The 10,000-entry
  acknowledged-list cap preserves its outage wait, and covering totals forget it.
  Epochs are freshly randomized for each client/process start.
- References increment only after all admission checks pass, including zero holds;
  settlement releases each once and repeated settlement is guarded. The server's
  deferred finishers cover cancellation, pre-routing errors, upstream failures and
  drain cuts. Removed/recreated scopes reuse held counters; window incarnations
  prevent releasing an old reservation from a new window. File-mode construction
  starts empty.
- Boot uses stream config, then a validated free-only seed for unavailability,
  otherwise exits; operator errors do not silently select the seed. Readiness waits
  for initial totals. Drain waits for request settlement, flushes while the sender
  still runs, and reports undelivered usage at exit.
- No remaining production data-directory setting, persistence writer, spool field
  or old spool metric was found. The queue metric names agree across implementation,
  tests and observability docs. Old names in deployment cleanup instructions and
  the explicitly rejected design are intentional; E-L2 is the stale live claim.

## Verification

The baseline gateway suite was compiled before the reproduction files were added.
`scripts/check-all.sh` passed with `GOCACHE=/tmp/kaiak-audit-e-gocache` and permission
for localhost fixtures and the pinned lint tool. Gateway formatting, vet,
staticcheck, race tests and live-kit self-tests passed. Control installation used
`npm ci --ignore-scripts`; control tests reported **613 passed, 0 failed, 1 existing
skip**, and control lint passed. The full script ended with:

```text
ok   kaiak/e2e  65.912s
all checks passed
```

The separate cross-half run also passed:

```text
ok   kaiak/e2e  65.548s
```

Both new reproductions fail with and without `-race`. Command:

```sh
cd gateway
GOCACHE=/tmp/kaiak-audit-e-gocache go test -race -count=1 -run '^TestAuditE' ./internal/control ./internal/limits
```

```text
--- FAIL: TestAuditEMemoryBoundReleasesDroppedPayloads (0.03s)
    audit_e_test.go:36: memory-bound-dropped record is still strongly reachable after GC
FAIL  kaiak/internal/control
--- FAIL: TestAuditEFileModePrunesExpiredRemovedCounters (0.00s)
    audit_e_test.go:29: 2 removed counters retained after their windows expired and all requests settled
FAIL  kaiak/internal/limits
```

These failures are intentionally left in place. Initial sandbox-only verification
attempts could not access the default Go cache/network or bind test listeners; the
successful baseline checks used the required permissions. Full verification output
is in `/tmp/kaiak-audit-e-all.log`; reproduction output is in
`/tmp/kaiak-audit-e-repros.log`.

## Outcome (2026-10-07)

All three findings are fixed in step 21 (`docs/plans/control-replicas/STEP-21-round-5.md`):

| ID | Outcome |
|---|---|
| E-M1 | **Fixed** in `4dc87df`. `dropFirst` clears the removed slot of the sealed batches and of the queue's head. Regression tests: `TestRecordsDroppedAtTheBoundLeaveMemory`, `TestARemovedHeadBatchLeavesMemory` |
| E-L1 | **Fixed** in `4dc87df`. The retained counts are pruned once an hour on ordinary limiter use (`expireRetainedLocked`). Regression test: `TestADeletedGroupsCountersEndWithTheirWindowsWithoutAReload` |
| E-L2 | **Fixed** in `4dc87df`. The file-mode snapshot is gone from `CONTROL-PROTOCOL.md` |
