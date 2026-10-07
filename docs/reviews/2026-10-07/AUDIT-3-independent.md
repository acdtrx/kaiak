# Audit C — broadcast config and changes-only totals

Reviewed 2026-10-07. Specs, current source and current tests were treated as authoritative; no earlier fix was assumed correct.

## Verdict

**Changes required before relying on this design for budget enforcement.** The ordinary broadcast/diff path is coherent, and the existing suites pass. The remaining failures concern stale totals during partial store failure, crash recovery, scope lifetime, and fault recovery for deltas. There are also validation, resource-bound and restore-guidance gaps.

Eight executable reproductions fail on the current code. They are left in the tree, plus one passing scale probe. No non-test implementation was changed. The findings below distinguish contract violations from cases where the documented design itself needs adjustment.

## High

### C1 — [High] [occasional] Failed totals reads leave healthy-looking streams and serve stale complete snapshots on reconnect

**Location:** [totals-feed.ts:88](/tmp/kaiak/control/kaiak-control/src/fastify/totals-feed.ts:88), [totals-feed.ts:116](/tmp/kaiak/control/kaiak-control/src/fastify/totals-feed.ts:116), [gateway-stream.ts:92](/tmp/kaiak/control/kaiak-control/src/fastify/gateway-stream.ts:92), [gateway-stream.ts:134](/tmp/kaiak/control/kaiak-control/src/fastify/gateway-stream.ts:134).

A failed snapshot query sets `failing` and schedules another read, but leaves the previous `state` available indefinitely. Existing streams keep sending heartbeats. A joining stream immediately sends that cached state as its complete baseline, even after a read has established that the feed cannot refresh it.

Reproduction: initialize the feed with no spend; make subsequent `totalsSnapshot` calls fail; successfully count usage into the store; connect another stream. Its first totals still declare no spend. This can arise during a query-specific database failure or repeated snapshot-query timeouts while small writes and config reads succeed. Stream contact and successful usage acks continue, so neither gateway outage condition protects the budget. Replicas enforce their own spend against stale remote bases beyond the documented reporting-interval drift. Reconnecting to this same feed does not repair it.

The spec explicitly retries totals reads, but does not provide a failure transition that makes stream contact stop claiming that budget information is available: **this is a design gap as well as a cache-serving issue.**

**Fix sketch:** make totals-read failure a bounded stream-delivery failure. After a bounded retry period, end affected streams and prevent reconnects from receiving the failed feed's cached state as a fresh baseline. Resume complete totals only after a successful read. Preserve the last successful snapshot internally for diff calculation if needed; do not periodically resend it as evidence of freshness.

**Confirmed:** `control/kaiak-control/src/fastify/audit-c.test.ts`, “a failed feed must not give reconnecting gateways a stale complete baseline” fails with `a reconnect was declared complete with zero spend while the totals query keeps failing`.

### C2 — [High] [rare] A crash restores durable usage for delivery but omits it from local budget enforcement

**Location:** [main.go:433](/tmp/kaiak/gateway/cmd/kaiak/main.go:433), [main.go:452](/tmp/kaiak/gateway/cmd/kaiak/main.go:452), [persist.go:130](/tmp/kaiak/gateway/internal/limits/persist.go:130), [usage.go:283](/tmp/kaiak/gateway/internal/control/usage.go:283).

`totals.json` is saved on a totals event and clean shutdown. During an outage, later settled usage can be sealed durably into the spool without updating this cache. On restart, the usage sender restores the batch, but gives the limiter only its generation number. `LoadShared` restores amounts from the old cache; it never reconstructs the newer batch's amounts. It then sets `totalsKnown = true`.

Reproduction: cache complete empty totals, settle and durably seal a batch spending a $1 budget, then recreate the client and limiter without a shutdown cache save. The spool restores one batch, but a further priced request is admitted. This is enforcement loss, not loss of the billing record: eventual delivery still works. With a last-known-good config and the control plane still unavailable, the restarted gateway can spend again during its new outage grace. The forgotten interval is the interval since the last totals, not just an unsealed batch.

The documented cache-write policy is followed, but the promise that restart keeps the spend is stronger than what that policy and restore API provide.

**Fix sketch:** restore base/cursor information and spooled amounts together, so the limiter can reconstruct uncounted usage without adding amounts already inside the cached base. If the cache cannot establish that relationship, treat money totals as unknown until a fresh stream snapshot arrives. Merely adding all spool records to the current cache would double-count overlapping usage.

