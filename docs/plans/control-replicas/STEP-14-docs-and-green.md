# Step 14 — round-2 docs, checklist, phase end

**Status:** done (2026-10-07)

## Intent

Clear what remains of AUDIT-2, prove the removal is complete, and end phase 5 and
the plan green.

## Scope

- **Leftovers (2M8)** not already fixed by steps 11–13:
  - `docs/ARCHITECTURE.md`: line 155; the module diagram at 354–381, which still has
    `config-versions` and the `fastify --> cv` edge, and lacks `config-publishing -->
    schemas` and `store-contract`;
  - `docs/TECH-STACK.md:227-230`;
  - `config.schema.json:5` (both copies);
  - `GATEWAY.md:175, 2235`;
  - `CONTROL-PROTOCOL.md:936`;
  - `BACKLOG.md:81`, and the "Totals size bound" entry (resolved: remove it).
- **2L10:** complete the host-app migration note (`DEPLOYMENT.md`, the note around
  1006–1015):
  - removed exports and their replacements;
  - `publishConfig`'s result;
  - the status hash fields;
  - `onDeliveryFailed` and `deliveryRetryDelaysMs`;
  - the store interface changes of this phase (no sequence, config text, the
    snapshot);
  - totals as changes.
- **2L11:** rewrite the narrating Rejected lines as why the alternative fails.
- **2L12:** ARCHITECTURE's control-plane bullet names the subscription in
  `start`/`stop`.
- **`docs/architecture/*.html`:** follow decisions 21–27 wherever they describe
  ordering, rollback, totals contents or contact.
- **AUDIT-2 Outcome:** a table with every finding marked fixed (with its commit) or
  dissolved (with the decision). Nothing is left unaccounted for.

## Acceptance criteria

- Every removal checklist pattern from step 11 greps clean over the repo outside
  `docs/plans/` and `docs/reviews/`, apart from its allowed exception. Record the
  commands and their output in Result.
- `scripts/check-all.sh` green **three times in a row**. Record the times and counts.
- No orphaned `node --test`, sample or gateway process left (`pgrep` empty).
- **Phase 5 and the plan end here.**

## Result

**Commits:**
- `d64cb8c`: the docs;
- `175e154`: the e2e hour-boundary fix;
- this Result, with AUDIT-2's Outcome and the OVERVIEW box.

**What changed**

- **`docs/ARCHITECTURE.md`:**
  - Deployment shape: the store contract without a sequence.
  - `limits`: every scope counted, limits on top; totals complete, then changes;
    stream contact only.
  - `storage`: the config's text and hash instead of the sequence; the lossy channel
    and the broken stores.
  - `usage`: every scope with usage; `counted_through` per epoch.
  - `control-plane`: `start`/`stop` take and release the subscription (2L12).
  - `fastify`: configs ordered by issue and skipped by hash; one totals read per
    push; teardown.
  - **The module diagram, regenerated from the imports:**
    - `config-publishing` replaces `config-versions`, with its `schemas` edge;
    - the stale `fastify --> storage` and `usage --> config` edges are removed;
    - `store-contract` is added.
- **`docs/architecture/control-plane.html`:**
  - Order by issue, and "A restored store is the current state" instead of the
    Rollback bullet.
  - Slow readers gather changes.
  - Teardown.
  - "Count everything, limits on top", "Complete, then changes", `counted_through`
    per epoch.
  - Contact is stream bytes only.
  - A publish sends a config event only.
  - The figure captions follow.
- **`docs/architecture/gateway.html`:** the `limits` row; contact.
- **`docs/TECH-STACK.md`** Transport: the one stream, no snapshot or cursor; the
  sample page's shape.
- **`README.md`:** the store contract without "one sequence".
- **`docs/BACKLOG.md`:**
  - "Totals size bound" removed (resolved by decision 22);
  - the config-in-secrets entry names the app's history, not config versions;
  - the publish-cost entry says "config event".
- **`docs/DEPLOYMENT.md`** migration note (2L10):
  - the store interface: text, the snapshot, no sequence, the `reconnect` hook;
  - the publish result shape and listeners that may hear a config twice;
  - totals;
  - the status hash fields;
  - the new core API (`onDeliveryFailed`, `deliveryRetryDelaysMs`, `readConfig`,
    `onConfigRead`, `readTotals`);
  - the removed exports (`validateConfigSnapshot`, `validateResync`).

  `onRollback` is not listed: it existed only on this branch and was never released.
- **Rejected lines (2L11):**
  - K's three narrating lines were already rewritten by step 11 (the greps below
    find none of their phrases);
  - this phase's own Rejected lines are now in the present: `CONTROL-PROTOCOL.md`
    (a failed read, a restored store, complete totals on every push, the H3 gate)
    and `GATEWAY.md` (the "only newer" gate).
