# Building a control plane on kaiak-control

> For a coding agent (or a person) building the real kaiak control plane — the app with
> the UI — in its own repository, on this library. Read it top to bottom before writing
> code. Written 2026-09-26 against kaiak 0.6.0; brought to the group tree (config
> format 2, protocol version 2) on 2026-09-27; to tiered prices (config format 3,
> protocol version 3) on 2026-09-29; to a backend type per server (formats unchanged)
> on 2026-10-01; to the cache-write unit (config format 4, protocol version 4) on
> 2026-10-02; to Messages and Responses passthrough — the Anthropic backend types, no
> model defaults (config format 5, protocol version 5) — on 2026-10-06.
>
> Background, in this order: `docs/architecture/control-plane.html` (how gateways and a
> control plane work together, with diagrams), `docs/specs/CONTROL-PROTOCOL.md` (the
> contract, authoritative), `control/sample/` (a complete, minimal host app — the
> reference for everything below).

## 1. The division of labor

kaiak gateways serve LLM traffic and never call the control plane on the request path.
The control plane owns config, usage totals and budgets. `kaiak-control` implements
the **whole protocol side** of that; your app supplies the rest.

| `kaiak-control` does | Your app does |
| --- | --- |
| The four gateway endpoints (`controlProtocolPlugin`) and their checks (token, protocol version, instance) | Everything a human touches: UI, login, roles, audit log |
| Config validation (the same schema and semantic rules the gateway runs), versioning, history, the config stream, `resync` | Building config documents (from forms, a DB model, an import) and deciding when to publish |
| Usage intake: de-duplication, exactly-once counting, hour/month totals per limit, totals pushes | Long-term usage storage, reports, invoices (fed from the store — §6) |
| Gateway status, the live set, expiry, conflict flags | Showing them; alerting on them |
| Key generation and hashing (`createKey`) | Showing the plaintext key once; mapping keys to people |
| Checking a backend and reading its model metadata (`verifyBackend`, §8) | Calling it when a backend or model is added, admins only; deciding what goes into config |
| The storage interface + an in-memory reference store | A durable store implementing that interface (§5) |

Never re-implement a protocol piece in the app — no hand-rolled `/v1/usage` route, no
own totals arithmetic sent to gateways, no own config validator. If the library lacks
something the protocol needs, the change belongs in the kaiak repo (§11).

## 2. Hard rules

1. **One control-plane process per store.** Config versions, the totals revision and
   stream subscriptions live in one process. The core takes a **lease** in the store at
   start and refuses to start (`store-lease-held`) while another process holds it.
   Deploy exactly one replica (Kubernetes: `replicas: 1`, `strategy: Recreate`). A
   second replica is not "HA" — it is refused, or worse if your store's lease is wrong.
2. **Every config goes through `controlPlane.publishConfig(doc)`.** Never write
   `saveConfig` yourself. Publishing runs in the totals' turn so budgets stay consistent
   across limit edits.
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

// Mounts GET /v1/config, GET /v1/stream, POST /v1/usage, POST /v1/status.
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
- Until the first config is published, gateways get `503 config-unavailable` and keep
  retrying; publish on startup (the sample reads its file in an `onReady` hook).
- Plugin options (all optional): `prefix` (default `/v1` — gateways expect `/v1`, so
  leave it), `heartbeatIntervalMs` (15 000), `totalsPushIntervalMs` (1 000),
  `stalledStreamTimeoutMs` (30 000).
- Core options worth knowing: `configHistorySize` (100 versions a stream can resume
  across), `recentRecordsSize` (100), `gatewayLiveTimeoutMs` (30 000),
  `gatewayForgetAfterMs` (1 h), `batchCursorRetentionMs` (7 days). Defaults match
  the gateway's timings; change them only with a reason.

## 5. Implementing the store

`ControlPlaneStore` (`src/storage/types.ts`) is the only thing you must implement.
Every method is async. `createMemoryStore()` (`src/storage/memory.ts`) is the
reference implementation — read it first, then mirror its behavior method by method.
Its tests (`src/storage/memory.test.ts`) state the contract points the core relies on;
port them to run against your store.