**Confirmed:** `gateway/internal/control/audit_c_test.go::TestAuditCCrashRestoresSpooledSpendIntoLimits` fails with `restart admitted a priced request although its restored spool already spends the full budget`.

## Medium

### C3 — [Medium] [occasional] Deleting and recreating a group discards its own current-window spend

**Location:** [limits.go:231](/tmp/kaiak/gateway/internal/limits/limits.go:231), [limits.go:259](/tmp/kaiak/gateway/internal/limits/limits.go:259), [limits.go:265](/tmp/kaiak/gateway/internal/limits/limits.go:265).

`sync` rebuilds the counter map exclusively from groups in the new config. Deleting a group drops its hour/month counters, including settled own usage. Recreating the ID creates new empty counters. In shared mode a previously pushed base can be recovered from `pushed`, but locally settled usage not yet represented there cannot. In file mode there is no later authoritative push to repair it. Outstanding reservations also retain pointers to the discarded counters, rather than the recreated ones.

The contract explicitly says that a recreated ID resumes its window's spend and that a fresh budget requires a new ID. A delete/create is also the prescribed group-move operation. The reproduction spends $1, applies a deletion, recreates the same ID with a $1 limit, and gets zero spend and an admitted request in both modes.

**Fix sketch:** retain hour/month counts independently of the current config until their windows expire; config membership controls which limits and paths are active. Reuse retained counters on recreation, including counters still held by in-flight requests. Keep per-minute removal behavior separate.

**Confirmed:** `gateway/internal/limits/audit_c_test.go::TestAuditCRecreatedGroupKeepsOwnSpend`, both `shared=false` and `shared=true`, fails with `spent budget = 0, want 1000000000`.

### C4 — [Medium] [rare] Publishing checks the parents rule against a mutable object different from the stored text

**Location:** [config-publishing/index.ts:154](/tmp/kaiak/control/kaiak-control/src/config-publishing/index.ts:154), [config-publishing/index.ts:159](/tmp/kaiak/control/kaiak-control/src/config-publishing/index.ts:159), [config-publishing/index.ts:168](/tmp/kaiak/control/kaiak-control/src/config-publishing/index.ts:168).

The core validates the caller's object and captures its JSON text synchronously. After awaiting `currentConfig`, it checks parents against `result.config`, which is still the caller's object. A host edit during that await can make the check describe different content from the text written. The returned `published.config` can likewise disagree with `published.text` and its hash.

Reproduction: current group `me` is under `a`; call publish with it under `b`; hold the store read and let the host revert its object to `a`; release the read. The parents check passes, but the captured text moving the group to `b` is stored. No store race or broken conditional write is needed. This requires a host to reuse/mutate an object while publishing, rather than handing off an immutable copy.

**Fix sketch:** take one private immutable snapshot before the first await and use it for validation, serialization, every parents check, and the returned document. Parsing the captured text once is sufficient to align the later checks with what will actually be stored.

**Confirmed:** `control/kaiak-control/src/config-publishing/audit-c.test.ts`, “parents validation must check the captured JSON that is written”, expects refusal but receives `ok: true`.

### C5 — [Medium] [rare] Skipping a malformed totals event permanently breaks the delta baseline

**Location:** [stream.go:116](/tmp/kaiak/gateway/internal/control/stream.go:116); documented skip policy at [GATEWAY.md:2050](/tmp/kaiak/docs/specs/GATEWAY.md:2050).

A totals decode error logs and continues on the same stream. A later valid event is only a delta against the sender's previous snapshot, including the event the gateway skipped. Windows appearing only in that skipped event can remain stale indefinitely. A later `counted_through` can additionally retire own usage whose replacement base was in the rejected event. If the first complete event was rejected, the first later delta is instead treated as complete and clears omitted bases.

The reproduction injects one malformed event between valid totals and shows that the subsequent delta is accepted on the old connection. **This is fault recovery, not a claim that the current core normally emits malformed totals, or that TCP silently drops events.** A bad peer/integration or protocol regression is needed. The spec's explicit malformed-event policy is incompatible with its changes-only merge policy.

**Fix sketch:** end and reopen the stream on any rejected totals event, obtaining a new complete baseline before accepting more deltas. Logging and continuing is still appropriate for independent, unknown events; totals are not independent.

