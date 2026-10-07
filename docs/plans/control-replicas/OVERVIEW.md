# Plan: control-plane replicas

## Goal

Let a control plane built on `kaiak-control` run as several processes over one
store. Today one process per store is enforced by a lease. A report from people
building on kaiak found that two control-plane servers disturb each other's usage.
That is by design (`CONTROL-PROTOCOL.md`, Control-plane processes, settled
2026-09-25), but the design does not fit their deployment.

The library keeps all protocol logic. What makes replicas agree moves behind the
store contract, so the host app chooses:

- the in-memory store for a single process, or
- a store of its own that holds the contract across processes and keeps them in
  sync (transactions, a change channel).

## Scope

- **The store contract carries what lives in the core's process memory today:**
  - the totals sequence that orders totals messages (*superseded by decision 21: no
    store sequence*)
  - consistent totals reads
  - publishes and batch counting that cannot interleave wrongly
  - change notification across processes
- **The core** (`kaiak-control`) works through that contract only. The lease and
  `controlPlaneId` go; the expiry sweep runs safely on every replica.
- **The memory store** implements the new contract and can be shared by several cores
  in one process, which is how the contract is tested.
- **Contract tests any store can run** (the memory store's, exported for host apps
  to port).
- **The gateway orders totals by the store's sequence**: the restart detection and the
  list of replaced control-plane processes go. *Superseded by decisions 18–21: the
  sending core orders each stream; totals carry no revision.*
- **Protocol:** the totals `revision` changes meaning and shape. *Superseded by
  decision 19: totals carry no revision.*
- **Broadcast only** (added 2026-10-07, steps 7–9): the app owns its config; the
  library validates the current document and broadcasts it with the totals. No config
  versions, history, epoch or totals revision on the wire; acks carry no totals;
  ordering is the sending core's job per stream.
- **Limits without model sets** (added 2026-10-06, step 3): a limit is `{ type, value }`
  per scope, identified by (scope, type); usage counts by scope whatever the config;
  the model-set carry-over goes. Publishing and counting no longer depend on each
  other.
- **Docs:** `CONTROL-PROTOCOL.md`, `GATEWAY.md`, the GUIDE (hard rules, store, operating,
  pitfalls), the control-plane architecture page.

## Out of scope

- A durable store in this repo (Postgres or other): the host app's job, with the
  GUIDE's sketch updated.
- Per-instance gateway tokens (backlog, unchanged).
- Leaving draining gateways out of the live count (backlog, unchanged).

## Decisions

Settled with the user (2026-10-06):

1. **The library passes multi-instance coordination to the app through the store.**
   - The memory store serves one process.
   - A host wanting replicas provides a store that holds the contract and syncs its
     processes.
2. **The gateway gets simpler:** totals are ordered by one store-wide sequence, so
   restarts and replicas need no special handling.
3. **Released together with `docs/plans/messages-responses/`**, under its protocol
   version 5, with no second bump. If this plan were ever released alone, it would
   bump to 6.

Made while planning (confirm in review):

4. *Superseded by decision 21 (2026-10-07): the store has no sequence.*
   **One totals sequence in the store**, an integer that grows by one with every
   change to the totals:
   - a counted batch
   - a publish
   - a live-set change

   Each write and its step are one atomic store operation.
5. **Every totals-changing write is conditional** on what it was computed against.
   A refused write is recomputed: optimistic concurrency, no locks across processes.
   - A batch is saved only if the instance's last batch is unchanged.
   - A publish is saved only if the latest version is unchanged.
   - *Changed 2026-10-06 (decision 15):* the batch's config-version condition and the
     publish's sequence condition existed only for the model-set carry-over, which
     step 3 removes.
   - A live-set change is saved only if the gateway record it read is unchanged.
6. *Changed by decision 26 (2026-10-07): the snapshot is the windows and the
   cursors only.*
   **A consistent totals read is a store operation.** It returns the sequence, the
   recipient's last batch, the latest config version and the current windows from one
   snapshot. This replaces the core's in-process lock.
