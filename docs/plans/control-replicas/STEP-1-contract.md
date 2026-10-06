# Step 1 — contract

**Status:** not started

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

(filled in when the step is done)
