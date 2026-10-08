# Building a control plane on kaiak-control

> For a coding agent (or a person) building the real kaiak control plane — the app with
> the UI — in its own repository, on this library. Read it top to bottom before writing
> code. It matches config format 5 and protocol version 5.
>
> Background, in this order: `docs/architecture/control-plane.html` (how gateways and a
> control plane work together, with diagrams), `docs/specs/CONTROL-PROTOCOL.md` (the
> contract, authoritative), `control/sample/` (a complete, minimal host app — the
> reference for everything below).
>
> Paths: `src/…` and `schema/…` are in this package; every other path is in the kaiak
> repository. `CONTROL-PROTOCOL.md`, `GATEWAY.md` and `BACKEND-VERIFY.md` are in its
> `docs/specs/`, which the package does not ship; references name their sections
> (`CONTROL-PROTOCOL.md` → Config → Prices).

## 1. The division of labor

kaiak gateways serve LLM traffic and never call the control plane on the request path.
The control plane owns config, usage totals and budgets. **Your app is the source of
truth for config**: it keeps the parts (keys, groups, models, backends) in its own
store, composes the whole document, keeps any history or audit of it, and decides who
may change what and when. `kaiak-control` takes the document your app hands it,
validates it, makes it the **current config** and broadcasts it to the gateways with
the usage totals. It keeps no earlier configs. It implements the **whole protocol
side**; your app supplies the rest.

| `kaiak-control` does | Your app does |
| --- | --- |
| The three gateway endpoints (`controlProtocolPlugin`) and their checks (token, protocol version, instance) | Everything a human touches: UI, login, roles, audit log |
| Config validation (the same schema and semantic rules the gateway runs), the current config and its `config_hash`, broadcasting it on the stream | Composing config documents (from forms, a DB model, an import), their history and audit, concurrent editing, and deciding when to publish |
| Usage intake: de-duplication, exactly-once counting, hour/month totals per scope, totals pushes | Long-term usage storage, reports, invoices (fed from the store — §6) |
| Gateway status, the live set, expiry, conflict flags | Showing them; alerting on them |
| Key generation and hashing (`createKey`) | Showing the plaintext key once; mapping keys to people |
| Checking a backend and reading its model metadata (`verifyBackend`, §8) | Calling it when a backend or model is added, admins only; deciding what goes into config |
| The storage interface + an in-memory reference store | A durable store implementing that interface (§5) |

Never re-implement a protocol piece in the app — no hand-rolled `/v1/usage` route, no
own totals arithmetic sent to gateways, no own config validator. If the library lacks
something the protocol needs, the change belongs in the kaiak repo (§11).

## 2. Hard rules

1. **How many processes your store allows is your store's choice.** The core keeps
   nothing replicas must agree on: the current config, counted batches and the live
   set are the store's, decided by conditional writes and
   announced through its `subscribe()` (§5). `createMemoryStore()` lives in one process:
   with it, run **one** process (the sample's protocol replicas share one store inside
   one process — a demonstration, not a deployment). A store that holds the contract
   across processes (a database with transactions and a change channel, §5) runs any
   number of replicas behind a load balancer. Prove it with the exported contract
   tests (§11) before running two.
2. **Every config goes through `controlPlane.publishConfig(doc)`.** Never write
   `publishConfig` on the store yourself: the core validates the document, checks the
   parents rule against the current config, and retries a publish that lost a race to
   another process's. A publish **replaces** the current config: two admins editing at
   once is your app's to arbitrate, before it publishes.
3. **Config holds key hashes, never keys.** `createKey(id)` returns `{ id, key, hash }`;
   store `id` + `hash`, show `key` once, never log or persist it.
4. **Backend secrets never enter config.** A backend names an environment variable
   (`api_key_env`, not starting with `KAIAK_` or `OTEL_`) set on the gateways; `base_url` carries
   no `user:password@`.
5. **The gateway endpoints only authenticate gateways.** The plugin checks the shared
   gateway token (`KAIAK_CONTROL_TOKEN`). Your UI and admin routes need their own
   authentication — the plugin does not protect them.
6. **Nothing sensitive in logs**: key IDs only, never keys, provider secrets, prompts
   or responses (usage records carry none of these by design).
7. **Treat protocol types as the library's.** Import `Config`, `Totals`,
   `GatewayStatus`, `UsageRecord` etc. from `kaiak-control`; do not redeclare them.

## 3. Getting the package

- `kaiak-control` is ESM and ships **TypeScript source** (`exports: ./src/index.ts`)
  run by Node's type stripping (Node current stable). It is `private` — not on npm.
- Node refuses to strip types in files under a `node_modules/` path. Consume the
  package so its real path is outside `node_modules`: an npm workspace, or a `file:`
  dependency (npm links it), pointing at `control/kaiak-control` of a kaiak checkout or
  submodule pinned to a release tag. A git or tarball dependency copies it into
  `node_modules` and fails at import with `ERR_UNSUPPORTED_NODE_MODULES_TYPE_STRIPPING`.
