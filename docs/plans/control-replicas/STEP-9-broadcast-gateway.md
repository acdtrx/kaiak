# Step 9 — broadcast-only gateway

**Status:** done (2026-10-07)

## Intent

Implement step 7's contract in the gateway, removing every gateway-side item on the
removal checklist. This step ends phase 3: the whole suite green, and the removal
checklist clean across the repo.

## Files likely touched

- `gateway/internal/control/`:
  - **Boot:** from the stream's first `config` event; the snapshot fetch is removed.
  - **Config events:** every `config` event is applied, and one whose `config_hash`
    equals the running config's is skipped.
  - **Status:** reports `applied_config_hash` and the rejected hash.
  - **Totals:** applied from the stream only; no ordering by revision or epoch; no
    retired-store list.
  - **Acks:** they only drop the batch from the spool.
  - **Own usage:** retired only by a stream totals message whose `counted_through`
    covers it.
- `gateway/internal/limits/`:
  - Windows are applied to the running limits by (scope, type), whatever config.
  - The config-mismatch state, its grace, metric and log lines are removed.
  - The outage and "no totals yet" refusals stay.
  - Totals cache format bump.
- `gateway/internal/state/` (`last-known-good.json`): config plus hash, format bump.
- `gateway/internal/metrics/`: `kaiak_control_config_mismatch` removed.
- `gateway/internal/fakecontrol/`: speaks the new stream and ack.
- `gateway/e2e/` and the cross-half tests: version, resync and mismatch expectations
  removed. Add:
  - a restored store with an older config is applied;
  - a rejected config keeps budgets enforced from stream totals;
  - an ack carries no totals.
- The live kit, if it reads versions.

## Acceptance criteria

- `scripts/check-all.sh` green, including both cross-half tests: **phase 3 ends
  here**.
- Step 7's removal checklist greps clean across the whole repo, outside `docs/plans/`
  and `docs/reviews/`. Every grep and its output is recorded in Result.
- No test was weakened to pass. Each deleted test is listed in Result with the removed
  behaviour it asserted.

## Result

**What changed** (commits `d5c45cd`, `b6022aa`, `bdeeb1b`, `bf7683d`, the staticcheck
fix and `28d11bf`)

- **`internal/control`:**
  - **Boot** opens `GET /v1/stream` (no parameters) and waits for its first `config`
    event within the boot wait, opening the stream again while the control plane is
    unavailable. The stream is closed once the config arrives; `Run` opens its own,
    whose first config — the same one — is skipped by its hash. A control plane with
    nothing published keeps the boot's stream open, and a config published within
    the wait arrives on it. The snapshot fetch (`fetchSnapshot`, `snapshotTimeout`)
    is removed.
  - **Config events:** every `config` event goes through the apply path, whatever it
    replaces, unless its `config_hash` is the running config's or the last rejected
    one's (`takeConfig`; logged at debug). The client keeps `appliedHash` and the
    rejection by hash. `configPosition`, `position`, `applied` (version and epoch),
    `takeSnapshot`, `takeStreamConfig`, `positionRefused`, `appliedEpoch`,
    `currentPosition` and `AppliedVersion` are removed; `AppliedConfigHash` replaces
    the last.
  - **Status:** `applied_config_hash`, and `last_rejection.config_hash`.
  - **Totals:** decoded from the stream only; every one is handed to the consumer, in
    stream order (`takeTotals(Totals)`). `appliedTotals`, `lastTotals`, `totalsMu`,
    the epoch filter, the revision ordering and the retired-store list are removed.
  - **Acks:** `UsageAck` is `{ batch }`; an ack drops the batch from the store and
    moves it to the acknowledged list (`noteAcked`). `countedGeneration` covers, in
    send order, every acknowledged or queued batch up to the one `counted_through`
    names, and forgets the acknowledged ones it covered, so an acknowledged batch's
    usage leaves the limiter once stream totals show it counted.
  - **Messages:** `ConfigEvent { config_hash, config }` replaces `ConfigSnapshot`;
    `Resync`, `Revision`, `Totals.ConfigEpoch`/`ConfigVersion`/`Revision`,
    `DecodeConfigSnapshot`/`DecodeResync` are removed; `TotalsUpdate.Totals` is a
    value. The walker checks `config_hash` (64 lowercase hex), the status without the
    version/epoch pair, the ack without totals.
  - **`last-known-good.json`** is format 7: `{ config_hash, config }`; a malformed
    hash is discarded at boot.