**Confirmed:** `gateway/internal/control/audit_c_test.go::TestAuditCMalformedTotalsCannotContinueWithDeltas` fails with `accepted a changes-only totals event after losing its preceding delta`.

### C6 — [Medium] [rare] The 50,000 effective-limit safeguard no longer bounds allocated counter memory

**Location:** [semantic.ts:91](/tmp/kaiak/control/kaiak-control/src/config/semantic.ts:91), [semantic.go:212](/tmp/kaiak/gateway/internal/config/semantic.go:212), [limits.go:245](/tmp/kaiak/gateway/internal/limits/limits.go:245), [CONTROL-PROTOCOL.md:528](/tmp/kaiak/docs/specs/CONTROL-PROTOCOL.md:528).

Both validators count only configured effective limits. The limiter now allocates two substantial window counters per scope even when it has no limits. There is no group-count bound. Thus arbitrarily many unlimited groups pass the resource check while producing two counters each; configured minute limits add still more. The documented rationale that this check limits counter memory to roughly 70 MB no longer holds. This is not an immediate capacity failure at 7,000 groups, but the safeguard has ceased to protect imports or accidental oversized publications from fleet-wide memory amplification.

The reproduction publishes no new limits and constructs 25,001 groups: validation accepts 50,004 mandatory counters. Its expected rejection represents the restored **resource safeguard**, not the literal current rule counting only configured limits.

**Fix sketch:** bound the actual resolved counter allocation: two fixed-window counters per scope plus its effective per-minute counters, or a separately justified group/counter byte bound. Change the two validators, fixtures and stated contract together. A compact representation for fixed windows can reduce the cost but does not itself provide a bound.

**Confirmed:** `control/kaiak-control/src/config-publishing/audit-c.test.ts`, “the counter allocation bound includes unlimited scopes”, fails with `50004 mandatory counters bypass the 50000-counter resource bound`.

### C7 — [Medium] [rare] The GUIDE's restore advice can invalidate the non-repeating gateway revision guarantee

**Location:** [GUIDE.md:221](/tmp/kaiak/control/kaiak-control/GUIDE.md:221), [GUIDE.md:293](/tmp/kaiak/control/kaiak-control/GUIDE.md:293), [types.ts:137](/tmp/kaiak/control/kaiak-control/src/storage/types.ts:137), [gateways/index.ts:205](/tmp/kaiak/control/kaiak-control/src/gateways/index.ts:205).

The store contract correctly requires gateway revisions never to repeat. The Postgres sketch obtains them from `gateway_revision`, then says a restored store needs no action. A restored allocator can reuse revisions retained by outstanding operations in surviving cores. An expiry or forget computed before the restore can then match a fresh post-restore record and overwrite/delete it. The live count becomes too small, inflating per-minute shares and backend concurrency shares until status repairs it.

The reproduction holds an expiry write after it read revision 1, swaps to restored state with a fresh live status also at revision 1, and releases the old write. The fresh gateway becomes non-live. **The wrapper deliberately models a restored allocator violating the non-repeat promise; this is a GUIDE/restore-contract gap, not a failure of the core with a compliant store.** Existing negative controls cover forgotten/recreated records but not restoring an allocator while old operations survive.

**Fix sketch:** qualify restore operations so all pre-restore operations are fenced before restored revisions can be used, or use write tokens that cannot repeat across restored store incarnations. Document and test that guarantee, including a paused expiry/forget crossing restore. This does not require restoring a global config/totals sequence or rollback detection in the core.

**Confirmed:** `control/kaiak-control/src/storage/audit-c.test.ts`, “a pre-restore sweep must not expire a fresh post-restore status”, expects `live: true` but gets `false`.

## Low

### C8 — [Low] [occasional] Expired pushed windows accumulate for the lifetime of a stream

**Location:** [shared.go:79](/tmp/kaiak/gateway/internal/limits/shared.go:79), [shared.go:83](/tmp/kaiak/gateway/internal/limits/shared.go:83), [totals-feed.ts:151](/tmp/kaiak/control/kaiak-control/src/fastify/totals-feed.ts:151).

