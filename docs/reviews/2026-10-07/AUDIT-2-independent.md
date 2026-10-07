# Audit B — broadcast control plane and shared-store replicas

Reviewed 2026-10-07 against the supplied copy and its current contracts. No git history was available. Production code was not changed.

## Verdict

**Do not ship this change yet.** There are **2 High, 5 Medium and 4 Low findings**. A rejected config can erase enforced spend, and ending a stream during config delivery can cause an unhandled response error. Acknowledged usage can also remain counted locally after it is included in the control plane's totals.

The store's per-epoch conditional counting is substantially sound: the accounting failures below concern gateway enforcement/retirement, not an observed duplicate insertion into the reference store. The current green suites miss the interleavings reproduced here.

Added **13 failing reproduction tests**: nine in `control/kaiak-control/src/fastify/audit-b.test.ts`, three in `gateway/internal/control/audit_b_test.go`, and one in `gateway/internal/limits/audit_b_test.go`. All fail at the intended assertions. The JSONB reproduction models a value-preserving key reorder; it does not run PostgreSQL. Frequency tags describe legitimate operating conditions, not how often a forced test fails.

## High

### B1 — Removing a limit in a rejected config erases its enforced spend

**[High] [occasional]**

**Locations:** `control/kaiak-control/src/usage/index.ts:150` and `:237`; `gateway/internal/limits/limits.go:294`; `gateway/internal/limits/shared.go:73`–`:81`. Contract conflict: `docs/specs/CONTROL-PROTOCOL.md:274` and `:296`.

The store counts all scope/type windows, but `listedWindows` sends only limits in the control plane's **current** config. A gateway can still enforce an earlier config because it rejected the current one, for example because a newly referenced backend credential is absent on that pod.

If that publish also removes an hour/month limit or its group, subsequent totals omit the window. The gateway sets the still-running counter's base to zero, and `counted_through` retires its own usage. Already counted spend disappears from enforcement. Every subsequent push repeats the omission. This can permit continued spending against an exhausted budget; the records remain in the store.

This is also a conflict within the contract: “current config's limits only” plus “absent means zero” cannot supply correct totals to every config a gateway may still run. Removing the mismatch state did not resolve the missing information.

**Reproductions:** Node `Audit B: totals still cover a running limit when the gateway rejects its removal` sees `undefined` instead of the stored `"100"`; Go `TestAuditBRejectedLimitRemovalMustNotEraseSpend` sees **0 instead of 100** after retirement.

**Fix sketch:** Make totals coverage independent of the published limit list, for example by sending all nonzero current scope/type windows, with an appropriate message-size bound. Alternatively, explicitly identify which scopes the recipient needs and supply those from the same snapshot. Resolve the contract on both sides. Merely retaining the old base cannot account for subsequent usage from other gateways.

### B2 — Ending a stream does not stop delivery; a subsequent write can crash the core's process

**[High] [occasional]**

**Locations:** `control/kaiak-control/src/fastify/gateway-stream.ts:67`, `:155`, `:167`–`:181`; `control/kaiak-control/src/config-publishing/index.ts:106`–`:109`.

The stream's `end()` only calls `raw.end()`. Aborting the connection, removing listeners, and cancelling timers wait for the later `close` event. Meanwhile, config delivery can continue synchronously: `observeSequence()` invokes the rollback listener, which ends the response, and then `notify(current)` invokes the still-subscribed config listener. `sendConfig` writes into that ended response.

On a real Fastify stream this emits **`ERR_STREAM_WRITE_AFTER_END`**. The response has no production `error` listener; the unhandled event can terminate the process, dropping every gateway connection. B5 provides an ordinary concurrent-read trigger, but genuine rollback and other closure/delivery overlaps also require safe teardown.

**Reproduction:** Node `Audit B: rollback closure cannot write another config into the ended response` asserts there were initially no response error listeners, installs one solely to capture the failure without killing the runner, and observes `['ERR_STREAM_WRITE_AFTER_END']` instead of `[]`.

**Fix sketch:** Make logical closure synchronous and idempotent: mark closed/abort, unsubscribe and cancel scheduled work before ending the response. All write paths and in-flight reads must honor that state. Handle transport errors through the same teardown. Fixing B5 alone does not make genuine rollback closure safe.

## Medium