- The package carries its own copy of the JSON Schemas (`schema/`); it needs nothing
  else from the kaiak repo at runtime.
- It depends on `fastify` (5.x) and `ajv`. Use the same Fastify major in your app.
- Pin the kaiak version you build against: gateways and control plane must speak the
  same protocol version (a mismatch is refused on both sides — no multi-version
  support).

## 4. The minimal host app

This is the sample control plane's wiring (`control/sample/src/app/index.ts`) with the
file source removed. Start here; everything else is additions.

```ts
import Fastify from "fastify";
import { controlProtocolPlugin, createControlPlane } from "kaiak-control";
import type { ControlPlane } from "kaiak-control";

import { createPostgresStore } from "./store/index.ts"; // yours: §5

const app = Fastify({ logger: true });

const controlPlane: ControlPlane = createControlPlane({
  store: createPostgresStore(pool),          // or createMemoryStore() for a prototype
  token: process.env.KAIAK_CONTROL_TOKEN!,   // validate it at startup instead of `!`
  onListenerError: (error, event) => app.log.error({ err: error, event: event.type }, "control-plane listener failed"),
  onExpirySweep: (run) => {
    if (!run.ok) app.log.error({ err: run.error }, "gateway expiry sweep failed");
  },
});

// Mounts GET /v1/stream, POST /v1/usage, POST /v1/status.
// Starts the core (runs the expiry sweep) when the app is ready,
// ends open streams and stops the core on app.close().
app.register(controlProtocolPlugin, { controlPlane });

// Your own routes: UI, config editing, keys, reports — behind your own auth.
app.register(adminRoutes, { controlPlane, prefix: "/admin" });

const close = () => app.close().then(() => process.exit(0), () => process.exit(1));
process.on("SIGTERM", close);
process.on("SIGINT", close);

await app.listen({ host: "0.0.0.0", port: 8090 });
```

- Gateways get `KAIAK_CONTROL_URL=https://<host>` (the base; they append `/v1/...`)
  and the same `KAIAK_CONTROL_TOKEN`.
- Until the first config is published, a gateway's stream stays open with nothing on
  it, and the gateway waits for its first config; publish on startup (the sample reads
  its file in an `onReady` hook). Usage batches are counted all the same.
- Plugin options (all optional): `prefix` (default `/v1` — gateways expect `/v1`, so
  leave it), `heartbeatIntervalMs` (15 000), `totalsPushIntervalMs` (1 000),
  `stalledStreamTimeoutMs` (30 000).
- Core options worth knowing: `recentRecordsSize` (100), `gatewayLiveTimeoutMs` (30 000),
  `gatewayForgetAfterMs` (1 h), `batchCursorRetentionMs` (7 days),
  `deliveryRetryDelaysMs` (100, 200, 400, 800, 1600: retries of a failed read of the
  current config). Defaults match the gateway's timings; change them only with a
  reason.
- **Serving streams without the plugin.** The stream rules — what goes out on connect,
  in which order, the totals, slow readers, a config that cannot be read — are
  `CONTROL-PROTOCOL.md` → Config stream, and the Fastify plugin implements them
  (`src/fastify/`). A host serving streams some other way follows the same rules with
  the core's reads: `readConfig()` and `onConfigRead()` (each read carries its place in
  the order this process issued them), `readTotals()`, and `onDeliveryFailed()` (the
  current config could not be read after a change, retries included — the plugin logs
  it and ends its streams, so gateways reconnect). Read that spec section and the
  plugin before writing one.
- `stop()` releases the core's store subscription; `start()` takes it again and
  catches up on what changed meanwhile.
- Several cores over one store are several `createControlPlane({ store })` calls —
  one per process in a real deployment (§2, rule 1). The sample shows the shape inside
  one process: `KAIAK_SAMPLE_PROTOCOL_PORTS` adds protocol replicas, each a core and a
  Fastify instance of its own over the sample's one in-memory store. The sample stays a
  single process; its replicas are a demonstration and a test harness.

## 5. Implementing the store

`ControlPlaneStore` is the only thing you must implement. Every method is async.

- **The contract**: `src/storage/types.ts` — the comment on each method and type says
  what it must hold. The guarantees across processes are `CONTROL-PROTOCOL.md` →
  Control-plane processes.
- **The reference**: `createMemoryStore()` (`src/storage/memory.ts`). Read it first,
  then mirror its behavior method by method.
- **The check**: `storeContractTests` from `kaiak-control/store-contract` (§11). Run
  it against your store.

In short, the store is where control-plane processes agree. Every write that changes what
gateways are sent — a publish, a counted batch, a change to the live set — is
**conditional** on what the core read, so two processes never both win; a refused
write changes nothing and the core decides again. The windows and the batch cursors are
read in **one snapshot**. Every change, by any process, reaches every process through
`subscribe()` — with a **catch-up** whenever the store's change channel comes back from
a disconnect. The store keeps **no order of its own**: each core orders what it sends
on a stream by when it issued each read, so nothing the store returns needs to arrive
in order.

The rest of this section is how that maps onto a database. Two things that are easy
to miss:

- **A read after a notification sees the change.** A core that hears of a publish
  reads the current config, of a gateway change the gateway records, of a counted
  batch the totals — make every one of those reads on the **primary**. A read replica
  can lag the commit the notification announced, and a config read that misses a
  publish keeps that core's gateways on the old config (a revoked key still works)
  until the next publish.
- **Every call settles.** Set a statement timeout: a call that never returns holds
  that core's config deliveries, which run one at a time, with nothing reported.

Several processes need their clocks kept in step: NTP-synchronized, within a second.
Each core picks the current hour and month from its own clock, and the expiry sweep
compares its clock with receipt times other processes wrote.

Per method:

- **Batch cursors: one per (instance, epoch)** (`lastBatches`, `saveCountedBatch`),
  never one per instance: a write stalled on an earlier epoch would read a newer
  epoch's batch as a new epoch's start and count again.
- **`totalsSnapshot` is one snapshot**: one `REPEATABLE READ` transaction, or one
  statement. Reading the cursors in a statement of their own is the classic mistake:
  the contract tests are likely to catch it, not certain — against a real database it
  shows only when a batch commits between the two statements.
- **Gateway revisions come from a store-wide sequence** (gaps are fine), never from
  the record itself (`revision + 1`): a gateway forgotten and recreated, or a store
  restored (below), would hand out a revision again.
- **`recentRecords` needs an insertion-order column**: records with equal receipt
  times still come back in a defined order.
- **`dropPastWindowTotals` may keep past windows** for your reports: the core only
  reads current windows.
- **The change channel** behind `subscribe`: Postgres `LISTEN`/`NOTIFY` sent in the
  writing transaction, or Redis pub/sub. A channel drops what is sent while it is
  down, so every time it reconnects, announce `{ type: "catch-up" }` to every
  subscriber once it listens again. The core does not poll: a change the channel
  missed reaches nobody until that catch-up.

Representation notes:

- **Window totals are `bigint`** (tokens, or nano-USD). Store them as `NUMERIC` /
  `DECIMAL(20,0)` or `BIGINT` — the protocol caps a window at 18 digits (< 10^18), so a
  signed 64-bit integer holds it. Never a float.
- **Window identity** is `(group, type, windowStart)`; `group` is absent for global.
  Normalize it to a stable non-null value for a primary key (primary-key columns
  cannot be `NULL`): e.g. `''` for global (a group ID is never empty). Usage counts
  toward every scope on a record's path whatever the config's limits, so windows exist
  for groups without a limit too, and totals carry them all. Windows are keyed by the
  group **ID**: those of a deleted group age out with their hour or month — and a group
  created again under that ID within the window carries on with them (§7).
- Times are milliseconds since the epoch, by the control plane's clock.
- **Return numbers as numbers.** Batch sequences, `countedAt`, `publishedAt` and
  revisions are JavaScript numbers; window totals are `bigint`. node-postgres returns
  `int8` and `numeric` columns as strings by default — convert them (or set a type
  parser), or the contract tests' equality checks fail on `"1"` against `1`.
- **Store the config as text** (`text`), never as `jsonb`: `jsonb` does not keep member
  order, and the text sent must stay the text hashed. The core has already validated
  it.
- **Return copies.** Gateway records returned must not share mutable state with what
  callers hold (the memory store `structuredClone`s).

A sketch, not a prescription (Postgres):

```sql
create table current_config (id int primary key check (id = 1),   -- at most one row
                             text text not null, hash text not null, published_at bigint not null);
create table batch_cursor (instance text, epoch text, sequence bigint not null,
                           counted_at bigint not null, primary key (instance, epoch));
create table window_total (group_id text not null,       -- '' for global: a key column is never null
                           type text, window_start bigint,
                           used numeric(20,0) not null, primary key (group_id, type, window_start));
create table usage_record (record_id text primary key, received_at bigint not null, record jsonb not null,
                           saved bigint generated always as identity);   -- recentRecords: order by saved desc
create table gateway      (instance text primary key, revision bigint not null, live boolean not null,
                           state jsonb not null);
create sequence gateway_revision;   -- never hands out a value twice, so revisions never repeat
```

Each conditional write is one transaction that fails cleanly when its condition does
not hold, and notifies on commit. A unique violation on a first write (two processes
inserting the config's row, the first cursor of an epoch, or the first record of a
gateway) is the condition failing too: roll back and return `{ saved: false, … }` with
what the store holds now, never an error. Batches of different instances touch the same
window rows (global's, a shared group's): add a batch's windows **in key order**, so
two batches never lock the same rows in opposite orders and deadlock.