| Methods | Contract to hold |
| --- | --- |
| `configEpoch()` | 32 random lowercase hex digits, created **once with the store** and never changed while the store keeps its versions. A store that survives restarts keeps its epoch — that is what lets gateways keep resuming across control-plane restarts. |
| `latestConfig`, `saveConfig(entry, keep)`, `configsAfter(version)` | Versions are written by the core only. `saveConfig` may drop versions older than the newest `keep`; keeping all of them (config audit history) is allowed. `configsAfter` returns oldest first. |
| `acquireLease(holder, now, expiresAt)`, `releaseLease(holder)` | Grant when free, when `holder` already holds it, or when expired by `now`; otherwise change nothing and return the lease in force. **Check and write in one transaction.** |
| `lastBatch(instance)`, `saveCountedBatch(counted, expectedLast, keepRecords)` | The exactly-once guarantee. Write the batch ID as the instance's last, add every `additions` entry to the window totals, and append `records` — **all in one transaction, and only if the instance's last batch still equals `expectedLast`** (`undefined` = none yet). Otherwise write nothing and return `{ saved: false, last }`. A compare-and-set on the cursor row (`UPDATE … WHERE last = $expected`, or `SELECT … FOR UPDATE`) is the usual shape. |
| `addWindowTotals(additions)` | Add to window totals (a window not stored starts at 0). Used for model-set carry-overs. |
| `currentWindowTotals({ hourStart, monthStart })` | `tokens_per_hour` windows starting at `hourStart`, `usd_per_month` windows starting at `monthStart`. |
| `dropPastWindowTotals(oldest)` | Past windows *may* be dropped; keeping them for reporting is allowed — the core only reads current windows. |
| `recentRecords(limit)` | Newest first. |
| `gateway`, `gateways`, `saveGateway`, `deleteGateways` | Latest status per instance, whole-record replace. Deleting a gateway keeps its batch cursor. |
| `dropBatchCursorsCountedBefore(cutoff)` | Drop cursors counted before `cutoff`, return their instances. |

Representation notes:

- **Window totals are `bigint`** (tokens, or nano-USD). Store them as `NUMERIC` /
  `DECIMAL(20,0)` or `BIGINT` — the protocol caps a window at 18 digits (< 10^18), so a
  signed 64-bit integer holds it. Never a float.
- **Window identity** is `(group, type, models, windowStart)`; `group` is absent for
  a global limit, `models` is a **sorted** array or absent (= all models). Normalize
  both to stable non-null values for a primary key (primary-key columns cannot be
  `NULL`): e.g. `''` for global (a group ID is never empty), and for models the JSON
  text of the sorted array, `''` for all. Windows are keyed by the group **ID**:
  those of a deleted group are no longer read and age out with their hour or month —
  unless a group is created again under that ID within the window, which reads them
  again (§7).
- Times are milliseconds since the epoch, by the control plane's clock.
- Store configs as JSON (`jsonb`) exactly as given; the core has already validated
  them. Returned objects must not share mutable state with what callers hold (the
  memory store `structuredClone`s).

A sketch, not a prescription (Postgres):

```sql
create table store_meta   (id int primary key default 1, config_epoch text not null,
                           lease_holder text, lease_expires_at bigint);
create table configs      (version int primary key, config jsonb not null, published_at bigint not null);
create table batch_cursor (instance text primary key, epoch text not null, sequence bigint not null,
                           counted_at bigint not null);
create table window_total (group_id text, type text, models text, window_start bigint,
                           used numeric(20,0) not null, primary key (group_id, type, models, window_start));
create table usage_record (record_id text primary key, received_at bigint not null, record jsonb not null);
create table gateway      (instance text primary key, state jsonb not null);
```

## 6. Usage records: your ledger

The core keeps only what enforcement needs: current hour/month totals and the last
`recentRecordsSize` records (a live view, not a history). **For billing, reports and
audit, persist records in `saveCountedBatch`**: `counted.records` holds every record of
a batch counted *now* — a resent batch is never passed again — so a table written in
that same transaction is an exactly-once usage ledger for free. `keepRecords` only
bounds what `recentRecords` must return; your ledger table can keep everything.