### B3 — One latest cursor cannot retire an acknowledged epoch after a previous process commits late

**[Medium] [rare]**

**Locations:** `control/kaiak-control/src/storage/memory.ts:55`, `:100`, `:110`; `control/kaiak-control/src/usage/index.ts:149`; `gateway/internal/control/usage.go:301`–`:322`.

Legitimate sequence:

1. Gateway process A sends epoch A/1; its control-plane write stalls. The gateway dies.
2. A new process with the same instance ID and a fresh spool sends B/1 through another core. B/1 is counted and acknowledged.
3. The original A/1 write finally commits. Per-epoch conditional writes correctly count it once.
4. Before the new gateway receives coverage for B/1, a totals push reads both batches, but its sole `counted_through` is **A/1**, the latest commit.

The new process knows only B. It cannot map A/1 to a usage generation, so it retains B's own usage alongside a base that already contains B. Further totals still name A/1; even retrying B/1 is a duplicate and does not change the latest cursor. If over-counting refuses further requests, no new B batch can repair this: the false refusal can persist through the window. This needs neither two simultaneously running gateways nor an instance-name misconfiguration.

**Reproductions:** Node `Audit B: a late previous-epoch commit must not hide coverage of the acknowledged epoch` confirms a total of **200**, both records counted once, and coverage of A rather than B even after B's duplicate. Go `TestAuditBAcknowledgedEpochCoveredAfterOldProcessWrite` confirms **generation 0 retired instead of 1**.

**Fix sketch:** Carry snapshot-consistent coverage for every relevant epoch, or explicitly request coverage for the recipient's outstanding epochs. A single “last commit across all epochs” cannot express this state. Update the protocol, store snapshot, gateway mapping and tests together. Do not infer coverage from an ack alone or discard the late old batch: either would defeat the accounting requirements.

### B4 — The ack transition temporarily removes the only mapping needed to retire usage

**[Medium] [rare]**

**Locations:** `gateway/internal/control/usage.go:430`–`:431`, `:345`–`:365`.

An acknowledged batch moves from `queue` to `acked` in two separately locked calls: `dropHead(e)` then `noteAcked(e)`. The stream goroutine can process the covering totals between those calls. It finds the batch in neither collection and returns `Counted == 0`, although it installs totals containing that batch. The limiter therefore counts the batch twice: in its new base and in its own usage.

Appending the ack mapping afterward does not revisit those totals. Heartbeats and ordinary status updates do not require another totals push. An idle or falsely budget-blocked gateway can remain over-counted until a later push/reconnect or the window boundary.

**Reproduction:** `TestAuditBAckTransitionCannotHideBatchFromTotals` forces precisely `dropHead → takeTotals → noteAcked`; the covering push retires **0 instead of generation 1**.

**Fix sketch:** Move the batch atomically from queued to acknowledged under one `usageSender.mu` acquisition, including queue counters and wakeups. Preserve the mapping continuously until a covering totals update consumes it.

### B5 — Normal read/notification interleavings are reported as store rollback

**[Medium] [daily]**

**Locations:** `control/kaiak-control/src/control-plane/index.ts:118`–`:124`, `:158`; `control/kaiak-control/src/config-publishing/index.ts:106`; `control/kaiak-control/src/usage/index.ts:140`.

`highestSequence` mixes three differently timed values: notification sequence, totals snapshot sequence, and the sequence **at which the current config was published**. Every lower value triggers rollback and lowers the watermark.

Two independent ordinary cases fail:

- Config was published at 1, usage advanced the store to 2, then catch-up rereads that unchanged config. Its publication sequence is still 1. That is not a rollback, but every stream is ended.
- A consistent totals read snapshots sequence 1, then a commit notification for 2 arrives before the read resolves. Completing the read reports rollback. The store obeyed both consistent-read and notification-order contracts.

Asynchronous database operations make this a routine source of reconnect churn under traffic. B2 can turn the false rollback into a process failure. There is another legitimate ordering to account for when fixing it: a newer read can complete before an earlier notification is delivered.

**Reproductions:** Node `Audit B: catch-up after usage is not a store rollback` and `Audit B: an ordinary snapshot overtaken by a notification is not rollback` each observe **one rollback where zero is expected**.

