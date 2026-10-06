# Step 2 — store contract

**Status:** not started

## Intent

Change `ControlPlaneStore` so it holds every guarantee replicas need, implement it in
the memory store so several cores in one process can share one instance, and turn
the memory store's tests into contract tests any store can run.

## Files likely touched

- `control/kaiak-control/src/storage/types.ts`, with these changes:
  - **The totals sequence:** returned by the consistent read; moved by each
    totals-changing write.
  - **`saveCountedBatch`:** also conditional on the config version its additions
    were computed under; returns the new sequence when saved.
  - **The publish write:** the config version, plus the carry-over additions, saved
    in one operation, conditional on the latest version and the sequence it read.
    This replaces `saveConfig` + `addWindowTotals` as separate calls.
  - **Live-set writes:** conditional on the gateway record read (status intake and
    sweep); a write that changes the live set moves the sequence.
  - **The consistent totals read:** sequence, the instance's last batch, the latest
    config, and the current windows, as one snapshot.
  - **Change subscription:** `subscribe(listener)` hearing config published, totals
    changed, gateways changed, from any process; returns the unsubscribe.
  - **Lease methods removed.**
- `control/kaiak-control/src/storage/memory.ts`: the new contract. Shared by several
  cores in one process; its notifications reach all of them.
- `control/kaiak-control/src/storage/memory.test.ts` → contract tests written against
  a store factory, run here against the memory store, exported for host apps (a
  `kaiak-control` entry such as `storeContractTests(createStore)` using `node:test`).

## Decisions made during planning

- Method names and exact signatures are chosen in this step. The OVERVIEW's decisions
  4–7 fix what they must guarantee.
- The exported contract tests use `node:test` only, so a host app runs them with no
  added dependency.
- The core keeps compiling only against the new interface in step 3. In this step the
  core may be red where it used the old methods (an expected red, cleared by step 3).

## Acceptance criteria

- The contract tests cover each conditional write refused on a stale read, the
  sequence step per change, the consistent read under concurrent writes, and
  notification to every subscriber.
- They pass against the memory store, including with two subscribers standing in for
  two cores.
- Suite run and recorded. Expected reds: the core's tests and lint (step 3), the
  gateway's totals fixtures (step 4), the cross-half e2e (step 4).

## Result

(filled in when the step is done)
