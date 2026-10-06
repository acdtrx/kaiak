# Audit B

## Verdict

**Changes required before relying on this design for durable, multi-process billing and budget enforcement.** The ordinary same-epoch races are handled well, but the review found double counting across a spool restart, loss of local budget enforcement after a rejected store epoch, and config delivery that can silently stop after one failed database read. There are also window-boundary, gateway-forgetting, and store-adapter verification gaps.

Eight findings follow: **3 High, 4 Medium, 1 Low**. Frequencies describe the triggering conditions in legitimate operation, not deliberate abuse. Nine failing reproduction tests were added and run. Production code was not changed. This was a source-copy review; no git history was available.

## High

### B1 — An outstanding old-epoch request can count an already acknowledged batch again

**[High] [occasional]** — gateway restarts during slow or timed-out usage delivery.

**Locations:** `control/kaiak-control/src/usage/index.ts:180`, `:245`; `control/kaiak-control/src/storage/memory.ts:93`.

**Mechanism:** The conditional write correctly detects a changed cursor, but the retry treats *any* different epoch as a fresh spool. A legitimate sequence is:

1. Core A starts counting epoch E1, batch 1, and waits in its store operation.
2. The gateway times out and resends through B. B counts E1/1 and acknowledges it.
3. The gateway restarts under the same instance name with a fresh in-memory spool. B counts E2/1.
4. A resumes. Its original conditional write loses, reads E2/1, classifies E1/1 as `new-epoch`, and successfully counts E1/1 again.

There are never two live gateways using the instance name. The outstanding server-side operation survives its client's timeout/restart. This occurs inside the seven-day retention period. It adds the old records and amounts twice, and moves the cursor back to E1, making further duplicates possible.

**Contract consequence:** The single latest cursor cannot deliver the promised exactly-once behavior across this normal restart race. Retaining an instance's cursor for seven days does not retain its previous spool's identity for seven days.

**Fix sketch:** Keep deduplication cursors per `(instance, spool epoch)` for the retention period, independently of the recipient's latest `counted_through`, or introduce an equivalent durable fence for superseded epochs. Update the store contract and its cross-handle tests; a process-local remembered-epoch set would not solve this.

**Confirmed:** `control/kaiak-control/src/control-plane/audit-b.test.ts:31` pauses A at the conditional write, drives the retry and restart through B, then releases A. **Three records are counted for two requests** (`3 !== 2`).

### B2 — Rejecting a restarted store's config disables budget protection without triggering mismatch handling

**[High] [rare]** — store replacement/recovery followed by a gateway-specific config rejection.

**Locations:** `gateway/internal/control/client.go:414`, `:419`, `:429`; `gateway/internal/limits/shared.go:157`, `:193`.

**Mechanism:** The client computes `Counted` before checking the totals' config epoch. It discards foreign-epoch totals but still sends their counted generation to the limiter. With no waiting totals, the limiter retires local usage against the *old* bases. It also never hears the new epoch, so it never starts the config-mismatch clock.

For example, a gateway runs store A's config and totals. The store is replaced by B; B's config requires a provider credential missing on this gateway, so the gateway retains A's working config. Usage POSTs to B succeed and its SSE stream supplies heartbeats. Every ack removes more local spend, but B's totals never apply. There is neither a connectivity outage nor a mismatch refusal after the grace period. Priced requests can keep spending under stale budgets indefinitely.

**Contract consequence:** The new epoch-filtering rule conflicts with `GATEWAY.md:1503`, which requires retaining own usage until matching bases apply and refusing priced money-limited requests after prolonged mismatch. “Counted somewhere” does not imply “included in the bases this gateway is enforcing.” The existing `TestTotalsFollowTheRunningConfigEpoch` checks callback values, not their effect on enforcement.

**Fix sketch:** Separate spool acknowledgement from retirement of locally enforced usage. Preserve foreign-config mismatch information without applying its windows or sequence. Retire a generation only when the applied bases cover it; start the mismatch grace when the authoritative current config is rejected, including across store epochs. Continue ignoring genuinely delayed messages from superseded stores.

**Confirmed:** `gateway/internal/control/audit_b_test.go:14`, `TestAuditBRejectedNewStoreEpochKeepsUsageAndFailsClosed`: **500,000,000 nano-USD becomes 0**, `ConfigMismatch()` stays false, and a priced request is admitted after grace instead of returning `budget_unavailable`.

### B3 — One transient database read failure silently loses a published config to connected gateways

**[High] [occasional]** — temporary database/read-connection failure during notification delivery.

**Locations:** `control/kaiak-control/src/config-versions/index.ts:92`, `:99`, `:107`, `:128`; `control/kaiak-control/src/fastify/gateway-stream.ts:165`.

