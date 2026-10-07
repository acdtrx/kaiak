# Pre-merge review, round 2 — 2026-10-07 (`control-replicas` at `a1e9bfe`)

## Scope

The branch after phases 3 and 4 (`docs/plans/control-replicas/`, steps 7–10):

- the broadcast-only control plane (current config and its hash, no versions);
- totals on the stream only, acks naming the batch;
- the store contract after round 1's fixes (per-epoch cursors, catch-up,
  non-repeating revisions, the torn-store negative control);
- the GUIDE's Postgres sketch.

## Reviewers

- Three read-only reviewers worked on a detached worktree at `a1e9bfe`:
  - **[R]**: store and core concurrency;
  - **[G]**: the gateway;
  - **[K]**: the contract across both halves, and the docs.
- **[B]** is the independent review (Codex, a plain copy):
  - its report is in `AUDIT-2-independent.md`;
  - the main session re-ran its 13 reproduction tests, and every one fails as
    claimed;
  - they are kept in the session scratchpad (`audit-b3/`) for porting.

Every finding is tagged by how often it shows up under legitimate use: `daily` /
`occasional` / `rare` / `adversarial`. When several reviewers reached a finding on
their own, each of them is named.

## Verdict

**Not ready to merge.**

- Counting inside the store holds: per-epoch conditional writes count every batch
  once, across cores and epochs.
- Three things fail under ordinary use:
  - **Rollback detection** fires on normal read and notification overlap, ending
    every stream on the core.
  - **Ending a stream** does not stop delivery, so a write after the end can crash
    the process.
  - **A gateway that rejected a config** reads a limit the new config dropped as
    unspent.

## Decisions taken with the user (2026-10-07)

Recorded in the plan's OVERVIEW as decisions 21–27 and implemented in phase 5
(steps 11–14):

- **No store sequence and no rollback detection.** Each stream sends what the core
  read last. A restored store is the current state.
- **Count everything, apply limits on top.** Totals carry every (scope, type) window
  with usage, whatever the config. Each stream sends the full totals on connect and
  then only the windows that changed.
- **`counted_through` per batch epoch** for the recipient instance.
- **The step-10 deviation is accepted:** replica clocks within a second, with no
  gateway-side window check.

## High

### 2H1: Rollback detection fires with no rollback, ending every stream again and again

`daily` on any database store; the memory store hides two of the three causes.
Found by [R] H-R1, [K] H1 and [B] B5.

- **Where:** `control-plane/index.ts:115-125, 157-158`; `config-publishing/index.ts:106`;
  `usage/index.ts:140`.
- **How:** `observeSequence` treats any sequence below the highest it has seen as a
  rollback. Three ordinary things produce lower values:
  - a config read reporting its *publish's* sequence (every catch-up, every
    `stop()`/`start()`);
  - a notification arriving after a read on another connection saw later state;
  - two concurrent totals reads resolving out of order.
- **Effect:** every stream on the core ends and its gateways reconnect. The
  reconnects' reads overlap again, which risks a reconnect storm.
- **Outcome:** dissolved by decision 21. The sequence and the detection are removed
  (steps 11–12).

### 2H2: Ending a stream does not stop delivery; a later write can crash the process

`occasional`. Found by [B] B2.

- **Where:** `fastify/gateway-stream.ts:67, 155, 167-181`;
  `config-publishing/index.ts:106-109`.
- **How:** `end()` only calls `raw.end()`. The listeners are removed on the later
  `close` event. A delivery in the same tick writes into the ended response, and
  `ERR_STREAM_WRITE_AFTER_END` is raised with no `error` listener.
- **Effect:** the process can exit, dropping every gateway connection.
- **Outcome:** step 12. Closing becomes synchronous and idempotent: mark closed,
  unsubscribe and cancel timers before ending. Every write path checks it.
  Transport errors go through the same teardown. This is needed even with 2H1
  gone.

### 2H3: A limit the current config dropped reads as unspent on a gateway that rejected it

`occasional`; the cost is money. Found by [G] M1 and [B] B1.

- **Where:** `usage/index.ts:150, 230-249` (`listedWindows` lists only the current
  config's limits); `gateway/internal/limits/shared.go:49-80`, `limits.go:294` (a
  missing window means a base of 0).
- **How:**
  - A gateway rejects config B, for example because a backend credential is
    missing, and keeps running A.
  - B removed or moved a limit A has.
  - Totals built under B never list that window, so the gateway enforces A's limit
    from zero.
- **Outcome:** dissolved by decision 22 (steps 11–13). The gateway counts every scope
  and type, and totals carry every window with usage. Retaining the old base was
  rejected because it cannot see other gateways' new spend.

## Medium

### 2M1: After a real restore, republishing the newer config never reaches the streams

