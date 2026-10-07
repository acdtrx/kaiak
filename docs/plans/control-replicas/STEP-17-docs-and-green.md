# Step 17 — round-3 docs, phase end

**Status:** done (2026-10-07)

## Intent

Clear the remaining AUDIT-3 wording and docs, account for every finding, and end
phase 6 and the plan green.

## Scope

- **3M7:** `DEPLOYMENT.md:997-999`. In both modes, a limit added mid-window checks its
  scope's count so far.
- **3L6:** the migration note in `DEPLOYMENT.md` adds:
  - `configEpoch()`, `configsSince()`, `publishConfig`'s `beforeSave`;
  - the `ListenerEvent` variants (`limit-carried-over` removed, `delivery-failed`
    added);
  - `StoredConfig` → `PublishedConfig`/`ConfigEntry`;
  - this phase's spool format and store contract changes.
- **3L7:** each Rejected line listed in AUDIT-3 says the failure it would cause today.
- **3L8:**
  - `GATEWAY.md:2260` (the ack metric is not contact);
  - `protocol/fixtures/duplicate-members/cases.json` ("retires");
  - the boot diagram in `docs/architecture/control-plane.html` (totals come on Run's
    stream; the retried-ID list);
  - `CONTROL-PROTOCOL.md:43, 722`;
  - `spool.go:25`;
  - the OVERVIEW's superseded Scope, Risks and Verification lines, marked as decisions
    4–19 are.
- **`docs/architecture/*.html` and `ARCHITECTURE.md`:** follow decisions 29–34 where
  they describe outage, spool, totals or counters.
- **AUDIT-3 Outcome:** a table with every finding marked fixed (with its commit) or
  dissolved (with the decision).

## Acceptance criteria

- No grep hit for the phrasings 3L8 names. Repeat step 14's removal-checklist greps;
  they are still clean.
- `scripts/check-all.sh` green **three times in a row**. Record the times and counts.
- No orphaned `node --test`, sample or gateway process left.
- **Phase 6 and the plan end here.**

## Result

**Commits:**
- `740989b`: the docs;
- `1be28a1`: AUDIT-3's Outcome;
- this Result, with the OVERVIEW box.

**What changed**

- **3M7, `DEPLOYMENT.md`:** in both modes, a limit added mid-window checks its scope's
  count so far, and a fresh budget needs a new group ID. The bound note names
  `counters-exceeded` replacing `effective-limits-exceeded`.
- **3L6, the migration note:**
  - `PublishedConfig` replaces `StoredConfig`, and the store holds a `ConfigEntry`;
  - no `beforeSave` argument;
  - the `delivery-failed` listener event;
  - removed `configEpoch()`, `configsSince()` and the `limit-carried-over` event;
  - the store rules of this phase: every call settles, a read after a notification
    sees the change (from the primary), records newest first in reverse save order,
    revisions that never repeat across a restore;
  - first totals from a read after connect;
  - past windows dropped by the sweep.

  Spool 4 and `totals.json` 5 were already there from `e206388`.
- **3L7:** five Rejected lines now say the failure the alternative causes.
  - `CONTROL-PROTOCOL.md`: the config history, the config epoch,
    `config-unavailable`, counting under the config's limits.
  - `GATEWAY.md`: the totals revision.
- **3L8:**
  - `GATEWAY.md`: the `kaiak_usage_last_ack_timestamp_seconds` row says an ack is not
    contact; a garbled "on and on" in the alert note is fixed.
  - `cases.json`: "the batch the ack names".
  - `spool.go`: the epoch-order invariant states why. Generations follow send order,
    and retirement relies on that. "The control plane never goes back to an older
    epoch" was wrong.
- **`docs/architecture/control-plane.html`:**
  - The boot diagram: the boot stream ends at its config; the running stream skips
    it by hash and brings the totals, read after it connected.
  - The usage diagram: on the ack the batch is never sent again and waits to be
    shown counted; the spool keeps it until a `totals.json` save covers it (the
    aria-label too).
  - "Retried with the same ID" includes an ack naming another batch.
  - "On the stream only" and the contact row name the acknowledged-batch wait.