**Mechanism:** Receiving a config notification schedules `configsAfter()`. If that read rejects, both delivery catches swallow the error. There is no retry of that event, no host error signal, and no resync/termination of affected streams. Only another publish or a gateway reconnect recovers the config. Heartbeats keep healthy streams open, potentially indefinitely.

A successful publish disabling a key, changing model access, or correcting routing can therefore remain unapplied on one replica's gateways while other replicas apply it. This does not require a lost store notification: the notification arrived, and the core discarded its work after a read fault. Later batches/statuses do not retry config delivery.

**Fix sketch:** Preserve and retry the failed delivery with bounded backoff, or surface a delivery-failure event that closes/resyncs affected streams through the existing snapshot/reconnect path. Report the database failure to the host. Do not depend on an unrelated future publish to recover a committed policy change.

**Confirmed:** `control/kaiak-control/src/control-plane/audit-b.test.ts:89` uses an adapter whose first `configsAfter()` fails and whose later reads work. The writer reports success; the other core delivers nothing and reports no error. The assertion requiring a surfaced failure fails. This is a deterministic notification/read fault, not a notification-channel-loss simulation.

## Medium

### B4 — A read crossing a window boundary can acknowledge usage that its windows omit

**[Medium] [daily]** — normal hour rollovers under concurrent traffic and database latency; the same mechanism applies at month boundaries.

**Locations:** `control/kaiak-control/src/usage/index.ts:128`; `gateway/internal/control/client.go:422`.

**Mechanism:** The core chooses hour/month bounds *before* awaiting the store snapshot. A read started just before 13:00 can wait while another core counts a 13:00 batch, then obtain the new sequence and cursor while requesting only the 12:00 window. Its message acknowledges the new batch but omits its current-hour amount.

The accurate ack from the counting core carries the **same sequence** and the 13:00 window. If the incomplete message arrives first, the gateway drops the batch's local usage, then rejects the accurate ack's windows because its revision is not higher. The amount remains absent until a subsequent revision supplies it. A transactionally consistent database snapshot alone does not fix the window bounds selected outside that snapshot. Clock skew between cores is another way to expose this boundary.

**Fix sketch:** Make the selected windows and snapshot ordering consistent. At minimum, retry a read whose window bounds changed while it awaited the snapshot; also define cross-core clock/window ordering so one sequence cannot describe incompatible current bases. Preserve locally counted generations until the corresponding window's base covers them. Simply accepting arbitrary equal-revision totals would permit regression in the opposite arrival order.

**Confirmed on both halves:**

- `control/kaiak-control/src/control-plane/audit-b.test.ts:117`: two messages have identical revision and `counted_through`, but one has `windows: []` and the other has the newly counted hour window.
- `gateway/internal/control/audit_b_test.go:73`, `TestAuditBBoundaryTotalsWithSameRevisionLoseUsage`: the first message followed by the accurate ack leaves **0 tokens instead of 500** enforced.

### B5 — Resetting a gateway revision after forgetting lets a stale sweep delete a newly live gateway

**[Medium] [rare]** — overlapping sweeps and a reconnect at the forget boundary, especially with a delayed database operation.

**Locations:** `control/kaiak-control/src/storage/types.ts:125`; `control/kaiak-control/src/storage/memory.ts:145`, `:158`; `control/kaiak-control/src/gateways/index.ts:183`, `:194`.

**Mechanism:** The revision is a compare-and-set token, but the contract requires it to restart at 1 after forgetting. A sweep reads an expired revision-1 record and waits before forgetting it. Another core forgets it; the gateway reconnects and is stored at revision 1 again. The old sweep's condition now matches the new incarnation and deletes the live gateway. A stale status/expiry write can similarly match a recreated record at a reused revision.

This is an ABA race: equality of the number no longer means equality of the record that was judged. The live count becomes too small and remaining gateways receive excessive per-minute/backend shares until another status restores membership.

**Fix sketch:** Include a non-reused incarnation token in every gateway comparison, or use a revision that never repeats across deletion/recreation. Change the contract's explicit reset requirement and the exported test that currently requires it. No sweep leader is needed.

**Confirmed:** `control/kaiak-control/src/control-plane/audit-b.test.ts:62`: a gateway has just rejoined and the count is 1; releasing the stale sweep changes it to **0**.

### B6 — The exported contract suite certifies an inconsistent `counted_through` implementation

**[Medium] [occasional]** — implementing or changing a database adapter; affected adapters can then miscount enforcement during routine concurrent traffic.

**Locations:** `control/kaiak-control/src/store-contract/index.ts:352`, `:362`, `:382`; contract being checked: `control/kaiak-control/src/storage/types.ts:82`.

**Mechanism:** The concurrent snapshot test collects `last` but never checks it for its concurrent reads. It checks only the relationship between aggregate windows and sequence. The cursor is checked once after writes are quiescent. A store that reads windows/sequence atomically and fetches the cursor in a separate statement passes, even though it can tell a gateway to retire a batch absent from those windows.