- **AUDIT-2 Outcome:** every finding fixed (with commits), dissolved (with the
  decision) or, for 2L5, documented.

**A failure found and fixed: the e2e hourly total across the top of the hour**

- The first `check-all.sh` run of this step failed:
  `TestAcrossHalves/a_lost_usage_ack_is_not_counted_twice`, "the hourly token total
  at 1620 not seen within 15s; latest … tokens_per_hour WindowStart 10:00 Used 1609".
- **Mechanism**, from the run's sample log:
  - the resend was acked at 10:59:59.300;
  - the "chat on gw-b" answer (11 tokens) came right after;
  - gw-b's batch arrived at 11:00:04.054.
- That record is stamped 10:59:59, so it counts in the previous hour's window
  (Usage intake: the record's own window when it is the current or previous one).
  Totals list only the current window, so 1609 + 11 = 1620 is never pushed.
- The watcher kept the 10:00 window it had merged before the hour ended, and waited
  on it. The old complete totals had the same gap: no 10:00 window after 11:00.
  So this was the test's boundary handling, not the product.
- **Fix** (`175e154`, test only):
  - `servedTokens.counted` checks the current UTC hour: the window listed for it,
    or 0, against the answers of that hour, with the same one-second edge;
  - `totalsWatch.wait` also looks again at the top of the hour, which brings no push
    of its own.
- The double-count check is unchanged within an hour; across the top of the hour
  only the new hour can be checked, since no push carries the previous one.
- The next three runs (below) passed.

**Removal checklist** (`git grep -n -E <pattern> -- . ':!docs/plans' ':!docs/reviews'`):

```
highestSequence|observeSequence|CurrentConfig\b                       (none)
TotalsSnapshot…sequence|change\.sequence|saved: true; sequence        (none)
  (storage/types.ts and memory.ts hold only batch sequences)
onRollback|[Rr]ollback
  docs/specs/CONTROL-PROTOCOL.md:181   dated Rejected line (allowed)
  docs/architecture/control-plane.html:278   the overview's Rejected line, mirroring
    the spec's (kept on purpose)
lastSent|\bdelivered\b                                              (none)
limitedOf|limitedWindowsOf|LimitedWindow|counted but not listed|only the windows the   (none)
config's limits
  GUIDE.md:198, 399; storage/types.ts:35   "whatever the config's limits", the current rule
  CONTROL-PROTOCOL.md:314   dated Rejected line
counted_through": ?null|CountedThrough == nil|\*BatchPosition|BatchPosition \| null
  protocol/fixtures/messages/totals/invalid/counted-through-null.json   the invalid
    fixture refusing it
snapshot\.(config|liveGateways|last|sequence)                         (none)
JSON\.stringify\((current|published|entry)\.config\)                   (none)
noteAcked|ackedGens                                                  (none)
config-versions|config snapshot|snapshot fetch|acknowledged batch|for another config|resumes from|kaiak\.config\.version
  "config snapshot" meaning a request's in-memory config (allowed): kaiak-control
    config/limits.ts:4, limits.test.ts:2; GATEWAY.md:1404, 1715; accounting.go:29,
    limits.go:374, limits_test.go:751, routing.go:38, 116, server/api.go:144
  CONTROL-PROTOCOL.md:51   dated Rejected line (the endpoint)
  "acknowledged batch(es)" for the batch an ack names (allowed):
    CONTROL-PROTOCOL.md:283, GATEWAY.md:2260, usage.go:318, 462, totals_test.go,
    usage_test.go:255
Totals size bound                                                    (none)
```

Also clean: `store's sequence|totals sequence|one sequence` (the remaining hits are
the input "one sequence" of token estimation and a dated Rejected line), and
`or a usage ack` (none).

**Suite** (2026-10-07, after `175e154`): `scripts/check-all.sh` three times in a
row, all green.

| Run | Ended (UTC) | Total | Gateway e2e | Control `npm test` | Lint | Cross-half |
|---|---|---|---|---|---|---|
| 1 | 11:07:27 | 207 s | 112.6 s | 605: 604 pass, 1 skipped | ok | 74.1 s |
| 2 | 11:10:48 | 201 s | 111.0 s | 605: 604 pass, 1 skipped | ok | 74.1 s |
| 3 | 11:14:11 | 203 s | 113.2 s | 605: 604 pass, 1 skipped | ok | 74.1 s |

- The skipped test is the memory store's catch-up contract test (its channel cannot
  drop a change). The lossy channel runs that test.
- Before the runs, two orphaned `node --test` processes were found and stopped. Both
  were about 10 hours old (parent 1, cwd a deleted scratchpad `prefix/control`),
  running `control-plane/review.test.ts` and `store-contract/memory.test.ts` from an
  earlier step's scratch copy.
- After the runs, no `node --test`, sample or `kaiak` process is left (`pgrep`
  empty).

**Phase 5 and the plan end here.**
