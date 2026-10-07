# Step 13 — round-2 gateway

**Status:** done (2026-10-07)

## Intent

Implement step 11's contract in the gateway, and fix the gateway-side round-2
findings (`docs/reviews/2026-10-07/AUDIT-2.md`). Removal discipline as in step 12.

## Scope

- **Count every scope** (decision 22; 2H3):
  - The limiter keeps `tokens_per_hour` and `usd_per_month` counts, pushed base plus
    own usage, for global and every group on each request's path, whether or not a
    limit exists.
  - A limit is a check over its scope's count. A reload adds, removes or changes
    limits without touching a count.
  - Per-minute limits stay local shares, as today.
  - File mode uses the same limiter. Its `limits.json` follows if its shape changes;
    bump the format and say so in `GATEWAY.md`.
- **Totals merging** (decision 22):
  - The first totals after each stream connect replace every pushed base.
  - Later totals replace only the windows they list.
  - A window whose `window_start` is not the current window's is ignored, as today.
  - `totals.json` holds every counted window; bump its format if the shape changes.
- **`counted_through` per epoch** (decision 23; 2M2): own usage is retired by each
  epoch the gateway holds (its current one and any restored from the spool).
- **The ack hand-off** (2M3): a batch moves from the queue to the acknowledged list
  under one lock acquisition, counters and wake-ups included.
- **Acknowledged list bound** (2L6): entries whose usage the window roll-over already
  cleared are dropped.
- **Contact** (decision 25; 2M6): only stream bytes refresh the outage contact. An
  ack does not.
- **Rejection** (2M10): a config event whose hash is the running config's clears a
  set rejection and reports status.
- **Metric** (2L7): a gauge that is 1 while `last_rejection` is set. Name it in the
  standard style (`docs/specs/GATEWAY.md` metrics table), and add it to the
  `DEPLOYMENT.md` alert list.
