# Step 15 — round-3 spec rules and kaiak-control

**Status:** done (2026-10-07)

## Intent

Write the spec rules for decisions 29–34 (`OVERVIEW.md`) on both halves' specs, then
fix the control-side round-3 findings (`docs/reviews/2026-10-07/AUDIT-3.md`) in the
store contract, the GUIDE and `kaiak-control`. Step 16 implements the gateway side.

Phase 6 is green only at its end. Gateway reds that step 16 clears are expected here;
name them.

## Scope

- **Specs, both halves**, written in this step, dated 2026-10-07:
  - `GATEWAY.md` and `CONTROL-PROTOCOL.md`:
    - the usage-waiting rule: an acknowledged batch waits until covered (decision 29);
    - the outage rule's reason;
    - a malformed totals event ends the stream (decision 31; replaces "logged and
      skipped" for totals only);
    - the spool keeping batches until saved totals cover them, and the restart
      rebuild (decision 32);
    - `totals.json` written on an interval and holding current windows only;
    - the spool format bump;
    - counters living by scope until their window ends (decision 33);
    - the memory bound counting allocated counters (decision 34).
  - `CONTROL-PROTOCOL.md`:
    - a stream's first totals come from a read issued after it joined (decision 30);
    - a push whenever a cursor moves.
- **kaiak-control:**
  - **First totals after join (decision 30, 3H1):** the feed issues a read on every
    join. A stream's complete totals wait for a read issued after its join. Until
    then the stream gets no totals. Heartbeats and configs are unaffected.
  - **A push when only a cursor moved:** confirm it, and test it with a zero-cost
    batch.
  - **Publish (3M3):** parse the captured text once before the first await. Use that
    document for validation, every parents check and the returned `config`.
  - **Memory bound (decision 34, 3M2):** the semantic check counts two counters per
    scope plus each scope's effective per-minute counters, 50 000 at most. Update the
    rule's name, message and fixtures as needed. The gateway's validator changes in
    step 16; shared fixtures change here.
  - **Prune (3L1):** dropping past windows after a counted batch is best-effort. A
    failure is logged and never fails the batch.
  - **The "0" rule (3L5):** add a control-side test.
- **Store contract and tests:**
  - **Read after notification (3M5):** for each change type, a listener's read sees
    the change:
    - `config-published` → `currentConfig()` has the announced hash;
    - `gateways-changed` → `gateway()` / `gateways()` show the record;
    - `batch-counted` → the snapshot, as today.
    - Add a negative control: a store that announces before its write is visible.
  - **Every call settles (3L2):** state it in `types.ts`. The GUIDE says to set
    statement timeouts.
  - **Negative controls (3L3):** match the failure count and the assertion text, not
    only the test name.
  - **Records newest first (3M6):** `types.ts` defines ties as insertion order.
- **GUIDE:**
  - **Restore (3M4):** stop every control-plane process; restore; move the revision
    allocator past every value issued; start (each start catches up). The contract
    states that revisions never repeat across a restore.
  - Every read after a notification goes to the primary (3M5).
  - **Postgres sketch (3M6):**
    - an insertion-order column for records;
    - forgets in instance order (also sort `toForget` in `gateways/index.ts`);
    - "likely to catch" for the torn-read claim;
    - the `int8`/`numeric`-as-string representation note.
- **Regression tests:** port [C]'s control reproductions from the session scratchpad
  (`audit-c3/fastify-audit-c.test.ts`, `config-publishing-audit-c.test.ts`,
  `storage-audit-c.test.ts`).
  - Adapt each to the decision taken. For C1, the reconnect gets no totals until a
    fresh read succeeds. For C7, test the contract's restore rule, not the sweep.
  - Write [R]'s from AUDIT-3.
  - Each fails before its fix; say so in Result.

## Files likely touched

- `docs/specs/CONTROL-PROTOCOL.md`, `docs/specs/GATEWAY.md`.
- `control/kaiak-control/src/{fastify,config-publishing,config,usage,gateways,storage,store-contract}/**`,
  `GUIDE.md`.
- `protocol/fixtures/**` (the bound).

## Acceptance criteria

- `npm test` and `npm run lint` from `control/` pass.
- Every negative control fails for its stated reason.
- Suite run and recorded. Expected reds: the gateway's bound fixture and anything that
  depends on step 16, named.

## Result

**Commits:**
- `86b905d`: the specs, both halves;
- `a7526cd`: kaiak-control, the store contract, the fixtures, the GUIDE, and the bound
  wording in `DEPLOYMENT.md` and `gateway.html`;
- this Result.

**What changed**

- **`CONTROL-PROTOCOL.md`** (all dated 2026-10-07, each with its Rejected line):
  - Config stream:
    - on connect, totals come from a read issued after the stream connected;
    - the Totals bullet says a counted batch that changes no window is still pushed
      for its cursor, and that a new stream gets no totals until a read issued after
      it succeeds;
    - a failing totals read shows on the gateways as usage that stays uncounted;
    - closing the streams on failing totals reads is Rejected;
    - a restored store points at the restore procedure.
  - Messages:
    - a `totals` event the gateway cannot decode ends the stream;
    - the ack: the gateway stops sending the batch, which stays in the spool until
      saved totals cover it.
  - Usage batches:
    - state per (instance, epoch);
    - the ack keeps the batch spooled until covered.
  - Config → The group tree: **Counters are bounded** replaces "Effective limits are
    bounded". The rule is now `counters-exceeded`: two per scope plus each effective
    per-minute limit, 50 000.
  - Usage intake → Past windows: dropped by each process's expiry sweep once an hour.
    The failure is the sweep run's; dropping on a batch is Rejected.
  - Status intake:
    - revisions never repeat across a restore;
    - the sweep forgets in instance order and drops past windows.
  - Control-plane processes:
    - **Every call settles**;
    - **A read after a notification sees the change**, from the primary;
    - **Restoring the store**: stop all processes, restore, move the revisions past
      every issued value, start.
  - Control-plane outage → What counts as an outage: usage waits for an answer, and
    an acknowledged own batch waits to be shown counted (decision 29).
  - Also reworded the stale summaries [C] C9 named (`:43` totals "per limit", `:722`
    "per instance").
- **`GATEWAY.md`:**
  - Local counters:
    - an hour and a month counter per scope, plus one per per-minute limit;
    - only per-minute counters hold buckets;
    - the 50 000 bound.
  - Every scope is counted:
    - **A count outlives its scope's config** (decision 33);
    - Config reload and the file-mode snapshot point to it.
  - Restart keeps the last totals, rewritten (decision 32):
    - `totals.json` **format 5**: bases of the current windows, the
      `counted_through` they include, and the live count (no `uncounted`);
    - own usage is rebuilt from the spool's batches beyond the saved
      `counted_through`;
    - written in the background every 30 s and at shutdown;
    - another instance's restored batches leave once this instance's first batch is
      shown counted.
  - Usage acks count for money limits: the two waits (for an answer; to be shown
    counted, since the oldest acknowledged uncovered batch's ack). Another instance's
    batches wait only for an answer.
  - Stream: a malformed `totals` event ends the stream.
  - The batch store remembers acknowledged batches until covered, at most 10 000;
    the wait keeps its time when the oldest is forgotten.
  - Usage spool **format 4**:
    - the index holds each epoch's last acknowledged sequence;
    - a batch file is deleted once a covering `totals.json` save has completed, or on
      its ack if it is another instance's.
  - The Client API table's outage row names the new wait.
- **Store contract** (`storage/types.ts`, `store-contract/`):
  - Every call settles; records are newest first in reverse save order; revisions
    never repeat across a restore; a read after a notification sees the change.
  - New test, "a change is heard after its write: a read in the listener sees what it
    announced". Each change type is read back by a listener on the other handle: the
    config hash after each of two publishes, the cursor after a batch, the record
    after a gateway write. It replaces the batch-only test.
  - New negative control `early-announcement-store.ts`: announces a publish 20 ms
    before writing it.
  - `negative-control.test.ts` now checks that exactly the named tests fail
    (`# fail N`) and that the failure's reason appears:
    - torn: 2 tests, "cursor and windows";
    - both catch-up stores: 1 test, "no notification within … catch-up 1";
    - early announcement: 1 test, "config missed".
- **kaiak-control:**
  - `fastify/totals-feed.ts`:
    - every read takes a number when issued;
    - `join` always issues a read and returns `{ leave, firstRead }`;
    - `TotalsState.read`.
  - `fastify/gateway-stream.ts`: complete totals wait for a read at or past the
    stream's `firstRead` (decision 30, 3H1).
  - `config-publishing`: the text is taken once, before the first wait. The parents
    rule and the returned `config` use `JSON.parse(text)` (3M3).
  - `config/semantic.ts`: `counters-exceeded` with `countCounters` (decision 34,
    3M2). `effective-limits-exceeded`, `MAX_EFFECTIVE_LIMITS` and
    `countEffectiveLimits` are removed.
  - `usage`: counting no longer drops past windows. `dropPastWindows()` drops once per
    hour per process (3L1).
  - `gateways`:
    - the sweep sorts `toForget` by instance (3M6);
    - it calls `dropPastWindows` last, a new option the core wires from the usage
      module.
- **Fixtures** (`protocol/fixtures/config/`):
  - `valid/effective-limits-at-bound.json` → `valid/counters-at-bound.json`: exactly
    50 000. Global has only hour and month limits (2), users 4, 12 498 children ×4,
    and a new `spare` with no limits (2).
  - `invalid/effective-limits-exceeded.json` → `invalid/counters-exceeded.json`: the
    same, with `spare` given one per-minute limit (50 001).
  - `cases.json` updated.
  - Counts checked by script: 50 000 and 50 001.
- **GUIDE:**
  - §5 intro: reads after a notification on the primary; every call settles (a
    statement timeout).
  - Table:
    - "likely to catch" for the torn read;
    - records newest first in reverse save order (an insertion-order column);
    - revisions never repeat across a restore.
  - Representation note: node-postgres returns `int8`/`numeric` as strings.
  - Sketch:
    - `usage_record.saved` identity column;
    - forgets in the order given (sorted by the core);
    - `totalsSnapshot` on the primary like every read after a notification.
  - The restore procedure (4 steps, including `setval` on `gateway_revision`)
    replaces "Nothing needs to be done".
  - §3:
    - "Counters are bounded";
    - a host serving its own streams starts each stream's totals from a read issued
      after it connected.
  - §11: the fourth broken store.
  - The header names the round-3 update.

**Decisions made in this step**

- **The past-window drop moves to the expiry sweep.** The brief asked for a
  best-effort prune whose failure is logged. The core has no logger, and a silent
  catch is not allowed, so the drop runs as the sweep's last step, at most once an
  hour per process. A failure fails that sweep run, which `onExpirySweep` reports to
  the host. Counting a batch never touches it.
- **The rule is renamed `counters-exceeded`.** Its count changed meaning, so the old
  name `effective-limits-exceeded` is removed with the old count, on both halves'
  fixtures. The gateway follows in step 16.
- **The spec fixes the waiting rules step 16 implements:**
  - the "to be shown counted" wait runs from the oldest acknowledged uncovered batch's
    ack, and an ack never restarts it (otherwise acks flowing past dead totals would
    keep resetting it);
  - at most 10 000 acknowledged batches are remembered without a data directory;
  - `totals.json` is written every 30 s, the file-mode snapshot's interval;
  - the formats are spool 4 and `totals.json` 5.
- **[C] C7 (restore reusing revisions) is fixed in the contract and the GUIDE, with no
  ported test.** The rule is on how a host restores its store. The memory store has
  no restore operation, and C7's reproduction models a store breaking the rule, so it
  would only assert that a broken store is broken.

**Regression tests** (each run first against the pre-fix code at `390fb73` in a
scratch copy, since deleted):

| Test | File | Finding | Before the fix |
|---|---|---|---|
| a new stream's first totals include every batch counted before it connected | `fastify/round-3.test.ts` | 3H1 ([R] R3-M1) | fails: `counted_through` `[]`, want seq 1 |
| while totals reads fail a new stream gets no totals; the first read that works sends them complete | same | 3H1 ([C] C1, [R] R3-M2) | fails: complete totals with no windows sent at once |
| a counted batch that changes no window is pushed for its cursor | same | decision 29 | passes: confirms the existing push on a cursor change |
| a window a restored store lacks within its window is pushed at 0 | same | 3L5 | passes: the "0" rule held, it had no test |
| a publish checks the parents rule against what it writes, whatever the caller does to its object meanwhile | `control-plane/round-3.test.ts` | 3M3 ([C] C4) | fails: the publish succeeds |
| a store failing to drop past windows never fails a batch; the sweep reports it | same | 3L1 ([R] R3-L1) | fails: the batch rejects with "prune failed" |
| a sweep forgets gateways in instance order | same | 3M6 ([K] K3-L1) | fails: `gw-c, gw-a, gw-b` |
| a config of groups without limits past the bound is refused | `config/config.test.ts` | 3M2 ([C] C6) | fails: accepted |
| the contract tests fail a store that announces a publish before its write is visible | `store-contract/negative-control.test.ts` | 3M5 ([R] R3-M3) | the old suite passes that store: 24 pass, 0 fail |

**Tests changed:**
- The bound test is rewritten for the new count. New cases: an empty group takes 2
  counters; hour and month limits add none.
- `usage.test.ts`'s hour rollover now drops by calling `dropPastWindows()` and
  asserts that counting drops nothing; it asserted the removed drop on the first batch
  of an hour.
- The batch-only read-after-notification contract test became the per-type one.

**Left for later steps:**
- Step 16:
  - the gateway's own bound, which is the expected red below;
  - every gateway rule written here;
  - `DEPLOYMENT.md`'s format notes (spool 4, `totals.json` 5).
- Step 17: `docs/architecture/control-plane.html:286, 339`, where the ack still "drops
  the batch from its spool".

**Suite** (2026-10-07):
- Control:
  - `npm test`: 614 tests, 613 pass, 0 fail, 1 skipped (the memory store's catch-up
    contract test; the lossy channel runs it);
  - `npm run lint`: passes.
- `scripts/check-gateway.sh`:
  - gofmt, vet and staticcheck pass;
  - every package passes, the gateway e2e included (115.0 s), except
    `kaiak/internal/config` — `TestInvalidFixtures/counters-exceeded.json`, the
    gateway's validator still counting effective limits. **Expected, cleared by step
    16.**
- Cross-half tests, run directly (`check-all.sh` stops at the gateway): `ok kaiak/e2e
  69.4 s`.
- No `node --test` process is left.