The gateway's `pushed` map deletes entries only when a complete snapshot replaces the map. Later deltas insert/overwrite by scope/type. The feed correctly omits windows that disappeared because their hour/month ended; therefore those omitted entries remain forever on a healthy long-lived connection, including scopes deleted months ago. Rolling a counter ignores an obsolete base but never prunes this map. With group-ID churn, memory grows with historical IDs rather than current scopes/windows. Reconnect happens to reclaim it; healthy streams must not depend on reconnects for collection.

**Fix sketch:** prune pushed entries once their windows are obsolete under the gateway's window rules, retaining current-window deleted scopes for recreation. Do not discard a still-current scope merely because the config removed it.

**Confirmed:** `gateway/internal/limits/audit_c_test.go::TestAuditCExpiredPushedScopesAreReclaimed` advances two months and applies an empty delta; the expired deleted group's entry remains.

### C9 — [Low] [daily] A few descriptions still state the removed per-limit or single-cursor model

**Location:** [CONTROL-PROTOCOL.md:43](/tmp/kaiak/docs/specs/CONTROL-PROTOCOL.md:43), [CONTROL-PROTOCOL.md:722](/tmp/kaiak/docs/specs/CONTROL-PROTOCOL.md:722), [spool.go:25](/tmp/kaiak/gateway/internal/control/spool.go:25).

The endpoint summary still calls totals “per limit”; the batch introduction describes a last batch per instance and constant state per gateway; the spool invariant says the control plane never goes back to an older epoch. The actual contract is all scopes/types with usage and one retained cursor per instance/epoch, and the core deliberately accepts a late earlier-epoch write. These are stale descriptions, not retained compatibility branches.

**Fix sketch:** align these summaries/comments with the settled model; distinguish the gateway sender's epoch ordering from what the control plane accepts. No executable reproduction applies to this editorial finding.

## What was checked and found sound

- **Config authority and conditional publication:** the stored JSON text is sent intact; hashes are content identifiers. Hash compare-and-set retries recheck parents against the winning config, subject to C4's caller-mutation issue. An ABA back to identical text does not invalidate that check. No config/store sequence or rollback-detection path was found in the active core.
- **Per-stream read ordering:** subscription precedes connect reads; read numbers are assigned before awaiting the store; each stream advances its read fence even when it skips an identical hash. The held-delivery/connect regression passes. Local notification duplication and catch-up rereads do not force duplicate config events.
- **Restore and rejection recovery:** ordinary restored current config is delivered; republishing the running hash clears gateway rejection and the `kaiak_control_config_rejected` gauge follows rejection state. Config rejection does not gate usage totals.
- **Healthy totals merge:** one feed snapshot serves all streams; reads are serialized. A change arriving during a read requests a trailing read. First events use the full latest feed state; later events use each stream's accumulated changes. Slow readers retain the latest value per changed identity, rather than replacing their pending delta with only the latest feed diff. Same-window disappearance generates `"0"`; ordinary ended windows are omitted. Reads spanning an hour/month boundary are retried with the new windows before cursors are sent.
- **Batch counting and retirement:** store writes atomically condition on the batch's own epoch cursor. Window/cursor snapshots are consistent. In the normal ordered sender history, ack handoff cannot hide a batch from totals: queue removal and acknowledged-list insertion share a lock. Stream totals can cover a still-outstanding batch, and a repeated retirement does not subtract twice. An ack contains only `batch`, neither retires own usage nor updates stream contact. Retained epochs are an array, not an aliased single object.
- **Counting and reloads:** global and every group on an existing request path receive hour/month usage regardless of limits. Adding/removing a limit while its scope persists reuses the count and reservation; priced/free status comes from the request's own config. C3 concerns deletion of the scope itself, not removal of a limit.
- **Shutdown and streams:** close is synchronous/idempotent before response end; the delayed-delivery write-after-end regression passes. Disconnect, stall, read failure and app shutdown release stream subscriptions and timers. The socket has an error handler. The totals feed stops notifying after close. No new crash was observed in the race suites.
- **Store tests and Postgres sketch:** the lossy-channel suite exercises catch-up to all subscribers; broken snapshot/no-catch-up/one-subscriber controls fail as intended. Normal CAS races and forgotten/recreated gateway revisions are tested. Text storage, additive window updates, transactionally committed notifications, primary repeatable-read totals, dedicated LISTEN connection and listen-before-catch-up are coherent. No real Postgres implementation is present to integration-test; C7 is the restore qualification missing from this sketch.
- **Removal:** schemas, types and runtime paths no longer use config-filtered totals, a single `counted_through`, ack totals, ack-as-contact, config history/resume, or store rollback detection. Gateway-record revisions and batch sequences remain intentionally; they are not aliases for the removed global store sequence. C9 lists remaining prose drift.