7. **Change notification is part of the store contract.**
   - The store calls the core when any process published a config, changed the
     totals, or changed the gateways.
   - The core turns these into the listener events streams use today.
   - The memory store notifies in process; a replicated store wires its own channel
     (Postgres LISTEN/NOTIFY, Redis pub/sub).
   - Lost notifications are the store's bug. No polling fallback in the core
     (AGENTS.md → Debugging: no stacked safety nets).
8. **The lease is removed**, and with it `store-lease-held`, `storeLeaseTtlMs`,
   `onStoreLeaseLost` and `controlPlaneId`. Every store must hold the conditional
   writes, so a second process is safe by contract.
   - Rejected: keeping the lease as an option. It protected stores without
     conditional writes, which the contract no longer allows.
   - Rejected: a leader for the sweep. Idempotent conditional writes need none.
9. **The expiry sweep runs on every core.** Its writes are conditional (decision 5),
   so two sweeps, or a sweep racing a fresh status, never drop a live gateway.
10. *Superseded by decision 18 (2026-10-07): totals carry no revision.*
    **`revision` becomes the sequence integer**, ordered within the message's
    `config_epoch`.
    - ~~The gateway applies totals when the epoch differs from the last applied (the
      store started over: adopt) or when the sequence is higher.~~ *Changed in step 1
      (2026-10-06, accepted):* the gateway applies totals only when their
      `config_epoch` is the epoch of the config it runs, and then only with a higher
      sequence (the first in an epoch whatever its sequence). Adopting another epoch
      would let a delayed answer from a store that started over reset the ordering;
      totals of another epoch cannot apply to the running config anyway.
    - Rejected: keeping `{ control_plane, sequence }` with a fixed ID. The object
      would carry nothing.
11. **The per-instance batch queue stays in each core.** It only orders a resend
    against its original within one process; across processes the conditional write
    already decides.
12. **The sample can run several cores over its one memory store on several ports**
    (`KAIAK_SAMPLE_PROTOCOL_PORTS`, a list). This lets the cross-half e2e connect two
    gateways to two cores, and it demonstrates the shape.
    - Rejected: testing replicas only inside `kaiak-control`. It would never show a
      real gateway following totals from two processes.

Changed or added in step 1 (2026-10-06, accepted):

13. *Superseded by decision 18 (2026-10-07): a core that sees the store's sequence go
    back closes its streams; there is no config epoch.*
    **A store that loses or rolls back its state takes a new config epoch** (a restore
    from a backup included). Without it, its sequence would go back and gateways would
    ignore its totals.
14. **Every gateway status write is conditional** on the record it was judged against,
    not only live-set changes: the rule that flags two processes under one instance
    name reads the previous record, and needs that across processes too.

Settled with the user after step 2 (2026-10-06):

15. **Limits lose their model sets.**
    - A limit is `{ type, value }`; a scope (global or a group) has at most one per
      type; identity is (scope, type).
    - Budgets are per group, not per model. An unpriced (local) model costs $0, so a
      group's USD budget already counts only priced models.
    - With no model sets there is no carry-over, so publishing and usage counting no
      longer depend on each other (step 3).
    - Rejected: limit IDs (one limit per type per scope needs none). Rejected: keeping
      model sets with an unconditioned carry-over.
16. *Changed by decision 22 (2026-10-07): totals list every window with usage.*
    **Usage counts toward every scope on a record's path, whatever the config's
    limits.** The totals read lists only the windows the latest config limits. A limit
    added mid-window starts with that window's usage so far.
17. **Provider budgets** (money limits on a set of backends, such as one provider
    account across regions) are a separate plan after this release, pending the
    user's research. They get a backlog entry in step 3.

Steps 3–5 of the first plan became 4–6 when step 3 was inserted (2026-10-06). Steps
1–2's Results name the old numbers.

Settled with the user after the pre-merge review (2026-10-07):

18. **The control plane only broadcasts.**
    - The app is the source of truth for config: it composes the parts, keeps any
      history, and owns concurrent editing.
    - `kaiak-control` holds the **current** config and its content hash, validates a
      publish (including the parents rule against the current config) and broadcasts
      it.
    - No config versions, history, resume, resync or config epoch. The gateway
      applies whatever config the control plane sends, skipping one identical by
      hash.
    - Rejected: "a gateway only accepts a newer config". It protects against nothing
      the sender cannot prevent itself, and it traps gateways on a config the control
      plane no longer has after a restore.