What a record carries (`UsageRecord`, `protocol/schema/usage-record.schema.json`):
record and request IDs, gateway instance, key ID, `groups` (the key's group path,
top-level first, 1–8 IDs, as the gateway's config had it), public model, deployment (backend + backend model), units (`tokens_in`,
`tokens_cached`, `tokens_cache_write`, `tokens_out`, `tokens_reasoning`), `cost_nano_usd`, flags `estimated`
and `partial`, `gateway_time`. Raw units travel beside the cost, so you can re-price.

## 7. Publishing config

The config document is the one format everything shares:
`schema/config.schema.json` in the package, `examples/config.json` in the kaiak repo,
field meanings in `docs/specs/CONTROL-PROTOCOL.md` → Config. Top level:
`format_version: 5`, `global`, `backends`, `models`, `keys`, and optional `groups` —
each collection an object keyed by ID.

**Backend types** (`CONTROL-PROTOCOL.md` → Config → Backend types; what each does:
`GATEWAY.md` → Providers): a backend's `type` names its server and picks the
gateway's module for it. `BACKEND_TYPES` lists them, for a form's choices.

- `openai` (OpenAI's API) and `azure-openai` (Azure's `/openai/v1/` API) require
  `api_key_env` and **force the standard service tier** (Prices, below).
- `vllm`, `llama-server` (llama.cpp) and `openai-compatible` — any other server
  speaking the OpenAI format (SGLang, …) — pass the client's `service_tier` untouched.
