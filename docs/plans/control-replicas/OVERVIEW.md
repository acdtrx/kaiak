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
  - the totals sequence that orders totals messages
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
  list of replaced control-plane processes go.
- **Protocol:** the totals `revision` changes meaning and shape.
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

4. **One totals sequence in the store**, an integer that grows by one with every
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
6. **A consistent totals read is a store operation.** It returns the sequence, the
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
10. **`revision` becomes the sequence integer**, ordered within the message's
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

13. **A store that loses or rolls back its state takes a new config epoch** (a restore
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
16. **Usage counts toward every scope on a record's path, whatever the config's
    limits.** The totals read lists only the windows the latest config limits. A limit
    added mid-window starts with that window's usage so far.
17. **Provider budgets** (money limits on a set of backends, such as one provider
    account across regions) are a separate plan after this release, pending the
    user's research. They get a backlog entry in step 3.

Steps 3–5 of the first plan became 4–6 when step 3 was inserted (2026-10-06). Steps
1–2's Results name the old numbers.

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
  pushes until the next change. Acks still carry fresh totals.
  - Mitigation: the contract tests check notification across two cores.

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

Expected reds inside phase 1:
- After step 1, both halves fail the totals fixtures. Step 4 clears `kaiak-control`'s
  and step 5 the gateway's.
- The cross-half e2e stays red until step 5.

## Verification

**Verification status:** done (2026-10-06); phases 1 and 2 green.

- [x] **Store contract tests** against the memory store (`STEP-2-store-contract.md`,
  `STEP-3-simple-limits.md`):
  - conditional batch, publish and live-set writes refused on a stale read
  - the sequence moving once per change
  - the consistent read
  - notifications reaching every subscriber
  - publishes and batches racing without refusing each other
- [x] **Two cores over one memory store** (`STEP-4-core.md`, five runs and the
  contended run):
  - concurrent batches from many instances counted exactly once
  - totals from either core ordered by one sequence
  - a publish on one core reaching streams on the other
  - a publish racing batches, neither refused
  - an edited limit value keeping its window
  - two sweeps and a sweep racing a status
  - a resend of one batch to both cores counted once
- [x] **Gateway** (`STEP-5-gateway.md`):
  - totals applied by sequence within an epoch
  - another epoch's totals not applied (decision 10 as changed in step 1)
  - an older sequence ignored whichever core it came from
- [x] **Cross-half e2e** (`STEP-6-e2e-and-docs.md`, `gateway/e2e/replicas_test.go`):
  two gateways on two sample cores over one store. Usage from both counts once in
  totals both cores serve; a config published through one core reaches both; a gateway
  moved to the other core resumes without a resync while the first is undisturbed.
- [x] **`scripts/check-all.sh` green at each phase end** (steps 5 and 6).