- **`internal/limits`:** `TakeTotals(Totals, counted)` applies every totals message —
  windows by group (or global) and type, whatever config runs — and retires the
  counted generations at once. Removed: `Totals.Config`, `waiting`, `latest`,
  `haveLatest`, `pendingCounted`, `mismatchSince`, `noteMismatchLocked`,
  `mismatchPastGraceLocked`, `ConfigMismatch`, `applyWaitingLocked`; the
  `budget_unavailable` refusal is the outage and "no totals yet" only.
  `totals.json` is format 4, with no config identity (windows restored by group and
  type onto the booted config).
- **`internal/config`:** `Version`, `Snapshot.Version` and `Applier.ApplyPublished`
  are removed: the gateway keeps no config identity beyond the control client's
  hash.
- **`internal/metrics`:** `kaiak_control_config_mismatch` and
  `ControlState.ConfigMismatch` are removed; contact and totals help text without
  snapshots or ack totals.
- **`cmd/kaiak`:** `OnTotals` hands every totals event to the limiter and saves the
  totals cache; the seed check uses `AppliedConfigHash`; `limitsTotals` takes a
  value.
- **`internal/fakecontrol`:** one current config (`Publish` returns its hash, `Hash`
  and `ConfigEvent` helpers); the stream sends the current config, then totals
  (none before a config), then every publish; usage is counted whatever is
  published; acks are `{ batch }`; `Restart` forgets the config; streams carry a
  `Seq` (`Opened`), so tests skip a boot's stream. Removed: `GET /v1/config`,
  `since`/`config_epoch`, resync, the revision, `ConfigEpoch()`, `Revision()`,
  `Snapshot()`.
- **Docs:** README, `docs/ARCHITECTURE.md` (gateway and control-plane modules),
  `docs/DEPLOYMENT.md` (boot, the alerts — the mismatch alert removed — the seed, the
  upgrade notes with the three data-file formats), `docs/architecture/gateway.html`,
  `docs/kaiak.md` (the app owns config; the library broadcasts it), the GATEWAY.md
  log table (the dead `kaiak.config.since` and `kaiak.config.position` rows), the
  backlog's totals-size entry.

**Tests deleted or rewritten** (each asserted removed behaviour; none weakened to
pass):

- `internal/control`:
  - `TestBootAppliesTheSnapshotAndSendsTheProtocolHeaders` → boot from the stream's
    first config: the snapshot request and the snapshot's version identity.
  - `TestReconnectResumesAfterTheAppliedVersion` → `TestReconnectTakesTheCurrentConfig`:
    resuming after a version.
  - `TestResyncAppliesALowerVersion` → `TestRestoredOlderConfigIsApplied`: resync.
  - `TestStreamNeverGoesBackAVersion` → `TestEveryConfigEventIsApplied`: "only newer".
  - `TestRejectionBelowTheAppliedVersionIsReported`: deleted — version numbers after
    a restart.
  - `TestLastKnownGoodFromAnotherEpochIsReplacedAtTheSameVersion` →
    `TestLastKnownGoodIsReplacedByTheCurrentConfig`: epochs and resync.
  - `TestSinceInvalidFetchesTheSnapshot`: deleted — `since-invalid` and the snapshot.
  - `TestStreamConfigFromAnotherEpochIsAppliedWhateverItsVersion`: deleted — epochs.
  - `TestMalformedLastKnownGoodIsDiscarded`: the epoch and version cases → the hash
    case.
  - `TestBootRetriesUntilTheControlPlaneComesUp`: its `503 config-unavailable` case →
    a control plane answering without the protocol header; new
    `TestBootTakesAConfigPublishedWithinTheWait`.
  - `TestTotalsAreAppliedInRevisionOrder` → `TestEveryTotalsEventIsAppliedInStreamOrder`:
    revision ordering.
  - `TestTotalsFollowTheRunningConfigEpoch` → `TestCountedThroughCoversBatchesInSendOrder`:
    the epoch filter and the retired-store list.
  - `TestPushCountingTheOutstandingBatch`: the ack delivering the generation again,
    without totals → only pushes count; new `TestAckedBatchLeavesWithTheTotalsThatCountIt`.
  - `TestUsageBatchesSealAtTheSizeLimit`, `TestLostAckIsResentAndCountedOnce`: their
    ack-totals assertions; `TestSpoolOfAnotherFormatStartsANewEpoch` waits for the
    acked result instead of ack totals.
  - Status, seed and fixture tests: versions and epochs → hashes; the `resync`
    decoder and its integer-spelling case.
