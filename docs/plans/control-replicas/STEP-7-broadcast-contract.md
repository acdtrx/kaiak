# Step 7 — broadcast-only contract

**Status:** not started

## Intent

The app owns its config: it composes the parts, keeps their history, and decides who
may change what and when. `kaiak-control` only validates the document the app gives
it and broadcasts the current one to the gateways, with the usage totals.

This step writes that contract: specs, schemas, fixtures and the store interface.
Steps 8–9 implement it on each half.

**This phase is a removal.** Within the phase, tests are expected to fail between
steps. Nothing is kept, renamed, aliased or left as a compatibility path to keep a
test green. A test that asserts removed behaviour is deleted, not adapted to keep the
behaviour alive.

## The new contract

- **Config:**
  - The control plane holds one **current config** and its **`config_hash`**: the
    lowercase hex SHA-256 of the config's JSON exactly as the control plane sends it.
    The hash identifies content; it carries no order.
  - Publishing **replaces** the current config. There are no versions, no history, no
    epoch.
  - The parents rule compares against the current config. A publish is stored only if
    the config it was checked against is still current; that condition is internal to
    the store, invisible on the wire.
- **Stream** (`GET /v1/stream`, no query parameters):
  - On connect, the first event is `config` with the current config and its hash,
    then `totals`.
  - After that, every change of the current config sends a `config` event, and totals
    are pushed on change (at most once per interval, as today).
  - The gateway applies every `config` event it receives (the control plane is
    authoritative). It skips one whose hash equals the config it already runs.
- **Ordering is the sender's job, per stream.** A core never sends on a stream a
  config or totals read before something it already sent there. It uses the store's
  sequence for this, internally.
- **Rollback.** When the store's sequence goes backwards under a running core (a
  restore, an asynchronous-standby failover), the core closes all its streams;
  gateways reconnect and take the current state. No store has to take a "new epoch"
  for this.
- **Totals:**
  - `{ live_gateways, counted_through, windows }`.
  - No `revision`, `config_epoch` or `config_version`.
  - Sent only on the stream.
  - A gateway applies the windows to the limits it runs, matched by (scope, type),
    whatever config it runs. A limit it doesn't run is ignored. There is no "totals
    for another config" state.
- **Usage ack:** `{ batch }` only. It acknowledges the batch so the gateway drops it
  from its spool. The gateway's own usage leaves its counters only through a stream
  totals message whose `counted_through` covers it.
- **Status:**
  - `applied_config_hash` (null before a control-plane config is applied).
  - `last_rejection` names the rejected config's `config_hash`.
  - No version or epoch fields.
- **Outage rule:** priced money-limited models fail closed after the grace without
  contact, and before the first totals since start, as today. The config-mismatch
  refusal and its gauge go.
- **`GET /v1/config`** (the snapshot endpoint) goes. The stream's first event is the
  config. Boot readiness waits for the first `config` and `totals` on the stream.
- **Data directory:**
  - `last-known-good.json` holds the config and its hash.
  - The totals cache is not keyed by config.
  - Both formats bump.
- **Store** (`kaiak-control`):
  - `currentConfig()` → `{ config, hash, sequence }`.
  - `publishConfig(entry, expectedHash)` replaces the config conditionally and moves
    the sequence.
  - **Removed:** `configEpoch()`, `configsAfter()`, the history `keep`, and the epoch
    in snapshots.
  - The totals snapshot keeps the sequence (internal) and drops the epoch and the
    config version.
- **The app/library split** is stated in `CONTROL-PROTOCOL.md` and the GUIDE.
  Composing config parts, history, audit and concurrent editing are the app's.

## Removal checklist (every item gone by the end of step 9)

Grep the whole repo outside `docs/plans/` and `docs/reviews/`. Each must return
nothing, except where the right column allows it:

| Must be gone | Allowed exception |
|---|---|
| `config_version`, `configVersion`, `ConfigVersion` | none |
| `config_epoch`, `configEpoch`, `ConfigEpoch`; `applied_config_epoch` | `epoch` inside a batch ID (the gateway's spool epoch) is unrelated and stays |
| `applied_config_version` | none |
| `revision` on totals: the field, the type, ordering code | the gateway-record revision (the store's conditional write on a gateway record) is a different thing and stays |
| `configsAfter`, `configsSince`, `configHistorySize`, `historySize` | none |
| `resync`, as an event, outcome or code | none |
| `since` as a stream parameter | none |
| `config-unavailable` | only if still used for "nothing published yet"; decide in step 7 |
| `GET /v1/config` and its handler, schema (`config-snapshot.schema.json`) and fixtures | none |
| `kaiak_control_config_mismatch`, `ConfigMismatch`, mismatch timers and log lines | none |
| totals on acks: `ack.totals`, ack-driven totals application in the gateway | none |
| the "a restored store takes a new epoch" rule | none |

**No renames standing in for removed concepts.** `config_hash` must be a content hash,
used only for skip-if-identical and status, never compared for order.

## Files likely touched

- Specs:
  - `docs/specs/CONTROL-PROTOCOL.md`: Endpoints, Config versions (the section becomes
    "Current config"), Config stream, Messages (totals, usage ack, status), Totals,
    Control-plane processes, Control-plane outage, Usage intake.
  - `docs/specs/GATEWAY.md`: Configuration sources (control-plane mode, boot,
    readiness, last-known-good), Limits (control-plane mode), Observability (metrics
    and log tables), Data directory.
- `protocol/schema/`: totals, usage-ack, status; `config-snapshot.schema.json` deleted;
  a config event schema if one exists; `common.schema.json`'s version and epoch
  definitions. Then `npm run sync-schemas`.
- `protocol/fixtures/**`: by scripted transform plus deliberate deletions, listed in
  Result.
- `control/kaiak-control/src/storage/types.ts`, `memory.ts`, `src/store-contract/`:
  the store interface above, with contract tests for the conditional replace, the
  sequence, and the snapshot without epoch.

## Decisions made during planning (confirm in review)

- **The stream is the one config channel.** `GET /v1/config` is removed rather than
  kept beside it. Two ways to get the config would be two paths to keep consistent.
- **`config_hash` is computed by the control plane** over the bytes it sends. The
  gateway takes it from the message and never recomputes it, so no canonical-JSON rule
  is needed.
- **Decisions this phase supersedes**, each marked in the OVERVIEW with a pointer to
  decision 18:
  - decision 10: the totals revision;
  - decision 13: a new epoch on rollback;
  - step 1's epoch rule;
  - the protocol's "Config versions (settled 2026-09-24)" and "Config epoch (settled
    2026-09-25)".

  The replacement text in the specs names what was rejected and why: ordering
  belongs to the sender; "only newer" at the gateway traps it after a restore.

## Acceptance criteria

- Specs, schemas and fixtures state the contract above, dated 2026-10-07.
- The store interface and contract tests are updated, and the memory store passes
  them.
- The removal checklist is written into this file's Result, with each item marked
  done (spec, schema, store) or left for step 8 or 9.
- Suite run and recorded. Expected reds: the core (step 8), the gateway (step 9), the
  cross-half tests (step 9). No red is avoided by keeping a removed thing.

## Result

(filled in when the step is done)