`rare`; severe when it happens (a revoked key works again). Found by [R] M-R1.

- **Where:** `config-publishing/index.ts:71, 107`.
- **How:** the core-wide `delivered` hash is never reset. The streams reconnect to
  the restored A, while `delivered` stays B, so republishing B is not handed out.
- **Outcome:** step 12 (decision 24). `delivered` is removed; each stream skips by its
  own last-sent hash.

### 2M2: One `counted_through` cannot cover a new epoch after an old epoch's late write

`rare`. Found by [B] B3 and [R] L-R2.

- **How:**
  - A gateway process dies while its batch write is stalled.
  - Its replacement, under the same instance name and a new epoch, gets B/1 counted.
  - The stalled A/1 then commits.
  - From then on, `counted_through` names A/1. The new process cannot map that, so
    it counts B's usage twice and may refuse requests until the window ends.
- **Outcome:** steps 11–13 (decision 23). `counted_through` lists the recipient's last
  counted batch for each epoch still kept.

### 2M3: The ack hand-off hides a batch from the totals that cover it

`rare`. Found by [B] B4 and [G] L1.

- **Where:** `gateway/internal/control/usage.go:430-431`.
- **How:** `dropHead` and `noteAcked` take the lock separately. Totals processed
  between the two find the batch in neither list and retire nothing, so the batch is
  counted twice until a later push.
- **Outcome:** step 13. The batch moves to the acknowledged list under one lock.

### 2M4: Configs and totals are not ordered together on a stream

`occasional`. Found by [B] B6, [R] L-R1 and [K] L2.

- **How:** a totals read issued under config A can be written after config B. A limit
  B added then has a base of 0 until the next push.
- **Outcome:** dissolved by decision 22 (totals no longer depend on the config). Each
  of the two message kinds is still ordered among its own reads (decision 21, step
  12).

### 2M5: A delayed first status read can overwrite a newer status

`occasional`. Found by [B] B7.

- **Where:** `gateways/index.ts:143-151`.
- **How:** the receipt-time check runs only after a failed conditional write. A slow
  first read sees the newer revision, and the write succeeds.
- **Outcome:** step 12. The check runs on every attempt.

### 2M6: Usage acks count as contact, so a broken stream never becomes an outage

`rare`. Found by [G] M2.

- **Where:** `gateway/internal/control/usage.go:500`; `limits/shared.go` `outageLocked`.
- **How:** acks no longer bring totals. Bases freeze while acks keep the contact time
  fresh, so N gateways can each spend what is left.
- **Outcome:** step 13. Contact is stream bytes only, in both specs.

### 2M7: The catch-up contract test never runs, and a one-subscriber store passes it

`occasional` for host-app stores. Found by [K] M1 and [B] B11.

- **Outcome:** step 11.
  - An in-repo lossy channel over the memory store drops changes while it is down.
  - The test writes while the channel is down and checks that the catch-up reaches
    every subscriber.
  - Negative controls: a store that reconnects without a catch-up, and one that
    tells only one subscriber.

### 2M8: Removal leftovers the checklist greps missed

Found by [K] M3, [G] L5, [R] L-R5 and [B] B10.

- **Where:**
  - `docs/ARCHITECTURE.md:155, 354-381` (`cv[config-versions]`, the `fastify --> cv`
    edge);
  - `docs/TECH-STACK.md:227-230`;
  - `config.schema.json:5` ("config snapshot");
  - `GATEWAY.md:175, 2235`;
  - `main.go:99, 211, 864`;
  - `client.go:145`, `messages.go:36`, `limits.go:78`, `headers.go:31`,
    `server/limits.go:113`;
  - `fixtures_test.go:5-7`, `shared_test.go:361-363`;
  - `loader_test.go:59-91` (still asserts `kaiak.config.version`);
  - `gateway-stream.ts:65`;
  - `CONTROL-PROTOCOL.md:936`;
  - `BACKLOG.md:81, 335-337`.
- **Outcome:** step 14, with the checklist widened to the phrasings that slipped
  through: `snapshot fetch`, `config snapshot`, `acknowledged batch`,
  `config-versions`, `for another config`, `resumes from`.

### 2M9: The concurrent-snapshot tests never check the config or the live count

`occasional`. Found by [K] M2.

- **Outcome:** dissolved by decision 26. The totals snapshot shrinks to the windows
  and the cursors, since nothing needs the config or the live count read with them.
  Step 11 tests what remains under concurrency.

### 2M10: Republishing the running config leaves the old rejection reported

`occasional`. Found by [G] M3 and [B] B9.

- **Where:** `gateway/internal/control/client.go:409-435`.
- **Outcome:** step 13. A config event whose hash is the running one clears the
  rejection and reports status. Spec: "cleared when a later config is applied, or is
  the one running".

