# Pre-merge review — 2026-10-07 (`control-replicas` at `50d087d`)

## Scope

Everything since `main` (`ea46ab5`) on the `control-replicas` branch:

- several control-plane processes over one store (the store contract, the memory
  store, the core without process state);
- the totals revision as the store's sequence;
- limits without model sets;
- the sample's extra protocol ports (`docs/plans/control-replicas/`).

## Reviewers

Three read-only reviewers worked on a detached worktree at `50d087d`:

- **[R]** store and core concurrency;
- **[G]** the gateway's totals ordering and limits;
- **[K]** the contract across both halves, the GUIDE's store guidance, and the docs.

The independent review **[B]** (Codex, a plain copy, no git history) is in
`AUDIT-independent.md`. The main session re-ran all nine of its reproduction tests
against the copy; every one fails as claimed.

Every finding is tagged by how often it shows up under legitimate use:
`daily` / `occasional` / `rare` / `adversarial`. A finding that several reviewers
reached on their own names each of them.

## Verdict

**Within one batch epoch, between two cores, the design holds:**

- ordinary resends count once;
- publishes and counting never wait on each other;
- totals are ordered by one sequence;
- two cores converge on the same totals.

The contract matches the code in both halves, and the model-set removal is complete
in code.

**It does not yet hold at its edges:**

- A stalled write from an old batch epoch counts again.
- Totals from a store that started over and whose config this gateway rejected
  silently stop enforcing budgets.
- A failed read after a config notification loses that config on one core.
- A totals read that straddles an hour boundary can acknowledge usage it leaves out.
- Gateway revisions are reused after a gateway is forgotten.
- The store contract is under-specified and under-tested in places a database store
  would hit.

None of this touches the single-process path that runs today. All of it must be fixed
before replicas are used for billing.

## High

### H1 — A stalled write from an old batch epoch counts a batch twice

Reviewers: [R] M1, [B] B1. Frequency: `occasional` — gateway restarts while usage
delivery is slow.

**Where:** `usage/index.ts:163-182`, `:245-250`.

**What happens:**

1. Core A stalls on epoch E1, batch 2.
2. The gateway's resend reaches core B, which counts it.
3. The gateway restarts on a fresh spool, epoch E2, and B counts E2 batch 1.
4. A's write is refused. It re-decides against E2/1, calls the old batch
   `new-epoch`, and counts it again.
5. The cursor moves back to E1, so later duplicates become possible too.

The single latest-cursor slot cannot give exactly-once counting across epochs.

**Confirmed:** both [R] and [B] reproduced it (211 instead of 111; three records for
two requests).

**Fix:** keep the cursor per (instance, batch epoch), dropped by the same retention.
Spec: Usage intake's "two copies… counted once" becomes true across epochs. Store
contract and tests.

### H2 — Totals from another config epoch retire the gateway's own usage and hide the mismatch

Reviewers: [G] M1, [K] M1, [B] B2. Frequency: `rare` (permanent) / `occasional`
(transient, at every epoch change).

**Where:** `control/client.go:414-431`, `limits/shared.go:56-79`, `:142-157`.

**What happens:**

- A message of another epoch has its windows dropped, but its `counted_through` and
  ack still retire own usage.
- The limiter never hears the new epoch, so there is no `config_mismatch`, no
  `budget_unavailable` after the grace, and no live-count update.
- Worst case: the store started over and this gateway rejects the new store's config.
  It then enforces stale bases forever while acks keep erasing its spend.
- Transient case: every epoch change, between the first new-epoch ack and the
  resync.

**Confirmed:** [G] and [B] (500,000,000 nano-USD went to 0; mismatch false;
admitted after the grace).

**Fix:**

- Retire own usage only against totals whose bases cover it: only from messages of
  the applied epoch.
- Pass messages of the position epoch to the limiter as "newest totals for another
  config" (windows not applied). The existing mismatch path then runs: the gauge, then
  `budget_unavailable` after the grace, and the live count.
