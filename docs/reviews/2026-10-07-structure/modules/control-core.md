# control-core — structure review

Scope: `control/kaiak-control/src/` minus `backend-verify` and `fastify` (calendar, config,
config-publishing, control-plane, gateways, keys, messages, protocol, schemas, storage,
store-contract, usage, index.ts), plus `control/kaiak-control/schema/` and
`control/kaiak-control/GUIDE.md`. Fastify, sample and gateway code read only as callers or
mirrors.

## Module summaries

Import graph between subsystems (non-test files; checked by `control/scripts/check-boundaries.ts`):
acyclic, every edge through an `index.ts`.

```
calendar  <- config, messages
schemas   <- config, messages, keys, protocol, config-publishing, backend-verify
config    <- messages, config-publishing, backend-verify
messages  <- storage, usage, gateways, store-contract, fastify
storage   <- config-publishing, usage, gateways, control-plane, store-contract
protocol  <- control-plane, fastify
config-publishing, usage, gateways <- control-plane <- fastify
```

### calendar (29 lines)
- Checks a schema-shaped date or timestamp names a real day or instant (`isRealDate`,
  `isRealTimestamp`).
- Used by: `config/semantic.ts`, `messages/semantic.ts`. Not exported from the package.
- Concept: RFC 3339 calendar rules (no leap seconds). Clean.

### schemas (94 lines)
- Loads every `schema/*.schema.json` into one Ajv 2020 instance; `schemaChecker<T>(file)`,
  `definitionChecker(file, def)`, `ValidationIssue`, `pointer()` (JSON Pointer builder).
- Used by: config, messages, keys, protocol, config-publishing (`pointer`), backend-verify.
- Dependency: `ajv` (the only third-party one besides fastify).
- Concept: "issue = {code, message, path}", code `"schema"` for structural failures. Clean.

### schema/ (package copy of `protocol/schema/`)
- Byte-for-byte copy, written by `control/scripts/sync-schemas.ts`; `sync-schemas.test.ts`
  fails `npm test` on drift. It exists so the package (and the sample image, which copies
  `kaiak-control/schema`) runs without the repo around it. Nobody else reads
  `protocol/schema` programmatically: the gateway re-encodes the schemas in Go and both halves
  test against `protocol/fixtures/`.
- Verdict: duplicated files, deliberately — one place to edit (`protocol/schema`), one command
  to sync, a test guarding drift. **Leave as is.** Making `protocol/schema` a link to the
  package copy (or the reverse) would save one command per schema change at the cost of
  making one half own the neutral contract.