```sql
-- publishConfig: the current hash is the condition.
begin;
  -- $expectedHash undefined (none published yet):
  insert into current_config values (1, $text, $hash, $publishedAt)
    on conflict (id) do nothing;                          -- 0 rows: refused, roll back
  -- otherwise:
  update current_config set text = $text, hash = $hash, published_at = $publishedAt
    where id = 1 and hash = $expectedHash;                -- 0 rows: refused, roll back
  select pg_notify('kaiak', json_build_object('type', 'config-published', 'hash', $hash)::text);
commit;

-- saveCountedBatch: compare-and-set on the batch's epoch's cursor.
begin;
  -- $expectedSeq set: update that cursor; none yet in the epoch: insert it.
  update batch_cursor set sequence = $seq, counted_at = $at
    where instance = $instance and epoch = $epoch and sequence = $expectedSeq;
  insert into batch_cursor values ($instance, $epoch, $seq, $at);
                                  -- 0 rows / unique violation: refused, roll back
  insert into window_total … on conflict (group_id, type, window_start)   -- in key order
    do update set used = window_total.used + excluded.used;
  insert into usage_record … on conflict (record_id) do nothing;   -- your ledger (§6)
  select pg_notify('kaiak', …'batch-counted'…);
commit;

-- saveGateway: the revision is the condition.
begin;
  -- $expectedRevision set: lock the record at that revision.
  select live from gateway where instance = $instance and revision = $expectedRevision
    for update;                                           -- 0 rows: refused, roll back
  update gateway set state = $state, live = $live, revision = nextval('gateway_revision')
    where instance = $instance returning revision;
  -- none stored: insert into gateway values ($instance, nextval('gateway_revision'),
  --   $live, $state) returning revision;                  -- unique violation: refused
  select pg_notify('kaiak', …'gateways-changed', liveChanged…);   -- on every write:
                                  -- liveChanged = the live read above (false when
                                  -- inserting) differs from $live
commit;
-- forgetGateways: delete … where instance = $instance and revision = $revision
-- returning live, one instance after another in the order given (the core sorts them
-- by instance, so two sweeps never lock the same rows in opposite orders); one
-- notification for the call, liveChanged when a live one went.
```

Run `totalsSnapshot` in one `REPEATABLE READ` transaction. Like every read the core
makes after a notification, it goes to the primary.

```sql
begin isolation level repeatable read;
  select group_id, type, window_start, used from window_total
    join unnest($types::text[], $starts::bigint[]) as current (type, window_start)
      using (type, window_start);          -- $types, $starts: the entries of `current`
  select instance, epoch, sequence from batch_cursor;
commit;
```

Every process `LISTEN`s on the channel from a **dedicated connection** — a pooler in
transaction mode (PgBouncer) cannot hold a `LISTEN` — and passes each payload to its
subscribers; `pg_notify` inside the transaction is delivered only on commit, in commit
order. When that connection drops, open a new one, `LISTEN` on it, and only then
announce a `catch-up` to the subscribers, so their reads see everything committed
while the channel was down.

**Restoring the store.** A store restored from a backup, or failed over to a standby
that was behind, is simply the current state: the cores read it and send its config
and totals like any other. Restore it this way:

1. Stop every control-plane process.
2. Restore.
3. Move the gateway revisions past every value handed out before the restore
   (`select setval('gateway_revision', <a value above any issued>)`). A restored
   sequence would hand out revisions that writes computed before the restore still
   hold, and such a write could then expire or overwrite a gateway that reported after
   it.
4. Start the processes. Each start reads the current config, totals and gateways.

A restore under running processes, with the change channel up, is announced to
nobody: their streams keep the old state until the next change.

## 6. Usage records: your ledger

The core keeps only what enforcement needs: current hour/month totals and the last
`recentRecordsSize` records (a live view, not a history). **For billing, reports and
audit, persist records in `saveCountedBatch`**: `counted.records` holds every record of
a batch counted *now*, so a table written in that same transaction is your usage
ledger.

- Insert its records **idempotently by `record_id`** (`on conflict (record_id) do
  nothing`). Within the batch cursor retention (7 days) a resent batch is never passed
  again, but a gateway resending a batch after the retention has it counted again in
  the enforcement windows (`CONTROL-PROTOCOL.md` → Status intake → Batch cursor
  retention). A plain insert would then fail the whole transaction on the duplicate
  key, every retry the same, and block that gateway's usage for good. With the
  idempotent insert the ledger never charges a record twice.
- `keepRecords` only bounds what `recentRecords` must return; your ledger table can
  keep everything.

What a record carries (`UsageRecord`, `schema/usage-record.schema.json`): record and
request IDs, gateway instance, key ID, `groups` (the key's group path, top-level
first, 1–8 IDs, as the gateway's config had it), public model, deployment (backend +
backend model), units (`tokens_in`, `tokens_cached`, `tokens_cache_write`,
`tokens_out`, `tokens_reasoning`), `cost_nano_usd`, flags `estimated` and `partial`,
`gateway_time`. Raw units travel beside the cost, so you can re-price.

## 7. Publishing config

The config document is the one format everything shares: `schema/config.schema.json`
in the package, `examples/config.json` in the kaiak repo, field meanings in
`CONTROL-PROTOCOL.md` → Config. Top level: `format_version: 5`, `global`, `backends`,
`models`, `keys`, and optional `groups` — each collection an object keyed by ID.

