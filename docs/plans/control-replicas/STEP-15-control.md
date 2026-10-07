# Step 15 — round-3 spec rules and kaiak-control

**Status:** not started

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