## Low

- **2L1: The Postgres sketch.** Found by [B] B8 and [K] L4.
  - `jsonb` does not keep key order, so the advertised hash may not match the JSON
    sent.
  - `store_meta` has no seed row.
  - `returning <old live>` is not valid Postgres.
  - The catch-up sequence is unspecified.
  - **Outcome:**
    - step 11 (decision 27): the store keeps the config's JSON text, and the core
      sends that text;
    - the sketch gets a seed row and a `for update` read;
    - the catch-up's sequence dissolves with decision 21.
- **2L2: A missed rollback plus ordering by sequence drops configs.** Found by
  [R] L-R3. Dissolved by decision 21.
- **2L3: A throwing host listener stops all later config deliveries** (`rare`). Found
  by [R] L-R4. Step 12: each run is chained with its own catch, and listener errors
  go to `onListenerError`.
- **2L4: The contract tests assert exact gateway revisions** (`occasional`, a false
  failure for a Postgres `SEQUENCE`). Found by [R] L-R6 and [K] L3. Step 11 asserts
  only that they differ and increase.
- **2L5: Usage restored from another instance's spool is retired late** (`rare`,
  over-count only). Found by [G] L2. Step 13 documents it in `GATEWAY.md`.
- **2L6: The acknowledged list grows without bound while the stream is down and acks
  flow** (`rare`). Found by [G] L3. Step 13 bounds it: entries whose usage the
  window roll-over already cleared are dropped.
- **2L7: No metric stays set while a gateway runs a config it was moved off**
  (`occasional`). Found by [G] L4. Step 13 adds a gauge that is 1 while
  `last_rejection` is set.
- **2L8: The e2e never asserts that a reconnect reloads nothing.** Found by [G] L6.
  Step 13 adds `never(config applied)` after a reconnect.
- **2L9: The valid fixture `config-event/valid/full.json` carries the wrong
  `config_hash`.** Found by [K] L1. Step 11 fixes it and adds a fixture test that
  checks the hash.
- **2L10: The host-app migration note is incomplete** (`DEPLOYMENT.md:1006-1015`).
  Found by [K] L5. Step 14.
- **2L11: History narration in Rejected lines** (`CONTROL-PROTOCOL.md:306-307, 794,
  812-813`). Found by [K] L6. Step 14.
- **2L12: ARCHITECTURE's control-plane bullet leaves out the subscription in
  `start`/`stop`.** Found by [K] L7. Step 14.

## Checked and sound

- **Counting in the store** ([R], [B]):
  - per-(instance, epoch) conditional writes, including an old epoch's delayed
    duplicate after a newer epoch;
  - cursor retention;
  - the window re-read across an hour boundary.
- **Retirement on the gateway** ([G], [B]):
  - the counted mark only moves up, so retiring twice is idempotent;
  - records that settle late are caught;
  - the restored lump is tagged correctly.
- **Publishing** ([R], [B]):
  - the conditional replace re-checks the parents rule against the winner;
  - the same content published again is skipped by hash.
