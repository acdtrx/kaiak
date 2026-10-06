# Step 7 — broadcast-only contract

**Status:** done (2026-10-07)

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

**What changed**

- **`docs/specs/CONTROL-PROTOCOL.md`** (`3487e0a`, `a7aed61` for GATEWAY.md):
  - Shape and Endpoints: one stream, no `GET /v1/config`.
  - **Config versions** becomes **Current config** (settled 2026-10-07):
    - the app owns config;
    - publish replaces;
    - `config_hash` (SHA-256 of the config's JSON as sent; content only, no order);
    - the conditional replace inside the store;
    - parents rule against the current config;
    - the gateway applies what it is sent.
  - Rejected: versions with history, resume and resync; "only newer" at the gateway;
    the config epoch.
  - **Config stream:**
    - no parameters;
    - the current config, then totals, on connect;
    - per-stream **Order** by the store's sequence (internal);
    - **Rollback**: a process that sees the sequence go back closes its streams;
    - totals on the stream only.
  - **Messages:**
    - `config-event` `{ config_hash, config }`;
    - totals `{ live_gateways, counted_through, windows }`;
    - ack `{ batch }`;
    - status `applied_config_hash`, `last_rejection { config_hash, codes }`;
    - Matching totals to limits whatever the config (rejected: the H3 config gate and
      its mismatch state).
  - **Usage batches:** the ack names the batch only.
  - **Usage intake:** counted whether or not a config is published.
  - **Budgets:** totals on the stream only.
  - **Control-plane processes:** one internal sequence; publish conditional on the
    config it was checked against; the epoch-on-rollback rule replaced by Rollback.
  - **Outage:** contact without a snapshot; the mismatch refusal removed.
- **`docs/specs/GATEWAY.md`:**
  - **Limits:**
    - own usage leaves only through stream totals;
    - "Totals apply whatever the config" replaces "Totals follow their config" and the
      mismatch rule;
    - `totals.json` format 4 without config identity.
  - **Control-plane mode:**
    - boot from the stream's first `config`, table updated;
    - readiness;
    - no totals yet;
    - stream: skip by hash (running or last rejected), apply whatever it replaces;
    - reconnect;
    - rejections by hash;
    - `last-known-good.json` format 7 (config + hash);
    - usage batches: acks retire nothing.
  - **Status:** `applied_config_hash`.
  - **Metrics:** `kaiak_control_config_mismatch` removed; contact without a snapshot.
  - **Log table:**
    - `kaiak.config.hash` replaces `kaiak.config.version`/`epoch`;
    - `previous_epoch`, `totals.config_epoch` and `totals.sequence` rows removed;
    - boot attempt line renamed.
- **Schemas** (`024eeca`):
  - Deleted: `config-snapshot.schema.json`, `resync.schema.json`.
  - New: `config-event.schema.json`.
  - `common`: `config_epoch` and `config_version` defs removed, `config_hash` added
    (64 lowercase hex).
  - `totals`: `revision`, `config_epoch`, `config_version` removed.
  - `usage-ack`: `totals` removed.
  - `status`: the version/epoch pair and its if/then/else replaced by
    `applied_config_hash`; `last_rejection.config_hash`.
  - Copies synced into `control/kaiak-control/schema/`.
- **Fixtures** (`024eeca`): a scripted transform (`config-snapshot` → `config-event`
  with the real SHA-256 of each config; revision/epoch/version stripped from totals;
  totals stripped from acks; status version/epoch → hash). A second script checked
  all 55 rewritten files equal their `HEAD` version under exactly that transform.
  - Deliberate deletions (each asserted a removed concept):
    - `config-snapshot/invalid/{config-epoch-missing,config-epoch-uppercase,version-missing,version-string,version-zero}`
    - the whole `resync/` kind
    - `totals/invalid/{config-epoch-missing,config-epoch-uppercase,config-version-missing,revision-missing,revision-negative,revision-object}`
    - `usage-ack/invalid/{totals-missing,totals-window-duplicate}`
    - `status/invalid/{applied-epoch-missing,applied-epoch-shape,applied-epoch-without-version,applied-version-missing,applied-version-without-epoch}`
    - `status/valid/rejected-below-applied`
  - New:
    - `config-event/invalid/{config-hash-missing,config-hash-uppercase}`
    - `status/invalid/{applied-config-hash-missing,applied-config-hash-shape}`
  - Duplicate members:
    - `config-snapshot-version-twice` → `config-event-config-hash-twice`
      (path `/config_hash`);
    - `config-snapshot-inner-config-member-twice` →
      `config-event-inner-config-member-twice`;
    - the totals and ack duplicates lost their removed members (by hand; they are raw
      bytes).
  - Every fixture checked against the new schemas with Ajv: valid ones pass, schema
    cases fail, rule and semantic cases pass the schema, and each duplicate file's
    last-occurrence reading is valid.
- **Store** (`58d9872`):
  - `ConfigEntry { config, hash, publishedAt }` and
    `CurrentConfig = ConfigEntry & { sequence }`.
  - `currentConfig()`.
  - `publishConfig(entry, expectedHash)`: a conditional replace, answering
    `{ saved, sequence }` or `{ saved: false, current }`.
  - **Removed:** `configEpoch`, `latestConfig`, `configsAfter`, `keep`, `StoredConfig`.
  - `TotalsSnapshot.config` is the `CurrentConfig`; the sequence is documented as
    internal.
  - `config-published` carries `hash`.
  - The memory store holds one config.
  - Contract tests: the epoch test and the `keep` test removed; publish tests rewritten
    to replace-by-hash (including the same content published again); 21 pass.
  - `storage/memory.test.ts` deleted: both its tests asserted removed behaviour (a
    store's own epoch; version order as a caller fault).

**Decisions made in this step**

- **`config-unavailable` is removed.** A usage batch is counted with no config
  published (counting does not depend on the config, and the ack carries nothing that
  needs one). A stream before the first publish stays open and sends the config when
  it comes. The gateway's boot treats "no config within the wait" as unavailable, as
  before. Its only mention left is the dated rejected alternative in Usage intake.
- **A `config` event is skipped when its hash equals the running config's or the last
  rejected one's.** The latter avoids re-validating and re-logging the same rejected
  config on every reconnect.
- **A publish of identical content is a publish:** it moves the sequence, and streams
  skip it by hash.
- **The publish condition is the hash of the config checked against.** A content
  condition is correct even across an A-B-A of publishes: the parents rule holds for
  identical content.
- **Rejected alternatives in the specs keep naming the removed concepts**
  (`resync`, `config-unavailable`, "config epoch", `revision`), dated. AGENTS.md asks
  decisions to record what was rejected. This is the one exception to step 9's grep,
  limited to `docs/specs/` "Rejected" text.
- **`config_hash` is computed by the core over `JSON.stringify(config)`** (step 8).
  Fixtures use `json.dumps(config, separators=(',', ':'))`, the same text for these
  documents.

**Removal checklist status** (outside `docs/plans/`, `docs/reviews/`):

| Item | Specs | Schemas / fixtures | Store | Left for |
|---|---|---|---|---|
| `config_version` | done | done | done | kaiak-control core and sample (step 8); gateway (step 9); `docs/architecture/control-plane.html` (step 8) |
| `config_epoch`, `applied_config_epoch` | done | done | done | core and sample (8); gateway, e2e (9) |
| `applied_config_version` | done | done | — | core, sample, GUIDE, control-plane.html (8); gateway (9) |
| totals `revision` | done | done | done (sequence internal) | core (8); gateway (9) |
| `configsAfter`, `configsSince`, history size | done | — | done | core, GUIDE (8) |
| `resync` | done, Rejected text only | done | — | core (8); gateway, sse, metrics, fakecontrol, e2e (9) |
| `since` parameter | done | — | — | core, sample, control-plane.html (8); gateway, fakecontrol, e2e (9) |
| `config-unavailable` | done, Rejected text only | — | — | core, sample, GUIDE, control-plane.html (8); gateway, fakecontrol, DEPLOYMENT (9) |
| `GET /v1/config`, `config-snapshot` | done | done | — | core (8); gateway (9) |
| config mismatch | done | — | — | gateway, metrics, limits, cmd, DEPLOYMENT, gateway.html (9); control-plane.html (8) |
| ack totals | done | done | — | core (8); gateway (9) |
| new epoch on restore | done | — | done | GUIDE (8) |

**Suite** (2026-10-07):

- Store contract (`store-contract/memory.test.ts`): 21 pass, 0 fail.
- Control `npm test`: 566 tests, 401 pass, 164 fail. Every failure is in the core
  and the sample, which still call the removed store methods (`configEpoch`,
  `latestConfig`, `configsAfter`, versioned `publishConfig`) and build the removed
  message shapes: config-versions, usage, control-plane, gateways, fastify, and the
  sample's app, page and config-file tests. A second run under the TAP reporter
  stalled in a stream test file whose streams never end while the core is broken.
  **Expected, cleared by step 8.**
- Control `npm run lint`: 29 TypeScript errors, all in the core and sample modules
  (config-versions 13, usage 8, sample page 6, fastify 1, control-plane 1). **Expected,
  cleared by step 8.** No errors in storage or store-contract.
- Gateway: `scripts/check-gateway.sh` passed, but only from Go's test cache: the
  fixture files live outside the gateway module, so a fixture change does not
  invalidate cached results. Uncached (`go test -count=1 ./internal/control/`), 25
  tests fail on the new schemas, fixtures and protocol shapes:
  boot/stream/resync/version/epoch/last-known-good tests, `TestEveryFixtureKindHasADecoder`,
  the fixture tests and the totals tests. **Expected, cleared by step 9.** Steps 8–9
  must run the gateway tests with `-count=1`.
- Cross-half tests: not run. They need both halves on the new protocol. **Expected red
  until step 9.**