**Fix sketch:** Separate historical config publication sequence from the store sequence observed by a current read. Compare only causally ordered observations: for example, independently ordered notification progress and a fresh read begun after the relevant watermark, with confirmation when needed. An old in-flight read must be discarded for delivery, not classified as a restore. Add delayed-read and delayed-notification tests; the current store contract allows both schedules.

### B6 — Config and totals are not ordered together on each stream

**[Medium] [occasional]**

**Locations:** `control/kaiak-control/src/fastify/gateway-stream.ts:55`–`:80`, `:123`–`:133`; `control/kaiak-control/src/usage/index.ts:147`–`:151`.

`lastSent` orders configs against other configs only. `totals()` discards the snapshot's sequence before returning it to the adapter. A totals read may start under config A, config B may be sent while that read is pending, and then A's totals are written after B with no sequence check. The reverse ordering also has no common fence.

The gateway deliberately accepts every totals event, so it installs a stale window list/base under its newly applied config until the trailing push catches up. An added limit can receive an implicit zero in this gap. This directly violates the stream-order contract even with a fully conforming store; serializing totals reads with each other is insufficient.

**Reproduction:** Node `Audit B: a delayed totals result is not sent after a newer config` uses a real HTTP stream. After observing the new config, it receives the previous snapshot's `tokens_per_hour: 100` window. The test delays the core's completed result, keeping this failure independent of B5.

**Fix sketch:** Retain internal snapshot sequence metadata through the core/adapter boundary and use one per-stream ordering rule for configs and totals. Drop stale completed reads and ensure config delivery is coordinated with the config represented by a totals snapshot. Keep sequence off the wire as required.

### B7 — The initial status read bypasses the stale-receipt guard

**[Medium] [occasional]**

**Location:** `control/kaiak-control/src/gateways/index.ts:143`–`:151`.

The code checks `previous.receivedAt > receivedAt` only after a failed conditional write. If an older request's **initial** `gateway()` read is delayed until after a newer status commits, that first read sees the newer revision. The older request then successfully overwrites it; no CAS failure occurs and the guard never runs.

The stored start time, applied hash, state and receipt time go backward. A restart can appear to revert to the dead process, subsequent status can falsely raise a conflict, and a sufficiently old receipt can make the next sweep expire a gateway that just reported.

**Reproduction:** Node `Audit B: a delayed initial gateway read cannot overwrite a newer receipt` stores the old `10:00` start after the newer `12:00` start, using receipt times one second apart.

**Fix sketch:** Check the stored receipt time at the top of every iteration, including the first, before judging status or attempting a write. Keep the receipt timestamp captured once.

## Low

### B8 — The Postgres sketch cannot preserve the documented hash and publication sequence as written

**[Low] [daily]** — when following the GUIDE to implement a Postgres store.

**Locations:** `control/kaiak-control/GUIDE.md:203`, `:210`–`:212`; `control/kaiak-control/src/config-publishing/index.ts:57`; `control/kaiak-control/src/storage/types.ts:30`. Test gap: `control/kaiak-control/src/store-contract/index.ts:57`–`:60`.

The core hashes `JSON.stringify(config)` before storage and later serializes the stored object for the stream. The GUIDE recommends `jsonb`, which does not preserve object member order. The advertised hash therefore need not equal SHA-256 of the JSON actually sent. This is a hash-contract violation, not a demonstrated wrong semantic config in the current gateway, which trusts the supplied hash.

Separately, the sketch stores only the mutable store-wide `sequence`, not the current config's publication sequence required by `CurrentConfig.sequence`. After usage advances the store, that historical value cannot be recovered from the listed columns. Returning the current global sequence changes the interface's meaning.

**Reproduction:** Node `Audit B: a JSON object round-trip through reordered keys must preserve the advertised hash` models an order-changing JSONB read, preserves all JSON values, and gets advertised hash `aca6f7ec…` versus sent-content hash `1cd90d9a…`. No live Postgres claim is made. The exported contract's one-member fake configs cannot detect this reordering.

**Fix sketch:** Preserve the exact serialized config text, or define one canonical encoding used before both hashing and sending. Persist the config's publication sequence separately if retaining that interface. Extend store contract tests with ordered multi-member config content and with a config read after unrelated sequence increments.

### B9 — Returning to the running hash leaves a stale rejection in status

**[Low] [occasional]**

**Location:** `gateway/internal/control/client.go:429`–`:435`.

