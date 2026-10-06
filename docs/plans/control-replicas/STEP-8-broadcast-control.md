# Step 8 — broadcast-only kaiak-control

**Status:** done (2026-10-07)

## Intent

Implement step 7's contract in `kaiak-control` and the sample, removing every
control-side item on the removal checklist. This is a removal step: nothing removed
survives under another name, and tests that assert removed behaviour are deleted.

## Files likely touched

- `src/config-versions/`: becomes the current config.
  - Publish: validate, apply the parents rule against the current config, then a
    conditional replace; retry when another publish won.
  - Delivery: on a change notification, read the current config and send it on each
    stream if it is newer by the store's sequence than what that stream last got
    (per-stream ordering).
  - No history, no `configsSince`, no resync.
  - Rename the module only if its name no longer says what it does. Moving code to
    keep an old concept alive is not allowed.
- `src/usage/`:
  - Totals without revision, epoch or config version.
  - Acks are `{ batch }` only.
  - Totals are pushed on the stream only, per stream, never older than the last one
    sent there.
- `src/fastify/`:
  - `GET /v1/config` removed.
  - `GET /v1/stream` takes no parameters: it sends the current config, then totals,
    then changes.
  - A store sequence going backwards (seen in a notification or a snapshot) closes
    every open stream on this core.
- `src/messages/`, `src/protocol/`, `src/index.ts`: message types and exports
  follow.
- Sample: no config versions in its page or logs; `KAIAK_SAMPLE_PROTOCOL_PORTS`
  unchanged.
- GUIDE:
  - the app/library split;
  - publish is replace;
  - the store interface;
  - the stream;
  - the Postgres sketch without history or epoch;
  - rollback handled by the core.
- Tests:
  - Delete the version, history, resume and resync tests.
  - Add:
    - a conditional replace racing another publish;
    - per-stream ordering with two reads racing;
    - a rollback closing streams;
    - a stream's first events being config then totals;
    - totals on the stream only, with the ack carrying none;
    - two cores converging on one current config.

## Acceptance criteria

- `npm test` and `npm run lint` from `control/` pass.
- Every control-side item on step 7's removal checklist greps clean in `control/`,
  recorded in Result with the grep commands and their empty output.
- The gateway and the cross-half tests may still fail (step 9). Name them.

## Result

**What changed** (commits `d9f8233`, `cd1beea`, `e07bcd6`, `726dbbb`, and this step's
Result)

- **`src/config-publishing/`** (renamed from `config-versions/`: the old name no longer
  said what the module does):
  - a publish validates, checks the parents rule against the current config, and
    replaces the current config by a conditional write on the hash it checked
    against, retrying on a lost race;
  - `configHash(config)` is the lowercase hex SHA-256 of `JSON.stringify(config)`, the
    text the stream sends;
  - delivery to this core's listeners reads the current config after each
    `config-published`, one read at a time; a config whose hash was the last handed
    out is not handed out again. A hash carries no order, so a store restored to an
    older config hands it out like any other.
  - **Removed:** versions, `configsSince`, the history size, the store epoch.
- **`src/usage/`:**
  - totals are `{ live_gateways, counted_through, windows }`, with no revision, epoch
    or config version;
  - the ack is `{ batch }`;
  - a batch is counted whether or not a config is published (`config-unavailable` is
    gone);
  - every totals read reports the store sequence it saw to the core.
- **`src/control-plane/`:**
  - the core keeps the highest store sequence it has read, from notifications, config
    reads and totals reads;
  - reading one below it is a rollback, announced through `onRollback(listener)`.
  - **Removed:** `configEpoch`, `configsSince`, `configHistorySize`.
- **`src/fastify/`:**
  - `GET /v1/config` and the stream's `since` / `config_epoch` parameters are removed
    (with `readPosition` and `since-invalid`);
  - the stream subscribes, reads the current config, sends it, then totals (none
    before a config was sent), then every change;
  - per stream, a config is sent only if its store sequence is above the last one sent
    there, so a slow connect read overtaken by a newer config is not sent after it;
  - totals reads are one at a time per stream, so they are in order by construction;
  - on `onRollback` the plugin ends every open stream on that core.
- **`src/messages/`:**
  - `ConfigEvent { config_hash, config }` and `validateConfigEvent` replace
    `ConfigSnapshot` / `validateConfigSnapshot`;
  - `Resync` / `validateResync` are removed;
  - `UsageAck` is `{ batch }`;
  - status has `applied_config_hash`, and `last_rejection` has `config_hash`;
  - `checkTotals` lost its path parameter, which only the ack used.
- **Sample:**
  - reload runs and log lines carry the config hash;
  - the page shows the config by its first 12 hash digits;
  - gateways are marked "not current" when their applied hash is not the current one;
  - rejections are shown by hash.
- **GUIDE:**
  - §1: the app is the source of truth for config; `kaiak-control` validates and
    broadcasts the current config.
  - §2: a publish replaces the current config; concurrent editing is the app's
    business.
  - §4: three endpoints; no `config-unavailable`.
  - §5: the store's sequence is internal and orders streams; a rollback ends streams;
    `currentConfig` / `publishConfig(entry, expectedHash)`; the Postgres sketch holds
    the current config in `store_meta`, with a conditional update on the hash.
  - §7, §9 and §10: hashes in place of versions; restarts and replicas reconnect to
    the current state.
  - §12: the epoch pitfall is replaced by "restoring a backup gives gateways the
    restored config".