**Fix sketch:** Add concurrent assertions tying the recipient cursor to that recipient's contributions and the same snapshot revision. Include config and live-count changes in snapshot consistency coverage. Exercise a separate attached handle, and retain a deliberately inconsistent adapter as a negative control for the suite.

**Confirmed:** `control/kaiak-control/src/storage/audit-b.test.ts:24` runs the complete exported suite against an intentionally broken adapter: **all 23 contract tests pass**. The added deterministic test at `:26` then gets sequence 0 and empty windows with `last.sequence === 1`, and fails. The production memory store is not claimed to have this torn-read bug; this is a demonstrated hole in adapter certification.

### B7 — The GUIDE's ledger insert permanently blocks a valid resend after cursor retention

**[Medium] [rare]** — a gateway returns after a partition longer than the configured cursor retention, with an unacknowledged durable batch.

**Locations:** `control/kaiak-control/GUIDE.md:193`, `:219`, `:237`; `control/kaiak-control/src/storage/memory.ts:169`; retention contract: `docs/specs/CONTROL-PROTOCOL.md:913`.

**Mechanism:** The protocol explicitly permits recounting a resend once its cursor has been forgotten. The GUIDE instead promises that a resent batch is never passed again, and sketches a permanent ledger keyed by `record_id` with a plain insert. After retention, the core treats the old batch as first and calls `saveCountedBatch()` with the old records. Their ledger primary keys already exist, so the transaction aborts. Every retry fails the same way and blocks later batches behind the outstanding one. Removing uniqueness instead would make the claimed exactly-once billing ledger double-charge.

**Fix sketch:** State the retention boundary of batch deduplication explicitly. Make durable ledger insertion idempotent by record identity, and distinguish that ledger's exactly-once guarantee from enforcement windows' documented recount policy after retention. Show the conflict handling in the SQL sketch and test this with a persistent adapter. Also correct the suggestion that calling `saveCountedBatch` itself means the attempt is necessarily new: concurrent losers enter that method too.

**Confirmed with a constraint simulation:** `control/kaiak-control/src/control-plane/audit-b.test.ts:152` models the illustrated unique ledger and transaction rollback, counts a batch, advances past seven days, sweeps, and resends. It fails with **`23505: duplicate key violates usage_record_pkey`**. No live Postgres server was used; the reproduced contradiction is between the core's retention behavior and the illustrated unique insert.

## Low

### B8 — Delayed notifications skip pruned configs on an open stream without the promised resync

**[Low] [rare]** — a notification delay exceeding the retained publish history, or a deliberately small supported history size.

**Locations:** `control/kaiak-control/src/config-versions/index.ts:85`, `:97`; `control/kaiak-control/src/fastify/gateway-stream.ts:81`; stream contract: `docs/specs/CONTROL-PROTOCOL.md:154`.

**Mechanism:** Delivery silently advances `delivered` past a notified version that has already been pruned. Its comment says affected streams resync, but no resync signal is emitted. The stream writer accepts any version above `lastSent`, so it sends a later version across the hole. With full config snapshots, the newest usable config can still converge, which limits the severity, but the explicit every-version/no-gap contract is violated and an intermediate usable config can be missed.

**Fix sketch:** Detect missing versions during delivery and signal affected open streams to resync and close, or retain the documents until notification consumers can retrieve them. Enforce contiguous delivery at the stream boundary rather than assuming every upstream callback is contiguous.

**Confirmed:** `control/kaiak-control/src/fastify/audit-b.test.ts:15` delays ordered, lossless notifications while history size 1 prunes version 2. A stream already at version 1 receives **version 3 directly**, without version 2 or a `resync` event.

## What was checked and found sound

These conclusions are bounded by the findings above and by testing the reference memory store, not a production database adapter.