### config (types 153, semantic 237, limits 83, tree 83, index 30)
- `validateConfig` (schema, then `checkSemantics`); `Config` and part types;
  `BACKEND_TYPES`; `resolveScopes` (effective limits and allowed models per scope, mirrored by
  the gateway's snapshot and held to it by `protocol/fixtures/config/resolved`);
  group-tree ancestry (`tree.ts`); the counters bound (`MAX_COUNTERS`).
- Exported surface used outside: `validateConfig` (config-publishing, messages, sample
  config-file via `publishConfig`), `resolveScopes` (sample `page/sections.ts`),
  `BACKEND_TYPES` (sample verify, backend-verify), types everywhere.
- Concepts: backend type (`types.ts:6`), limit type (`types.ts:18`), usage unit and price unit
  (`types.ts:25-28`), group tree and max depth, semantic rule codes (contract, shared with Go).
- Clean. (`ALL_MODELS = "*"` is defined twice, `semantic.ts:32` and `limits.ts:58` — trivial.)

### messages (types 125, semantic 96, index 64)
- One validator per protocol message (`validateWith(schemaChecker, rules)`), the message rule
  codes, and the TS view of every message (`UsageRecord`, `Totals`, `TotalsWindow`, `BatchId`,
  `UsageBatch`, `UsageAck`, `GatewayStatus`, …).
- Used by: usage (`validateUsageBatch`), gateways (`validateGatewayStatus`), storage/types
  (types), fastify, store-contract; the package exports all six validators (only tests and
  `fastify` tests call `validateTotals`/`validateUsageAck`/`validateConfigEvent`/
  `validateUsageRecord` — they are the public "check a message" API for hosts).
- Concepts: protocol message shapes; `TotalsLimitType` (the two counted types,
  `types.ts:35`); `protocol_version: 5` literal (`types.ts:94`).
- The TS types are a hand-kept third mirror (schema → TS, schema → Go). Fixtures hold both
  validators to the schema; nothing checks the TS types against it except `BACKEND_TYPES`.
  Generating them needs a new dependency; **leave as is**.

### protocol (103 lines)
- Gateway request checks (bearer token in constant time, `Kaiak-Protocol`, `Kaiak-Instance`),
  `PROTOCOL_VERSION`, header names, `errorBody`.
- Used by: control-plane (`checkGatewayRequest` bound to the token), fastify (headers,
  version, `errorBody`). Clean.

### keys (58 lines)
- `createKey`, `hashKey`, `isConfigId` (shape of a config ID via the schema's `id` def).
- Used by: sample keygen. Clean.

### storage (types 250, memory 189)
- `ControlPlaneStore`: the interface where control-plane processes agree (conditional
  publish, conditional batch count, totals snapshot, gateway records with revisions,
  housekeeping drops, `subscribe` with `catch-up`), and `createMemoryStore` (reference
  implementation, the sample's store).
- Used by: config-publishing, usage, gateways, control-plane, store-contract; exported to
  hosts (they implement it).
- Concepts encoded here that are really usage/gateway domain: which limit type uses which
  window (`CurrentWindows {hourStart, monthStart}` and the `type === "tokens_per_hour"`
  switch in `memory.ts:45,111` — F3), "the batch counted last across epochs"
  (`BatchCursors.latest`, `memory.ts:47-55` — F5), whether a write changed the live set
  (`liveChanged`, `memory.ts:127`).
- The "three places per store change" question: interface (`types.ts`), reference store
  (`memory.ts`), contract tests (`store-contract/index.ts`) — plus the negative-control
  broken stores when a guarantee is added, plus GUIDE §5 and CONTROL-PROTOCOL.md. The first
  three are the intrinsic minimum for a pluggable interface with a reference
  implementation and a conformance suite; the GUIDE's restatement is the avoidable one (F6).
  What *can* shrink is how much domain logic every store must carry (F3, F5).

### store-contract (index 713, lossy-channel 79, broken stores ~75, tests)
- `storeContractTests(subject)` — the store contract as `node:test` tests, a separate
  package entry (`kaiak-control/store-contract`); a lossy change channel over the memory
  store so the catch-up test runs here; four deliberately broken stores run in a child
  process by `negative-control.test.ts`, which asserts exactly the named tests fail.
- Used by: hosts with their own store (documented in GUIDE §11); this repo's
  `memory.test.ts`, `lossy-channel.test.ts`, negative controls.
- Clean and earns its place: it is the only executable statement of the multi-process
  contract. (The broken stores ship in the package, since `files` excludes only
  `*.test.ts` — harmless.)

### config-publishing (221 lines)
- `publishConfig` (validate, write text once, hash, conditional publish with retry against
  the winner, parents-never-change rule), `currentConfig`, `readConfig` (numbered reads for
  stream ordering), `onConfigPublished`/`onConfigRead`, retried delivery after a store
  change, `onDeliveryFailed`; `configHash`.
- Used by: control-plane only (its types re-exported). `configHash` is exported but used
  only by tests (the publish path uses the private `hashOf`).
- Concepts: config hash, read order (`ConfigRead.read`), the parents rule. Clean apart from
  the listener-set pattern (F4).

### usage (index 267, aggregate 51, windows 47)
- `acceptUsageBatch` (validate, per-instance serial queue, conditional count with outcome
  first/next/gap/new-epoch/duplicate), `readTotals` (snapshot + live count + window starts,
  re-read across a boundary), `totals(instance)` (a gateway-shaped `Totals`), `recentRecords`,
  `onTotalsChanged`, `dropPastWindows` (once an hour per process); `aggregate.ts`
  (record → window additions, the "tokens that load the backend" rule); `windows.ts`
  (current/previous UTC hour and month, the record's own-window rule, formatting).
- Used by: control-plane (composes it); fastify's totals feed (`readTotals`,
  `onTotalsChanged`); sample (`totals("")`, `recentRecords`, `onTotalsChanged`).
- Concepts: counted limit types (`aggregate.ts:12`), amount per type (`aggregate.ts:46` —
  mirrored in Go `gateway/internal/limits/limits.go:629`), window kinds, batch outcome,
  `MAX_USED` wire ceiling.
- Its control-plane window logic is a different concern from the gateway's `limits/window.go`
  (enforcement with reservations and pushed bases); no real duplication there beyond the
  shared counting rule.

### gateways (298 lines)
- Status intake (validate, instance check, judge against the stored record: live-set join,
  restart vs two-processes conflict, conditional write), `gateways()` view, `liveGateways()`,
  `onGatewaysChanged`, **and the expiry sweep**: expire, forget, drop batch cursors past
  retention, call `dropPastWindows`, timer start/stop (F1).
- Used by: control-plane; sample (`gateways()`, `onGatewaysChanged`); fastify feed
  (`onGatewaysChanged`).
- Concepts: live set, conflict flag, revision-conditional writes, the sweep.

### control-plane (199 lines)
- `createControlPlane`: builds config-publishing, gateways and usage over one store,
  holds the one store subscription and fans each change to the three modules, binds the
  token into `checkGatewayRequest`, wires `onListenerError`/`onDeliveryFailed`, `start`/`stop`.
- The `ControlPlane` interface is `Omit<ConfigPublishing,"takeChange"> &
  Pick<Usage, …> & Omit<Gateways,"takeChange">` plus four members; the object literal
  forwards 17 methods by hand (`index.ts:150-167`).
- After the replicas change the core holds no state of its own beyond the subscription and
  `started` — it is a composition root. Its shape is right; what is wrong is what it
  delegates *to* (the sweep, F1) and part of its public surface (F1, F2, F8).
- Used by: fastify, sample (app, page, config-file). Every host goes through it.

### index.ts (42 lines)
- The package entry: values and `export type *` from each subsystem.
- `libraryName` is exported for one scaffolding test (`index.test.ts`) and nothing else.

### GUIDE.md (718 lines, 23 commits on main — the most-changed doc)
- A how-to for building the real control plane: division of labour, hard rules,
  packaging, minimal app, implementing the store (with a Postgres sketch), ledger, config
  publishing (backend types, group tree, prices, LiteLLM import), backend verify, reading
  state, operating, testing, pitfalls.
- It restates three contracts owned elsewhere (F6), which is why every feature edits it.

## Findings

### F1 — The expiry sweep is the core's housekeeping run, but lives in `gateways` and pulls usage work in by injection
- **Kind** — cross-module (inside the library).
- **Where** — `gateways/index.ts:95-110` (`GatewaysOptions.batchCursorRetentionMs`,
  `sweepIntervalMs`, `onExpirySweep`, `dropPastWindows`), `gateways/index.ts:192-233`
  (`sweep`), `:280-292` (`startExpirySweep`/`stopExpirySweep`);
  `control-plane/index.ts:124-140` (the forward reference `dropPastWindows: () =>
  usage.dropPastWindows()` with the comment "The usage module is made below"; `usage` gets
  `liveGateways: gateways.liveGateways`), `:62-64,166-167,181-190`.
- **Now** — one "expiry sweep" run does four jobs: expire silent gateways, forget long-silent
  ones (gateways), drop batch cursors past retention (a usage/intake concept — the option
  `batchCursorRetentionMs` is passed into `createGateways`), and drop past window totals
  (usage, injected as a callback). Gateways and usage inject into each other (usage needs the
  live count, gateways needs `dropPastWindows`), so the core creates them in an order that
  only works because the callback is late-bound. `ControlPlane` inherits
  `startExpirySweep`/`stopExpirySweep` through `Omit<Gateways,"takeChange">` beside
  `start`/`stop`, which call them — two public ways to run the timer; only
  `gateways.test.ts` calls the former. Calling `stopExpirySweep()` on a started core leaves
  `started === true`, so a later `start()` does nothing.
- **How it got here** — the sweep was born in `gateways` as the live-set expiry (pre-0.7.3).
  `c2b1b87` (review fixes) added per-epoch cursors and their retention to the same run;
  `a7526cd` (round 3, plan step 15) moved past-window pruning out of batch counting into
  "the sweep" because it was the existing timer. The spec settled *what* the run does
  (CONTROL-PROTOCOL.md:978-993); it was placed where the timer already was.
- **Proposed shape** — the core owns the run: `control-plane` has the timer and a
  `housekeeping(trigger)` (keep the public name `expireSilentGateways` if wanted) that calls
  `gateways.expireSilent(at)` → `{expired, forgotten}`, then
  `store.dropBatchCursorsCountedBefore(at - retention)`, then `usage.dropPastWindows()`, and
  reports one `ExpirySweepRun`. `GatewaysOptions` loses `batchCursorRetentionMs`,
  `sweepIntervalMs`, `onExpirySweep`, `dropPastWindows`; `Gateways` loses the timer methods;
  `ControlPlane` loses `startExpirySweep`/`stopExpirySweep` (`start`/`stop` are the only
  lifecycle). Optionally the live count also moves to the core's `readTotals`
  composition (`usage` returns the snapshot, core adds `gateways.liveGateways()`), which
  removes the other injection and leaves usage and gateways independent siblings.
- **Payoff** — no mutual injection, no forward reference; 4 options and 2 methods gone from
  `gateways`; 2 redundant public methods gone from `ControlPlane`; a new housekeeping job
  (e.g. ledger pruning) is one line in the core instead of another injected callback.
- **Cost / risk** — small: ~60 lines move; `gateways.test.ts` setup and its timer tests move
  to `control-plane` tests; public API loses two methods (no host uses them; sample uses
  `onExpirySweep`, unchanged). No protocol or spec change (the spec already describes one
  run per process).
- **Confidence** — high.

### F2 — Hosts read spend through a gateway-shaped `totals(instance)` with a fake instance; the gateway path does not use it
- **Kind** — cross-module.
- **Where** — `usage/index.ts:59-61,216-219` (`totals(instance)`), `:235-240`
  (`countedThrough`, exported, used only there); `control-plane/index.ts:63`;
  `fastify/gateway-stream.ts:113-115` (the same `counted_through` filter, inline);
  `sample/src/page/sections.ts:225-233` (`const PAGE_READER = ""`, `core.totals(PAGE_READER)`)
  and `:280,292-295` (recomputes the current hour/month start); GUIDE.md:594,603
  (recommends `totals("")`).
- **Now** — `Totals` is the gateway message (with `counted_through` for one instance). The
  streams build it themselves from the feed's `readTotals()` state. The only non-test caller
  of `totals(instance)` is the sample's page, passing `""` and ignoring `counted_through`,
  and the GUIDE teaches hosts to do the same. Because `Totals` carries no window starts, the
  sample recomputes them (`HOUR_MS`, `monthStart`) — a third copy of `usage/windows.ts`.
- **How it got here** — `totals(instance)` predates the shared totals feed (round 2,
  `635b126`: "one totals read per push for every stream"); the feed switched to
  `readTotals()`, and `totals(instance)` survived for the page.
- **Proposed shape** — remove `totals(instance)` and `countedThrough`; hosts read
  `readTotals()` (rename to taste, e.g. `spend()`), which already has windows, live count
  and `windowStarts` — the page then shows window starts without recomputing them. The
  `counted_through` filter keeps its one home in `gateway-stream.ts`.
- **Payoff** — one method and one export gone from the public API, the `""` sentinel gone,
  the sample's window arithmetic gone (~15 lines), GUIDE §9 simpler.
- **Cost / risk** — ~21 test call sites (`control-plane.test.ts` 6, `status-totals.test.ts`
  4, `usage-route.test.ts` 3, `usage.test.ts` 3, others) switch to `readTotals()` or the
  stream; GUIDE §9 and the sample page edit. No protocol change.
- **Confidence** — high that the shape is redundant; medium on whether `readTotals` is the
  name hosts should see (it also carries all instances' cursors, harmless).

### F3 — Every store encodes which limit type uses which window
- **Kind** — cross-module (storage interface vs usage).
- **Where** — `storage/types.ts:52-56` (`CurrentWindows {hourStart, monthStart}`),
  `:204-213`; `storage/memory.ts:44-45` (`isCurrent`), `:109-113`
  (`dropPastWindowTotals`), both `total.type === "tokens_per_hour" ? … .hourStart :
  … .monthStart`; `usage/windows.ts:19-21` (`windowStartFor`, the same switch);
  `usage/index.ts:141-155`; GUIDE.md §5 table and the snapshot SQL
  (`where (type = 'tokens_per_hour' and window_start = $hourStart) or (type = 'usd_per_month' …)`).
- **Now** — the store interface passes windows as named hour/month fields, so the memory
  store and every database store must know the domain mapping tokens_per_hour→hour,
  usd_per_month→month (the GUIDE's SQL hard-codes it). The usage module holds the same
  switch, and `TotalsRead.windowStarts` is already keyed by type
  (`Record<TotalsLimitType, string>`).
- **How it got here** — `CurrentWindows` came with the store contract (`ecfd040`) when
  counting moved behind the store; the field names mirrored the two window kinds.
- **Proposed shape** — `type WindowStarts = Record<TotalsLimitType, number>`; the store
  filters `total.windowStart === current[total.type]` and drops `< oldest[total.type]`;
  `windowStartFor` becomes indexing; the SQL becomes a join on `(type, window_start)` pairs.
  `currentWindows`/`previousWindows` in `usage/windows.ts` return the record.
- **Payoff** — stores (memory, the real DB store, the GUIDE sketch) hold no limit-type
  knowledge; the switch exists once (in `usage/windows.ts`, where windows are computed).
  A future counted type (e.g. a daily window) touches usage and the schemas, not every store.
- **Cost / risk** — small: ~10 lines in storage/usage, store-contract constants
  (`CURRENT`/`PREVIOUS`, `store-contract/index.ts:55-58`), GUIDE §5. Public store
  interface changes — acceptable in feature-building mode; no external store exists yet.
- **Confidence** — high.

### F4 — The listener-set pattern is hand-written five times
- **Kind** — cross-function.
- **Where** — `storage/memory.ts:57-68,170-177`; `config-publishing/index.ts:91-99,175-182`;
  `gateways/index.ts:131-141,266-273`; `usage/index.ts:117-126,221-228`;
  `control-plane/index.ts:106-120,168-175` (plus `rethrowLater`, `:194-198`, and the same
  rethrow in `memory.ts:62-66`). Test support repeats it again (`store-contract/lossy-channel.ts`,
  `early-announcement-store.ts`).
- **Now** — each has a `Set`, a wrapper "so the same function subscribed twice is two
  subscriptions" (the comment is copied verbatim five times), a snapshot `[...listeners]`
  before iterating, a per-listener try/catch to an error handler, and an idempotent
  unsubscribe.
- **How it got here** — each module gained listeners as it gained store-fed changes
  (`db982e2`, "store-fed listeners"); the subtle rules were fixed in each copy by review
  rounds.
- **Proposed shape** — one small leaf helper (e.g. `src/listeners/index.ts`):
  `createListeners<T>(onError: (error, value: T) => void)` → `{ add(listener): () => void;
  emit(value: T): void }`. Each module keeps its own `onX` signature and error mapping.
- **Payoff** — ~45 lines removed; the three subtle rules (duplicate subscriptions, mutation
  during emit, isolated failures) live and are tested once.
- **Cost / risk** — small; behaviour-preserving; no public API change. The lint needs the
  new subsystem to be a leaf (it would be).
- **Confidence** — high. (The two promise-chain serializers — `config-publishing` `deliveries`
  and `usage` `serialized` — are a similar but only two-use pattern with different keys;
  leave them.)

### F5 — `BatchCursors.latest` makes every store compute "the batch counted last" for a log field
- **Kind** — cross-module (storage contract vs usage).
- **Where** — `storage/types.ts:86-98` (`BatchCursors`), `:186-187` (`lastBatch`);
  `storage/memory.ts:47-55` (`cursorsOf`: max by `countedAt`, ties broken by Map insertion
  order); `usage/index.ts:195,258-262`; store-contract `index.ts:221-251,325-326`; GUIDE §5
  table ("greatest `countedAt`").
- **Now** — the core uses `latest` for two things: whether the instance has *any* cursor
  (`first` vs `new-epoch`) and the `previous` field of the intake's log line. Every store
  must implement max-by-`countedAt` across epochs, with a tie rule the memory store gets from
  Map ordering and a database would need a column for.
- **How it got here** — per-epoch cursors arrived in `c2b1b87`; `latest` kept the old
  single-cursor log ("the batch before it") working.
- **Proposed shape** — `lastBatches(instance)` returns the instance's cursors (one per epoch,
  with `countedAt`); the core picks `inEpoch` and the latest itself (ties don't matter for a
  log line). The refused `saveCountedBatch` returns the same list. Alternatively keep the
  method but return `{ inEpoch, anyEpoch: boolean }` and drop `previous` for new epochs.
- **Payoff** — one derived rule out of the store contract (memory store, every DB store, the
  contract tests and the GUIDE); stores do a plain `select … where instance = $1`.
- **Cost / risk** — medium-small: storage interface, memory store, ~6 contract-test asserts,
  usage, GUIDE §5. No protocol change.
- **Confidence** — medium (the payoff is real but modest; worth it mainly because the
  contract is the part external implementers must get right).

### F6 — GUIDE.md restates contracts owned elsewhere, so every feature edits it
- **Kind** — cross-module (docs).
- **Where** — GUIDE.md:5-19 (a dated change history of format/protocol bumps);
  §4 bullet at :147-162 (stream ordering rules restating CONTROL-PROTOCOL Config stream →
  Order); §5 table :194-202 (per-method store contract, restating `storage/types.ts` comments
  and CONTROL-PROTOCOL "Control-plane processes"); §7 :358-470 (backend types, service tier,
  pricing rules restating CONTROL-PROTOCOL → Config and GATEWAY.md → Providers).
- **Now** — a store method's contract lives in four homes (types.ts doc comments, GUIDE §5,
  the spec, the contract tests); pricing and backend-type rules in three (spec, GATEWAY.md,
  GUIDE §7). The GUIDE is the most-changed doc for this reason (`7d51838` alone rewrote 48
  lines for one format bump). The preamble narrates history that git holds, against the
  project's documentation rule.
- **Proposed shape** — the GUIDE keeps what only it says: division of labour, hard rules,
  the wiring, database-specific advice for the store (the SQL sketch, representation notes,
  restore steps, pitfalls), the ledger, the LiteLLM import mapping, operating, testing,
  "things that look wrong". Per-method store contracts: point to `storage/types.ts` (the
  comments there are already complete) and the contract tests. Backend types and prices:
  one paragraph of "what your UI must know" plus links. Preamble: one line ("matches config
  format 5, protocol 5").
- **Payoff** — a store-contract change touches 3 places instead of 4; a provider/pricing
  change stops touching the GUIDE; ~150-200 lines less to keep true.
- **Cost / risk** — a doc edit. Risk: the external agent reading the GUIDE must open two
  more files — acceptable, it is already told to read the spec.
- **Confidence** — medium (a judgment on how self-contained the GUIDE must be; the user may
  want it standalone for the other repo's agent).

### F7 — Small items (each a few lines)
- **Kind** — in-function / cross-function.
- **Where / proposed**:
  - `index.ts:3` `libraryName` and `index.test.ts`: scaffolding, used by nothing else —
    delete both.
  - `config-publishing/index.ts:73-76` `configHash`: exported, used only by tests; the
    publish uses the private `hashOf`. Either make the publish use it or move it to a test
    helper.
  - Window keys: `storage/memory.ts:187-189` `windowKey` is a verbatim copy of
    `usage/aggregate.ts:16-18` `windowKeyOf`; the scope+type identity
    `JSON.stringify([group ?? null, type])` is written in `messages/semantic.ts:42`,
    `fastify/totals-feed.ts:58-60`, `sample/src/page/sections.ts:257-259` and GUIDE §9.
    Put `windowKeyOf` in `storage` (aggregate imports it), and export one
    `scopeTypeKey`/`windowIdentity` from `messages` for the rest.
  - `gateways/index.ts:236-247` and `usage/index.ts:200-214`: the same intake prologue
    (validate → map `"schema"` to `<x>-invalid` → join messages → `instance-mismatch`) and two
    identical error types (`StatusError`, `UsageBatchError`: `{code, message, status: 400}`)
    plus two identical `invalid()` helpers. One `IntakeError` type and one helper.
  - `messages/types.ts:94` `protocol_version: 5` duplicates `protocol/index.ts:10`
    `PROTOCOL_VERSION = 5`; `protocol_version: typeof PROTOCOL_VERSION` (messages → protocol
    adds no cycle) makes a protocol bump one constant + schema in this package.
- **Payoff** — ~40 lines and four duplicated rules.
- **Cost / risk** — trivial; no contract change.
- **Confidence** — high.

### F8 — Stream ordering is protocol logic, but lives in the Fastify adapter; the core exposes primitives only it uses
- **Kind** — cross-module (control-plane ↔ fastify; the fastify reviewer owns half of it).
- **Where** — `fastify/totals-feed.ts` (one totals read per push, join → first read after
  joining, change diffing with `windowStarts`), `fastify/gateway-stream.ts:55-123`
  (config read order and hash skip, complete-then-changes totals, `counted_through` per
  instance); core surface that exists for them: `readConfig`, `onConfigRead`, `ConfigRead.read`,
  `readTotals` (`TotalsRead.cursors`, `windowStarts`), `onDeliveryFailed`
  (`control-plane/index.ts:59,71,169-176`); GUIDE.md:147-162 tells a host serving streams
  another way to re-implement all of it.
- **Now** — the core's header says it "knows nothing of HTTP", yet the rules that make a
  stream correct across replicas (CONTROL-PROTOCOL Config stream → Order; Messages → Totals)
  are implemented in the adapter. Only `log: FastifyBaseLogger` and `reply.raw` tie them to
  Fastify.
- **How it got here** — the replicas plan (round 2, `635b126`) replaced store sequences with
  per-stream read ordering and put it where the streams were.
- **Proposed shape** — a framework-free `streams` module under the core:
  `controlPlane.openStream(instance, sink: { send(event, data): void; end(): void })`
  returning `close()`; it owns the totals feed, read ordering, complete-then-changes and
  ending streams on delivery failure. Fastify keeps hijack, headers, heartbeat, backpressure
  and stall detection. `ControlPlane` then drops `readConfig`, `onConfigRead`, `readTotals`'
  cursors and `onDeliveryFailed` from its public surface.
- **Payoff** — the ordering contract lives in the core next to what it orders; ~5 public
  members and the GUIDE's "if you serve streams yourself" paragraph go away; a second
  adapter would not need to re-derive it.
- **Cost / risk** — medium: ~250 lines move, backpressure (`writableNeedDrain` gating in
  `sendTotals`) must become a sink capability; fastify round-2/3 tests stay (they go over
  HTTP). Only one adapter exists, so the payoff is placement and API size, not reuse.
- **Confidence** — low-medium; raise it by checking with the fastify reviewer whether the
  backpressure coupling splits cleanly.

### Clean / leave as is
- Subsystem cuts and the boundary lint: acyclic, entry-only, and the cuts match the
  concepts (config document vs current config vs usage vs gateways vs storage). The only
  misplaced responsibility is the sweep (F1).
- `config` (validation, tree, resolution), `schemas`, `calendar`, `keys`, `protocol`,
  `messages` validators: clean.
- `schema/` copy: deliberate, guarded; leave.
- Store interface / memory store / contract tests triad: the intrinsic minimum; shrink the
  domain logic inside it (F3, F5), not the triad.

## Cross-module hints
- The "tokens that count against a limit" rule (tokens_in + tokens_cache_write + tokens_out;
  cost in nano-USD) is written twice: `usage/aggregate.ts:46-50` and Go
  `gateway/internal/limits/limits.go:629-638`, with no shared fixture (unlike config
  resolution, held by `protocol/fixtures/config/resolved`). It changed on 2026-10-05 on both
  halves by hand.
- `sample/src/page/sections.ts:280,292-295` recomputes UTC hour/month starts (third copy of
  `usage/windows.ts`), because the host totals API carries no window starts (F2).
- `fastify/gateway-stream.ts:113-115` duplicates `usage/index.ts:236-240` `countedThrough`.
- `fastify/totals-feed.ts:58-60` `windowIdentity` is the same key as
  `messages/semantic.ts:42` and the sample's `limitKey`.
- Tests across both packages hard-code `"5"` / `format_version: 5` (the protocol-5 commit
  `7d51838` touched ~10 test files for it); a shared test helper or `PROTOCOL_VERSION` would
  make the next bump smaller.
- `messages/types.ts` `TotalsLimitType` and `config/types.ts` `LimitType` are separate
  literal unions; the gateway has the same pair. Harmless, noted for the mirror inventory.
- The store-contract broken stores (`torn-store.ts` etc.) ship in the package (`files`
  excludes only `*.test.ts`).

## Bugs noticed in passing
- Not a live bug (no caller): `ControlPlane.stopExpirySweep()` on a started core leaves
  `started === true`, so a later `start()` returns early and the sweep stays off
  (`control-plane/index.ts:166-167,181-185`). Goes away with F1.
