# Step 11 — round-2 contract

**Status:** done (2026-10-07)

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

**Commits:** `ee0792f` (store interface, memory store, contract tests, schema and
fixtures), `ab0b11c` (specs, GUIDE §5 and §11), and this Result (with two leftover
lines in `GATEWAY.md` and `config.schema.json`).

**What changed**

- **`CONTROL-PROTOCOL.md`** (dated 2026-10-07 throughout):
  - Current config: the store keeps the config's JSON text; the hash is over it.
    Rejected: storing a JSON value and writing it out per stream (`jsonb` reorders
    members).
  - Config stream:
    - **Order** by when each read was issued, configs and totals apart. Rejected:
      ordering by a store sequence.
    - **Rollback** replaced by **A restored store is the current state**. Rejected:
      rollback detection; a config epoch.
    - Totals: complete on connect, then only the changed windows; a publish changes no
      totals. Rejected: the complete totals on every push.
    - Slow readers: the held push carries every window changed since the last totals
      sent.
    - The stream ends when a config cannot be read; rollback is gone from the list.
  - Messages → Totals:
    - `counted_through` is a per-epoch list (rejected: one cursor of any epoch, which
      after a late old-epoch write names that epoch for good);
    - the consistent snapshot covers the windows and every cursor, and a changes-only
      message lists the difference between two snapshots;
    - `windows` covers every scope and type with usage (rejected: only the current
      config's limits).
  - Matching totals to limits: a count per scope and type, limits checked over it.
    The first totals on a stream set every base; later ones replace those they list.
    Rejected: keeping the last base of a window no longer listed.
  - Status: `last_rejection` is also cleared when the config received is the running
    one.
  - New message rule: `counted-through-epoch-duplicate`.
  - Usage intake:
    - `counted_through` lists each epoch's last batch;
    - counting moves no sequence;
    - totals list every window;
    - narration in the touched Rejected lines removed.
  - Budgets: complete on connect, then changes.
  - Status intake:
    - the live count is read apart from the snapshot;
    - the two-processes paragraph no longer claims a shared de-duplication slot. With
      per-epoch cursors there is none; it now names the shared live-set place.
  - Control-plane processes:
    - the One sequence guarantee is removed;
    - consistent reads cover the windows and cursors;
    - new: the config as text, catch-up visibility, "No order of its own";
    - Rejected: a store-wide sequence.
  - Outage: contact is stream bytes only. Acks are not contact; the reason is given.
- **`GATEWAY.md`:**
  - Limits:
    - new **Every scope is counted; limits apply on top** (rejected: a count per
      configured limit);
    - Config reload: a reload changes no hour or month count;
    - `limits.json` holds every scope's windows and restores the current ones, still
      format 3, since the shape is unchanged;
    - counts are `base + own`, matched by group and type;
    - retirement by each epoch's `counted_through` entry;
    - Totals apply whatever the config: the first totals after a connect replace every
      base, later ones only those listed;
    - `totals.json` holds every scope, still format 4;
    - Outage refusal: stream bytes only.
  - Control-plane mode → Stream: the running config's hash clears a set rejection and
    reports status. The H11 reason no longer mentions acks.
  - Metric `kaiak_control_last_contact_timestamp_seconds`: stream bytes only.
  - Client API table (`budget_unavailable`): the mismatch clause is removed.
- **Store interface** (`storage/types.ts`, `memory.ts`):
  - `ConfigEntry` is `{ text, hash, publishedAt }`. It holds no document object: the
    store keeps and returns the text unchanged.
  - `CurrentConfig` is removed; `currentConfig()` returns a `ConfigEntry`.
  - No sequence anywhere:
    - write results are `{ saved: true }`, `{ saved: true, revision }`, or
      `{ saved: false, … }`;
    - `StoreChange` has no `sequence`, and catch-up is `{ type: "catch-up" }`.
  - `totalsSnapshot(current)` → `{ windows, cursors }`, every instance's cursor of
    each kept epoch from one snapshot; the `instance` parameter, `config`, `last` and
    `liveGateways` are gone.
  - `BatchCursors.latest` is the one counted last by `countedAt`; it is used only for
    the intake outcome and its log line.
  - The `subscribe()` comment states that reads after a catch-up see what the
    channel missed.
- **Contract tests** (`store-contract/`):
  - `StoreContractSubject.reconnect(handle, whileDown)`: the hook takes the channel
    down, runs the writes, and brings it back.
  - New `lossy-channel.ts`: a memory store handle whose channel drops changes while
    down and announces a catch-up. `lossy-channel.test.ts` runs the whole suite with
    `reconnect`, and the catch-up test runs.
  - The catch-up test:
    - uses two subscribers on the reconnecting handle;
    - writes a publish and a batch while the channel is down;
    - repeats the reconnect twice;
    - checks reads after the catch-up;
    - checks changes are heard again afterwards.
  - Negative controls (`negative-control.test.ts`, three child runs): the torn store
    (cursors read apart from the windows), `silent-reconnect-store.ts` (no catch-up)
    and `one-subscriber-catch-up-store.ts`. Each fails the suite at the test naming
    its guarantee.
  - New tests:
    - the config text comes back exactly as published (out-of-order members, nested,
      escapes, unicode);
    - a snapshot over 8 instances × 2 epochs written concurrently, with each
      (instance, epoch)'s window equal to its cursor in every snapshot;
    - cursors of each epoch in the snapshot, the late old-epoch one included;
    - an empty snapshot.
  - Revisions are asserted to differ and increase, not to take exact values (2L4).
  - **Deleted** (each asserted removed behaviour):
    - "moves by one with every change to the totals, and only then" (the sequence);
    - "a snapshot counts the live set" (the live count left the snapshot);
    - "the stored config shares no state with what callers hold" (the config is a
      string now);
    - the sequence and `snapshot.config` assertions in the publish, batch, forget
      and notification tests.
- **Schemas and fixtures** (`protocol/`, synced):
  - `totals.schema.json`: `counted_through` is an array of `{ epoch, sequence }`, and
    the descriptions cover every scope, complete then changes.
  - `config.schema.json`: "config event" replaces "config snapshot", and the outage
    grace description loses the snapshot, the ack and the mismatch.
  - Totals fixtures:
    - every `counted_through` moved to the list shape;
    - new valid `two-epochs.json` and `changes-only.json`;
    - new invalid `counted-through-object`, `-null`, `-epoch-missing` and
      `-epoch-duplicate` (semantic);
    - `-instance` and `-sequence-zero` in list form;
    - the raw duplicate-member totals fixture follows.
  - `config-event/valid/full.json`: the correct `config_hash` (2L9).
  - `messages.test.ts`: every valid config event's hash is checked against SHA-256 of
    `JSON.stringify(config)`.
- **GUIDE:**
  - §2: no store sequence.
  - §5 rewritten:
    - conditional writes, one snapshot, catch-up, no order of its own;
    - the table without sequences;
    - config stored as `text`.
  - The Postgres sketch:
    - a `current_config` single-row table (insert-on-conflict for the first publish,
      so no seed row is needed);
    - `create sequence gateway_revision` replacing the counter row;
    - `select … for update` replacing `returning <old live>`;
    - windows added in key order, which replaces the `store_meta` lock order;
    - the snapshot's two selects in one `REPEATABLE READ`;
    - catch-up announced after `LISTEN` is back;
    - a restored store is the current state.
  - §11: the `reconnect(handle, whileDown)` hook, the lossy channel and the three
    broken stores.

**Decisions made in this step**

- **The store keeps only the config text, not the document.** Two representations of
  one config could disagree. The core parses the text where it needs the document (the
  parents rule, the sample page) — step 12.
- **The live count needs no store method.** The core already counts it from
  `gateways()`.
- **`BatchCursors.latest` stays, defined by `countedAt`.** Without a sequence it
  needs an order. It only tells a fresh epoch from a first batch, and names the
  previous batch in the intake log.
- **`counted-through-epoch-duplicate` is a message rule**, since JSON Schema cannot
  say "unique by epoch". It is listed in the spec's Message rules. kaiak-control
  (`messages/semantic.ts`, step 12) and the gateway (step 13) implement it.
- **No format bump for `limits.json` or `totals.json`.** Their entries already name
  (group, type); they now hold more entries, of the same shape.
- **Postgres sketch:** no `store_meta`. Revisions come from a Postgres sequence (gaps
  are allowed by the contract), and the config row is inserted by the first publish.
  This dissolves the seed-row gap (2L1).

**Removal checklist** (greps over the repo outside `docs/plans/`, `docs/reviews/`;
"code" means `control/kaiak-control/src` or the sample unless named):

| Pattern | Spec / schema / store / contract | Left for |
|---|---|---|
| store sequence (`CurrentConfig`, `TotalsSnapshot`, `StoreChange`, write results), `highestSequence`, `observeSequence` | done: none in `storage/`, `store-contract/`, specs, GUIDE §5 | core: config-publishing, control-plane, usage, fastify and their tests (12); sample page (12) |
| `onRollback`, `rollback`, `Rollback` | done in specs (one dated Rejected line), GUIDE §5 | core and GUIDE §4 line 142 (12); `docs/ARCHITECTURE.md:348`, `docs/architecture/control-plane.html:278` (14) |
| `lastSent` by sequence; core-wide `delivered` | — | `fastify/gateway-stream.ts` (12); `delivered` greps clean already (renamed earlier) — 12 checks the dedup is per stream |
| `limitedOf`, `listedWindows`' config filter, "counted but not listed", "the config's limits" | done in specs (one Rejected line) | `usage/index.ts`, `usage.test.ts`, `status-totals.test.ts` (12); `docs/ARCHITECTURE.md:319` (14). The hits for "whatever the config's limits" (GUIDE §5, §7; `types.ts`) state the current rule and stay |
| `counted_through` as one object or `null` | done: schema, fixtures (the one hit is the invalid fixture `counted-through-null.json`) | `messages/types.ts`, `usage`, `status-totals.test.ts`, sample `page.test.ts` (12); gateway `messages.go`, schema walker, limits, fakecontrol (13) |
| the snapshot's `config` and `liveGateways` | done in the store | `usage/index.ts` (12). `liveGateways` in `gateways/` is the core's own count and stays |
| acks as contact (`touch()` on an ack) | done in specs | `gateway/internal/control/usage.go` (13) |
| `JSON.stringify` of the config at send time | done in the store (text) | `config-publishing`, `gateway-stream.ts` (12) |
| `config-versions`, `config snapshot`, `snapshot fetch`, `acknowledged batch`, `for another config`, `resumes from`, `kaiak.config.version` | done: `config.schema.json`, `GATEWAY.md:175` | `gateway-stream.ts:65` (12); `ARCHITECTURE.md:360` (14); gateway `main.go`, `messages.go`, `usage.go`, `limits.go`, `server/limits.go`, `loader_test.go`, `fixtures_test.go`, `totals_test.go`, `usage_test.go`, `limits_test.go` (13). Allowed: "config snapshot" meaning the gateway's in-memory config of a request (`GATEWAY.md:1404, 1711`, `accounting.go`, `routing.go`, `server/api.go`, `config/limits.ts`); "acknowledged batch" for the batch an ack names (`CONTROL-PROTOCOL.md:283`, `kaiak_usage_last_ack_timestamp_seconds`) |
| BACKLOG "Totals size bound" | — | 14 |

**Suite** (2026-10-07):

- Store contract on its own (`node --test src/store-contract/*.test.ts`): 53 tests,
  52 pass, 1 skipped (the memory store's catch-up: its channel cannot drop a change).
  The lossy-channel run passes the catch-up test, and the three negative controls
  each fail as expected.
- Control `npm test`: 598 tests, 528 pass, 51 fail, 18 cancelled, 1 skipped.
  **Expected, cleared by step 12.** Every failure is in the core or the sample, which
  still use the removed store API (`sequence`, `CurrentConfig`, the snapshot's
  `config`/`last`/`liveGateways`, the config object):
  - `usage.test.ts` 28;
  - `fastify.test.ts` 13;
  - `status-totals.test.ts` 10;
  - `control-plane.test.ts` 9;
  - `config-publishing.test.ts` 5;
  - `usage-route.test.ts` 3;
  - `review.test.ts` 1;
  - sample `app.test.ts` 2;
  - `messages.test.ts` 1: the new `counted-through-epoch-duplicate` fixture, whose
    rule `messages/semantic.ts` gets in step 12.

  The 18 cancelled are the fastify stream tests, whose streams never send with the
  core broken. No `node --test` process left.
- Control `npm run lint`: 44 TypeScript errors, all in the core and sample:
  - `usage/index.ts` 10;
  - `fastify.test.ts` 8;
  - `control-plane.test.ts` 8;
  - `config-publishing/index.ts` 4;
  - `sample/src/page/sections.ts` 4;
  - `control-plane/index.ts` 3;
  - 2 each in `review.test.ts`, `usage.test.ts` and sample `page.test.ts`;
  - 1 each in `config-publishing.test.ts` and `gateway-stream.ts`.

  None in `storage/` or `store-contract/`. **Expected, cleared by step 12.**
- `scripts/check-gateway.sh`:
  - gofmt, vet and staticcheck pass;
  - every package passes, the gateway e2e included (110.8 s, since its fake control
    plane still speaks the old totals);
  - except `kaiak/internal/control`: `TestValidMessageFixtures`,
    `TestValidMessageFixturesRoundTrip` and `TestTotalsAmountBeyondSafeInteger`
    (`counted_through` is a list now); `TestInvalidMessageFixtures` (the
    `counted-through-*` cases and the semantic totals cases, which no longer decode);
    `TestTotalsEventsReachTheConsumer`.
  - **Expected, cleared by step 13.**
- Cross-half tests: not run (they need both halves on the new protocol). **Expected
  red until step 13.**

