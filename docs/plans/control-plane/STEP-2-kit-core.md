# Step 2 — `kaiak-control` core

**Status:** done (2026-09-24)

## Intent

The framework-agnostic core: storage interface and in-memory store, config versions,
key generation, request authentication and protocol-version checks.

## Files likely touched

- `control/kaiak-control/src/` — `storage/` (interface + in-memory), `configs/`
  (versions, publish, history for resume), `keys/` (generation, hashing), `protocol/`
  (headers, auth, version check helpers); package entry exports

## Decisions made during planning

- Storage interface is async (a database implements it); the in-memory store is the
  reference and the sample's store.
- Publishing a config validates it (`validateConfig`) and assigns the next integer
  version; an invalid one is rejected with the issues and the current version stays.
- A bounded history of recent versions lets a stream resume `since=<version>`; older
  → `resync`.
- `createKey(id)` returns `{ id, key, hash }`: `kaiak-` + 43 chars `[A-Za-z0-9]` from
  32 random bytes (rejection sampling, no modulo bias), hash `sha256:<hex>`.
- Auth: bearer token compared in constant time; missing/wrong → 401 `{ error }`;
  `Kaiak-Protocol` mismatch → a distinct error code.
- The core exposes operations and an event emitter/subscription for pushes; no HTTP.

## Acceptance criteria

- Unit tests: versions increment, invalid publish keeps current, resume within/outside
  history, key format and entropy source, hash matches the gateway's (shared fixture
  pair: a key and its hash), auth and version checks.

## Result

- `control/`: `npm test` — 215 tests, 215 pass, 0 fail; `npm run lint` — `tsc` clean,
  `boundaries ok`.
- `scripts/check-gateway.sh` — `gateway checks passed` (unaffected).
- No expected reds.

### Decisions made while implementing

- Subsystems: `storage` (interface in `types.ts`, `createMemoryStore`),
  `config-versions` (`createConfigVersions`), `keys`, `protocol`, `control-plane`
  (`createControlPlane`, composing the others). `schemas` gained
  `definitionChecker(file, def)`, so the key-ID and instance checks use the schema's
  own `$defs` (`config.schema.json#/$defs/id`, `common.schema.json#/$defs/instance`)
  — one validation mechanism, one home per pattern (CODING-RULES §9).
- Storage interface: `latestConfig()`, `saveConfig(entry, keep)`,
  `configsAfter(version)`. The core owns the history bound and passes `keep`; a store
  may keep more (a database keeping every version for audit is fine). The core checks
  contiguity of what `configsAfter` returns, so a store keeping less yields `resync`,
  never a gap. The memory store clones on save.
- `StoredConfig = { version, config, publishedAt }`, `publishedAt` in epoch ms from the
  injected clock (`() => number`, default `Date.now`) — the same clock shape steps 4–5
  use for windows and the live set.
- `publishConfig` returns `{ ok: true, published } | { ok: false, issues }` (an
  invalid edit is expected input, not an exception). Publishes are serialized
  in-process so two cannot claim one version. Identical documents are not
  de-duplicated: every publish is a new version (the sample's watcher debounces).
- Subscription idiom: `onConfigPublished(listener)` returning an idempotent
  unsubscribe; listeners are called synchronously after the store write, each wrapped
  in its own subscription (the same function twice = two subscriptions). A throwing
  listener does not stop the others; afterwards `publishConfig` rejects with
  `config-subscriber-failed` (an `AggregateError`) — the version stays published.
- `configsSince(v)`: `resync` for nothing published, non-integer or negative `v`,
  `v` > current, `current − v` > history size, or a gap in the store; `[]` for
  `v` = current. `v = 0` within history replays from 1.
- Token check hashes both sides with SHA-256 and compares digests with
  `timingSafeEqual` — equal-length buffers always, so length does not leak either.
  Bearer scheme is case-insensitive; a repeated header is refused. Errors never echo
  the token.
- Error codes and statuses: `unauthorized` 401, `protocol-version-mismatch` 400,
  `instance-invalid` 400 (all in CONTROL-PROTOCOL.md, Request checks). `errorBody()`
  gives `{ error: code, detail: message }`.
- Headers are read from a lower-cased `Record<string, string | string[] | undefined>`
  (Node's `IncomingHttpHeaders` fits). `PROTOCOL_VERSION = 1` (number; the header is
  compared to its string form, the status body's `protocol_version` is the number).
- `createKey`: 43 characters by rejection sampling on random bytes (bytes ≥ 248
  discarded) — about 256 bits, the entropy the spec's "32 random bytes" names; it
  draws more than 32 bytes. An invalid ID throws `key-id-invalid`.
- `createControlPlane({ store, token, configHistorySize = 100, clock = Date.now })`;
  an empty token throws `token-missing`; a bad history size throws
  `config-history-size-invalid`.
- Package entry exports: `createControlPlane`, `createMemoryStore`, `createKey`,
  `hashKey`, `PROTOCOL_VERSION`, `PROTOCOL_HEADER`, `INSTANCE_HEADER`, `errorBody`, and
  the types. The individual header checks stay subsystem-level (the Fastify plugin,
  inside the package, imports `protocol/index.ts`); hosts check through
  `controlPlane.checkGatewayRequest(headers)`.

### Deviations from the plan

- None in substance. "32 random bytes" is read as the entropy, not the byte count
  drawn (rejection sampling discards some).

### For later steps

- Step 3: subscribe *before* calling `configsSince`, then drop pushed versions at or
  below the last one sent — otherwise a publish between the two is lost or doubled.
  Map `checkGatewayRequest` failures with `errorBody` and `error.status`; set
  `Kaiak-Protocol: 1` on every response. `GET /v1/config` with nothing published
  needs an answer (not settled yet).
- Steps 4–5 extend `ControlPlaneStore` (last batch per instance, totals, recent
  records, gateway statuses) and `ControlPlane` the same way `config-versions` is
  composed.
- Step 6: after a sample restart versions start again at 1 (CONTROL-PROTOCOL.md,
  Config versions); the gateway must take a `resync` snapshot whose version is below
  its applied one.