- `internal/limits`:
  - `TestTotalsOfAnotherConfigKeepTheSpentBudget` → `TestTotalsApplyWhateverTheConfig`:
    totals waiting for their config, the mismatch state and refusal.
  - `TestCountedUsageIsNeitherDoubledNorDropped`: the ack's generation and stale ack
    updates → a repeated push and the next push.
  - `TestUnpricedModelsAreNeverRefusedForBudgets`: its mismatch part.
  - `TestSharedStateSurvivesARestart`: "a file of another config is discarded" →
    windows matched by group and type.
  - `TestNoTotalsYetRefusesMoneyLimitedModels`: "totals for another config wait".
- `internal/metrics`: the mismatch gauge's assertions.
- `cmd/kaiak`: `TestLimitsTotalsCarryTheirConfig` →
  `TestLimitsTotalsCarryWindowsByGroupAndType`.
- `gateway/e2e`: `TestRejectedConfigKeepsTheSpentBudget` →
  `TestRejectedConfigKeepsBudgetsEnforcedFromStreamTotals` (v1's bases kept against
  v2's windows → each gateway judges the stream's window against its own config's
  limit); the replicas test's checks for resync and other-epoch log lines (lines
  that no longer exist); the cross-half test's resync waits (the restarted sample's
  config is the one the gateways run: skipped by hash).