19. **Totals travel only on the stream, without a revision.**
    - Acks only acknowledge batches; own usage is retired by stream totals.
    - *Changed by decision 21 (2026-10-07): ordering by read issue, no rollback
      detection.* Each core orders what it sends per stream using the store's
      sequence internally, and closes its streams when the store's sequence goes
      back (rollback).
    - The gateway applies totals windows to its own limits by (scope, type), whatever
      config it runs. There is no config-mismatch state.
    - Accepted cost: after an ack the gateway keeps counting that batch locally until
      the next push (about a second).
20. **Removal discipline.**
    - Phase 3 is a removal. Nothing removed survives renamed, aliased or behind a
      compatibility path.
    - Tests asserting removed behaviour are deleted, not adapted.
    - Step 7's checklist must grep clean at the phase end (step 9).

Settled with the user after the second pre-merge review (2026-10-07,
`docs/reviews/2026-10-07/AUDIT-2.md`):

21. **No store sequence, no rollback detection.**
    - Each stream sends what its core read last, ordered by when each read was
      issued, never by a value the store returns.
    - A store restored from a backup is simply the current state: its config and
      totals are sent like any other.
    - Removed: the store's sequence (in writes, snapshots, notifications and
      `CurrentConfig`), `observeSequence`, `onRollback` and the Rollback rule in the
      spec.
    - Why: every source of the sequence (notifications, concurrent reads, a config's
      publish sequence) arrives out of order under a database store, so the detection
      fired on ordinary traffic (2H1). Order by issue needs nothing from the store.
    - Rejected: a causal check (compare against what the core knew when the read
      started). It needs special cases for notifications and catch-up, and still
      protects nothing the app cannot see itself.
22. **Count everything; limits apply on top.**
    - Control plane: usage already counts toward global and every group on a record's
      path, for `tokens_per_hour` and `usd_per_month`, with or without a limit. Every
      record is priced whatever the limits.
    - Totals carry **every (scope, type) window with usage**, whatever the config.
    - Gateway: counts its own usage for every scope and type in the same way. A
      limit is a check over those counts, so a config change never changes a count.
    - **Changes only:** each stream's first totals are complete. Later totals on that
      stream list only the windows that changed since its last push; a window not
      listed keeps its value. Usage only grows within a window, so merging is safe.
    - **One snapshot read per core per push** serves every stream on that core: the
      core diffs it against its previous read and merges the changes into each
      stream's pending totals. A new stream starts from the full read.
    - Per-minute limits stay local shares, unchanged.
    - Why: a gateway that rejected a config still enforces limits the current config
      dropped (2H3), and a limit added mid-window is never briefly zero. Rejected:
      keeping the last base for a missing window, which misses other gateways' new
      spend.
    - Resolves the backlog's "Totals size bound" (removed in step 14).
23. **`counted_through` per epoch.** Totals list, for the recipient instance, the last
    counted batch of each epoch still kept (an array; empty before the first). The
    gateway retires its own usage by each epoch it holds. Why: one "latest" cursor
    names an old epoch after its late write, and the new process can never retire
    its usage (2M2).
24. **Each stream skips a config by its own last-sent hash.** The core-wide
    `delivered` hash is removed (2M1).
25. **Contact for the outage rule is stream bytes only.** Acks no longer bring totals,
    so they prove nothing about the bases (2M6).
26. **The totals snapshot is the windows and the cursors**, read consistently. The
    config and the live count are read separately: nothing needs them in the same
    snapshot once totals no longer depend on the config (2M9 dissolved).
27. **The store keeps the config's JSON text**, and the core sends that text
    verbatim. The hash is over that text, so a store whose JSON type reorders keys
    cannot break it (2L1).