- Keep ignoring messages from superseded stores (neither the applied epoch nor the
  position epoch).
- Spec: GATEWAY.md and CONTROL-PROTOCOL.md for the rejected case and for
  `counted_through` across epochs.

### H3 — A failed read after a config notification silently loses that config on one core

Reviewer: [B] B3; [R] L6 (silent failures). Frequency: `occasional` — any transient
database read error.

**Where:** `config-versions/index.ts:92-131`, `fastify/gateway-stream.ts:165`.

**What happens:** `configsAfter()` rejects and the delivery is swallowed. That core's
streams never get the version until the next publish or a reconnect. A revoked key
can keep working on part of the fleet.

**Confirmed:** [B].

**Fix:**

- Retry the delivery with bounded backoff from the last delivered version.
- If it cannot recover, end the affected streams so the gateways resync through the
  snapshot path.
- Report every failure to the host (`onListenerError` or a delivery-error hook).
- No silent catches.

## Medium

### M1 — Lost notifications after a channel reconnect are never made up

Reviewers: [R] M2, [K] M2. Frequency: `occasional` (every listener reconnect in a
database store).

**Where:** `storage/types.ts:222-227`, GUIDE §5.

**What happens:**

- Postgres LISTEN/NOTIFY and Redis pub/sub drop notifications while the listener is
  down.
- A missed `config-published` leaves that core's streams on the old config until the
  next publish. The spec understates this as "totals stale until the next change".

**Fix (contract):**

- On every (re)subscribe or channel reconnect, the store emits catch-up changes:
  `config-published` (latest), `batch-counted` and `gateways-changed`
  (`liveChanged: true`). The core already tolerates repeats.
- GUIDE: a dedicated `LISTEN` session (not through a transaction-mode pooler),
  catch-up on reconnect, and reads after a notification from the primary.
- Correct the spec sentence.
- Add a contract test with a reconnect hook.

### M2 — A totals read across a window boundary can acknowledge usage its windows omit

Reviewer: [B] B4; [R] L5 (clock skew). Frequency: `daily` (hour rollovers under
concurrent traffic).

**Where:** `usage/index.ts:128-134`, `control/client.go:422`.

**What happens:**

1. The core picks the current windows before awaiting the snapshot.
2. A read started at 12:59:59 returns a sequence and cursor that include a 13:00
   batch, but only the 12:00 window.
3. The gateway retires that usage.
4. The accurate ack with the same sequence is ignored as not newer.

**Confirmed:** [B], on both halves.

**Fix:**

- The core retries a snapshot whose window bounds changed while it awaited the read.
  Simplest form: take the bounds from the clock after the snapshot, and read again if
  they differ.
- The gateway retires a counted generation only when the applied totals cover its
  window. This also covers [R] L5's clock skew across cores.
- Spec and GUIDE: replicas need synchronized clocks, with the bound stated.

### M3 — Gateway revisions restart at 1 after a forget (A-B-A)

Reviewers: [R] L1, [B] B5. Frequency: `rare`.

**Where:** `storage/types.ts:125-127`, `memory.ts:145-167`, `gateways/index.ts:183-194`,
and the contract test at `store-contract/index.ts:508-510`.

**What happens:** a stale sweep's forget at revision 1 deletes a gateway that was
forgotten and rejoined at revision 1. The live count drops; shares inflate.

**Confirmed:** [R] and [B].

**Fix:** revisions never repeat for an instance. A store-wide counter works, since
the totals sequence already moves on each live change. Change the contract and its
test.

### M4 — The exported contract tests certify a store with a torn snapshot

Reviewers: [B] B6, [K] L4. Frequency: `occasional` (whenever a host app writes a
store).

**What happens:** the concurrent snapshot test never checks `last` (the cursor)
against the same snapshot. A store that reads the cursor in a separate statement
passes all 23 tests.

**Missing checks:**

- `gateways-changed` with `liveChanged: false` on every status;
- a forget of a live gateway notifying `liveChanged: true`;
- notification order under concurrent writers.

