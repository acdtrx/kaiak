# Step 2 — store contract

**Status:** done (2026-10-06)

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

**What changed**

- `control/kaiak-control/src/storage/types.ts`: the store contract, with what each
  method guarantees. Methods:
  - `configEpoch()`: unchanged; it now also bounds the totals sequence. A store that
    loses or rolls back its state takes a new one.
  - `latestConfig()`, `configsAfter(version)`: unchanged.
  - `publishConfig({ entry, carried }, { version, sequence }, keep)`: the version and
    the carried spend in one write, only while the latest version and the totals
    sequence are the expected ones. It moves the sequence by one and answers the new
    sequence, or `{ saved: false, latestVersion, sequence }`. It replaces
    `saveConfig` + `addWindowTotals`.
  - `lastBatch(instance)`: unchanged.
  - `saveCountedBatch(counted, expectedLast, keepRecords)`:
    - `CountedBatch` gains `configVersion`.
    - The write is also conditional on that version still being the latest.
    - It moves the sequence and answers `{ saved: true, sequence }`, or `{ saved:
      false, last, configVersion }`.
  - `totalsSnapshot(current, instance?)`: the consistent read. It returns `sequence`,
    the latest `config`, the instance's `last` batch, the current windows and
    `liveGateways`, all from one point between writes. It replaces
    `currentWindowTotals`, and the core's separate `lastBatch` / `latestConfig` /
    live-count reads for a totals message.
  - `dropPastWindowTotals(oldest)`: unchanged. It does not move the sequence.
  - `recentRecords(limit)`: unchanged.
  - `gateway(instance)`, `gateways()`: records now carry a store-assigned `revision`,
    from 1 per write. The record a core writes is `GatewayRecord`.
  - `saveGateway(record, expectedRevision)`: conditional on the stored record's
    revision (undefined means none). A change to `live` moves the sequence in the same
    write. It answers `{ saved, revision, sequence }` or `{ saved: false, current }`.
  - `forgetGateways([{ instance, revision }])`: replaces `deleteGateways`. Each forget
    is conditional on its revision, it answers the instances forgotten, and forgetting
    a live gateway moves the sequence once per call.
  - `dropBatchCursorsCountedBefore(cutoff)`: unchanged.
  - `subscribe(listener)`: every change any process makes, its own included, after
    its write and in the store's order. The change types are `config-published`
    (version, sequence), `batch-counted` (instance, sequence) and `gateways-changed`
    (liveChanged, sequence). The store reports a throwing listener; the others still
    hear the change.
  - Removed: `acquireLease`, `releaseLease`, `StoreLease`, `AcquireLeaseResult`,
    `saveConfig`, `addWindowTotals`, `currentWindowTotals`, `deleteGateways`.
- `control/kaiak-control/src/storage/memory.ts`: implements the contract. Every write
  is synchronous from its comparison to its notification, so several cores in one
  process can share one store, and every subscriber hears every change. A publish
  whose version does not follow the expected one is a caller fault
  (`config-version-out-of-order`).
- `control/kaiak-control/src/store-contract/index.ts`, new: `storeContractTests({
  name, create, attach?, dispose?, notifyTimeoutMs? })`, written with `node:test`
  only. `attach` gives a second handle on the same store, as another process holds
  it; the tests write through one handle and read and listen through the other, and
  race writes across both. It is exported as `kaiak-control/store-contract`, a second
  package entry (`package.json` `exports`), so a running control plane never loads
  `node:test`.
- The 23 contract tests:
  - Config epoch: its form, and that it is stable.
  - Publishing:
    - conditional on version and sequence;
    - the carry written with the version, and a refused publish carrying nothing;
    - `keep`;
    - concurrent publishes, one stored;
    - stored configs isolated from callers.
  - Counting:
    - conditional on the last batch;
    - conditional on the config version;
    - one batch raced from both handles, counted once;
    - records newest first, bounded;
    - a batch cursor outliving a forgotten gateway, then the retention.
  - The sequence and snapshots:
    - one step per change, and none for refused writes or past-window drops;
    - snapshot consistency: 32 batches counted from both handles while both read, and
      every snapshot's windows equal the batches counted at its sequence;
    - current versus past windows;
    - windows adding up, and model sets kept apart;
    - the live count in snapshots.
  - Gateways:
    - conditional on the revision;
    - one of six concurrent writes wins;
    - conditional forgetting, and a live one moving the sequence;
    - records isolated from callers.
  - Notification:
    - every subscriber on both handles hears every change, in order;
    - a read in the listener sees the write;
    - refused writes are not announced, and unsubscribing stops the calls.
- `src/store-contract/memory.test.ts` runs the contract against the memory store.
  `src/storage/memory.test.ts` keeps what goes beyond it: a new epoch per store, and
  the version-order fault.
- A mutation check, by hand: each of four broken memory stores fails the contract
  (no sequence check on publish → 1 failure; no config-version check on counting → 1;
  no revision check on gateways → 3; live changes not moving the sequence → 3).
- `docs/plans/control-replicas/OVERVIEW.md`: step 1's changes are recorded as
  accepted. Decision 10's epoch rule is changed; decisions 13 (a rollback takes a new
  epoch) and 14 (every status write is conditional) are added.

**Suite** (2026-10-06)

- `node --test` on `src/store-contract/` and `src/storage/`: 25 pass, 0 fail.
- Control `npm test`: 597 tests, 444 pass, 152 fail. **Expected, cleared by step 3.**
  Every failure is in code that drives the core over the old store methods:
  `fastify` 65, `usage` 30, `config-versions` 21, `control-plane` 12, `gateways` 7,
  the sample's `config-file` 8, `app` 5 and `page` 2. Storage, store-contract,
  config, messages, keys, protocol, backend-verify and the sample's verify, settings
  and keygen all pass.
- `npm run lint`: `tsc` reports 53 errors. **Expected, cleared by step 3.** All are in
  the core and its tests calling removed methods or using removed types:
  - `usage/index.ts` 8, `usage.test.ts` 19
  - `gateways/index.ts` 4
  - `config-versions/index.ts` 1 and its test 1
  - `control-plane/index.ts` 2 and its test 16
  - `fastify/status-totals.test.ts` 2

  None are in `storage` or `store-contract`. The boundary lint passes.
- `scripts/check-all.sh`: gofmt, vet and staticcheck pass. `go test -race` fails only
  in `internal/control` with step 1's five totals tests, cleared by step 4. The script
  stops there, so the control half and the cross-half e2e did not run in it; the
  control results are above.