28. **Replica clocks within a second** (step 10's deviation, accepted). No gateway-side
    window-coverage rule.

Settled with the user after the third pre-merge review (2026-10-07,
`docs/reviews/2026-10-07/AUDIT-3.md`):

29. **One stale-totals signal, on the gateway.** An acknowledged own batch keeps the
    usage-waiting clock (`UsageWaitingSince`) running until stream totals whose
    `counted_through` covers it are applied. Totals that stop coming, for any cause
    (a dead notification channel, failing or hung totals reads, a stuck feed), put a
    gateway that spends into outage after the grace (3H1).
    - The control plane pushes whenever a cursor moves, even when no window changed
      (a zero-cost batch), so covering never depends on spend.
    - Batches restored from another instance's spool, which no `counted_through`
      names, do not hold the clock.
    - Rejected: the control plane closing its streams when totals reads keep failing.
      It is a second net for the same concern (AGENTS.md → Debugging), and misses a
      dead channel.
30. **A stream's first totals come from a read issued after it joined.** Complete
    totals never go back across reconnects or replicas, and a failing feed never
    serves its last good read as a new baseline (3H1).
31. **A totals event that fails decoding ends the stream.** The reconnect brings
    complete totals. Other unknown or malformed events are still logged and skipped
    (3H1).
32. **The spool keeps a batch until saved totals cover it.**
    - A batch leaves the spool only when a `totals.json` save whose
      `counted_through` covers it has completed. Until then it is kept, marked
      acknowledged and never resent.
    - On restart, own usage is rebuilt as the spooled batches beyond the saved
      `counted_through`, so the saved totals plus the spool give the spend exactly
      (3H2).
    - `totals.json` is written in the background on an interval and at shutdown,
      never on the stream goroutine (G3-M2). It holds only current windows.
    - The acknowledged list becomes part of the spool and is bounded by the spool's
      own limits (3L4).
    - The spool format bumps.
    - Without a data directory nothing changes: own usage is in memory, as today.
    - Rejected: treating money totals as unknown after any unclean restart. It is
      simpler, but priced models fail closed until the control plane is back.
33. **Hour and month counters live by scope until their window ends, whatever the
    config.** A deleted group's counters are kept, and reused if the ID comes back
    within the window. In-flight reservations stay on the same counters. Pushed
    windows are pruned once their window has passed (3M1).
34. **The memory bound counts the counters a config allocates:** two per scope plus
    its per-minute counters, 50 000 at most, on both halves. Minute buckets are
    allocated only for per-minute counters (3M2).

## Constraints

- Protocol changes land on both halves at once.
- No backwards compatibility: totals with the old `revision` are refused by schema
  on both halves.
- The control plane is never in the request path; nothing here changes the
  gateway's outage behaviour.

## Risks

- **Store implementers carry more.** A wrong conditional write double-counts or loses
  spend.
  - Mitigation: exported contract tests, including two cores over one store, that a
    host app runs against its store. The GUIDE's Postgres sketch shows each
    conditional write and the notification channel.
- **Write conflicts under load:** batches conflict only with a resend of the same
  instance's batch, and publishes only with publishes (decision 15).
  - Step 4 measures a contended run in tests and records it.
- **A notification gap:** a store that drops notifications leaves streams without
  pushes until the next change. *Changed by decisions 19 and 29: acks carry no totals;
  an acknowledged batch no totals have shown counted puts a spending gateway into
  outage after the grace.*
  - Mitigation: the contract tests check notification across two cores, and catch-up
    after a channel reconnect through the lossy channel.

## Tag

The `v0.10.1` tag from `docs/plans/messages-responses/` is the anchor for both plans.

## Branch and worktree

Branch `control-replicas`, worktree `.claude/worktrees/control-replicas`. It starts
from `main` after `messages-responses` merges, and rebases onto `main` before the ff
merge.

## Phases and steps

- **Phase 1 — contract, library, gateway** (steps 1–5). Green at the end.
  1. `STEP-1-contract.md`: specs, schemas, fixtures.
  2. `STEP-2-store-contract.md`: the store interface, the memory store, the exported
     contract tests.
  3. `STEP-3-simple-limits.md`: limits without model sets, counting by scope; the
     contract and the store.
  4. `STEP-4-core.md`: the core over the new contract; lease and carry-over gone;
     sweep everywhere; two-core tests.
  5. `STEP-5-gateway.md`: totals ordering by sequence; limits by scope and type; the
     fake control plane.
- **Phase 2 — end to end and docs** (step 6). Green at the end.
  6. `STEP-6-e2e-and-docs.md`
- **Phase 3 — broadcast only** (steps 7–9). A removal; green only at the end.
  7. `STEP-7-broadcast-contract.md`: the contract, schemas, fixtures, store interface,
     removal checklist.
  8. `STEP-8-broadcast-control.md`: `kaiak-control` and the sample.
  9. `STEP-9-broadcast-gateway.md`: the gateway; the checklist greps clean.
- **Phase 4 — review fixes** (step 10). Green at the end.
  10. `STEP-10-review-fixes.md`: the pre-merge review's surviving findings.
- **Phase 5 — round-2 fixes** (steps 11–14). A removal and a protocol change; green
  only at the end.
  11. `STEP-11-contract.md`: specs, schemas, fixtures, the store interface and its
      contract tests, the GUIDE sketch, the removal checklist.
  12. `STEP-12-control.md`: `kaiak-control` and the sample.
  13. `STEP-13-gateway.md`: the gateway.
  14. `STEP-14-docs-and-green.md`: leftovers, migration note, the checklist greps
      clean, the full suite three times.
- **Phase 6 — round-3 fixes** (steps 15–17). Green only at the end.
  15. `STEP-15-control.md`: the spec rules, the store contract, the GUIDE, and
      `kaiak-control`.
  16. `STEP-16-gateway.md`: the stale-totals signal, the spool, counters by scope, the
      memory bound.
  17. `STEP-17-docs-and-green.md`: wording, migration note, AUDIT-3 outcome, three
      green runs.

Expected reds inside phase 1:
- After step 1, both halves fail the totals fixtures. Step 4 clears `kaiak-control`'s
  and step 5 the gateway's.
- The cross-half e2e stays red until step 5.

## Verification

**Verification status:** done (2026-10-06); phases 1 and 2 green.

- [x] **Store contract tests** against the memory store (`STEP-2-store-contract.md`,
  `STEP-3-simple-limits.md`):
  - conditional batch, publish and live-set writes refused on a stale read
  - the sequence moving once per change (*superseded by decision 21*)
  - the consistent read
  - notifications reaching every subscriber
  - publishes and batches racing without refusing each other
- [x] **Two cores over one memory store** (`STEP-4-core.md`, five runs and the
  contended run):
  - concurrent batches from many instances counted exactly once
  - totals from either core ordered by one sequence (*superseded by decision 21: each
    stream ordered by read issue*)
  - a publish on one core reaching streams on the other
  - a publish racing batches, neither refused
  - an edited limit value keeping its window
  - two sweeps and a sweep racing a status
  - a resend of one batch to both cores counted once
- [x] **Gateway** (`STEP-5-gateway.md`; *superseded by decisions 18–22: totals carry
  no revision or epoch, and apply by scope and type in stream order*):
  - totals applied by sequence within an epoch
  - another epoch's totals not applied (decision 10 as changed in step 1)
  - an older sequence ignored whichever core it came from
- [x] **Cross-half e2e** (`STEP-6-e2e-and-docs.md`, `gateway/e2e/replicas_test.go`):
  two gateways on two sample cores over one store. Usage from both counts once in
  totals both cores serve; a config published through one core reaches both; a gateway
  moved to the other core resumes without a resync while the first is undisturbed.
  *Phase 5 adds: a gateway that rejected a config dropping a budget still enforces its
  spend.*
- [x] **`scripts/check-all.sh` green at each phase end** (steps 5 and 6).
- [x] **Phase 5** (`STEP-14-docs-and-green.md`, done 2026-10-07): every round-2 reproduction ported and
  passing; the removal checklist greps clean; `scripts/check-all.sh` green three
  times in a row.
- [ ] **Phase 6** (`STEP-17-docs-and-green.md`): every round-3 reproduction ported and
  passing; `scripts/check-all.sh` green three times in a row.