**Confirmed:** [B] ran the suite against a deliberately broken store: 23/23 pass.

**Fix:**

- Concurrent assertions tying the cursor, windows, config, live count and sequence to
  one snapshot.
- The three notification checks.
- A deliberately broken adapter kept as a negative control in kaiak-control's own
  tests.

### M5 — The GUIDE's ledger insert blocks a valid resend after cursor retention

Reviewer: [B] B7. Frequency: `rare`.

**What happens:** the protocol allows recounting a resend once its cursor has been
forgotten. The GUIDE's ledger, with `record_id` as primary key and a plain insert,
aborts that transaction. Every retry fails, and later batches queue behind it.

**Confirmed:** [B] (a constraint simulation).

**Fix:**

- Ledger insertion is idempotent by record ID (`ON CONFLICT DO NOTHING`).
- Document the retention boundary: windows may recount after retention, the ledger
  never double-charges.
- Correct "a resent batch is never passed again".

### M6 — The GUIDE's Postgres sketch has flaws an implementer would copy

Reviewers: [R] L7, [K] L1, [K] L2.

- `window_total`'s primary key includes a nullable `group_id`, so global windows
  cannot be stored. Use a sentinel or `UNIQUE NULLS NOT DISTINCT`.
- Lock order can deadlock concurrent batches: take the `store_meta` row first.
- A unique violation on a first write must map to `{ saved: false }`.
- The gateway sketch lacks the first insert, the sequence step on live changes, and
  the notify.
- No recipe for taking a new epoch on a restore or an asynchronous-standby failover.

### M7 — Docs still describe the old core and model sets

Reviewers: [K] M3, [K] M4, [G] L2.

- ARCHITECTURE's `usage` bullet still describes the old core.
- GATEWAY.md `:1193`, `:1196`; CONTROL-PROTOCOL `:407-408`; README `:65`.
- The broken sentence at CONTROL-PROTOCOL `:1001-1002`.

## Low

- **L1 — A late status from a restarted process raises a false conflict** ([R] L2).
  It also becomes the stored status. Fix: read the receipt time once. On a refusal,
  if the stored record is newer, drop this status.
- **L2 — `configsSince` resyncs a resumable stream when a publish lands during the
  read** ([R] L3). Fix: accept extras that are contiguous from `version + 1`.
- **L3 — `config_epoch` is read outside the snapshot** ([R] L4, [K] L3). Fix: add the
  epoch to `TotalsSnapshot` and to the config reads.
- **L4 — A core never unsubscribes from the store** ([R] L6). Fix: `stop()`/`close()`
  releases its subscriptions. Silent failures are covered by H3.
- **L5 — Delayed notifications skip pruned configs without a resync** ([B] B8). Fix:
  delivery detects a gap and resyncs the affected streams. The stream boundary
  enforces contiguous versions.
- **L6 — The small-share warning loops over every model** ([G] L1, [K] L5). It
  ignores the scope's `allowed_models`. Frequency: `daily` false warnings. Fix:
  filter by the scope's allowed models.
- **L7 — Leftovers.**
  - Dead `copySettled`.
  - Stale carry-over comments: `limits.go:171-172`, `:219-222`; `totals_test.go`
    comments; `shared_test.go:85`, `:327`; `fixtures_test.go:293`.
  - `rejected.json` uses the removed `limit-model-unknown`.
  - A test name mentions "lease".
  - BACKLOG `:313`; CONTROL-PROTOCOL `:994`; DEPLOYMENT `:514`; the
    `control-plane.html` footer comma.

  Sources: [G] L3, [K] L6, [K] L7.
- **L8 — Three e2e checks were weakened** ([G] L4). They moved from a priced model
  outside every USD limit to an unpriced one. Fix: use `evalKey` with `"priced"`.
- **L9 — Repeating log line** ([G] L5). "totals ignored: from another config epoch"
  logs at Info for every message. Fix: once per epoch.