## Scale observations

Measured locally; these are component probes, not fleet throughput guarantees.

- **7,000 extra groups:** 14,012 fixed-window counters in the test fixture; initial limiter sync approximately **9.9 ms**; **10,000 reserve/settle pairs in 8.9 ms** with one group on the request path and no configured limits. The steady-state request path visits its own scopes, not all 7,000 groups. Per-minute limits, deep paths, contention and JSON decoding were not part of this timing.
- **7,000 groups × two windows × 30 streams**, through the actual totals feed and stream serializer with in-memory writable replies: first snapshot **1,386,075 bytes per gateway**. One changed group: **273 bytes per gateway**, approximately **5.3 ms** for the feed/diff/fanout turn. A subsequent near-full change: about **1.386 MB per gateway**, approximately **66 ms**. About **42 MB of payload per second** remains possible if essentially all windows change every second. Deltas improve sparse changes, not that worst case. These snapshots are below the gateway's 16 MiB event cap; larger historical scope populations can grow beyond the nominal current group count.
- Fixed-window counters still embed the three 60-element minute arrays, so mandatory counting allocates meaningful memory even without configured limits (C6). Per-stream pending changes are coalesced by identity and stalled streams close; the shared feed retains one current snapshot. C8 concerns the separate gateway-side retained map.
- Acknowledged batches age out on subsequent acks at the last relevant hour/month boundary. This is a time bound, not a byte bound: high-rate priced traffic without USD limits during a prolonged stream outage can retain many generations, and acknowledged-list pruning plus `addLocal` perform linear scans. I did not establish a failure threshold at the requested workload; no separate severity finding is claimed for that observation.

## Verification and reproductions

Baseline, before adding audit tests:

```text
npm ci --ignore-scripts
added 69 packages

npm test
ℹ tests 605
ℹ pass 604
ℹ fail 0
ℹ skipped 1

GOCACHE=/tmp/audit-c-go-cache scripts/check-gateway.sh
[gofmt, vet, staticcheck, uncached race tests and live-test self-test]
gateway checks passed
```

The one skipped control test is the in-memory store's inapplicable reconnect test; the lossy-channel reconnect test runs. Initial sandbox attempts could not access the default Go cache or open local sockets; the successful runs used a writable cache and approved execution with local sockets/tool downloads.

After adding the Go reproductions, `GOCACHE=/tmp/audit-c-go-cache scripts/check-all.sh` passed formatting, vet and staticcheck and ran the gateway race suite. It failed only in the two packages containing the intentional audit failures:

```text
--- FAIL: TestAuditCCrashRestoresSpooledSpendIntoLimits
--- FAIL: TestAuditCMalformedTotalsCannotContinueWithDeltas
FAIL kaiak/internal/control
--- FAIL: TestAuditCRecreatedGroupKeepsOwnSpend
--- FAIL: TestAuditCExpiredPushedScopesAreReclaimed
FAIL kaiak/internal/limits
```

The command stops at that stage by design, so control checks and cross-half checks were run separately. Cross-half command:

```text
GOCACHE=/tmp/audit-c-go-cache go -C gateway test -race -tags crosshalf -run '^TestAcrossHalves' -count=1 ./e2e
ok kaiak/e2e 84.232s

cd control && npm run lint
boundaries ok
```

The final control suite includes the intentionally failing tests for C1, C4, C6 and C7:

```text
npm test
ℹ tests 609
ℹ pass 604
ℹ fail 4
ℹ cancelled 0
ℹ skipped 1
```

 Every reproduction was also run directly and failed at the assertion described in its finding, not at setup. The Go failures ran under the race detector in the full command. `TestAuditCScaleSevenThousandGroups` passes.

Added test files:

- `gateway/internal/control/audit_c_test.go`
- `gateway/internal/limits/audit_c_test.go`
- `control/kaiak-control/src/fastify/audit-c.test.ts`
- `control/kaiak-control/src/config-publishing/audit-c.test.ts`
- `control/kaiak-control/src/storage/audit-c.test.ts`