Run A, reject B because a backend credential is absent, then restore/publish A. `takeConfig` skips A because it is already running and never clears B's rejection. Status keeps saying the latest received config was rejected even though the latest is the successfully running A. It persists across reconnects. This contradicts the status contract and confuses rollout/rollback monitoring.

**Reproduction:** `TestAuditBReturnToRunningHashClearsRejection` uses a schema-valid B referencing a missing backend key; after A returns, `LastRejection()` still reports B and `api-key-env-unset`.

**Fix sketch:** Distinguish the running-hash skip from the rejected-hash skip. Receiving the running hash should clear a stale rejection and trigger status reporting without rebuilding the config.

### B10 — Removed config-version behavior survives in current documentation and a test

**[Low] [daily]** — encountered while reading the current implementation guidance.

**Locations:** `docs/TECH-STACK.md:227`–`:230`; `docs/ARCHITECTURE.md:360`; `gateway/internal/config/loader_test.go:59`, `:62`, `:90`–`:91`; `control/kaiak-control/src/fastify/gateway-stream.ts:65`.

The stack document still mandates GET snapshot + version cursor + resume/re-fetch. The architecture diagram still names `config-versions`. The generic apply-path test explicitly supplies and asserts `kaiak.config.version` values, and the stream comment says reconnection resumes from the running version.

These are surviving documentation/test assumptions, **not an observed runtime compatibility path**. The actual old endpoint is gone and its rejection is tested.

**Evidence:** Direct text inspection at the lines above. No new failing test for prose; adding a wording assertion would not substantiate it further.

**Fix sketch:** Update the transport description and diagram to broadcast config publishing, use hash attributes in the apply-path test, and remove the stale resume comment.

### B11 — The catch-up contract test passes a store that misses subscribers

**[Low] [occasional]** — a test-coverage gap affecting adapters with reconnecting channels.

**Location:** `control/kaiak-control/src/store-contract/index.ts:641`–`:653`.

The catch-up test installs only one listener on the reconnecting handle. A store that announces catch-up to its first subscriber only passes the entire exported suite, even though the interface requires delivery to every subscriber. With several cores sharing that handle, a missed config notification during channel downtime can then leave all but one core's streams stale. The existing all-subscriber tests check ordinary writes, not reconnect.

**Reproduction:** Node `Audit B: exported store contract rejects catch-up delivered to only one subscriber` launches the exported suite against exactly that broken wrapper. The child suite exits **0**; the negative-control assertion expecting rejection fails. Ordinary notifications still reach every subscriber, isolating this omission from the checks that already work.

**Fix sketch:** Register at least two listeners/cores on each relevant handle during reconnect tests and assert catch-up reaches all of them. Exercise writes while the channel is down and repeated reconnects. Make skipping reconnect coverage an explicit assertion that the channel cannot lose notifications, rather than silently treating any omitted hook as that guarantee. The memory store legitimately satisfies that assertion.

## What was checked and found sound