- **Docs:** `GATEWAY.md` documents 2L5 (usage restored from another instance's spool
  is retired only once the new instance's first batch is counted; over-count only).
- **fakecontrol** follows: complete first totals, then changes; the `counted_through`
  array.
- **e2e** (2L8): `never(config applied)` after a reconnect to the same config. The
  cross-half replicas test covers a gateway that rejected a config which dropped a
  limit, and still enforces that limit's spend.
- **Leftovers in the gateway** (2M8): every gateway file listed in AUDIT-2 2M8, by
  rewording.
- **Regression tests:** port [B]'s Go reproductions from the session scratchpad
  (`audit-b3/audit_b_test.go`, `audit-b3/limits_audit_b_test.go`), renamed to
  describe the behaviour; [G]'s are rewritten from their descriptions. Each fails
  before its fix; say so in Result.

## Files likely touched

- `gateway/internal/limits/**`, `gateway/internal/control/**` (`client.go`,
  `usage.go`, `messages.go`, `schema.go`), `gateway/internal/metrics/`,
  `gateway/internal/fakecontrol/`, `gateway/cmd/kaiak/main.go`, `gateway/e2e/**`.
- `docs/specs/GATEWAY.md`, `docs/DEPLOYMENT.md`.

## Acceptance criteria

- `scripts/check-gateway.sh` passes (uncached).
- `scripts/check-all.sh` passes once, cross-half tests included.
- The removal checklist items on the gateway side grep clean, with the commands and
  their output recorded in Result.

## Result

**Commits:**
- `2a8590c`: the gateway;
- `06f7d0a`: the e2e tests, `GATEWAY.md` and `DEPLOYMENT.md`;
- this Result.

**What changed**

- **Limiter** (`internal/limits`):
  - **Every scope is counted** (2H3). `sync` gives global and every group a
    `tokens_per_hour` and a `usd_per_month` counter, limited or not. A counter
    carries `limited`; one without a limit has effective limit 0 and is never
    checked.
    - Per-minute windows exist only for their limits.
    - `Reserve` and `Settle` count on every counter of the subject's scopes, so own
      usage is kept for a limit a reload adds. Only limited counters refuse, give
      headers, or are refused by an outage.
    - `keyOf(group, type)`.
  - **Totals merging:**
    - `Totals.Complete`: the first totals of each connection replace every base;
      the others replace only the windows they list.
    - `pushed` keeps every window received since the last complete totals, those
      of scopes the config lacks included, so a reload adding the scope finds its
      base.
    - On a changes-only message, per-minute shares follow the live count and only
      the listed hour or month counters are re-based.
    - A window listed at `"0"` sets 0.
  - **Visible output:**
    - `Usage()` lists every counter with `Limited`;
    - `limits.json` and `totals.json` hold every scope's windows, with formats
      unchanged (3 and 4, same entry shape).
    - The headers and `Rejection` comments lose the mismatch.
- **Control client** (`internal/control`):
  - `Totals.CountedThrough` is `[]BatchPosition`. The schema walker takes an array;
    the new rule `counted-through-epoch-duplicate` is in `semantic.go`.
  - `countedGeneration(counted []BatchPosition)` covers each held batch by its own
    epoch's entry (2M2).
  - `TotalsUpdate.Complete` comes from the stream: true for the first totals after
    each connect.
  - **Ack hand-off** (2M3): `acknowledged(e, batch)` moves the head to the
    acknowledged list under one lock (`dropHeadLocked`). `noteAcked` and the
    parallel `ackedGens` slice are removed.
  - **Bound** (2L6):
    - each acknowledged entry holds `clearedAt`: the end of its last record's UTC
      hour, or of its month when a record cost anything;
    - entries past it are dropped at the next ack.
  - **Contact** (2M6): an ack no longer calls `touch()`; only stream bytes do.
  - **Rejection** (2M10): `takeConfig` clears a set rejection when the event's hash
    is the running one. It logs `config rejection cleared: the control plane sent
    the running config` and requests a status report, without reloading.
  - Wording: the `Client` doc and the `Contact` doc.
- **Metric:** `kaiak_control_config_rejected` (gauge: 1 while `last_rejection` is
  set). `ControlState.ConfigRejected`; `kaiak_control_last_contact_timestamp_seconds`
  help: stream bytes only.
- **fakecontrol:**
  - cursors per instance and epoch;
  - `counted_through` as an array;
  - each stream's first totals complete, later ones only its changed windows, a
    dropped window listed at `"0"`;
  - `PushTotals` raw data is sent as it is.
- **e2e:**
  - the totals watch merges changes-only totals;
  - `logLines.count`;
  - the cross-half "control plane back" step checks no control config is applied
    on the reconnect (2L8);
  - the replicas test checks the same for gw-b's move to the other core;
  - new replicas subtest: a config dropping research's budget and adding a backend
    whose key only gw-a has. gw-a applies it and spends; gw-b rejects it
    (`kaiak_control_config_rejected` 1) and refuses the budget's model with
    `budget_exceeded` once the totals carry research's spend.
- **Leftovers (2M8):**
  - `main.go:99, 211, 864`;
  - `client.go:145`;
  - `messages.go:36`;
  - `limits.go:78`;
  - `headers.go:31`;
  - `server/limits.go:113`;
  - `fixtures_test.go:5-7`;
  - `shared_test.go:390`;
  - `loader_test.go` (`kaiak.config.hash`);
  - `persist.go` and `snapshot.go` comments.
- **Docs:**
  - `GATEWAY.md`:
    - usage restored from another instance's spool (2L5);
    - the metric row, and the alert note naming it;
    - `kaiak_config_size_bytes` "a config event's `config`".
  - `DEPLOYMENT.md`: the starter alert "Running a config the control plane moved
    off".

**Decisions made in this step**

- **A count without a limit is a counter like the others,** with `limited` false.
  Reservations are held on it too, so the in-flight part counts if a reload adds a
  limit mid-request. Cost: two counters per group on the request path, and about
  14 k counters at 7 k groups.
- **`Usage()` lists every counter.** Only tests read it; the metrics never did, so
  no series is added per group.
- **The acknowledged list is bounded by the windows, not by a count.** An entry
  whose usage the limiter already left behind retires nothing. Dropping it changes
  no count, and a covering push of a later batch still retires every generation
  below it.
- **The gateway also checks `counted-through-epoch-duplicate`** (step 11), with the
  same code constant style as the other rules.
- **No format bump** for `limits.json` or `totals.json` (step 11's decision held:
  same entry shape, more entries).

**Tests deleted or rewritten** (each asserted removed behaviour):

- `TestSnapshotDropsPassedWindowsAndRemovedLimits` →
  `TestSnapshotKeepsUnlimitedScopesAndDropsPassedWindows`. A removed limit's scope
  is now restored, not dropped.
- `TestTotalsMatching` rewritten. Its "not listed → 0" held only for complete totals.
- The counts asserted in `TestSnapshotRoundTrip` (8 windows, not 3) and in
  `TestSharedStateSurvivesARestart`: 4 windows, and the team's budget removed keeps
  its month.
- `TestCountedThroughCoversBatchesInSendOrder`: the per-epoch list.

**Regression tests** (each failed before its fix; [B]'s originals were run against
the pre-fix gateway at `7b38420` in a scratch worktree, since deleted):

| Test | Finding | Before the fix |
|---|---|---|
| `TestEachEpochIsCoveredByItsOwnEntry` (control) | 2M2 | [B] `TestAuditBAcknowledgedEpochCoveredAfterOldProcessWrite`: generation 0, want 1 |
| `TestAckHandOffNeverHidesABatchFromTotals` (control, 2000 runs racing the ack) | 2M3 | [B] `TestAuditBAckTransitionCannotHideBatchFromTotals`: generation 0. The ported test also fails on the current code with the hand-off split into two lock holds: generation 0 |
| `TestRunningConfigReceivedAgainClearsTheRejection` (control) | 2M10 | [B] `TestAuditBReturnToRunningHashClearsRejection`: the rejection still reported. The ported test at `7b38420`: no status with the rejection cleared |
| `TestAnAckIsNotContact` (control, [G] M2) | 2M6 | at `7b38420`: last contact moved on the ack |
| `TestALimitKeepsItsSpendWhateverTheControlPlanesConfig` (limits) | 2H3 | [B] `TestAuditBRejectedLimitRemovalMustNotEraseSpend`: 0 tokens, want 100 |
| `TestAcknowledgedBatchesAreForgottenOnceTheirWindowsPassed` (control) | 2L6 | no equivalent before: the list had no bound (`usage.go` `noteAcked`) |
| replicas e2e "a gateway that rejected a config dropping a budget still enforces its spend" | 2H3, end to end | — (the control half is step 12's) |

**Removal checklist, gateway side** (greps over `gateway/`, `docs/specs/GATEWAY.md`,
`docs/DEPLOYMENT.md`):

```
highestSequence|observeSequence                                   (none)
onRollback|[Rr]ollback                                            (none)
counted_through":null|CountedThrough == nil|CountedThrough \*|\*BatchPosition   (none)
noteAcked|ackedGens                                               (none)
touch\(\)
  stream.go:58, stream.go:92 (stream bytes), client.go:251 (the definition) — no ack
config-versions|config snapshot|snapshot fetch|acknowledged batch|for another config|resumes from|kaiak\.config\.version
  "config snapshot" meaning a request's in-memory config: accounting.go:29,
    limits.go:374, limits_test.go:751, server/api.go:144, routing.go:38, 116,
    GATEWAY.md:1404, 1715 (step 11's allowed exception)
  "acknowledged batch(es)" for batches an ack named: usage.go, totals_test.go,
    usage_test.go:255, GATEWAY.md:2260 (step 11's allowed exception)
```

**Suite** (2026-10-07):

- `scripts/check-gateway.sh` (uncached, race): every package passes. Gateway e2e
  114.1 s; the live-test kit self-test passes.
- `scripts/check-all.sh`, once, green in 209 s:
  - gateway checks pass (e2e 113.5 s);
  - control `npm test`: 605 tests, 604 pass, 1 skipped (the memory store's catch-up
    contract test);
  - `npm run lint`: boundaries ok;
  - cross-half tests 78.9 s, `TestAcrossHalvesReplicas` with its new subtest
    included.
- The new control tests were also run 20 times in a row under `-race`: all pass.
- No `node --test`, sample or gateway process left (`pgrep` empty).
