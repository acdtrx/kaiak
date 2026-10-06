# Step 1 — contract

**Status:** done (2026-10-06)

## Intent

Rewrite the contract for several control-plane processes over one store: what the
store guarantees, how totals are ordered, what the gateway does with them.

## Files likely touched

- `docs/specs/CONTROL-PROTOCOL.md`:
  - **Control-plane processes:** replaced. Any number of processes over one store
    whose implementation holds the store contract. The guarantees are listed (the
    sequence, conditional writes, consistent read, notification), dated 2026-10-06,
    with what was rejected (the lease, a sweep leader, keeping `control_plane`).
  - **Messages → Totals:** `revision` is the store's sequence. The ordering rule
    is "higher sequence within the same `config_epoch`; a different epoch is adopted".
    The paragraphs on replaced processes and the 16 remembered IDs go. The
    consistent-snapshot paragraph is restated as a store guarantee.
  - **Usage intake, Budgets → Model-set edits:** the conditional publish replaces
    "runs in the totals' turn".
  - **Gateway status:** the sweep runs on every process; its writes are conditional.
  - Protocol version stays 5 (released with messages-responses); note it in the
    plan's Result if that plan has not landed when this step starts.
- `docs/specs/GATEWAY.md`: Limits → Control-plane mode, the totals ordering as the
  gateway applies it.