- **`docs/architecture/control-plane.html`:**
  - three endpoints; boot from the stream;
  - config pushes: replace current, status by hash;
  - "one current config, ordered by its sender" (the rejected "only newer" named);
  - acks naming the batch only; totals on the stream only and applied whatever the
    config; contact without a snapshot; reconnect;
  - the mismatch paragraph is removed;
  - the core's `config-publishing`.
- **`scripts/check-gateway.sh`:** `go test -race -count=1`, so fixture changes outside
  the module are never hidden by Go's test cache. `AGENTS.md`'s command line says
  "uncached".

**Tests deleted** (each asserted removed behaviour):

- `config-publishing.test.ts` (from `config-versions.test.ts`):
  - "versions start at 1 and increase by one"; "concurrent publishes get distinct
    versions": version numbering.
  - "the history size must be a positive integer": `configHistorySize`.
  - The whole "resuming from a version" suite, 9 tests: the current version replays
    nothing; within the history replays newer; older than the history → resync;
    ahead → resync; before the first publish → resync; not a version → resync;
    another epoch → resync; the epoch is the store's; a short history → resync.
  - "publishes racing from both get consecutive versions": version order.
- `control-plane.test.ts`:
  - "the core's history size bounds resuming".
  - The revision assertion in "totals read through either core carry one revision".
- `usage.test.ts`:
  - "no config published: the batch is refused with config-unavailable".
  - "the revision is the store's sequence…" and "two processes over one store read one
    revision": revisions on the wire.
  - "an ack's totals hold the batch it acknowledges": ack totals.
- `gateways.test.ts`: "a rejection below the applied version is accepted (a restarted
  control plane counts from 1)": versions.
- `fastify.test.ts`:
  - `GET /v1/config`'s three tests: snapshot, `503 config-unavailable`, prefix on
    `/config`.
  - The 12 `since-invalid` cases.
  - The request checks on `/v1/config`.
  - The six replay and resync tests: replays newer versions; since current replays
    nothing; another epoch → resync; older than the history → resync; ahead → resync;
    before anything published → resync.
- `status-totals.test.ts`: "with nothing published a stream gets resync and no
  totals"; the revision and `config_version` assertions in the stream tests.
- `usage-route.test.ts`: "a batch before any config is answered 503
  config-unavailable".
- The sample's `GET /v1/config` and version/epoch checks in `app.test.ts` and
  `main.test.ts`.

**Tests added:**

- conditional replace with its hash;
- identical content published again;
- concurrent publishes, the last stored is current;
- a store restored to an older config hands it out;
- a lost race re-checked against the winner;
- rollback announced once, not after moving on, unsubscribe;
- the store sequence a totals read sees, which never goes on the wire;
- the ack names the batch only;
- a batch counted with no config published, listed once a config limits it;
- the stream sends the current config, then totals, on connect;
- before the first publish the stream stays open, then gets config and totals;
- every config published while connected;
- a restored older config on a new stream;
- a config published during the connect read is sent once, after it;
- a connect read overtaken by a newer config is not sent after it;
- a store going back ends every stream and the gateways reconnect to its current
  state;
- `GET /v1/config` answers `404 not-found`;
- a publish followed by totals listing the new config's limits.

The sample main test's "one store behind every port" now compares the first config's
hash on each port. That is weaker than the epoch it compared, which no longer exists;
`app.test.ts`'s replicas test proves the shared store by counting a batch once across
cores.

**Removal checklist, control side** (greps over `control/` without `node_modules`,
plus `docs/architecture/control-plane.html`):

```
config_version|configVersion|ConfigVersion                          (none)
config_epoch|configEpoch|ConfigEpoch|applied_config_epoch           (none)
applied_config_version                                              (none)
configsAfter|configsSince|configHistorySize|historySize|config-versions|ConfigVersions|createConfigVersions   (none)
resync|Resync                                                       (none)
since=|readPosition|since-invalid|ConfigPosition                    (none)
config-unavailable                                                  (none)
config-snapshot|ConfigSnapshot|validateConfigSnapshot|"/config"
  control/kaiak-control/src/messages/index.ts:36: const CONFIG_PATH = "/config";
    — the JSON Pointer prefix of a config event's issue paths, not the endpoint
config_mismatch|ConfigMismatch                                      (none)
ack\.totals|ack: { batch, totals                                    (none)
new epoch|takes a new epoch|StoredConfig|latestConfig
  usage.test.ts:387 "a new epoch is counted from any sequence"; fastify/index.ts:132
  "usage batch starts a new epoch" — the gateway's batch epoch, which stays
```

`revision` remains only as the gateway-record revision: `storage/types.ts`,
`storage/memory.ts`, `gateways/index.ts`, `store-contract/index.ts`. That is the
allowed exception.

**Suite** (2026-10-07):

- **Control:**
  - `npm test`: 550 tests, 550 pass.
  - `npm run lint`: `tsc` clean, boundaries ok.
- **`scripts/check-all.sh`** stops at the gateway's uncached tests:
  - gofmt, vet and staticcheck pass;
  - every gateway package passes except `kaiak/internal/control`, which has 63
    failing tests: boot from the snapshot, resume, resync, versions and epochs,
    last-known-good, ack totals and counted-through, the message fixtures. **Expected,
    cleared by step 9.**
  - The cross-half tests do not run (the script stops first). **Expected red until
    step 9.**
- No orphaned `node --test` or sample process was left (`pgrep` empty).