- **L10 — DEPLOYMENT notes** ([K] L7).
  - The "limit added mid-window" note holds in control-plane mode only.
  - No host-app migration note: the store interface was rewritten; the lease options,
    `controlPlaneId`, `onLimitCarriedOver` and `limitIdentity` are gone.
- **L11 — Spec wording narrates history** ([K] L8): CONTROL-PROTOCOL `:812-813` and
  `:851-855`. Fix: phrase these as dated rejected alternatives.
- **L12 — A host's publish that loses a race overwrites the winner's edit** ([R]
  note). This is a lost update. It was there before this branch, but is more likely
  with several admin replicas. An optional `expectedVersion` on `publishConfig` would
  close it. Recorded; decide in review.

## Checked and sound

- **Same-epoch exactly-once:**
  - a conditional cursor write with the additions, records and sequence in one write;
  - losers re-decide;
  - simultaneous resends to both cores count once [R][B].
- **Publishes vs counting:** independent. A losing publish re-checks the
  parent-change rule against the winner [R][B][K].
- **Snapshot and ordering:**
  - one store sequence orders every change;
  - duplicate or reordered notifications are harmless;
  - subscribe-before-replay, with dedup by `lastSent`;
  - two cores converge [R][B][K].
- **Live set:** conditional expire and forget; two sweeps expire once; a status
  defeats a stale expiry [R][B]. The exceptions are M3 and L1.
- **Limits:** identity by scope and type in both halves; `child_defaults` merged by
  type; counting by path whatever the config; an edited value keeps its window; data
  files at format 3; the file-mode vs control-plane-mode difference is as specified
  [G][K][B].
- **Ordering within an epoch:** a higher sequence wins; the first message in an epoch
  is taken; delayed answers from a superseded store are ignored [G][B]. The
  exceptions are H2 and M2.
- **Schemas, fixtures, the log table and versions** agree in both halves [K].
- **Sample:** replica ports work. The cross-half replica test exercises two gateways
  on two cores [K][B].

## Triage proposal

**Fix before the merge:**

- H1–H3;
- M1–M7;
- L1–L11.

**Recorded for a decision:** L12 (`expectedVersion` on publish).

## Outcome (2026-10-07)

Phase 3 (`docs/plans/control-replicas/`, steps 7–9: the broadcast-only control plane)
removed config versions, the config epoch, the totals revision and totals on acks,
which dissolved some findings. Step 10 fixed the rest. Commits: `c2b1b87`
(kaiak-control), `9791c7e` (gateway), `d943b88` (docs).