- `protocol/schema/totals.schema.json` (and the ack's embedded totals): `revision` an
  integer ≥ 0. Then `npm run sync-schemas`.
- `protocol/fixtures/**`: every totals and ack fixture's `revision` changes to the
  integer, by a scripted transform; a new invalid fixture, the old object form.

## Decisions made during planning

- The store contract's details (method names, signatures) are the library's and live
  in `control/kaiak-control/src/storage/types.ts` and the GUIDE, not in
  `CONTROL-PROTOCOL.md`. The spec states the guarantees a control plane gives
  gateways, and that `kaiak-control` holds them through its store contract.

## Acceptance criteria

- The specs state the new ordering, the guarantees and the rejected options, dated.
- Schema copy in sync. The fixture diff is exactly the transform plus the new invalid
  fixture.
- Suite run and recorded. Expected reds: the totals fixture and version tests in
  both halves (steps 3 and 4), and the cross-half e2e (step 4).

## Result

**What changed**

- `docs/specs/CONTROL-PROTOCOL.md`:
  - **Control-plane processes** replaced (settled 2026-10-06):
    - Any number of processes over one store, and gateways may reach any of them.
    - The store's guarantees: one totals sequence moved in the same write as each
      change; conditional writes for batches, publishes, statuses and expiries;
      consistent totals reads; change notification to every process.
    - A store that loses or rolls back its state takes a new config epoch.
    - Processes start and stop freely, and every process sweeps.
    - Rejected: the lease (one process per store), a sweep leader, a revision per
      process, and the core polling the store.
  - **Messages → Totals:**
    - `revision` is the store's totals sequence, an integer.
    - The gateway applies totals only of the config epoch it runs, with a revision
      higher than the last applied in that epoch (the first in an epoch whatever its
      revision). Totals of another epoch are not applied, and what they show counted
      still counts.
    - Replaced-process text removed; the old form named as rejected.
    - The consistent snapshot is restated as a store guarantee.
    - The message table shows `revision` plain.
  - **Config versions:** "One order across processes". A publish is stored only as
    the next version after the one it was checked against; a loser is checked again
    (parents rule included).
  - **Config stream:** versions and totals pushes come from whichever process made
    them.
  - **Usage intake:**
    - Per-instance serialization is per process; the store's conditional write
      decides across processes.
    - The atomic count also moves the totals sequence.
    - New bullet "Counted under the config in force": the write is conditional on
      the config version.
  - **Budgets → Model-set edits:**
    - "The totals' turn" is replaced by the guarantee.
    - The carry and the version are one store write, conditional on the latest
      version and the totals sequence. That write also moves the sequence.
    - Rejected: two writes.
  - **Status intake:**
    - Status writes are conditional on the record judged against.
    - The live set is the same for every process; joining or leaving moves the
      sequence.
    - Every process sweeps, with conditional expiry and forgetting.
- `docs/specs/GATEWAY.md`:
  - The stream follower applies totals of the running config's epoch with a higher
    revision.
  - "Totals follow their config": a message of another epoch does not wait.
  - Log table: the `kaiak.totals.control_plane` / `current_…` / `previous_…` row is
    replaced by `kaiak.totals.config_epoch`, for the line
    `totals ignored: from another config epoch`. The `kaiak.config.epoch` row names
    that line too.
- `protocol/schema/totals.schema.json`:
  - `revision` is `common.schema.json#/$defs/count`, an integer from 0 up to
    2^53 − 1.
  - The `revision` object definition is removed.
  - Descriptions updated.
  - Synced to `control/kaiak-control/schema/` (`npm run sync-schemas`;
    byte-identical).
- Fixtures:
  - By a one-off script outside the repo, every totals and ack fixture's
    `revision: {control_plane, sequence}` becomes its sequence (28 JSON fixtures).
  - The same, by a text transform, for the two raw `duplicate-members` fixtures.
  - A second script checked every changed fixture equals its `HEAD` version under
    exactly that transform. It also checked that no `control_plane` is left except
    the deliberate invalid case.
  - `totals/invalid/revision-sequence-negative.json` → `revision-negative.json`
    (`revision: -1`).
  - `totals/invalid/revision-control-plane-uppercase.json` → `revision-object.json`:
    the old object form, now invalid. Its case reason is "the revision is the
    store's totals sequence, an integer — not a per-process object".
  - `cases.json` updated.

**Planning decisions to review**

1. **Decision 10's ordering rule is narrowed.** The OVERVIEW says "the epoch differs
   from the last applied → adopt". The spec instead applies totals only when their
   `config_epoch` is the epoch of the config the gateway runs:
   - Totals of another epoch could not be used anyway; windows follow their config.
   - Adopting them would let a delayed answer from a store that started over replace
     the ordering. That is the case the removed list of replaced processes handled.
   - The new store's totals arrive again on the stream that opens after its config is
     applied.
   - Small consequence: such a message's live-gateway count no longer applies at once.
     It arrives again with the next message.
2. **New guarantee:** a store that loses or rolls back its state (a restore from
   backup) takes a new config epoch. Otherwise its sequence would go back under the
   same epoch and gateways would ignore its totals until it passed the old value.
   The epoch already had this meaning for versions.
3. **Conditional status writes cover every status, not just live-set changes.** The
   conflict rule (two processes under one instance name) reads the previous record,
   so it needs the same guard across processes.

**Suite** (2026-10-06). The worktree needed `npm ci --ignore-scripts` in `control/`
(fresh worktree; lockfile unchanged).

- `scripts/check-all.sh`: exit 1, stopped at the gateway's race tests.
  - gofmt, vet and staticcheck pass.
  - `kaiak/internal/control` fails `TestTotalsEventsReachTheConsumer`,
    `TestValidMessageFixtures`, `TestInvalidMessageFixtures`,
    `TestValidMessageFixturesRoundTrip` and `TestTotalsAmountBeyondSafeInteger`. The
    gateway still decodes and validates the object revision. **Expected; cleared by
    step 4.**
  - Every other gateway package passes.
- control `npm test`: 545 pass, 32 fail. All 32 are tests that produce or validate
  totals: usage, fastify totals pushes and acks, protocol and messages fixtures. The
  core still emits the object revision. **Expected; cleared by step 3.**
- control `npm run lint`: passes.
- Cross-half e2e, run on its own: **passes** (`ok kaiak/e2e 58.979s`). Both halves
  still speak the object form at run time, and neither validates the other's totals
  against the new schema on that path. **Expected red from step 3**, when
  `kaiak-control` sends the integer and the gateway still expects the object, **until
  step 4.**