- **`docs/architecture/gateway.html`:** the `limits` and `control` rows:
  - counts kept until their window ends;
  - the three outage causes;
  - `totals.json` in the background;
  - a malformed totals event ends the stream;
  - the spool kept until covered and rebuilt after a crash.
- **`docs/ARCHITECTURE.md`:** the `limits` and `control` bullets, likewise.
- **OVERVIEW:**
  - Scope, Risks and Verification lines superseded by decisions 18–22 and 29 are
    marked so (K3-L9);
  - the Risks mitigation names the lossy-channel catch-up;
  - the Phase 6 box is checked.
- **AUDIT-3 Outcome:** every finding is fixed with its commits, except two:
  - 3M4 is documented (a restore rule; the memory store has no restore);
  - the clock skew is accepted (decision 28).

**Removal checklist** (`git grep -n -E <pattern> -- . ':!docs/plans' ':!docs/reviews'`):

```
highestSequence|observeSequence|CurrentConfig\b                       (none)
change\.sequence|saved: true; sequence                               (none)
onRollback|[Rr]ollback
  docs/specs/CONTROL-PROTOCOL.md:192, docs/architecture/control-plane.html:280
    dated Rejected lines (allowed)
lastSent|\bdelivered\b                                              (none)
limitedOf|limitedWindowsOf|LimitedWindow|counted but not listed|only the windows the   (none)
counted_through": ?null|CountedThrough == nil|\*BatchPosition|BatchPosition \| null
  protocol/fixtures/messages/totals/invalid/counted-through-null.json  (the invalid
    fixture refusing it)
snapshot\.(config|liveGateways|last|sequence)                         (none)
JSON\.stringify\((current|published|entry)\.config\)                   (none)
noteAcked|ackedGens                                                  (none)
Totals size bound                                                    (none)
config-versions|snapshot fetch|for another config|resumes from|kaiak\.config\.version   (none)
store's sequence|totals sequence|one sequence
  CONTROL-PROTOCOL.md:1084 (a dated Rejected line); the rest the token estimate's
    "input one sequence sees"
existed only|it existed|existed because|needed only while            (none)
```

Step 16's removed names:

```
restoredGeneration|RestoredGeneration|clearedAt|restoredUntagged|totals event ignored|
MaxEffectiveLimits|CodeEffectiveLimitsExceeded|countEffectiveLimits|
effective-limits-exceeded|base_window_start|"uncounted"
  docs/DEPLOYMENT.md:1001   the migration note naming the replaced rule (allowed)
  gateway/internal/limits/shared_test.go:956-957   the format-4 file the discard test
    refuses (allowed)
restored lump|uncounted usage restored|written whenever totals are applied|drops the batch from
  (none)
```

The other hits are the same allowed phrases as before:
- "config snapshot" means a request's in-memory config: 10 hits, plus the dated
  Rejected endpoint line;
- "acknowledged batch" is the batch an ack names, which the spool now keeps.

**Suite** (2026-10-07): `scripts/check-all.sh` three times in a row, all green.

| Run | Ended (UTC) | Total | Gateway e2e | Control `npm test` | Lint | Cross-half |
|---|---|---|---|---|---|---|
| 1 | 13:29:17 | 200 s | 113.0 s | 614: 613 pass, 1 skipped | ok | 69.2 s |
| 2 | 13:32:33 | 196 s | 109.8 s | 614: 613 pass, 1 skipped | ok | 69.1 s |
| 3 | 13:35:52 | 199 s | 113.9 s | 614: 613 pass, 1 skipped | ok | 69.1 s |

- The skip is the memory store's catch-up contract test, whose channel cannot drop a
  change; the lossy channel runs that test.
- No `node --test`, sample or `kaiak` process is left; the run logs are deleted.
- **Phase 6 and the plan end here.**