- **Gateways** ([R], [B]):
  - the store-wide revision counter;
  - conditional sweeps and forgets;
  - a late status losing to a later receipt (beyond 2M5's first-read gap).
- **Delivery** ([R]): retries bounded and reported; the connect path reads directly.
- **Schemas** ([K]): the two copies are byte-identical, and the gateway's walker
  matches them.
- **Boot and hashes** ([G], [B]):
  - boot from the stream;
  - the last-known-good and seed order;
  - skip by hash;
  - a rejected config re-checked after a restart.
- **Removal in code** (all four): no versions, history, epoch, resync,
  `configsSince`, lease or `GET /v1/config` path remains. The hits are 2M8's
  wording.
- **Catch-up skip for the memory store** ([R], [B]): acceptable, since its channel
  cannot lose a change. A database store must supply the reconnect hook (2M7).

## Outcome (2026-10-07)

Phase 5 of `docs/plans/control-replicas/` (steps 11–14). Decisions are the plan
OVERVIEW's. Commits:
- `ee0792f`: store, contract tests, schema and fixtures;
- `ab0b11c`: the specs and GUIDE §5, §11;
- `a2bc01c`: two leftover lines;
- `635b126`: kaiak-control and the sample;
- `6ff61d5`: the GUIDE;
- `2a8590c`: the gateway;
- `06f7d0a`: e2e, `GATEWAY.md` and `DEPLOYMENT.md`;
- `d64cb8c`: the remaining docs.

| Finding | Outcome | Where |
|---|---|---|
| 2H1 false rollback | **Dissolved** (decision 21): the store sequence and rollback detection are removed; streams order by when each read was issued | `ee0792f`, `ab0b11c`, `635b126` |
| 2H2 write after a stream's end | **Fixed**: teardown is synchronous and idempotent, every write checks it, and a response error goes through it | `635b126` |
| 2H3 a dropped limit unspent after a rejection | **Dissolved** (decision 22): totals list every scope with usage, and the gateway counts every scope, limits on top. End-to-end test in the replicas e2e | `ee0792f`, `ab0b11c`, `635b126`, `2a8590c`, `06f7d0a` |
| 2M1 a republish after a restore not delivered | **Fixed** (decision 24): the core-wide `delivered` is removed; each stream skips by its own last-sent hash | `635b126` |
| 2M2 one `counted_through` across epochs | **Fixed** (decision 23): one entry per epoch, the `counted-through-epoch-duplicate` rule on both halves, and the gateway retiring by its own epoch's entry | `ee0792f`, `ab0b11c`, `635b126`, `2a8590c` |
| 2M3 the ack hand-off gap | **Fixed**: one lock acquisition; `noteAcked` is removed | `2a8590c` |
| 2M4 configs and totals not ordered together | **Dissolved** (decision 22): totals no longer depend on the config; each kind is ordered among its own reads | `635b126` |
| 2M5 a delayed first status read | **Fixed**: the receipt-time check on every attempt | `635b126` |
| 2M6 acks as contact | **Fixed** (decision 25): stream bytes only | `ab0b11c`, `2a8590c` |
| 2M7 the catch-up contract test never ran | **Fixed**: the in-repo lossy channel runs it with two subscribers and writes while the channel is down; two new negative controls | `ee0792f` |
| 2M8 removal leftovers | **Fixed**: every location listed, plus the README and architecture pages (step 14 Result: the greps) | `a2bc01c`, `ab0b11c`, `635b126`, `2a8590c`, `06f7d0a`, `d64cb8c` |
| 2M9 snapshot tests missing config and live count | **Dissolved** (decision 26): the snapshot is the windows and the cursors, tested under concurrency (8 instances × 2 epochs) and by the torn store | `ee0792f` |
| 2M10 a stale rejection after the running config | **Fixed**: the running hash clears the rejection and reports status; spec wording | `ab0b11c`, `2a8590c` |
| 2L1 the Postgres sketch | **Fixed** (decision 27): the config stored as text and sent verbatim; no `store_meta` (so no seed row); `select … for update`; the catch-up carries nothing | `ee0792f`, `ab0b11c`, `635b126` |
| 2L2 a missed rollback drops configs | **Dissolved** (decision 21) | `635b126` |
| 2L3 a throwing host listener stops deliveries | **Fixed**: each run settles on its own; listener errors go to `onListenerError` | `635b126` |
| 2L4 exact revisions in the contract tests | **Fixed**: only "differ and increase" is asserted | `ee0792f` |
| 2L5 another instance's restored spool retired late | **Documented** in `GATEWAY.md` (over-count only) | `06f7d0a` |
| 2L6 the acknowledged list unbounded | **Fixed**: an entry is dropped once its windows have passed | `2a8590c` |
| 2L7 no metric for a config moved off | **Fixed**: `kaiak_control_config_rejected`, with a starter alert | `2a8590c`, `06f7d0a` |
| 2L8 the e2e never asserts that a reconnect reloads nothing | **Fixed**: both cross-half reconnects assert no control config applied | `06f7d0a` |
| 2L9 the wrong hash in `full.json` | **Fixed**, with a fixture test of every valid config event's hash | `ee0792f` |
| 2L10 the migration note | **Fixed**: store interface, publishing result, totals, status fields, new and removed API | `d64cb8c` |
| 2L11 narrating Rejected lines | **Fixed**: K's three were rewritten with the sections they sit in; this phase's own Rejected lines are in the present | `ab0b11c`, `d64cb8c` |
| 2L12 ARCHITECTURE's control-plane bullet | **Fixed**: `start`/`stop` take and release the subscription | `d64cb8c` |

The step-10 deviation (replica clocks within a second, no gateway-side window check)
was accepted by the user (decision 28).

**Regression tests:** each failed before its fix (Results of steps 12 and 13):
- `control/kaiak-control/src/fastify/round-2.test.ts`;
- the gateway's `TestEachEpochIsCoveredByItsOwnEntry`,
  `TestAckHandOffNeverHidesABatchFromTotals`,
  `TestRunningConfigReceivedAgainClearsTheRejection`, `TestAnAckIsNotContact`,
  `TestALimitKeepsItsSpendWhateverTheControlPlanesConfig`;
- the replicas e2e subtest.

Reproductions of dissolved findings (2H1's rollback counts, 2M4) were replaced by
tests of the new rule.