- **Same-spool exactly-once counting:** conditional cursor writes include cursor, additions, recent records, and sequence together. Losing cores re-evaluate the returned cursor. Ordinary simultaneous resends in the same epoch count once; lower sequences are duplicates. Config publishes do not condition on usage and usage does not condition on config.
- **Config publication:** competing publishes are assigned distinct consecutive versions. The loser revalidates the parent-change rule against the winner. Invalid configs do not replace the current version.
- **Scope/type limits:** both halves use group/global plus type as identity. Own child limits replace defaults by type; other defaults remain. Counting uses every group in the recorded path plus global, independently of current configuration. Removed/deleted scopes keep their stored amounts, and adding a control-plane limit mid-window exposes previously counted usage. Cache-write tokens count; cache-read tokens do not. Money sums use `bigint` and are converted to decimal strings at the wire boundary.
- **Local enforcement:** ordinary per-minute shares, zero limits, reservation/settlement, changes to limit values, money-limit outage grace, unpriced-model exemptions, and same-epoch rejected-config handling have passing coverage. File-mode new limits intentionally start empty, unlike a newly listed control-plane limit; this agrees with `GATEWAY.md:1459`.
- **Persistence formats:** `limits.json` and `totals.json` are version 3, identified by scope/type. File snapshots preserve settled hour/month usage; totals caches match both config epoch and version and preserve uncounted usage. Minute windows are intentionally not persisted.
- **Retention/pruning:** forgetting gateway status leaves batch cursors intact; cursors age independently by counted time. Current and previous hour/month windows are retained and late records use the specified window rules. A resend after cursor retention being counted again is documented behavior, not itself a new finding; B7 is the incompatible ledger advice.
- **Normal totals ordering:** the memory store snapshot is atomic; a single sequence orders committed publishes, batches, and live-set changes. Duplicate/older messages within an epoch do not replace newer totals. The gateway does not replace current bases with a delayed foreign-store message. B2 and B4 identify where dropping counted generations makes those ordering checks insufficient.
- **Live-set races without record recreation:** a status changes the revision and defeats an expiry based on the earlier record. Concurrent sweeps expire a record once. Draining gateways stay live; normal restarts do not falsely trigger the two-start-time alternation rule. B5 is the missing deletion/recreation case.
- **Notification/stream ordinary paths:** subscriptions precede replay; config delivery is ordered within a core; totals are reread and coalesced, slow readers are bounded, and an ordinary publish reaches both cores. Arbitrarily lost or reordered store notifications are outside the store contract, and the core has no polling fallback. B3 and B8 happen even when notifications satisfy the contract.
- **Sample and cross-half behavior:** the sample's extra protocol ports create separate cores over the same memory store. The cross-half tests exercised two gateways on two cores, shared live counts/totals, config propagation, and failover to the other core without resync. They passed.
- **GUIDE's main transaction design:** conditional writes, a persistent epoch, the sequence in the mutation transaction, consistent totals reads, and transactionally emitted notifications agree with the intended store contract. B5 requires a stronger gateway comparison token; B7 corrects the ledger promise. No real database implementation was available to validate the SQL sketches operationally.

## Verification and reproduction

The initial sandbox blocked local test listeners and the default Go cache. Tests were rerun with the necessary execution permissions and `GOCACHE=/tmp/kaiak-audit-b-go-cache`.

- `cd control && npm ci --ignore-scripts`: succeeded.
- Unchanged control baseline, `npm test`: **596 passed, 0 failed**.
- `GOCACHE=/tmp/kaiak-audit-b-go-cache scripts/check-all.sh`: gateway formatting, vet, staticcheck, all gateway race tests, and the live-test kit passed. By the time this run reached control tests, the first three audit tests had been added: **596 passed, 3 expected audit failures**. The script correctly stopped there; it was not a green full-suite run.
- Cross-half stage run separately: `go test -race -tags crosshalf -run '^TestAcrossHalves' -count=1 ./e2e`: **`ok kaiak/e2e 79.522s`**.
- Final `cd control && npm run lint`: **`boundaries ok`**, TypeScript checks passed.
- Final `cd control && npm test`: **626 tests: 619 passed, 7 failed, 0 cancelled, 0 skipped**. All seven failures are the new audit reproductions. The 619 passes include the 596 original tests and the 23 exported-contract tests run against the deliberately broken adapter.
- `GOCACHE=/tmp/kaiak-audit-b-go-cache go -C gateway test -race ./internal/control -run TestAuditB -count=1`: **both new Go tests failed as intended**; no race-detector report.

Run just the reproductions:

```sh
cd /tmp/kaiak/control
node --test kaiak-control/src/control-plane/audit-b.test.ts kaiak-control/src/fastify/audit-b.test.ts kaiak-control/src/storage/audit-b.test.ts
cd /tmp/kaiak
GOCACHE=/tmp/kaiak-audit-b-go-cache go -C gateway test -race ./internal/control -run TestAuditB -count=1
```

Representative confirmed failures:

```text
two requests must produce two counted records, not three: 3 !== 2
the old revision must not match the re-created gateway: 0 !== 1
foreign-epoch ack erased own usage: got 0, want 500000000
after mismatch grace: rejection = <nil>, want budget_unavailable
current-hour usage = 0, want 500 after the accurate ack
same revision acknowledges the current-hour batch but omits its entire window
a committed config was lost to this core without any error or recovery signal
23505: duplicate key violates usage_record_pkey
stream silently skipped a config instead of replaying or resyncing
a revision-0 snapshot cannot claim that batch 1 was counted
```

Added files: this report and the four `audit-b.test.ts` / `audit_b_test.go` files cited above. They intentionally leave the test suite red until the findings are addressed.