**Backends and models: what a UI must know.** The rules are `CONTROL-PROTOCOL.md` →
Config → Backend types, and `GATEWAY.md` → Providers (each type's behavior, Endpoint
support, Base URLs) and → Model metadata.

- A backend's `type` names its server and picks the gateway's module for it;
  `BACKEND_TYPES` lists them, for a form's choices. Offer the server's own type
  whenever it has one, and `openai-compatible` only for a server without one: the
  backend answers the same under either, but only its own type gets that server's
  rules — an OpenAI backend left as `openai-compatible` runs on the service tier its
  clients ask for, and priority bills about twice the prices config holds.
  `verifyBackend` notes a vLLM or llama-server answer under another type (§8).
- The type decides which client APIs reach a model: the gateway passes each request
  through to a backend speaking the same API and translates nothing, so a model gets
  the endpoints its deployments' types serve (a Claude model is Messages-only).
- `openai`, `azure-openai`, `anthropic` and `azure-anthropic` require `api_key_env`.
  What `base_url` holds depends on the type, and no type has a default: the
  descriptions of `type` and `base_url` in `schema/config.schema.json` give each
  type's form, with examples — use them as a form's help text.
- A model's `metadata` (`context_length`, `capabilities` — `MODEL_CAPABILITIES` lists
  those config requires — and `reasoning_efforts`) is information for clients;
  `output_limit` is the one request parameter a model's config sets, because limits
  reserve it. The gateway applies no other request defaults (temperature, a chat
  template's switches) and config has no field for them: suggested settings your UI
  shows people are your app's data, outside config.

**The group tree** (`CONTROL-PROTOCOL.md` → Config → The group tree): each group is
`{ parent?, labels?, allowed_models?, limits?, child_defaults? }`; no `parent` = a
top-level group, `global` is the root above them, at most 8 levels. Your domain
model decides what the levels mean (team → project → env → workload, a `users` group
whose children are people, …); the config carries no meaning for them — put the
kind of node, a cost center or an owner in `labels`, which gateways ignore. Who may
edit which group or mint keys in it is your app's business, never config.

- `allowed_models` intersect down the path; no list anywhere on a path = every model.
- `child_defaults` (`allowed_models`, `limits`) apply to **direct children** only; a
  child's own list replaces the default list, a child's limit replaces the default
  limit of the same type.
- **`child_defaults` is a default, not a ceiling**: since a child's own list or limit
  replaces it, a child can be given more models or a higher value. A hard
  restriction for a whole subtree goes on the parent's **own** `allowed_models` and
  `limits`, which bind every group below it. For a `users` group: the models and
  budget no person may exceed are `users`' own; its `child_defaults` are what each
  person gets unless their own group says otherwise.
- **Counters are bounded**: every gateway keeps an hour and a month counter for
  global and every group, limited or not, plus one counter per effective per-minute
  limit (its own merged with its parent's `child_defaults.limits`); a config may
  allocate at most **50 000** (`counters-exceeded`) — so at most about 25 000 groups,
  fewer with per-minute limits. Defaults multiply: D default per-minute limits on a
  group with N children are D × N. Before offering "a default limit for everyone" in
  a UI, check what it costs across the children.
- A group's **parent never changes**. To move a group, delete it and create a new
  one (a new ID, or the same ID in a later publish).
- **A group ID used again resumes its window's spend**: windows are keyed by the ID,
  so a group deleted and created again with the same ID within the same hour or
  month — a move included, whatever its new parent — gets that window's spend back.
  **For a fresh budget, use a new ID.**
- **Editing a limit keeps its spend**: a scope holds at most one limit per type, so a
  limit is its scope and type; a new value keeps the window. Usage counts toward
  every scope on a record's path whatever the config's limits, so **a limit added
  mid-window starts with the window's usage so far**, and one removed and added back
  shows everything counted meanwhile.

**Prices: what a UI must know.** The rules are `CONTROL-PROTOCOL.md` → Config →
Prices, Tiered prices, and Units and price units; how each backend type keeps
requests at standard prices is `GATEWAY.md` → Providers → Service tier, and Standard
price on Anthropic types.

- A model's `prices` is a dated list; a model without one is free (no USD limit
  refuses it). When a price changes, add an entry dated from the change and keep the
  old ones, so records re-priced later use the price of their day.
- Each entry's `tiers` hold per-million USD rates for `tokens_in`, `tokens_cached`,
  `tokens_cache_write` and `tokens_out` — one tier at 0, plus one per long-context
  threshold the provider bills at. Each tier lists all its own prices, nothing is
  inherited from the tier below, and a cache unit a tier leaves out is charged at
  that tier's `tokens_in`.
- Enter standard-tier rates: the gateway keeps `openai`, `azure-openai`, `anthropic`
  and `azure-anthropic` requests at standard prices, and the self-hosted types bill
  no tier.
- **Azure deployment types price differently**: Data Zone (EU/US) is about 10%
  above Global for the same model. Price a model at the rate of the deployment type
  its deployments use.

**Importing a price list is your app's job.** LiteLLM publishes one
(`model_prices_and_context_window.json` in the BerriAI/litellm repository, keys such
as `azure/gpt-5`, `azure/eu/gpt-5`).

- Its per-token USD fields map to per-million prices, all in the tier at 0:
  `input_cost_per_token` × 10⁶ → `tokens_in`, `cache_read_input_token_cost` × 10⁶ →
  `tokens_cached`, `cache_creation_input_token_cost` × 10⁶ → `tokens_cache_write`,
  `output_cost_per_token` × 10⁶ → `tokens_out`.
- A field ending in `_above_<N>k_tokens` is a long-context rate and goes to the tier
  at N × 1000: `input_cost_per_token_above_272k_tokens` → the 272000 tier's
  `tokens_in`, and the same for `cache_read_input_token_cost_…`,
  `cache_creation_input_token_cost_…` and `output_cost_per_token_…`.
- A tier prices every unit itself, so when the list gives no long-context rate for
  `tokens_in` or `tokens_out`, copy that unit from the tier at 0 (leaving `tokens_out`
  out would make its output free). Copy nothing else: a cache unit with no
  long-context rate stays out of the tier, and is charged at that tier's own
  `tokens_in`. Copied, the tier at 0's cache-write rate would price long-context
  writes below the tier's plain input; the fallback errs high instead.
- The other fields are not priced by kaiak: the `_priority`, `_flex`, `_batches` and
  `_ultrafast` variants (service tiers the gateway never uses),
  `cache_creation_input_token_cost_above_1hr` and its `_above_1hr_above_<N>k_tokens`
  variants (a one-hour cache lifetime, not a long-context rate: kaiak counts writes
  as one unit, as Azure reports them), audio, image and per-second rates (endpoints
  the gateway does not serve), and `search_context_cost_per_query` (a per-search
  fee).
- Azure deployment names are yours, so the mapping from a model to its LiteLLM key is
  a setting in your app, not something to guess from names.
- The list carries no dates and is community-kept: add an entry dated today only when
  a price changed, and let a person review it before publishing.

Typical flow for a UI edit:

1. Load your domain model (DB rows) and build a complete `Config` from it. The
   control plane always publishes whole documents — no patches.
2. Call `validateConfig(doc)` for immediate form feedback; each issue has `code`,
   `message` and a JSON Pointer `path`. Stable codes (`key-group-unknown`,
   `group-cycle`, `limit-duplicate`, `output-limit-above-context`, …) are listed in
   `CONTROL-PROTOCOL.md` → Config → Semantic rules.
3. Call `controlPlane.publishConfig(doc)`: `{ ok: true, published: { config, text, hash, publishedAt } }`
   or `{ ok: false, issues }` (nothing published, the current config stays). Validation
   runs again inside; step 2 is for UX only. Publishing also compares with the current
   config: a group given another `parent` than it has there is refused
   (`group-parent-changed`, path `/groups/<id>/parent`) — `validateConfig` cannot see
   this, so surface publish issues in the UI too.
4. Watch the result land: `gateways()` shows each gateway's
   `status.applied_config_hash` — the `hash` publish returned once it applied it — or
   `status.last_rejection` (`{ config_hash, codes }`) when a gateway refused it
   (typically a backend `api_key_env` not set on that pod). Keep the hashes your app
   published, beside your own history of the documents, to tell which edit a gateway
   runs.

Keys: `createKey(id)` — `id` matches `^[A-Za-z0-9][A-Za-z0-9._@-]{0,127}$` (throws
`key-id-invalid` otherwise); `isConfigId(id)` checks that shape, the same for key and
group IDs — check a group ID before minting a key for it. Add `{ hash, group }` under
`keys[id]` — any group, leaf or not — and publish. Revoking a key = removing it, or setting `disabled: true`
or an `expires_at`, and publishing. Keys have no limits of their own: a request
passes the limits of its key's group, every ancestor and global, and its usage
counts toward each of them.

## 8. Verifying a backend when it is added

Model metadata is declared in config: the gateway serves what config says and asks
backends nothing. `verifyBackend` fills the declaration in for you. The contract,
field by field, is `BACKEND-VERIFY.md`.

- **When**: when an operator adds a backend, and when they add a model (a deployment's
  backend-side name) — the moment your form is about to write a `base_url` or a
  `metadata` block. Never on a schedule, never from a request.
- **Call**: `verifyBackend({ type, baseUrl, credential?, model?, timeoutMs?, signal? })`
  with `type` and `baseUrl` exactly as config will hold them, and the credential
  **value** your app has (config names an env variable; the helper needs the key
  itself). Invalid input throws `verify-input-invalid` and sends nothing.
- **The report** (`BackendReport`, plain JSON — show it or store it as it is):
  - `ok`, and `failure: { code, message }` when not: `unreachable`, `timeout`,
    `credential-refused`, `not-a-models-list`, `model-not-listed`. Branch on the code;
    `message` and the `notes` are for people.
  - `server`: `vllm`, `llama-server` or `unknown` (OpenAI, Azure, Anthropic, anything
    else — only reachability, the credential and the listed ids are checked there;
    for `anthropic` also `context_length`, from the list's `max_input_tokens`).
    Recognized from the answer, whatever `type` was given; a `vllm` or `llama-server` under
    another type adds a note naming the type to use — show it before the operator
    saves the backend.
  - `models[]`: every listed model, with what the server reports (`context_length`;
    for llama-server also `capabilities.vision`, `tools`, `reasoning`) and a `sources`
    entry per value: the URL and field it came from, and `hint`.
  - `metadata`, when `model` was given and the check passed: the reported values under
    their config names.
- **`metadata` is partial.** Merge it into the model's `metadata`, then have the
  operator complete the rest: `capabilities.streaming` always, every capability not
  reported (`MODEL_CAPABILITIES` lists those config requires), `reasoning_efforts`.
  A value the backend does not report is absent, never guessed — do not default it in
  your form either; leave it for the operator to decide, and publishing refuses the
  config until they do.
- **Hints stay out of `metadata`.** `tools` and `reasoning` from llama-server are read
  from its chat template's capabilities and can be wrong (an embedding model reports
  `tools: true`); they are in `models[]` with `hint: true`. Prefill a form from them
  if you like, labelled as unconfirmed — never copy them into config silently.
- **Admin-only.** The helper fetches a URL the caller supplies and sends a credential
  to it, and private addresses are allowed (backends live there). Expose it only to
  people allowed to configure backends, behind your app's own authentication (hard
  rule 5); anyone else could use it to probe your network or send a key elsewhere.