- **Counting inside the reference store:** per-(instance, epoch) CAS prevents retry double-counting across cores, including an old epoch's delayed duplicate after a newer epoch. Adding windows, records, cursor and sequence is synchronous/atomic in the memory implementation. B3 does not undo that guarantee; it exposes insufficient coverage on the wire.
- **Acknowledgement separation:** acks contain only the batch identity and do not directly retire limiter usage. The limiter's monotone generation watermark prevents repeated covering pushes from subtracting the same own usage twice. Late settlement of an already covered generation is suppressed. B4 is the mapping handoff gap, not a failure of that watermark.
- **Restart behavior:** the spool continues its sequence when its index survives; fresh spools use random epochs; batches are durable before sending; old-instance spooled batches retain their own request identity. Shared totals restore by scope/type. The documented aggregate restore deliberately retains all restored own usage until the newest restored generation is covered, so it can conservatively over-count during partial backlog delivery. This is explicit policy, distinct from B3/B4's missed retirement.
- **Retention and windows:** cursors survive forgetting gateway records and expire per epoch after the retention. Resends after retention counting again is documented, not an audit finding. Current/previous-window placement, prior-window pruning, exact BigInt addition, cache-write token inclusion, cached-read exclusion and wire saturation match the stated rules. The totals reader retries when its read crosses an hour/month boundary. Gateway window-incarnation checks prevent releasing a reservation into a cleared-and-returned window.
- **Publishing:** validation and the current-config parents rule are rerun after losing the hash CAS. Publishing and counting do not condition on each other. Only current config is retained. Config delivery retries failed reads and signals exhaustion; totals reads retry at the push interval. Stream closure after a terminal condition needs B2's correction.
- **Boot and hashes:** boot uses the stream; seed/last-known-good behavior and first-totals budget readiness have passing coverage. There is no newer-version gate preventing an older restored config from applying. A different successful config clears the rejected hash, so republishing an earlier rejected document after that is reconsidered. Gateway rejection currently depends on fixed config content and process credentials; changing an external shell's environment does not change a running process's credentials. B9 is the actual already-running-hash status defect.
- **Live set:** gateway revisions are allocated from a store-wide write counter and do not repeat after forgetting/recreation. Sweeps and forgets condition on the revision they judged. The suite exercises concurrent sweeps and a fresh status racing a sweep. Draining gateways remain live, as specified. Status's initial stale-read hole is B7.
- **Notification contract:** ordered notification of every committed change to every process is explicitly a store obligation. Arbitrarily lost/out-of-order notifications without catch-up are a nonconforming store, not an additional core guarantee. Catch-up triggers config, totals and gateway refresh; stop/start resubscribes. B5 mishandles otherwise legal read/notification overlap.
- **Store contract tests:** the memory suite passes and the torn-snapshot negative control is detected. **Skipping reconnect catch-up for the memory store is acceptable:** its synchronous in-process channel cannot disconnect or lose a committed notification. This skip does not certify a database implementation. Such an implementation must supply independent `attach` handles and a real `reconnect` hook; B11 describes the remaining catch-up test gap. No external durable store or real Postgres failover was exercised here.
- **Limits without model sets:** schema and semantic checks enforce at most one type per scope; inherited limits override by type; aggregation follows every recorded group plus global independent of config. Adding/removing/re-adding limits preserves stored spend; the mismatch is the filtered wire view in B1.
- **Sample and removal:** the sample's protocol replicas have independent cores and Fastify servers over one memory store, correctly documented as one process. Its replica tests and the two-core cross-half e2e pass. No runtime config history/version/epoch, resume/resync, config snapshot endpoint, config-mismatch state or model-set limit compatibility path was found. Remaining prose/test drift is B10; explicit “rejected alternative” paragraphs and invalid fixtures are not compatibility code.

## Verification

The initial sandbox prevented Go's default cache access, the staticcheck download and local test listeners. Retried with `GOCACHE=/tmp/kaiak-audit-b-go-cache` and the required execution access; those environmental failures are not counted as findings. The blocked initial node runner was stopped.

Before adding reproductions:

```text
cd control && npm ci --ignore-scripts && npm test
tests 565; pass 564; fail 0; skipped 1

GOCACHE=/tmp/kaiak-audit-b-go-cache scripts/check-gateway.sh
gofmt, vet, staticcheck, uncached race tests: passed
live-test kit self-test: passed
gateway checks passed
```

Additional cross-half verification:

```text
cd gateway
GOCACHE=/tmp/kaiak-audit-b-go-cache go test -race -count=1 -tags crosshalf ./e2e
ok  kaiak/e2e  168.324s
```

With all reproductions left in place:

```text
cd control && npm test
tests 574; pass 564; fail 9; skipped 1
# All nine failures are Audit B assertions.

cd control && npm run lint
tsc: passed
boundaries ok

cd gateway
GOCACHE=/tmp/kaiak-audit-b-go-cache go test -race -count=1 ./internal/control ./internal/limits -run TestAuditB -v
FAIL TestAuditBAckTransitionCannotHideBatchFromTotals
FAIL TestAuditBAcknowledgedEpochCoveredAfterOldProcessWrite
FAIL TestAuditBReturnToRunningHashClearsRejection
FAIL TestAuditBRejectedLimitRemovalMustNotEraseSpend
# All four fail at their intended assertions; no race-detector failure.
```

For just the Node reproductions:

```sh
cd control
node --test kaiak-control/src/fastify/audit-b.test.ts
```

The final tree is intentionally red because the requested regression tests demonstrate unfixed production defects. No existing test was deleted, skipped or weakened. The production source and contract documents were left unchanged.