- `anthropic` (Anthropic's API) and `azure-anthropic` (Claude in Microsoft Foundry)
  require `api_key_env` and **keep requests at the standard price**: options billed
  above it are refused (Prices, below).
- **A type decides which client APIs reach a model** (`GATEWAY.md` → Providers →
  Endpoint support). The gateway passes each request through to a backend speaking
  the same API and translates nothing, so a model is reachable through the APIs its
  deployments' types serve: OpenAI's chat, completions and embeddings on every type
  but the Anthropic ones; Anthropic Messages on `vllm`, `llama-server`, `anthropic`
  and `azure-anthropic`; OpenAI Responses on `vllm`, `llama-server`, `openai` and
  `azure-openai`. A Claude model is Messages-only; `openai-compatible` serves
  OpenAI's three only. Model entries on the gateway list their `endpoints`.
- **Choosing a type**: the server's own type whenever it has one;
  `openai-compatible` only for a server without one. The backend answers the same
  under either, but only its own type gets that server's rules: an OpenAI backend
  left as `openai-compatible` runs on the tier its clients ask for, and priority
  bills about twice the prices config holds. `verifyBackend` notes a vLLM or
  llama-server answer under another type (§8).
- `base_url` is what an OpenAI client would use, `/v1` included
  (`https://api.openai.com/v1`, `http://vllm:8000/v1`) — for `anthropic`, what an
  Anthropic client would use with `/v1` (`https://api.anthropic.com/v1`); for
  `azure-openai` and `azure-anthropic` it is the resource endpoint
  (`https://<resource>.openai.azure.com`, `https://<resource>.services.ai.azure.com`).
  No type has a default.

**Models carry no defaults** (`GATEWAY.md` → Model metadata): request parameters
such as temperature or a chat template's switches are the backends' to set — the
gateway applies none, and the config has no field for them. `metadata`
(`context_length`, `capabilities`, `reasoning_efforts`) is information for clients;
`output_limit` is the one request parameter a model's config sets, because limits
reserve it (the gateway's own passthrough edits — model name, standard tier, `store:
false` — are `GATEWAY.md`'s, Providers). Suggested settings your UI shows people are your app's data, outside config.

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
- **Effective limits are bounded**: global's limits plus every group's effective
  limits (its own merged with its parent's `child_defaults.limits`) add up to at
  most **50 000** (`effective-limits-exceeded`, reported once at path `""`). Each is
  a counter on every gateway (about 1.4 KB, so about 70 MB at the bound) and a
  window in your totals, and defaults multiply: D default limits on a group with N
  children are D × N. Before offering "a default limit for everyone" in a UI, check
  what it costs across the children.
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

**Prices** (`CONTROL-PROTOCOL.md` → Config → Prices, Tiered prices, Units and price
units): a model's `prices` is a list of `{ effective_from, tiers }` in increasing date
order; the gateway prices each request with the entry in force on its UTC date, and
a model without `prices` is free (no USD limit refuses it). Keep old entries when a
price changes — add a new one dated from the change — so records re-priced later
use the price of their day.

- **Tiers** carry the rates: `tiers` lists 1 to 8 `{ above_input_tokens,
  usd_per_million }`, the first at 0 and each later one higher. A request is priced
  whole — input, cached input, input written to the cache and output — at the last
  tier whose `above_input_tokens` is below its input size (`tokens_in` +
  `tokens_cached` + `tokens_cache_write`, the backend's prompt tokens). A model
  without long-context pricing has one tier at 0; one billed at about twice the
  rates above 272k input tokens has a second tier at 272000. Each tier lists all its
  own prices: nothing is inherited from the tier below.
- Four units take a price: `tokens_in` (plain input), `tokens_cached` (input read
  from the cache), `tokens_cache_write` (input written to the cache — Azure OpenAI
  bills it above plain input, gpt-5.6 and later at 1.25× the input rate) and
  `tokens_out` (all output, reasoning included). `tokens_reasoning` is never
  priced — it is inside `tokens_out`. A tier without `tokens_cached` or
  `tokens_cache_write` charges that input at the tier's `tokens_in` price: cached
  input is never free, written input never cheaper than plain input. A backend
  that does not report cache writes records 0 written, so pricing the unit for it
  changes nothing.
- **Prices are standard-tier rates.** On `openai` and `azure-openai` backends the
  gateway keeps every request on standard processing: a client's `service_tier`
  becomes `"default"`, and every chat and Responses request carries it even when the
  client sent none, since the default (`auto`) follows the Azure deployment's or OpenAI
  project's setting, which may be Priority (`GATEWAY.md` → Providers → Service
  tier). Priority, flex and batch rates never apply there. On `anthropic` and
  `azure-anthropic` the gateway refuses fast mode (`speed`), a non-global
  `inference_geo` and 1-hour cache writes, and `anthropic` sends every request with
  `service_tier: "standard_only"` (`GATEWAY.md` → Providers → Standard price on
  Anthropic types): price Claude models at their standard rates, 5-minute cache
  writes as `tokens_cache_write`. The self-hosted types pass the client's
  `service_tier` untouched — no tier is billed there.
- **Azure deployment types price differently**: Data Zone (EU/US) is about 10%
  above Global for the same model. Price a model at the rate of the deployment type
  its deployments use.
- **Importing a price list is your app's job.** LiteLLM publishes one
  (`model_prices_and_context_window.json` in the BerriAI/litellm repository, keys
  such as `azure/gpt-5`, `azure/eu/gpt-5`); its per-token USD fields map to
  per-million prices as: `input_cost_per_token` × 10⁶ → `tokens_in`,
  `cache_read_input_token_cost` × 10⁶ → `tokens_cached`,
  `cache_creation_input_token_cost` × 10⁶ → `tokens_cache_write`,
  `output_cost_per_token` × 10⁶ → `tokens_out`, all in the tier at 0. A field ending
  in `_above_<N>k_tokens` is a long-context rate and goes to the tier at N × 1000:
  `input_cost_per_token_above_272k_tokens` → the 272000 tier's `tokens_in`, and the
  same for `cache_read_input_token_cost_…`, `cache_creation_input_token_cost_…` and
  `output_cost_per_token_…`. A tier prices every unit itself, so when the list gives
  no long-context rate for `tokens_in` or `tokens_out`, copy that unit from the tier
  at 0 (leaving `tokens_out` out would make its output free). Copy nothing else: a
  cache unit with no long-context rate stays out of the tier, and is charged at
  that tier's own `tokens_in` (`CONTROL-PROTOCOL.md`, Config → Units and price
  units). Copied, the tier at 0's cache-write rate would price long-context writes
  below the tier's plain input; the fallback errs high instead. The other fields
  are not priced by kaiak: the
  `_priority`, `_flex`, `_batches` and `_ultrafast` variants (service tiers the
  gateway never uses), `cache_creation_input_token_cost_above_1hr` and its
  `_above_1hr_above_<N>k_tokens` variants (a one-hour cache lifetime, not a
  long-context rate: kaiak counts writes as one unit, as Azure reports them), audio,
  image and per-second rates (endpoints the gateway does not serve), and
  `search_context_cost_per_query` (a per-search fee). Azure deployment names are
  yours, so the mapping from a model to its LiteLLM key is a setting in your app, not
  something to guess from names. The list carries no dates and is community-kept:
  add an entry dated today only when a price changed, and let a person review it
  before publishing.

Typical flow for a UI edit:

1. Load your domain model (DB rows) and build a complete `Config` from it. The
   control plane always publishes whole documents — no patches.
2. Call `validateConfig(doc)` for immediate form feedback; each issue has `code`,
   `message` and a JSON Pointer `path`. Stable codes (`key-group-unknown`,
   `group-cycle`, `limit-duplicate`, `output-limit-above-context`, …) are listed in
   the spec.
3. Call `controlPlane.publishConfig(doc)`: `{ ok: true, published: { version, … } }` or
   `{ ok: false, issues }` (nothing published, current version stays). Validation runs
   again inside; step 2 is for UX only. Publishing also compares with the current
   version: a group given another `parent` than it has there is refused
   (`group-parent-changed`, path `/groups/<id>/parent`) — `validateConfig` cannot see
   this, so surface publish issues in the UI too.
4. Watch the result land: `gateways()` shows each gateway's
   `status.applied_config_version`, or `status.last_rejection` (`{ version, codes }`)
   when a gateway refused it (typically a backend `api_key_env` not set on that pod).

Keys: `createKey(id)` — `id` matches `^[A-Za-z0-9][A-Za-z0-9._@-]{0,127}$` (throws
`key-id-invalid` otherwise); `isConfigId(id)` checks that shape, the same for key and
group IDs — check a group ID before minting a key for it. Add `{ hash, group }` under `keys[id]` — any group,
leaf or not — and publish. Revoking a key = removing it, or setting `disabled: true`
or an `expires_at`, and publishing. Keys have no limits of their own: a request
passes the limits of its key's group, every ancestor and global, and its usage
counts toward each of them.

## 8. Verifying a backend when it is added

Model metadata is declared in config: the gateway serves what config says and asks
backends nothing. `verifyBackend` fills the declaration in for you. The contract,
field by field, is `docs/specs/BACKEND-VERIFY.md`.

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
    for `anthropic` also `context_length`, from the list's `max_input_tokens`). Recognized
    from the answer, whatever `type` was given; a `vllm` or `llama-server` under
    another type adds a note naming the type to use — show it before the operator
    saves the backend.
  - `models[]`: every listed model, with what the server reports (`context_length`;
    for llama-server also `capabilities.vision`, `tools`, `reasoning`) and a `sources`
    entry per value: the URL and field it came from, and `hint`.
  - `metadata`, when `model` was given and the check passed: the reported values under
    their config names.
- **`metadata` is partial.** Merge it into the model's `metadata`, then have the
  operator complete the rest: `capabilities.streaming` always, every capability not
  reported, `reasoning_efforts`. A value the backend does not report is absent, never
  guessed — do not default it in your form either; leave it for the operator to
  decide, and publishing refuses the config until they do.
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
| Current config and its version | `currentConfig()` → `{ version, config, publishedAt }`; `configEpoch()` |
| Gateways, health, what they run | `gateways()` → `{ instance, status, receivedAt, live, conflict? }[]`; `liveGateways()` |
| Spend vs limits (current windows) | `totals("")` + `resolveScopes(config)` + `limitIdentity(limit)` — below |
| A group's path, effective limits and models | `resolveScopes(config)` → `{ group?, path, limits, allowed_models? }[]` — global first (no `group`, `path` `[]`), then every group in config order; `allowed_models` absent = every model |
| Live usage feed | `recentRecords()` |
| Push updates to browsers | `onConfigPublished`, `onTotalsChanged`, `onGatewaysChanged` (each returns an unsubscribe) |

Totals vs limits (as the sample's status page does, `control/sample/src/page/sections.ts`):

```ts
const current = await controlPlane.currentConfig();
const totals = await controlPlane.totals(""); // "" = read under no gateway's name
if (current && totals) {
  const used = new Map(totals.windows.map((w) => [JSON.stringify([w.group ?? null, limitIdentity(w)]), BigInt(w.used)]));
  for (const { group, limits } of resolveScopes(current.config)) { // group undefined = global
    for (const limit of limits) {
      const spent = used.get(JSON.stringify([group ?? null, limitIdentity(limit)])) ?? 0n; // absent = nothing used
      // tokens_per_hour / usd_per_month only: per-minute limits are enforced on
      // gateways and never counted here. USD amounts are nano-USD.
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
  event says which: `config-published`, `totals-changed` or `gateways-changed`; the
  publish, count or status it came from has succeeded either way. Listeners hear the
  changes of every process over the store, not only this one's.

## 10. Operating it

- **TLS** in front of the control plane outside local runs; the token is a bearer
  secret. Tokens never go in URLs.
- **Proxies must not buffer or cut the stream**: `GET /v1/stream` is SSE held open for
  hours (it sets `X-Accel-Buffering: no` and `Cache-Control: no-cache`); give it an
  idle timeout well above the 15 s heartbeat and disable response compression for it.
- **Body limits** are set by the plugin (usage 2 MiB, status 64 KiB); a proxy limit
  below that breaks usage delivery.
- **Restarts** are safe with a durable store: gateways keep serving, hold usage,
  reconnect and resume from their config version. Priced models under a USD limit are
  refused by gateways only after the outage passes `global.control_outage_grace_ms`
  (15 min default) — keep restarts well below it.
- **Replacing the process**: the new one starts once the old lease is released (clean
  `app.close()`) or expired (crash: up to `storeLeaseTtlMs`). Rolling updates that start
  the new pod before stopping the old one wait on the lease — use `Recreate`.
- Token holders can report usage and status under any instance name (one shared token
  by design); treat `KAIAK_CONTROL_TOKEN` like a provider key.
- Logging: Fastify's pino. JSON in production; for development, `pino-pretty` with
  `translateTime: 'SYS:HH:MM:ss.l'`, `singleLine: true`,
  `ignore: 'pid,hostname,reqId,req.host,req.remoteAddress,req.remotePort'`
  (the sample's `src/logging/`).

## 11. Testing

- Unit-test your store against the contract (§5): port `memory.test.ts`, then drive the
  core with it — the kaiak-control tests (`src/**/**.test.ts`) show how to call
  `acceptUsageBatch`, `acceptStatus` and `publishConfig` directly without HTTP.
- Validate against the shared fixtures in the kaiak repo's `protocol/fixtures/` (valid
  and invalid configs and messages, with `cases.json` naming each expected code).
- End to end: run a real `kaiak` binary (or the gateway image) against your app with a
  fake OpenAI-compatible backend (`gateway/internal/fakebackend/cmd/fakebackend`), then
  check a request's usage arrives exactly once, a config edit reaches the gateway, and
  a control-plane restart loses nothing. The kaiak repo's cross-half e2e
  (`gateway/e2e/sample_test.go`, build tag `crosshalf`) does this against the sample.

## 12. Things that look reasonable and are wrong

- Running two replicas "for availability" — refused by the lease, by design.
- Sending gateways a remaining *allowance* per scope — they must get totals; the
  library already does this.
- Treating `recentRecords()` as the usage history — it is the last 100.
- Parsing `used` or `cost_nano_usd` sums as JavaScript numbers — sums must be `BigInt`.
- Rebuilding config from the previous published document plus a patch without
  validating — always build whole and publish through the core.
- A `models` array stored unsorted (or `[]` for "all models") in window keys — the
  identity breaks and totals stop matching limits.
- Moving a group by editing its `parent` — refused (`group-parent-changed`). A move
  is a delete and a create; the old group's spend stays with its old ancestors, and
  a group created again under the same ID within the window resumes that ID's spend
  (§7) — use a new ID for a fresh budget.
- Giving tree levels a meaning in config (a `kind` field, a level that must be a
  "team") — there is none; use `labels` and keep the meaning in your app.
- Adding children's windows to their parent's — a record counts toward every group
  on its path, so a parent's window already holds its descendants' spend. Totals
  exist only for hour and month limits; for reports, group your ledger (§6) by any
  prefix of the records' `groups`.
- Resetting the store's config epoch on migration — every gateway resyncs; harmless,
  but a sign the store is not keeping what it should.
- Editing `kaiak-control` inside your app. Protocol or library changes land in the
  kaiak repo, on both halves and the spec at once, then you move your pin.