- **`azure-anthropic` is not checkable.** Foundry has no models list, so the helper
  sends nothing and answers `ok: false` with the failure code `not-checkable` — never
  a report that reads as verified: reachability and the credential show only at the
  gateway's first request.
- **Nothing is saved.** A backend restarted with another context size leaves config
  stale until someone verifies again.

The sample's `verify` command wraps it for a config file:
`npm run verify -w sample -- --base-url <url> [--type <type>] [--api-key-env <NAME>] [--model <name>]`
(`control/sample/src/verify/`).

## 9. Reading state for the UI

| Need | Call |
| --- | --- |
| The current config | `currentConfig()` → `{ config, text, hash, publishedAt }` (`text` is what the store keeps and the stream sends; `config` is it parsed) |
| Gateways, health, what they run | `gateways()` → `{ instance, status, receivedAt, live, conflict? }[]`; `liveGateways()` |
| Spend vs limits (current windows) | `readTotals()` → `{ windows, liveGateways, windowStarts, cursors }` + `resolveScopes(config)`, matched by scope and type — below |
| A group's path, effective limits and models | `resolveScopes(config)` → `{ group?, path, limits, allowed_models? }[]` — global first (no `group`, `path` `[]`), then every group in config order; `allowed_models` absent = every model |
| Live usage feed | `recentRecords()` |
| Push updates to browsers | `onConfigPublished`, `onTotalsChanged`, `onGatewaysChanged` (each returns an unsubscribe) |