| Finding | Outcome |
|---|---|
| H1 stalled old-epoch write counts twice | **Fixed** `c2b1b87`: one batch cursor per (instance, epoch); a batch is decided against its own epoch's last batch; `counted_through` is the latest of any epoch. Regression tests from [R] R1 and [B] B1 (and the crash variant). |
| H2 other-epoch totals retire own usage, hide the mismatch | **Dissolved** by phase 3: there is no config epoch and no mismatch state; totals apply to the running limits by (scope, type) whatever the config, and own usage leaves only through stream totals whose `counted_through` covers it (`TestRejectedConfigKeepsBudgetsEnforcedFromStreamTotals`). [B] B2's test asserted the removed epoch path: not ported. |
| H3 failed read after a notification loses the config | **Fixed** `c2b1b87`: the read is retried (100 ms doubling, five retries, `deliveryRetryDelaysMs`), then announced (`onDeliveryFailed`); the Fastify plugin logs it and ends its streams. A failed totals read is retried at the push interval. [B] B3 ported, adapted to `currentConfig`. |
| M1 lost notifications after a reconnect | **Fixed** `c2b1b87`: a `catch-up` store change after a reconnect; the core rereads the config, the totals and the live set; contract test behind a `reconnect` hook; GUIDE: dedicated `LISTEN` session, reads on the primary. |
| M2 boundary read omits the counted window | **Fixed** `c2b1b87` at its cause: the core takes the windows current once the read is over and reads again when they changed. [B] B4's control half ported; its gateway half (an ack with the same revision ignored) dissolved with acks carrying no totals and no revision. **Not built**: the gateway-side "retire only when the windows cover it" — it would need the totals to state the core's window starts on the wire, and what remains after the core fix is clock skew between processes (or between a process and a gateway), which predates this branch. The spec and GUIDE now require replica clocks within a second. |
| M3 gateway revisions repeat after a forget | **Fixed** `c2b1b87`: revisions never repeat for an instance (a store-wide counter in the memory store; the contract test changed). [R] R2 and [B] B5 ported. |
| M4 contract tests certify a torn snapshot | **Fixed** `c2b1b87`: concurrent snapshot consistency (cursor, windows, sequence), status and forget notifications, notification order, catch-up; a torn store (`store-contract/torn-store.ts`) as negative control, run in a child process and required to fail. [B] B6 ported as that control. |
| M5 ledger insert blocks a resend after retention | **Fixed** `d943b88` (GUIDE §6 and the sketch: `on conflict (record_id) do nothing`; the retention boundary stated; the store interface says so). [B] B7 not ported: it modelled the GUIDE's SQL, which the fix changes; no library code path changes. |
| M6 Postgres sketch flaws | **Fixed** `d943b88`: `group_id` not null with `''` for global, `store_meta` locked first, unique violations as `{ saved: false }`, per-epoch cursors, a store-wide gateway revision, the full gateway write and notify. The restore and epoch part **dissolved** (no epoch; a core that sees the sequence go back ends its streams). |
| M7 docs describe the old core and model sets | **Fixed** `d943b88` (ARCHITECTURE's `storage`, `config-publishing` and `usage` bullets; GATEWAY.md Limits; CONTROL-PROTOCOL public model names; README). The broken sentence was already fixed in phase 3. |
| L1 late status raises a false conflict | **Fixed** `c2b1b87`: the receipt time is read once; a record that won with a later receipt time drops the status. [R] R4 ported. |
| L2 `configsSince` resync race | **Dissolved**: no `configsSince`. [R] R3 not ported. |
| L3 epoch read outside the snapshot | **Dissolved**: no config epoch. |
| L4 a core never unsubscribes | **Fixed** `c2b1b87`: one store subscription per core, released by `stop()`, retaken with a catch-up by `start()`. |
| L5 pruned configs skipped without resync | **Dissolved**: no history. [B] B8 not ported. |
| L6 share warning loops over every model | **Fixed** `9791c7e`: filtered by the scope's allowed models. |
| L7 leftovers | **Fixed** `9791c7e`, `d943b88`: dead `copySettled` and its test, stale comments, the status fixture naming a removed code, docs wording (BACKLOG and the stale "lease" test name were already gone in phase 3). |
| L8 three weakened e2e checks | **Fixed** `9791c7e`: each again shows a priced model outside every USD limit serving. |
| L9 repeating "another epoch" log | **Dissolved**: the line went with the epoch filter. |
| L10 DEPLOYMENT notes | **Fixed** `d943b88`: the mid-window note per mode; the host-app migration note. |
| L11 history narration | **Fixed** `d943b88`: rephrased as dated rejected alternatives. |
| L12 `expectedVersion` on publish | **Dissolved** by decision 18: a publish replaces the current config, and concurrent editing is the app's. |

Found while verifying step 10: two tests still synchronized as if acks carried totals,
and failed now and then — `TestUsageAcksFailingPastTheGraceRefusePricedBudgets` (a push
showing the batch counted arrived before the ack was taken; about 1 in 60 runs) and the
cross-half "control plane back" subtest (the new store's totals come on the stream;
gw-a enforced the spend restored at its boot until they did). **Fixed** `238b6ee`: the
tests wait for the ack and for the stream totals. No product change: the gateway ends
the "usage waiting" outage at the ack and replaces restored totals with the first
pushed ones, as specified.