**Tests added** (the step's list): a restored older config applied
(`TestRestoredOlderConfigIsApplied`, e2e and client); a rejected config keeps budgets
enforced from stream totals (e2e, above); an ack carries no totals (e2e: the
applied-totals time stays put after an ack, moves with the next push; client:
`TestAckedBatchLeavesWithTheTotalsThatCountIt`).

**Removal checklist** — every grep, across the repository outside `docs/plans/` and
`docs/reviews/`:

```text
$ git grep -n -I -E 'config_version|configVersion|ConfigVersion' -- . ':(exclude)docs/plans' ':(exclude)docs/reviews'

$ git grep -n -I -E 'config_epoch|configEpoch|ConfigEpoch|applied_config_epoch' -- . ':(exclude)docs/plans' ':(exclude)docs/reviews'

$ git grep -n -I -E 'applied_config_version' -- . ':(exclude)docs/plans' ':(exclude)docs/reviews'

$ git grep -n -I -E 'revision|Revision' -- . ':(exclude)docs/plans' ':(exclude)docs/reviews'
control/kaiak-control/GUIDE.md:171:| `gateway`, `gateways`, `saveGateway(record, expectedRevision)`, `forgetGateways([{ instance, revision }])` | Latest status 
control/kaiak-control/GUIDE.md:203:create table gateway      (instance text primary key, revision int not null, live boolean not null,
control/kaiak-control/GUIDE.md:233:-- saveGateway / forgetGateways: the revision is the condition.
control/kaiak-control/GUIDE.md:234:update gateway set state = $state, live = $live, revision = revision + 1
control/kaiak-control/GUIDE.md:235:  where instance = $instance and revision = $expectedRevision;   -- 0 rows: refused
control/kaiak-control/src/gateways/index.ts:135:      const written = await store.saveGateway(judged.record, previous?.revision);
control/kaiak-control/src/gateways/index.ts:186:          toForget.push({ instance: gateway.instance, revision: gateway.revision });
control/kaiak-control/src/gateways/index.ts:189:          const { revision, ...record } = gateway;
control/kaiak-control/src/gateways/index.ts:190:          const written = await store.saveGateway({ ...record, live: false }, revision);
control/kaiak-control/src/storage/memory.ts:123:    async saveGateway(record, expectedRevision) {
control/kaiak-control/src/storage/memory.ts:125:      if (stored?.revision !== expectedRevision) return { saved: false, current: stored && structuredClone(store
control/kaiak-control/src/storage/memory.ts:126:      const revision = (stored?.revision ?? 0) + 1;
control/kaiak-control/src/storage/memory.ts:127:      gateways.set(record.instance, { ...structuredClone(record), revision });
control/kaiak-control/src/storage/memory.ts:131:      return { saved: true, revision, sequence };
control/kaiak-control/src/storage/memory.ts:137:      for (const { instance, revision } of forget) {
control/kaiak-control/src/storage/memory.ts:139:        if (stored?.revision !== revision) continue;
control/kaiak-control/src/storage/types.ts:136:  revision: number;
control/kaiak-control/src/storage/types.ts:143:  | { saved: true; revision: number; sequence: number }
control/kaiak-control/src/storage/types.ts:146:// A gateway to forget, with the revision of the record the sweep judged.
control/kaiak-control/src/storage/types.ts:149:  revision: number;
control/kaiak-control/src/storage/types.ts:210:  // is still the one at `expectedRevision` (undefined: none stored). A write that
control/kaiak-control/src/storage/types.ts:212:  saveGateway(record: GatewayRecord, expectedRevision: number | undefined): Promise<SaveGatewayResult>;
control/kaiak-control/src/storage/types.ts:213:  // Forgets each gateway whose stored record is still at the given revision; returns the
control/kaiak-control/src/store-contract/index.ts:287:          assert.deepEqual(await a.forgetGateways([{ instance: "gw-1", revision: saved.revision }]), ["gw-
control/kaiak-control/src/store-contract/index.ts:308:          const again = await a.saveGateway(gatewayRecord("gw-1", true, 10), joined.revision);
control/kaiak-control/src/store-contract/index.ts:312:          const left = await a.saveGateway(gatewayRecord("gw-1", false, 10), again.revision);
control/kaiak-control/src/store-contract/index.ts:428:        "a record is written only against the expected revision",
control/kaiak-control/src/store-contract/index.ts:432:          assert.equal(first.revision, 1);
control/kaiak-control/src/store-contract/index.ts:433:          // Another process that judged against "no record" finds revision 1.
control/kaiak-control/src/store-contract/index.ts:436:          assert.equal(stale.saved === false && stale.current?.revision, 1);
control/kaiak-control/src/store-contract/index.ts:439:          assert.equal(second.revision, 2);
control/kaiak-control/src/store-contract/index.ts:441:          assert.equal(stored?.revision, 2);
control/kaiak-control/src/store-contract/index.ts:451:        "two processes writing one record against the same revision: one is written",
control/kaiak-control/src/store-contract/index.ts:459:          assert.equal((await b.gateway("gw-1"))?.revision, 2);
control/kaiak-control/src/store-contract/index.ts:464:        "a gateway is forgotten only at the revision judged; forgetting a live one moves the sequence",
control/kaiak-control/src/store-contract/index.ts:470:          // A status taken meanwhile by the other process moves gw-1's revision on.
control/kaiak-control/src/store-contract/index.ts:471:          assert.ok((await b.saveGateway(gatewayRecord("gw-1", true, 9), live.revision)).saved);
control/kaiak-control/src/store-contract/index.ts:474:              { instance: "gw-1", revision: live.revision },
control/kaiak-control/src/store-contract/index.ts:475:              { instance: "gw-2", revision: idle.revision },
control/kaiak-control/src/store-contract/index.ts:476:              { instance: "gw-unknown", revision: 1 },
control/kaiak-control/src/store-contract/index.ts:481:          assert.deepEqual(await b.forgetGateways([{ instance: "gw-1", revision: 2 }]), ["gw-1"]);
control/kaiak-control/src/store-contract/index.ts:486:          assert.equal(back.saved && back.revision, 1);
docs/ARCHITECTURE.md:301:    instance's last batch, gateway records on their revision), the consistent totals
docs/specs/CONTROL-PROTOCOL.md:237:  to order totals by a revision, and a store restored to an earlier sequence then
docs/specs/CONTROL-PROTOCOL.md:242:    Order). The gateway applies each one as it comes. Rejected: a `revision` on every
docs/specs/CONTROL-PROTOCOL.md:948:  - a revision per process (`{ control_plane, sequence }`), and later one per store
docs/specs/GATEWAY.md:1515:    revision (settled 2026-10-06) — needed only while acks carried totals too.

$ git grep -n -I -E 'configsAfter|configsSince|configHistorySize|historySize' -- . ':(exclude)docs/plans' ':(exclude)docs/reviews'

$ git grep -n -I -E 'resync|Resync' -- . ':(exclude)docs/plans' ':(exclude)docs/reviews'
docs/specs/CONTROL-PROTOCOL.md:131:  - config versions with a bounded history, a resume position (`since`) and `resync`
docs/specs/GATEWAY.md:2044:    10 s, usage batch 30 s). Rejected: a resume position and `resync` (settled

$ git grep -n -I -E 'since=|"since"|SinceEpoch|sinceInvalid|since-invalid' -- . ':(exclude)docs/plans' ':(exclude)docs/reviews'

$ git grep -n -I -E 'config-unavailable' -- . ':(exclude)docs/plans' ':(exclude)docs/reviews'
docs/specs/CONTROL-PROTOCOL.md:717:  needed for. Rejected: `503 config-unavailable` before the first publish (settled

$ git grep -n -I -E 'GET /v1/config|/v1/config|config-snapshot|ConfigSnapshot' -- . ':(exclude)docs/plans' ':(exclude)docs/reviews'
control/kaiak-control/src/fastify/fastify.test.ts:201:    { name: "GET /v1/config: the stream is the one way to get the config", method: "GET", path: "/v1/confi
docs/specs/CONTROL-PROTOCOL.md:52:  (`GET /v1/config`) — the stream's first event is the current config, and two ways to

$ git grep -n -I -E 'kaiak_control_config_mismatch|ConfigMismatch|mismatchSince|config mismatch|config-mismatch' -- . ':(exclude)docs/plans' ':(exclude)docs/reviews'

$ git grep -n -I -E 'ack\.totals|ack totals|UsageAck.*Totals|"totals": *\{|takeTotals\(ack' -- . ':(exclude)docs/plans' ':(exclude)docs/reviews'

$ git grep -n -I -E 'new epoch on|takes a new epoch|new config epoch|store takes a new' -- . ':(exclude)docs/plans' ':(exclude)docs/reviews'
```

Every remaining hit is an allowed exception:
- the specs' dated "Rejected" lines (`CONTROL-PROTOCOL.md` 52, 131, 237, 242, 717,
  948; `GATEWAY.md` 1515, 2044);
- the gateway-record revision in `kaiak-control` (the store's conditional write on a
  gateway record), and `ARCHITECTURE.md` 301 naming it;
- **flagged:** `control/kaiak-control/src/fastify/fastify.test.ts:201`, step 8's
  test that `GET /v1/config` answers as an unknown route — it asserts the removal
  and does not keep the endpoint; left as is.

**Suite** (2026-10-07): `scripts/check-all.sh` green three times in a row (gateway
checks with uncached race tests including both e2e packages; the live kit's lint and
self-test; control `npm test` 550/550 and lint; both cross-half tests: 69.2 s, 69.1 s,
69.2 s). **Phase 3 ends here.**