Totals vs limits (as the sample's status page does, `control/sample/src/page/sections.ts`):

```ts
const current = await controlPlane.currentConfig();
const totals = await controlPlane.readTotals();
if (current) {
  const used = new Map(totals.windows.map((w) => [scopeTypeKey(w.group, w.type), BigInt(w.used)]));
  for (const { group, limits } of resolveScopes(current.config)) { // group undefined = global
    for (const limit of limits) {
      // Per-minute limits are enforced on gateways and never counted here.
      if (!isCountedType(limit.type)) continue;
      const spent = used.get(scopeTypeKey(group, limit.type)) ?? 0n; // absent = nothing used
      // USD amounts are nano-USD. The window is totals.windowStarts[limit.type].
    }
  }
}
```

- `used` is a decimal **string**: always `BigInt(...)`, never `Number(...)`.
- Listeners fire often (every counted batch, every status). Coalesce before pushing to
  browsers (the sample pushes at most once a second) and read fresh state when you
  push rather than queueing values.
- A listener that throws goes to `onListenerError`; by default that rethrows and
  crashes the process — keep listeners total, or log in `onListenerError`. The
  event says which: `config-published`, `totals-changed`, `gateways-changed` or
  `delivery-failed`; the publish, count or status it came from has succeeded either
  way, and the next config delivery goes ahead. Listeners hear the changes of every
  process over the store, not only this one's. `onConfigPublished` may hear one config
  more than once (a publish here is heard for its own read and for the store's
  announcement; a catch-up rereads it): compare hashes if that matters.

## 10. Operating it

- **TLS** in front of the control plane outside local runs; the token is a bearer
  secret. Tokens never go in URLs.
- **Proxies must not buffer or cut the stream**: `GET /v1/stream` is SSE held open for
  hours (it sets `X-Accel-Buffering: no` and `Cache-Control: no-cache`); give it an
  idle timeout well above the 15 s heartbeat and disable response compression for it.
- **Body limits** are set by the plugin (usage 2 MiB, status 64 KiB); a proxy limit
  below that breaks usage delivery.
- **Restarts** are safe with a durable store: gateways keep serving, hold usage,
  reconnect and get the current config and totals again. Priced models under a USD
  limit are refused by gateways only after the outage passes
  `global.control_outage_grace_ms` (15 min default) — keep restarts well below it.
- **Replicas** (with a store that holds the contract across processes, §5): run them
  behind any load balancer, no stickiness needed. A gateway whose replica goes away
  reconnects to another and gets the current config and totals from it. **Rolling
  updates** are fine: old and new processes run side by side over the store. With the
  in-memory store, run one process.
- Token holders can report usage and status under any instance name (one shared token
  by design); treat `KAIAK_CONTROL_TOKEN` like a provider key.
- Logging: Fastify's pino. JSON in production; for development, `pino-pretty` with
  `translateTime: 'SYS:HH:MM:ss.l'`, `singleLine: true`,
  `ignore: 'pid,hostname,reqId,req.host,req.remoteAddress,req.remotePort'`
  (the sample's `control/sample/src/logging/`).

## 11. Testing

- Run the store contract against your store from a `node:test` file:

  ```ts
  import { storeContractTests } from "kaiak-control/store-contract";
  storeContractTests({ name: "postgres", create: makeEmptyStore, attach: connectAgain, dispose });
  ```

  `attach` gives a second handle on the same store, as a second process holds it
  (another pool and listener): the tests race writes across both handles and listen
  through the other. Pass `reconnect(handle, whileDown)` too — it takes a handle's
  listening connection down, runs `whileDown` (writes the channel then misses), and
  lets it come back — and the suite checks the catch-up reaches every subscriber. A
  store whose channel can drop changes must pass it; only one that cannot (the
  in-memory store) may leave it out.
- The kaiak repo runs the suite against the memory store behind a lossy channel
  (`src/store-contract/lossy-channel.ts`), and against deliberately broken stores — a
  cursor read outside the snapshot, a channel that comes back without a catch-up, a
  catch-up told to one subscriber only, a publish announced before its write is
  visible (`control/kaiak-control/src/store-contract/negative-control.test.ts`): it
  must fail each, exactly at the tests naming the broken guarantee, and does.
- Then drive the core with your store. The kaiak-control tests in the kaiak repo
  (`control/kaiak-control/src/**/*.test.ts`; the package does not ship them) show how
  to call `acceptUsageBatch`, `acceptStatus` and `publishConfig` directly without
  HTTP, two cores over one store included.
- Validate against the shared fixtures in the kaiak repo's `protocol/fixtures/` (valid
  and invalid configs and messages, with `cases.json` naming each expected code).
- End to end: run a real `kaiak` binary (or the gateway image) against your app with a
  fake OpenAI-compatible backend (`gateway/internal/fakebackend/cmd/fakebackend`), then
  check a request's usage arrives exactly once, a config edit reaches the gateway, and
  a control-plane restart loses nothing. The kaiak repo's cross-half e2e
  (`gateway/e2e/sample_test.go`, build tag `crosshalf`) does this against the sample,
  and `gateway/e2e/replicas_test.go` with two gateways on two cores over one store.

## 12. Things that look reasonable and are wrong

- Running two replicas over a store that does not hold the contract across processes
  — the in-memory store, or a database store without conditional writes or a change
  channel. Nothing refuses it: totals go out of order or miss pushes, and spend is
  counted twice or lost. Run the contract tests (§11) with `attach` first.
- One batch cursor per instance, a gateway revision kept on the record itself
  (`revision + 1`), or a `LISTEN` through a transaction-mode pooler — each passes a
  quick test and fails under replicas (§5).
- A plain `insert` into a ledger keyed by `record_id` — a batch resent after the
  cursor retention then blocks its gateway for good (§6).
- Sending gateways a remaining *allowance* per scope — they must get totals; the
  library already does this.
- Treating `recentRecords()` as the usage history — it is the last 100.
- Parsing `used` or `cost_nano_usd` sums as JavaScript numbers — sums must be `BigInt`.
- Rebuilding config from the previous published document plus a patch without
  validating — always build whole and publish through the core.
- Moving a group by editing its `parent` — refused (`group-parent-changed`). A move
  is a delete and a create; the old group's spend stays with its old ancestors, and
  a group created again under the same ID within the window resumes that ID's spend
  (§7) — use a new ID for a fresh budget.
- Giving tree levels a meaning in config (a `kind` field, a level that must be a
  "team") — there is none; use `labels` and keep the meaning in your app.
- Adding children's windows to their parent's — a record counts toward every group
  on its path, so a parent's window already holds its descendants' spend. Totals
  exist only for the hour and month windows; for reports, group your ledger (§6) by
  any prefix of the records' `groups`.
- Restoring the store from a backup and expecting gateways to keep the newer config —
  they get the restored one: the control plane is the authority, and every core sends
  the store's current config once its change channel catches up. Publish your app's
  current config again after a restore if the backup is older than it.
- Editing `kaiak-control` inside your app. Protocol or library changes land in the
  kaiak repo, on both halves and the spec at once, then you move your pin.
