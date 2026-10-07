# Step 11 — round-2 contract

**Status:** not started

## Intent

Write the contract for decisions 21–27 (`OVERVIEW.md`): specs, schemas, fixtures, the
store interface, the memory store and the exported store contract tests, and the
GUIDE's store section and Postgres sketch. Steps 12 and 13 implement it on each half.

**Phase 5 is a removal and a protocol change.** Tests are expected to fail between
steps. Nothing is kept, renamed, aliased or left as a compatibility path to keep a
test green. A test that asserts removed behaviour is deleted, not adapted.

## The contract

- **No store sequence** (decision 21).
  - Writes, snapshots, notifications and the current config carry none.
  - Spec, Config stream → Order: each process sends on a stream what it read last,
    by when it issued each read. Configs are ordered among configs and totals among
    totals; the two kinds no longer depend on each other.
  - The Rollback rule is removed. Replace it with: "a store restored to earlier state
    is the current state; its config and totals are sent like any other". Keep a
    dated Rejected line naming rollback detection and why it fails.
- **Totals** (decisions 22, 23):
  - `windows` lists every (scope, type) window with usage, whatever the config.
  - The first totals on a stream are complete. Later ones list only the windows that
    changed since that stream's last push; a window not listed keeps its value.
  - `counted_through` is an array of `{ epoch, sequence }`: the recipient instance's
    last counted batch for each epoch still kept. An empty array means none.
  - Spec text that says totals are built from, filtered by, or complete for the
    current config goes.
- **Outage contact** (decision 25): stream bytes only. Update `CONTROL-PROTOCOL.md`
  (What counts as an outage) and `GATEWAY.md` (Outage refusal).
- **Status** (2M10): `last_rejection` is cleared when a later config is applied, *or
  when the config received is the one running*.
- **Store interface** (`storage/types.ts`, `memory.ts`):
  - `sequence` removed from `CurrentConfig`, every write result, `TotalsSnapshot` and
    `StoreChange` (catch-up included).
  - `totalsSnapshot(current)` returns the windows and every instance's cursors still
    kept, read consistently (decision 26). The config and the live count leave it;
    the live count gets its own read.
  - `ConfigEntry` carries the config's JSON **text** and its hash (decision 27). The
    store keeps the text and returns it unchanged.
  - Every comment on the interface states the new guarantees.
- **Store contract tests** (`store-contract/`):
  - **Catch-up (2M7):** add an in-repo lossy channel over the memory store that drops
    changes while it is down and announces `catch-up` on reconnect. Run the full suite
    with `reconnect` against it.
    - Write while the channel is down, then check that the catch-up reaches **every**
      subscriber.
    - Negative controls: a store that reconnects with no catch-up, and one that tells
      only one subscriber. Each must fail the suite.
  - **Snapshot under concurrency:** the windows and the cursors, including several
    instances and epochs. Keep the torn-store negative control.
  - **Revisions (2L4):** assert only that they differ and increase.
  - **Config text:** a multi-member config is returned byte for byte, so its hash
    still matches after a round trip.
  - Delete the tests of the sequence and of the config or live count in the snapshot.
- **GUIDE** (§5 store, Postgres sketch):
  - no sequence;
  - the config as `text`;
  - a `store_meta` seed row (if the table still exists);
  - a `for update` read instead of `returning <old live>`;
  - catch-up with no sequence;
  - the snapshot as windows plus cursors.
  - Drop any lock-order rule that existed only for the sequence row.
- **Schemas and fixtures** (`protocol/`, then `npm run sync-schemas`):
  - `totals`: `counted_through` as an array of `{ epoch, sequence }`, with valid and
    invalid fixtures (duplicate epoch, missing member, the old object shape). Use
    totals fixtures for both the complete case and the changes-only case (the
    schema does not distinguish them; the spec does).
  - `config-event/valid/full.json`: correct `config_hash` (2L9).
  - Add a fixture test on the control side: every valid config event's `config_hash`
    is the SHA-256 of the config's JSON text as written in the fixture.

## Removal checklist (gone by step 14)

Grep the whole repo outside `docs/plans/` and `docs/reviews/`. A pattern returns
nothing, or only the exception the right column allows. The patterns include the
phrasings round 2 found slipping past the earlier greps (2M8).

| Pattern | Allowed exception |
|---|---|
| store sequence: `sequence` on `CurrentConfig`, `TotalsSnapshot`, `StoreChange`, write results; `highestSequence`, `observeSequence` | a batch's `sequence` (batch ID, cursors, `counted_through` entries) |
| `onRollback`, `rollback`, `Rollback` | dated Rejected lines in `docs/specs/` |
| per-stream `lastSent` by sequence; the core-wide `delivered` hash | none |
| `limitedOf`, `listedWindows`'s config filter; "counted but not listed"; "only the windows the … config limits" | none |
| `counted_through` as one object or `null` | none |
| the snapshot's `config` and `liveGateways` | none |
| acks as contact (`touch()` on an ack feeding the outage rule) | an ack metric, if kept, says it is not contact |
| `JSON.stringify` of the config at send time | the publish, which produces the text once |
| `config-versions`, `config snapshot`, `snapshot fetch`, `acknowledged batch`, `for another config`, `resumes from`, `kaiak.config.version` | dated Rejected lines |
| BACKLOG "Totals size bound" | none (resolved by decision 22) |

## Files likely touched

- `docs/specs/CONTROL-PROTOCOL.md`: Config stream (Order, Rollback), Messages (totals,
  status), Totals, Budgets, Control-plane processes, Control-plane outage, Usage
  intake.
- `docs/specs/GATEWAY.md`: Limits (control-plane mode, own usage, totals merging,
  every scope counted), Control-plane mode (status, rejection), Outage refusal, Data
  directory (any format bump step 13 needs is stated here).
- `protocol/schema/totals.schema.json`, `protocol/fixtures/messages/totals/**`,
  `protocol/fixtures/messages/config-event/valid/full.json`.
- `control/kaiak-control/src/storage/{types.ts,memory.ts}`, `src/store-contract/**`,
  `GUIDE.md` §5.

## Decisions made during planning (confirm in review)

- **The changes-only rule is in the spec, not the schema.** Both kinds of totals have
  one shape, and the gateway knows which is the first on its stream.
- **A window's value is replaced, never added to.** A listed window carries its full
  `used`, so a repeated or coalesced push is harmless.
- **`counted_through` lists only the recipient's epochs.** At most a few per instance
  within retention, so the message stays small.

## Acceptance criteria

- The specs, schemas and fixtures state the contract above, dated 2026-10-07, with
  the replaced rules named in Rejected lines.
- The store interface and its contract tests are updated:
  - the memory store passes them;
  - the lossy channel passes them with `reconnect`;
  - both new negative controls fail them.
- The removal checklist is copied into Result, each item marked done or left for
  step 12, 13 or 14.
- Suite run and recorded. Expected reds: the core and the sample (step 12), the
  gateway (step 13), the cross-half tests (step 13).

## Result
