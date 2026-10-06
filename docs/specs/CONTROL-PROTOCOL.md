# Control Protocol

> The contract between gateways and a control plane. The control-plane side is
> implemented once, in `kaiak-control`; the sample control plane and the real one both
> use it. Machine-readable form: JSON Schemas and fixtures in `protocol/` — a change here
> updates both, and both halves, in the same change (`AGENTS.md`, Project-Specific
> Rules). Decisions settled 2026-09-24 unless dated otherwise; message shapes are
> settled (Messages), endpoint paths stay provisional until the control-plane plan
> implements them.

## Shape

- **Control plane → gateway**: a config **snapshot** (GET, carrying a version) and one
  **SSE stream** per gateway resuming from that version (snapshot + subscribe).
- **Gateway → control plane**: plain POSTs — usage batches and status.
- Every request and response carries the **protocol version** (the `Kaiak-Protocol`
  header, below); a mismatch is refused with a clear error on both sides. No
  multi-version support before the project graduates.
- **Errors** use the `{ error, detail? }` shape (CODING-RULES §5) on every endpoint.
  `error` is a stable code: lowercase ASCII words joined by hyphens
  (`^[a-z]+(-[a-z]+)*$`), at most 64 characters (settled 2026-10-05). The gateway
  logs a code only in that shape and never logs `detail`: an answer that is not the
  control plane's own (a proxy, a misdirected URL) can carry anything, credentials
  included (`GATEWAY.md`, Observability → Logs: no remote text).
- **Auth** (settled 2026-09-24): the gateway only makes outbound calls, so there is one
  bearer token, gateway → control plane (`KAIAK_CONTROL_TOKEN`); the gateway names
  itself in a `Kaiak-Instance` header. TLS vouches for the control plane and is
  expected outside local runs; tokens never go in URLs. What a token holder can do:
  Control-plane processes → Trust model.
- **No redirects** (settled 2026-10-01): the gateway does not follow a redirect from
  the control URL — Go resends the token to a redirect target with the same host name
  whatever its port or scheme — and logs the 3xx as an error;
  `KAIAK_CONTROL_URL` must be the address that answers.
- **Protocol version** travels as a `Kaiak-Protocol` header on every request and
  response (the SSE stream's response included); the current version is `5`
  (settled 2026-10-06, with config format 5: model `defaults` removed).

## Endpoints (provisional)

- `GET /v1/config` → a config snapshot `{ config_epoch, version, config }`. Versions
  are integers from 1, increasing with every config change, assigned by the control
  plane within its store's config epoch (Config versions). Before
  the first config is published: `503` with `config-unavailable` — the gateway retries
  with its reconnect backoff (settled 2026-09-24).
- `GET /v1/stream?since=<version>&config_epoch=<epoch>` → SSE events (behavior:
  Config stream):
  - `config` — a full new config with its version;
  - `totals` — the control plane's usage totals per limit (global and group limits)
    for hourly and monthly windows, the number of live gateways, and the last of this gateway's batches they
    include (Budgets; Messages → Totals);
  - `resync` — the gateway is too far behind; fetch the snapshot again.
- `POST /v1/usage` → one usage batch; the answer is the ack, carrying fresh totals
  (Usage batches).
- `POST /v1/status` → gateway status; `204` with no body when accepted (Status
  intake).
- Every body above is one of the messages below (Messages).

## Request checks (settled 2026-09-24)

Every gateway request passes three checks before its endpoint runs, in this order —
the token first, so a caller without it learns nothing else. A failure answers with
the `{ error, detail }` body, `error` being the stable code:

| Check | Code | Status |
| --- | --- | --- |
| `Authorization: Bearer <token>` carries the token (missing, not a bearer token, or wrong) | `unauthorized` | 401 |
| `Kaiak-Protocol` is the current version, `5` (other or missing) | `protocol-version-mismatch` | 400 |
| `Kaiak-Instance` is an instance ID (Messages; missing or malformed) | `instance-invalid` | 400 |

- **Repeated headers** (settled 2026-09-25): Node's HTTP layer joins a repeated
  `Kaiak-Protocol` or `Kaiak-Instance` into one value (`5, 5`), which fails its check
  as any other wrong value does, and keeps only the first of repeated `Authorization`
  headers — so the checks see one value per header and have no "repeated" case of
  their own.

- Endpoint-level failures use the same body: `not-found` (404, a path or method the
  protocol does not define under the plugin's prefix — after the request checks),
  `since-invalid` (400, Config stream),
  `config-unavailable` (503, above; also a usage batch before any config — Usage
  intake), `usage-batch-invalid` and `instance-mismatch` (400, Usage intake),
  `status-invalid` (400, Status intake); a
  request the HTTP layer itself refuses answers `request-invalid` with its 4xx status
  (a usage body over 2 MiB or a status body over 64 KiB included — a full batch and a
  large deployment's status are far below); a control-plane fault answers `500`
  `internal-error` with no detail (logged on the control plane).
- **Every answer under the plugin's prefix carries `Kaiak-Protocol`** (settled
  2026-09-25) — the routes', the checks', Fastify's own refusals and `not-found`:
  the header is set by the plugin's first hook. One answer is outside the plugin's
  reach: a URL Fastify cannot decode (`/v1/%zz`) is refused by the host app before
  any prefix applies. Gateways build every URL themselves and never send one.
- The token is compared in constant time (SHA-256 digests of both, compared with a
  timing-safe equality, so neither content nor length shows in the timing), and no
  error echoes the presented token.

## Config versions (settled 2026-09-24)

- The control plane assigns versions: `1` for the first config published, then one
  more with every publish. A config that fails validation (schema or semantic rules)
  is refused with its issues and never gets a version; the current version stays.
- **Parents never change** (settled 2026-09-27): a publish that gives a group the
  current version defines another `parent` than it has there — a top-level group
  given a parent and a child made top-level included — is refused like a config that
  fails validation, code `group-parent-changed` (control plane only; the issue names
  the group). A move is a delete and a create: the group's ancestors are what its
  usage records name and its spend was counted toward, so they never change under
  it. The check compares with the current version only — the first publish has
  nothing to compare, and a group deleted in one publish may come back under another
  parent in a later one. The gateway does not check it: it is stateless and may boot
  with no previous config (a seed, a fresh start), so it has nothing to compare
  with.
- The control plane keeps a bounded history of recent versions (default 100) so a
  stream can resume: `since` equal to the current version replays nothing; `since`
  within the history replays every newer version, oldest first; otherwise — older
  than the history, newer than the current version, not a version at all, or nothing
  published yet — the answer is `resync`.
- Versions are the control plane's own count. A control plane that loses its store
  (the sample's is in memory) starts again at `1`; gateways ahead of it get `resync`
  and take the snapshot. **After a `resync` the gateway applies the snapshot whatever
  its version** — lower than the one it runs included — and resumes from it; outside
  a resync it never goes back to a lower version within an epoch.
- **Config epoch** (settled 2026-09-25): versions count within the store's
  `config_epoch` — 32 random lowercase hex digits created with the store and kept, like
  the versions, for the store's life (a store that survives a process restart keeps
  it; the in-memory store makes a new one per process). A version number alone says
  nothing across stores: a new store that has reached `7` again holds another config
  than the old store's `7`. So every snapshot and `config` event carries the epoch, the
  gateway keeps it with the version it runs (and in its last-known-good copy), and a
  version is compared only within its epoch — **a snapshot or event of another epoch
  is applied whatever its version**, the `resync` rule generalized. Rejected: relying
  on `resync` alone, which misses a new store that has caught up to the gateway's
  number (a last-known-good boot, then `since` equal to the new current version,
  replays nothing and the gateway keeps a config the control plane no longer has).

## Config stream (settled 2026-09-24)

- `since` and `config_epoch` are **required**: the config version the gateway runs, a
  non-negative decimal integer (no sign, no leading zeros, one value), and the epoch
  it counts in (32 lowercase hex digits, one value). Either missing or malformed →
  `400 since-invalid` before the stream opens; a gateway refused so fetches the
  snapshot and resumes from its position (settled 2026-09-25, N-P10). A gateway with
  no config fetches the snapshot first. A gateway that rejected a newer version resumes after that one
  (settled 2026-09-24): replaying it on every reconnect would only reject it again.
  The control plane is unaffected — `since` is a position either way.
- The response is `text/event-stream`, `Cache-Control: no-cache`,
  `X-Accel-Buffering: no` (proxies must not buffer it), never compressed, carrying
  `Kaiak-Protocol` like every response.
- **Resume without gaps or doubles**: the control plane subscribes to new versions
  *before* it reads the ones to replay, then sends the replay (every version newer
  than `since`, oldest first — Config versions), then every version published while
  the stream stays open. A version is sent at most once per stream, in increasing
  order; each `config` event's `id:` is its version.
- **`resync` ends the stream**: when `since` cannot be resumed from (Config
  versions; `config_epoch` not the store's included), the stream carries one `resync`
  event (`{}`) and closes. The gateway
  fetches the snapshot, applies it (a lower version included) and reconnects with the
  snapshot's version as `since`. Rejected: keeping the stream open after `resync` —
  its position would be undefined until the gateway's snapshot, and a reconnect is the
  one resume path already needed.
- **Heartbeat**: a comment line (`: heartbeat`) every 15 s, so idle proxies and load
  balancers keep the connection. The gateway may treat a much longer silence as a dead
  connection and reconnect.
- **Totals** (settled 2026-09-24): a `totals` event (`data:` the totals on one line,
  no `id:` — resume is by config version only) follows the replay when a config
  exists, then goes out whenever the totals change (a counted batch, a publish — new
  `config_version` and limits — or a gateway joining or leaving the live set), **at
  most one per second per stream**: the first change after a quiet second is pushed
  at once, later ones within the second become one trailing push. A push reads the
  totals when it is written, so it carries the newest; the gateway orders pushes and
  acks by their revision (Messages → Totals).
- **Slow readers** (settled 2026-09-24): config events wait in the control plane's
  send buffer until the gateway reads them — none is dropped; a heartbeat is skipped
  while earlier events are still waiting. Totals never queue: while the buffer waits
  to drain, one push is held and sent on drain with the totals of that moment. A
  stream whose socket takes nothing for **30 s** (the plugin's
  `stalledStreamTimeoutMs`) is closed; the gateway reconnects with the version it
  runs. Rejected: queueing every totals event behind a slow reader — frequent totals
  would grow the buffer with values already out of date.
- The stream ends when the gateway disconnects (the control plane drops its
  subscription), after a `resync`, when it stalls (Slow readers), or when the control
  plane shuts down — the gateway
  reconnects (to another replica, or with backoff) with the version it runs.

## Messages (settled 2026-09-24)

- **The schemas are the source of truth** — one file per message in
  `protocol/schema/`, fixtures per message in `protocol/fixtures/messages/<kind>/`
  (valid and invalid, with `cases.json` as for config). Each schema has an `$id` under
  `https://kaiak.invalid/protocol/schema/` — a reserved domain, never fetched; it only
  lets the files `$ref` each other (`config.schema.json` included) by relative name.
  This section is the summary; the conventions of Config (snake_case, strict, UTC
  timestamps) hold.
- **No protocol version in bodies**: it travels in the `Kaiak-Protocol` header. The
  one exception is status, which states the version the gateway speaks so the control
  plane can store and show it with the rest of the report.
- **Integers** in messages stay at or below 2^53 − 1, so a JavaScript number holds them
  exactly — except usage totals, which are strings (Totals, below).
- **Instance ID** (`KAIAK_INSTANCE_ID`, default the hostname): a letter or digit, then
  up to 252 letters, digits, `.`, `_`, `-` — hostname- and pod-name-shaped, and safe in
  the `Kaiak-Instance` header. A body's instance must equal the header's.

| Message (fixture kind) | Shape |
| --- | --- |
| config snapshot (`config-snapshot`) | `{ config_epoch, version, config }` — `config` is a full config document |
| totals (`totals`) | `{ revision: { control_plane, sequence }, config_epoch, config_version, live_gateways, counted_through, windows: [window…] }` |
| totals window | `{ group?, type, models?, window_start, used }` |
| resync (`resync`) | `{}` |
| usage record (`usage-record`) | the record (Usage records) |
| usage batch (`usage-batch`) | `{ batch: { instance, epoch, sequence }, records: [record…] }` |
| usage ack (`usage-ack`) | `{ batch: { instance, epoch, sequence }, totals }` |
| status (`status`) | `{ instance, protocol_version, state, started_at, applied_config_version, applied_config_epoch, last_rejection, backends, models }` |
| status backend | `{ in_flight, max_in_flight?, deployments: { <backend model name>: { circuit, opened_at? } } }` |
| status model | `{ queued }` |

- **Stream events**: SSE `event:` names the event, `data:` holds one JSON message on
  one line: `config` → a config snapshot, `totals` → totals, `resync` → resync. The
  event name picks the schema; the data carries no type field of its own. A `config`
  event's `id:` is its version.
- **Config snapshot**: a snapshot is valid only if its config passes the config schema
  and the semantic rules. The gateway reads the envelope first and hands the config
  to its config loader: a config it rejects is a *config rejection* — kept out of
  force and reported in status with its version and codes — not a malformed message.
- **Usage batch**: `epoch` is 32 lowercase hex digits (128 random bits, new with each
  fresh spool); `sequence` starts at 1 and increases by one per batch within the
  epoch. A batch holds **1 to 500 records**: a gateway with nothing to report sends
  nothing, and a gateway with more queues further batches behind the outstanding one.
  Every record's `gateway_instance` is the batch's instance, and record IDs are unique
  within a batch.
- **Usage ack**: the batch ID it acknowledges — counted now or before, the answer is
  the same — and the totals after it, made for the batch's instance (its
  `counted_through` is at or past the batch).
- **Totals** — the `totals` event's data and every ack's `totals`, one shape, made
  for one gateway (the stream's, or the acknowledged batch's instance):
  - `revision` — orders totals messages (settled 2026-09-24): `control_plane` is 32
    random lowercase hex digits created when the control-plane process starts,
    `sequence` an integer from 0 increasing with every change to the totals in that
    process (a counted batch, a publish, a change to the live set). The gateway
    applies a message's totals only when it is **newer** than the last applied: the
    same `control_plane` and a higher `sequence`, or another `control_plane` (a
    restarted control plane — adopted, and ordering starts again from it). Pushes and
    acks travel on different connections, so without it a push read before an ack
    could arrive after it and take the gateway back to older totals. **Replaced
    processes** (settled 2026-09-25, the independent audit's finding 5): the gateway
    remembers the `control_plane` values it moved away from (the last 16) and never
    applies totals from one again — a delayed ack from the process a restart
    replaced, arriving after the new process's push, would otherwise count as
    another restart and take the gateway back to older totals (A 100 → B 200 → A
    150). Such a message's `counted_through` still counts (below): its process
    counted those batches before it was replaced, so the replacement's totals
    include them. The case served is one control plane restarting, not several
    running at once; the config epoch and version cannot order processes (both
    serve the same config). Rejected: a durable term in the store (a stored
    counter every start increments) — a protocol and store change for a case the
    gateway can settle alone.
  - `config_epoch`, `config_version` — the config whose limits the totals were
    computed under: the store's epoch and the version in it, as a snapshot names a
    config (settled 2026-09-25, H3). A gateway applies the windows only when both
    equal its applied config's (`GATEWAY.md`, Limits → Control-plane mode: Totals
    follow their config); a version alone is ambiguous across stores.
  - `live_gateways` — the live-gateway count. Each gateway enforces its share of
    every per-minute limit (limit ÷ count, rounded down, at least 1 unless the limit
    is 0) and of every backend `max_in_flight` cap (cap ÷ count, rounded up, at least
    1), treating 0 as 1 (Totals → Per-minute windows). The ack carries the count too,
    so one shape serves both.
  - `counted_through` — `{ epoch, sequence }`, the last usage batch the control plane
    has counted for the recipient's instance, or `null` before its first (settled
    2026-09-24). On any totals message — newer or not — the gateway stops counting as
    its own usage every batch of that epoch at or below it: those are inside the
    totals it applies now or applied before. So a push that already includes a batch
    whose ack has not arrived works like that ack, and nothing is counted twice. The
    ack stays the only thing that removes a batch from the spool.
  - **Consistent snapshot**: a message's windows include exactly the batches the
    control plane had counted when it took the revision — `counted_through` among
    them. `kaiak-control` takes batch counting and totals reads in turns to hold it
    (a batch's store write and its sequence step, against a read of the sequence,
    `counted_through` and the windows); it is what lets the gateway act on
    `counted_through` from a message older than the totals it holds. Rejected: a
    sequence without that guarantee, where a message could show a batch counted that
    the applied totals lack — usage dropped from enforcement until the next push.
  - `windows` — **complete**: every hour and month limit with usage in the control
    plane's current window, one entry per limit. A limit not listed has used nothing
    in the control plane's current window. Per-minute limits are never listed.
  - A window names its limit by the identity a config reload keeps a counter by
    (`GATEWAY.md`, Limits; settled 2026-09-27): `group` (the group the limit
    belongs to; absent for a global limit), `type` (`tokens_per_hour` or
    `usd_per_month`) and `models` (the limit's model set, order ignored; absent = all
    models). A group's limits are its effective ones, its parent's `child_defaults`
    merged in (Config → The group tree).
  - `window_start` is the top of a UTC hour (`tokens_per_hour`) or the first of a UTC
    month at midnight (`usd_per_month`), by the control plane's clock.
  - `used` counts what the gateway counts against that limit: tokens as
    `tokens_in + tokens_cache_write + tokens_out` (input read from the cache does not
    count: `GATEWAY.md`, Limits → Settle), USD in nano-USD.
    It is a **string of decimal digits** (no leading zeros, at most 18 digits — below
    10^18 nano-USD, one billion dollars per window). A JSON number is exact in
    JavaScript only up to 2^53 nano-USD, about 9 million dollars, and a month of an
    organization's spend can pass that; a string stays exact in every JSON parser —
    `kaiak-control` reads it as a `BigInt`, the gateway as an `int64`. Rejected: a
    number with a documented 2^53 bound (a silently rounded budget total is the worst
    failure mode), and strings for USD only (one representation for every `used` keeps
    one parsing path).
- **Matching totals to limits** (enforced by the gateway, `GATEWAY.md`, Limits): only
  totals computed under the gateway's applied config apply; then a
  window applies to the gateway's counter with the same group (or global), type and
  model set (order ignored). Its `used` is the counter's pushed base for the window
  `window_start` names; a window newer than the counter's current one starts that
  window. A window matching no counter (the configs differ) is ignored; a counter
  with no window has a pushed base of 0.
- **Status**: `state` is `starting`, `ready` or `draining`; `started_at` is when the
  gateway process started; `applied_config_version` is `null` until a config from the
  control plane (or its last-known-good copy) is applied — a gateway serving its seed
  config reports `ready` with `null` (settled 2026-09-25: `ready` means a config is
  in force); `applied_config_epoch` is the epoch that version counts in, `null`
  exactly when the version is (settled 2026-09-25, N-P1: a version compares only
  within its epoch — without it a gateway running an old store's v3 from its
  last-known-good copy read as the current store's v3); `last_rejection` is `{ version, codes }` when **the latest config the
  gateway received from the control plane was rejected**, else `null` (`codes`: the
  rejection's issue codes, as the gateway logs them) — set on a rejection, cleared when
  a later config is applied, whatever the version numbers (settled 2026-09-24): after a
  control-plane restart versions count from 1 again, so a rejected config can be below
  the applied version, and hiding it would leave the control plane unaware its config
  was refused. Rejected: reporting a rejection only while its version is above the
  applied one. A last-known-good boot does not clear it (that copy is not a config the
  control plane sent). Routing state (settled 2026-09-24), both collections empty
  before the first config is applied:
  - `backends` — backend ID → `{ in_flight, max_in_flight?, deployments }`: every
    backend of the applied config (idle ones with `in_flight` 0, so the control plane
    sees each cap), plus any backend a reload dropped while requests on it still run
    (no cap, no deployments). `max_in_flight` is the applied cap, absent when there is
    none — the configured value, not this gateway's share of it (the share follows
    from `live_gateways`; the gateway's metrics show it). `deployments` maps the model name on that backend → `{ circuit, opened_at? }`
    for each of the applied config's deployments there: `circuit` is `closed`,
    `open` or `half_open` (a probe succeeded and the gateway waits on a trial request,
    `GATEWAY.md`; settled 2026-09-25 — reporting it `open` made an idle recovered
    deployment look down), `opened_at` (a timestamp) present exactly when open or
    half-open (a half-open circuit keeps its opening time). Deployments are
    nested under their backend because a deployment *is* backend + backend-side model
    name (routing's identity, the usage record's `deployment`): keying them so makes
    each one unique by construction. Rejected: a flat `deployments` array, which
    needed a duplicate rule of its own.
  - `models` — public model name → `{ queued }`: every model of the applied config,
    `queued` 0 when its queue is empty, so the control plane can show every model's
    queue, not only the busy ones; plus any model a reload dropped while requests for
    it still wait.
- **Message rules** — what the schemas cannot express, checked by code in both
  halves, each with a stable code both report for the same fixture:
  - `timestamp-invalid` — a timestamp names no real instant;
  - `totals-window-duplicate` — two windows for one limit;
  - `record-instance-mismatch` — a record's `gateway_instance` is not the batch's
    instance;
  - `record-id-duplicate` — two records in a batch share a record ID.

  A config snapshot also reports its config's own codes. Schema violations have no
  per-rule code, as for config.
- **Repeated members** (settled 2026-09-25; the independent audit's finding 1): a
  message or config in which an object names a member twice, at any depth, is
  refused by the gateway before any other check, code `duplicate-member` with the
  repeated member's path (a config rejection for a config, the decode error for a
  message). JSON leaves a repeat's meaning open and decoders disagree on it; the
  gateway's schema check and typed decode did. kaiak-control does not detect
  repeats: `JSON.parse` keeps the last occurrence, and everything kaiak-control sends is
  written by `JSON.stringify`, which cannot repeat a member — so a repeat can only
  come from a hand-written or tampered document. Rejected: a repeat-detecting JSON
  reader in kaiak-control — a hand-written tokenizer or a dependency, for documents
  kaiak-control never produces. Raw-byte fixtures in `protocol/fixtures/duplicate-members/`
  (`cases.json`: kind, path, reason) hold both halves to this: the gateway refuses
  each file at its path, kaiak-control's reading of each (the last occurrence) is valid,
  so the repeat is each file's only defect.

## Config

- One JSON document: backends (with an optional concurrency cap), models
  (deployments, metadata, output-limit default and ceiling, prices, queue and retry
  overrides — no request defaults: `GATEWAY.md`, Model metadata → No model defaults,
  settled 2026-10-06), the group tree (groups with their allowed models, limits, defaults for
  their children and labels), keys (hashes, each in one group), global settings
  (global limits, and the queue, retry and circuit-breaker settings — their defaults
  and bounds: `GATEWAY.md`, Routing and reliability). The schema is the source of
  truth for every field: `protocol/schema/config.schema.json`; fixtures in
  `protocol/fixtures/config/` (settled 2026-09-24): `valid/`, `invalid/` (with
  `cases.json`: kind, code, reason) and `resolved/` (The group tree → Resolution
  fixtures).
- **Top-level shape**: `format_version` (the integer `5`; settled 2026-10-06, with
  model `defaults` removed — a format-4 document with them is refused as an unknown
  field), `global`, `backends`, `models`, `keys` (required), `groups`
  (optional, omitted = none).
  Collections are **objects keyed by ID** — uniqueness comes for free; the model key is
  the public model name clients send.
- **Conventions**: snake_case field names (as in the OpenAI API); durations are integer
  milliseconds named `*_ms`; timestamps are RFC 3339 in UTC (`Z`); price dates are
  `YYYY-MM-DD`, read as UTC; key hashes are `sha256:` + 64 lowercase hex.
- **Strict everywhere** (settled 2026-10-06): every object has a closed set of
  fields. The one open map there was, a model's `defaults`, went with model defaults
  (`GATEWAY.md`, Model metadata → No model defaults): the gateway sets no request
  parameter but the output limit, so the config names none.
- **Integers are bounded** (settled 2026-09-25): every config integer (and every limit
  `value`) is at most 2^53 − 1, so both halves read it exactly — `kaiak-control` as a
  JavaScript number, the gateway as an `int64`. An integer may be spelled with a
  fraction or exponent (`4096.0`, `1e3`), as JSON Schema counts it.
- **Public model names** (the `models` keys, and every `allowed_models` and limit
  `models` entry) must not end in `/props` (settled 2026-09-24): the gateway serves
  `GET /v1/models/{id}/props`, and model names may contain `/`, so such a name would
  lose its own `/v1/models/{id}` path to the props endpoint. The schema expresses it
  (`public_model_name`).
- **Backend model names** (a deployment's `model`, the status `deployments` keys and
  the usage record's `deployment.model`) are in the backend's own naming
  (`backend_model_name`, settled 2026-09-25, E12): 1 to 512 printable ASCII
  characters, no spaces — llama-server's path-style ids
  (`/models/qwen3-embedding-0.6b-q8_0.gguf`) included. ASCII keeps the byte and
  character counts equal in both halves and the names safe in logs and metric
  labels; a name outside it gets an alias on the backend (llama-server `--alias`).
- **Backend types** (settled 2026-09-30; the Anthropic types 2026-10-06): a backend's
  `type` is one of `openai-compatible` (the generic type), `openai`, `azure-openai`,
  `vllm`, `llama-server`, `anthropic`, `azure-anthropic` — what each does and which
  client API endpoints it serves: `GATEWAY.md`, Providers. `openai`,
  `azure-openai`, `anthropic` and `azure-anthropic` require `api_key_env`. **Types bump no version** (settled
  2026-10-01): adding types is additive — every config of the current format stays
  valid — and a gateway that does not know a type rejects the config with a schema
  error, which the control plane sees like any rejection; no protocol message
  changes. A bump would not catch an OpenAI backend left as `openai-compatible`
  either: the operator edits the number, not the type. Rejected: a config format and
  protocol bump for the types — churn across every fixture and the last-known-good
  format for no check that helps.
- **`api_key_env`** names an environment variable (`^[A-Za-z_][A-Za-z0-9_]*$`) that
  does not start with `KAIAK_` (settled 2026-09-25, N-S2): the gateway's own tokens
  live there, and a config author could otherwise have them sent to any backend URL.
  **Nor with `OTEL_`** (settled 2026-10-05, the 2026-10-05 review's H1): the
  gateway's log export reads its collector's credentials from
  `OTEL_EXPORTER_OTLP_HEADERS` and `OTEL_EXPORTER_OTLP_LOGS_HEADERS`. The whole
  prefix is reserved, not those two names: an endpoint variable can carry
  credentials too (`https://user:token@…`, a token in its query), and a list of
  names would miss the next one the gateway reads. Both prefixes are case-sensitive,
  as environment names are: `otel_…` is another variable, which the gateway never
  reads. Rejected: a list of the secret-bearing names.
- **The group tree** (settled 2026-09-27). Who may use which models and under which
  limits is one generic tree of groups:
  - **Shape**: `groups` maps group ID → `{ parent?, labels?, allowed_models?, limits?,
    child_defaults?: { allowed_models?, limits? } }`. A group without `parent` is
    top-level; `global` is the implicit root above every top-level group (its limits
    are `global.limits`). A plain single-parent tree, **at most 8 levels** (a
    top-level group is level 1). The order of levels is free per branch — team →
    project → env → workload in one, team → env → project → workload in another —
    and neither half gives levels any meaning. For example, a team and its workloads
    are a two-level branch; people with personal keys are the children of a `users`
    group whose `child_defaults` give each of them the same models and limits.
  - **Keys**: a key names one `group` — any group, leaf or not — and carries no
    limits or allowed models of its own. Its **path** is its group and every
    ancestor, top-level first.
  - **Scopes**: a request's scopes are the groups on its key's path, plus global. It
    must pass every limit of each scope whose model set covers its model, and its
    usage counts toward each of them.
  - **Limits**: `{ type, value, models? }`; `models` omitted means all models,
    counted together. A limit's **identity** is (group, type, model set, order
    ignored); a global limit has no group. Totals windows, the gateway's counters,
    model-set carry-over and the file-mode snapshot all key limits by it.
  - **Allowed models intersect down the path**: each level of the path has an
    effective list — the group's own `allowed_models`, else its parent's
    `child_defaults.allowed_models`, else none, and then that level restricts
    nothing. `["*"]` alone is an explicit "restricts nothing"; `[]` allows no model.
    A key may use a model only if every restricting level on its path lists it;
    **no restricting level anywhere on the path means every model**: narrowing is
    opt-in. Rejected: no model unless some level lists it — every new branch would
    need a list before its keys could do anything.
  - **`child_defaults`** apply to each **direct child**, never further down: a
    child's own `allowed_models` replaces the default list whole; each of a child's
    limits replaces the default limit with the same type and model set, in place,
    and default limits it does not override still apply. A group's **effective
    limits** are that merge: the parent's `child_defaults.limits` in their order,
    each replaced in place by the group's limit of the same identity, then the
    group's other limits in their order. `child_defaults` on a group without
    children has no effect and is not an error.
  - **`child_defaults` is a default, not a ceiling** (settled 2026-09-27): since a
    child's own list or same-identity limit replaces the default, a child can be
    given more than the default — more models, a higher value. A hard restriction
    for a whole subtree goes on the parent's **own** `allowed_models` and `limits`,
    which bind every group below it (allowed models intersect down the path, and
    every scope's limits apply). For the `users` example: the models and budget no
    person may exceed are `users`' own list and limits; its `child_defaults` are
    what each person gets unless their own group says otherwise.
  - **Effective limits are bounded** (settled 2026-09-27): the sum over global and
    every group of their effective limits (`global.limits`, and each group's own
    limits merged with its parent's `child_defaults.limits`) is at most **50 000**
    (`effective-limits-exceeded`). Every effective limit is a counter on every
    gateway (about 1.4 KB each, so the bound is about 70 MB) and a window in the
    control plane's totals; `child_defaults` multiply — D default limits on a group
    with N children are D × N — so a config a few hundred KB long could otherwise
    take gigabytes on every gateway, and a gateway killed by it refetches it on
    restart: a fleet-wide crash loop. The value is a constant in both halves.
  - **Parents never change** once published: a move is a delete and a create.
    `kaiak-control` refuses a publish that changes one (`group-parent-changed`,
    Config versions); the gateway does not check it. A group created again under an
    ID used before is a new group in the tree, but not a fresh budget: its windows
    are keyed by the ID (Usage intake → Totals).
  - **Labels**: `labels` maps a key (`^[a-z][a-z0-9_.-]{0,62}$`) to a value of 1 to
    256 characters (Unicode code points) with no control characters (U+0000–U+001F,
    U+007F–U+009F); at most 16 per group. They are for the control plane (a UI, reports:
    what kind of node a group is, its cost center); the gateway checks their shape
    and otherwise ignores them — no label reaches a refusal, a log line or a metric.
    A group has no `kind` field: a label says what kind of node it is, since levels
    mean nothing to either half.
  - **Resolution fixtures**: `protocol/fixtures/config/resolved/` pins what both
    halves derive from a valid config. Each file is `{ reason, config, expected:
    { groups: { <group ID>: { path, allowed_models, limits } } } }`, listing every
    group of `config`: `path` top-level first; `allowed_models` the models the
    group's keys may use across the whole path, sorted by code point, or `"all"`
    when no level on the path restricts; `limits` the group's effective limits
    (merge order above, each limit as written in config). Both halves check that
    their resolution produces exactly this.
- **Prices**: `prices` lists `{ effective_from, tiers }` entries in strictly
  increasing date order; the entry in force is the latest one effective on or before
  the request's UTC date. No prices = unpriced, cost 0.
- **Tiered prices** (settled 2026-09-29). Providers bill a request whose prompt passes
  a size threshold at that threshold's rates — OpenAI and Azure above 272k input
  tokens, Claude on Azure AI Foundry above 200k, Qwen's hosted APIs in brackets — so
  an entry's `tiers` lists `{ above_input_tokens, usd_per_million: { <unit>: number } }`:
  - **Bounds**: 1 to 8 tiers; `above_input_tokens` is an integer from 0 to 2^53 − 1.
    The first tier's is 0 (`price-tier-first-not-zero`) and each later one is above
    the one before (`price-tiers-not-increasing`). A model without long-context
    pricing has one tier, so every model is priced by the same rule.
  - **Input size** is `tokens_in + tokens_cached + tokens_cache_write` — the
    backend's `prompt_tokens`. An estimated record (`GATEWAY.md`, Accounting →
    Estimation) uses its estimated input, the figure limits reserved. Written tokens
    count (settled 2026-10-02): they are input, and the providers' thresholds count
    the whole prompt.
  - **Which tier applies**: the last tier whose `above_input_tokens` is strictly less
    than the input size; the first tier applies to every other record, an input of
    0 included. "Above 272000" matches the providers' ">272K".
  - **The whole record** is priced at that tier — input, cached input, input written
    to the cache and output alike — as the providers bill it, not only the tokens
    past the threshold.
  - **Each tier is complete in itself**: it lists its own prices, nothing is inherited
    from the tier below; the price-unit rules (below) hold within each tier.
  - Tiers add nothing to usage records: a record carries every input unit
    (`tokens_in`, `tokens_cached`, `tokens_cache_write`), so the control plane can
    re-price by the same rule. USD limits are unaffected: they reserve nothing before
    a request runs, and cost is known only when it settles.
  - Rejected: a base price plus a separate long-context block (`usd_per_million` +
    `usd_per_million_large` + one threshold) and any fixed two-tier shape — flat, but
    capped at two tiers, so bracketed pricing (Qwen) would need another format change.
- **Units and price units** (settled 2026-09-24). Usage units are disjoint where they
  are priced, so a cost is a plain sum of units × price:
  - `tokens_in` — plain input: the backend's prompt tokens minus those read from the
    cache and those written to it;
  - `tokens_cached` — input read from the cache (OpenAI
    `prompt_tokens_details.cached_tokens`);
  - `tokens_cache_write` — input written to the cache
    (`prompt_tokens_details.cache_write_tokens`; settled 2026-10-02, below);
  - `tokens_out` — all output, reasoning included (providers bill reasoning as output);
  - `tokens_reasoning` — the reasoning share of `tokens_out`
    (`completion_tokens_details.reasoning_tokens`), recorded for visibility only.

  The three input units add up to the backend's prompt tokens. `usd_per_million`
  accepts only `tokens_in`, `tokens_cached`, `tokens_cache_write` and `tokens_out`;
  `tokens_reasoning` is never priced (it is already inside `tokens_out`, and pricing
  it would charge reasoning twice). Within a tier, an unpriced `tokens_cached` or
  `tokens_cache_write` is charged at that tier's `tokens_in` price — leaving it out
  must never make cached input free, nor written input cheaper than plain input; an
  unpriced `tokens_in` or `tokens_out` costs 0 for that unit.
  - **`tokens_cache_write`** (settled 2026-10-02). Azure OpenAI bills prompt tokens
    written to its cache above plain input — gpt-5.6 and later at 1.25× input, reads
    at 0.1× (LiteLLM's `cache_creation_input_token_cost`) — and reports them as
    `prompt_tokens_details.cache_write_tokens` beside `cached_tokens`, both inside
    `prompt_tokens`, streamed or not; writes need no opt-in. Counted as plain input,
    written tokens would be charged about 25% under. One unit for every backend: a
    backend that does not report the field counts 0. Rejected: one unit per cache
    lifetime (5 minutes, 1 hour), as Anthropic and Bedrock price writes — Azure
    reports one count, and the Bedrock provider is not built.
- **Semantic rules** — what the schema cannot express, checked by code in both halves,
  each with a stable code that both halves report for the same fixture:
  - `key-group-unknown` — a key's `group` has no entry.
  - `key-hash-duplicate` — two keys share one hash.
  - `group-parent-unknown` — a group's `parent` has no entry.
  - `group-cycle` — following parents from a group leads back to it (a group naming
    itself included); reported for each group on the cycle.
  - `group-depth-exceeded` — a group deeper than 8 levels (a top-level group is
    level 1). Depth is judged only where the parents reach a top-level group: a
    group below an unknown parent or a cycle reports those rules alone.
  - `effective-limits-exceeded` — the effective limits of global and every group
    add up to more than 50 000 (Config → The group tree). Reported once, at the
    document root (path `""`). The count needs only each group's direct parent,
    so it is made whatever the other tree rules find; a group whose `parent` has
    no entry counts its own limits alone.
  - `deployment-backend-unknown` — a deployment's `backend` has no entry.
  - `allowed-model-unknown` — an `allowed_models` entry (a group's or a
    `child_defaults`') names no model.
  - `allowed-models-wildcard-mixed` — `"*"` listed alongside model names (a group's
    list or a `child_defaults` list).
  - `limit-model-unknown` — a limit's `models` entry names no model (global, a
    group's or a `child_defaults` limit).
  - `limit-duplicate` — two limits in one list (`global.limits`, one group's
    `limits`, one group's `child_defaults.limits`) share type and model set, order
    ignored. A group's limit with the identity of one of its parent's defaults is an
    override, not a duplicate.
  - `output-limit-default-above-ceiling` — `output_limit.default` > `ceiling`.
  - `output-limit-above-context` — `output_limit.ceiling` > `metadata.context_length`.
  - `reasoning-efforts-without-reasoning` — `reasoning_efforts` listed while
    `capabilities.reasoning` is false.
  - `price-dates-not-increasing` — a price's `effective_from` is not after the previous.
  - `price-tier-first-not-zero` — a price entry's first tier has an
    `above_input_tokens` other than 0. Path: that tier's `above_input_tokens`
    (`/models/<model>/prices/<i>/tiers/0/above_input_tokens`).
  - `price-tiers-not-increasing` — a tier's `above_input_tokens` is not above the
    previous tier's. Path: the offending tier's `above_input_tokens`
    (`/models/<model>/prices/<i>/tiers/<j>/above_input_tokens`).
  - `date-invalid` — a `YYYY-MM-DD` that names no real day.
  - `timestamp-invalid` — a timestamp with a field out of range (leap seconds included).
  Schema violations have no per-rule code: each half reports its own validator's
  message; the fixtures only require rejection.
- **Keys are created by the control plane**: `kaiak-control` generates the key, its ID
  and its hash; only the ID and hash enter config. **Key format** (settled 2026-09-24):
  `kaiak-` + 43 characters from `[A-Za-z0-9]` (32 random bytes) — a prefix secret
  scanners can match without false positives and that tells a leaked key's origin; the
  body is alphanumeric so it stays one word for double-click selection.
  The key ID is chosen by whoever creates the key. The plaintext key is shown once and
  never stored. The sample control plane exposes this as a `keygen` command (also the
  way to mint keys for file mode).
- Strict: unknown fields and wrong types are invalid, and the gateway refuses an
  object naming a member twice (`duplicate-member`; Messages → repeated members). A
  gateway that rejects a config keeps its current one and reports the rejection
  (version + reason) in its status.
- Provider credentials appear only as environment-variable names. A backend's
  `base_url` carries no userinfo (`user:password@`; settled 2026-09-25): a URL
  credential would travel in snapshots, last-known-good copies and host pages.
- The same document format is used by file mode and by the sample control plane's
  config file.

## Usage records

- One record per routed request (what "routed" covers: `GATEWAY.md`, Accounting),
  plus one per retried attempt whose request reached the backend and got no answer
  (`GATEWAY.md`, Usage across attempts) — so a request ID may appear on several records; the record
  ID is the unique one. Fields:
  record ID, request ID, gateway instance, key ID, groups (the key's path), model,
  deployment, usage units, cost, flags (`estimated`, `partial`), gateway
  timestamp.
- **Record fields** (settled 2026-09-24 with the gateway's accounting;
  `protocol/schema/usage-record.schema.json`):

  ```json
  {"record_id": "<32 hex, random>", "request_id": "…", "gateway_instance": "gw-1",
   "key_id": "k-eval-ci", "groups": ["research", "rag", "eval-pipeline"],
   "model": "qwen3-32b", "deployment": {"backend": "vllm-a", "model": "Qwen/Qwen3-32B"},
   "units": {"tokens_cache_write": 0, "tokens_cached": 0, "tokens_in": 812,
             "tokens_out": 240, "tokens_reasoning": 96},
   "cost_nano_usd": 0, "estimated": false, "partial": false,
   "gateway_time": "2026-09-24T10:00:00.123456789Z"}
  ```

  `record_id` is 32 lowercase hex digits; `request_id` is the request's
  `X-Request-Id` (1–128 letters, digits, `.`, `_`, `:`, `-`); `groups` is the key's
  path in the config the request ran under — 1 to 8 group IDs, each once, top-level
  first, the key's own group last (settled 2026-09-27: the path, not only the
  group, so the control plane counts a record without re-deriving ancestors from a
  config that may have changed since); `model` is the public name;
  `units` always carries the five token units, zeros included (settled 2026-10-02,
  with `tokens_cache_write`; other units join the map when their models do);
  `cost_nano_usd` is the cost in billionths of a dollar — an integer, so totals over
  any number of records add up exactly (a JavaScript number holds it exactly up to
  about 9 million dollars, the bound the schema puts on one record; aggregates beyond
  that need `BigInt`, and travel as strings — Messages, Totals); `gateway_time` is
  when the gateway settled the record (it picks the record's window, Usage intake).
  Records carry the raw units beside the cost, so the control plane can re-price.
  Never a key, a credential, or request or response content.
- Sent in batches in the background, never on the request path (Usage batches).
- **Time reference is the control plane**: its clock decides which windows are
  current; a record's `gateway_time` picks its window among the current and the
  previous one (Usage intake → Counted in its own window).

## Usage batches (settled 2026-09-24)

- Each gateway fills one batch of usage records and sends it; it has **at most one
  batch outstanding**. Records settled meanwhile go into the next batch.
- A batch carries an ID: the gateway's instance ID, an **epoch** (random, created with
  a fresh spool, so a replaced pod reusing an instance name is never mistaken for the
  old one) and a **sequence number** increasing per batch within the epoch.
- The control plane remembers the last batch ID it counted per instance. A batch it
  has already counted (a resend after a lost ack) is acked again without counting —
  delivery is at-least-once, counting exactly-once, with constant state per gateway.
  Record IDs stay in the records for audit and storage.
- **The ack carries the fresh totals** the batch produced. On ack the gateway drops the
  batch from the spool, and — in one step — adopts the totals if they are newer and
  stops counting the batch as its own (Messages → Totals: revision,
  `counted_through`), so its own usage is never counted twice or missed in between.
- Batches are sealed every 5 s or at 500 records, whichever comes first. With a data
  directory they are written to it (the spool) before they are sent, so a restart
  resends them with their IDs, and a crash loses only the records settled since the
  last seal; without one (the default) they wait in memory, bounded, under an epoch
  new with every process, and a killed gateway loses what was not acknowledged.
  Graceful shutdown seals the last records and flushes them either way, within a
  reserve kept at the end of the drain (`GATEWAY.md`, Usage batches, Usage spool,
  Lifecycle).
- **Sending** (settled 2026-09-24, the gateway's side):
  - A batch ID is bound to one set of records: it is written with them before its
    first send, and a restart never seals other records under a sequence already
    used.
  - The outstanding batch is retried with the **same ID** after a network error, a
    `5xx` (`503 config-unavailable` included), a malformed ack or an ack naming
    another batch, on the reconnect backoff (Config stream).
  - A refusal that means the batch itself can never be accepted — `400`/`413` with
    `usage-batch-invalid`, `record-instance-mismatch`, `record-id-duplicate`,
    `timestamp-invalid`, `instance-mismatch` or `request-invalid` — **sets the batch
    aside** (kept on the gateway for inspection, logged at error level with the code)
    and the next batch is sent: retrying it would block every batch behind it.
  - Any other refusal — `401 unauthorized`, `protocol-version-mismatch`,
    `instance-invalid`, a missing protocol header, an unknown answer — is a
    configuration or version problem, not the batch's: retried on the backoff and
    logged at error level, so no usage is dropped over it. Rejected: setting aside on
    any `4xx`, which would discard billing data behind a misconfigured proxy.
  - Batches go in the order they were sealed; each epoch's batches are all sent before
    another epoch's, so a resend never follows a batch of a newer epoch.
  - A spooled batch goes out under **its own instance ID** (body and
    `Kaiak-Instance` header), even when the gateway now runs under another one — a
    container whose hostname changed keeps delivering the usage its previous name
    recorded; new batches carry the new name and a fresh epoch.

## Usage intake (settled 2026-09-24)

How the control plane takes `POST /v1/usage`, as `kaiak-control` implements it.

- **Checks, in order**, after the request checks: the batch message (schema, then the
  message rules) — a schema violation answers `400 usage-batch-invalid`, a rule
  violation `400` with the rule's code (`record-instance-mismatch`,
  `record-id-duplicate`, `timestamp-invalid`; the first one found, every issue in
  `detail`); the batch's instance equal to the `Kaiak-Instance` header, else `400
  instance-mismatch` (the code status reports use too); a published config, else `503
  config-unavailable` — with no config there are no limits to count toward and no
  `config_version` to answer with, so nothing is counted and the gateway keeps the
  batch and retries.
- **De-duplication** by the last counted batch ID per instance: no batch yet → counted;
  same epoch and a sequence above the last → counted (a skipped sequence is counted
  too and logged: records lost on the gateway side are its loss, and the control plane
  must not stall on them); same epoch and a sequence at or below the last → acked
  again, not counted (a resend, or a straggler behind a later batch); a different epoch
  → counted and logged (a fresh spool). Batches from one instance are taken one at a
  time, so two copies racing (a retry beside the original) are counted once.
- **Counted atomically**: the batch ID becomes the instance's last, its amounts join
  the totals and its records join the recent records in one store write — a batch is
  counted and remembered, or neither. Rejected: separate writes, where a crash between
  them loses a batch or counts it twice.
- **Exactly once in the store** (settled 2026-09-25, D7): the write is conditional on
  the last batch ID the decision was made against — the store compares and writes in
  one atomic operation, and when another writer counted a batch of the instance in
  between, it writes nothing and returns the last batch it holds, and the batch is
  decided again (typically a duplicate now). So the store, not one process's
  serialization, guarantees a batch is counted once; the per-instance serialization
  stays as the common-case shortcut. Rejected: de-duplication read and write as two
  store calls, which two processes on one store (or one process and a stale retry of
  itself) can interleave — two cores on one store counted one batch twice.
- **Stamped on receipt**: one control-plane clock reading per batch is every record's
  receipt time (recent records); the current windows are the UTC hour and UTC month
  it falls in.
- **Counted in its own window** (settled 2026-09-25, D4): per window type (hour,
  month), a record counts in the window its `gateway_time` falls in when that is the
  current or the immediately previous window, otherwise in the current one. So the
  backlog a gateway sends after an outage lands in the hours and months it was used
  in, not all in the current one, and a batch sent before a boundary and received
  after it counts where it was settled; usage older than the previous window (which
  nothing enforces any more) and a gateway clock ahead of the control plane's count
  in the current window. The gateway keeps its uncounted usage by the same rule
  (`GATEWAY.md`, Limits → Uncounted usage stays in its own window). Rejected:
  counting by receipt time only — through an outage it charged the whole backlog to
  the current window, turning an hour limit into "since the outage began".
- **Counted toward** (Budgets; settled 2026-09-27): a record's scopes are global and
  every group its `groups` lists that the config in force at receipt (the current
  published version) defines — the path as recorded, never re-derived. Within those
  scopes, every `tokens_per_hour` and `usd_per_month` limit whose model set covers
  the record's model adds
  `tokens_in + tokens_cache_write + tokens_out` (written tokens count, settled
  2026-10-02; input read from the cache does not, settled 2026-10-05 — `GATEWAY.md`,
  Limits → Settle) or `cost_nano_usd`
  to that limit's window for the record (Counted in its own window). A group's limits
  are its effective ones (`child_defaults` merged; Config → The group tree).
  Per-minute limits are not counted (they stay local to gateways).
- **Groups the config no longer defines** (deleted after the gateway settled the
  record) are skipped; the listed groups that remain and global still count — so
  usage settled just before a delete counts toward the ancestors that remain, and
  records stay readable after the tree is reorganized. Parents never change (Config
  versions), so a listed group that still exists has the ancestors it had when the
  record was made — unless it was deleted and created again under the same ID,
  which counting cannot tell apart (the record counts toward what the ID names
  now, as its windows do: Totals). Rejected: dropping such records, which would
  under-count the surviving scopes' budgets on every config edit that races
  traffic.
- **Totals** follow the current config: a limit's window is kept by its identity
  (group or global, type, model set), so a limit removed from the config leaves the totals
  and one that returns within the same window comes back with what was counted while
  it existed; a new limit starts at 0 — unless only its model set changed (Budgets →
  Model-set edits). **A group ID used again resumes its window's spend** (settled
  2026-09-27): a group deleted and created again with the same ID within the same
  hour or month — a move included, whatever its new parent — gets that window's
  amounts back for each limit of the same identity; it is a new group in the tree,
  but the ID's budget in the current window carries on. A fresh budget takes a new
  ID. Rejected: dropping a deleted group's windows at publish — every other limit
  that returns within its window comes back with its spend, and a group's limits
  are no exception. Windows with nothing used are not listed. A
  window past the 18-digit ceiling of `used` is reported at the ceiling.
- **Sums are exact**: amounts are summed as `BigInt` from record to wire.
- **Past windows** older than the previous hour or month are no longer needed (the
  previous one still takes late records); the first counted batch of each hour lets
  the store drop them (the drop can also be run by hand). Totals only ever read
  current windows, so a store that keeps old ones (for audit) answers the same.
- **Recent records**: the last 100 received records (configurable) are kept with their
  receipt time, for a host's view of recent traffic; duplicates add none.

## Budgets

- The control plane aggregates usage per group and global, per hourly and monthly
  limit window: its clock picks the current windows, each record counts
  in its own window when that is the current or previous one (Usage intake).
- **Totals, not allowances** (settled 2026-09-24): it pushes each scope's used amount
  per window; limits come from the config both sides hold. Each gateway enforces
  `pushed totals + its own usage not yet counted` against the limit — not yet counted
  meaning above the `counted_through` of the totals it has seen, in-flight requests
  included. Pushing a remaining
  allowance instead would let each of N gateways spend all of it. The overshoot is
  bounded by the other gateways' unreported usage — about one batch interval each
  (principle 6).
- Totals are pushed after each batch that changes them, at most once a second per
  gateway, and in every ack — each time the complete set (Messages, Totals). The push
  rules — on connect, on a publish and on live-set changes too, coalescing, slow
  readers — are in Config stream.
- **Model-set edits** (settled 2026-09-25, D5): a publish whose new limit matches, by
  group (or global) and type, limits the previous config had and the new one dropped —
  only the model set changed — carries their spend: the new limit's current hour or
  month window is raised to the largest predecessor's amount there (it may hold its
  own already, from when that identity last existed in the window). Several
  predecessors are ambiguous: the largest is carried and the host hears of it
  (`onLimitCarriedOver`, `ambiguous`; the sample logs a warning). `kaiak-control`
  runs every publish in the totals' turn, so no batch counts between the publish
  and the carry, and batches count under the config in force when their turn comes.
  The gateway carries its counters the same way (`GATEWAY.md`, Limits → Model-set
  edits keep the spend). **Each side carries against the config it last held**
  (settled 2026-09-27): `kaiak-control` against the previously published version,
  a gateway against the config it applied before — which differ when a gateway
  skips versions (a resync, a rejected config), so the two may carry different
  predecessors for a while. The totals computed under the new config settle the
  difference: once they reach the gateway, they are its base for every limit
  (`GATEWAY.md`, Limits → Control-plane mode). Rejected: a new identity starting
  at 0 — adding a model to a spent monthly budget forgave the month. **The carry is
  written before the version is stored** (settled 2026-10-01; the 2026-09-30
  review's C4 and B2): a store write that fails fails the publish with nothing
  stored or announced, and the host retries; a carry only raises a window up to its
  predecessor's amount, so a retry after a carry written and a version not stored
  adds nothing twice. The host hears of each carry (`onLimitCarriedOver`) once the
  publish has succeeded; a callback that throws goes to `onListenerError` and never
  fails it. Rejected: storing the version first — a failed carry then left the new
  config live with the spend lost, and the retry, comparing against the stored
  version, found nothing to carry.
- **Per-minute windows** stay local to each gateway; the control plane pushes only the
  number of live gateways, and each gateway enforces limit ÷ live gateways, rounded
  down, 0 counting as 1, never below 1 unless the limit is 0 (`GATEWAY.md`, Limits →
  Control-plane mode). The same count splits backend `max_in_flight` caps: each
  gateway enforces cap ÷ live gateways, rounded up (settled 2026-09-25, D3;
  `GATEWAY.md`, Routing and reliability → Concurrency cap).

## Gateway status

- Sent on connect (when the client starts, and whenever a config stream connects), on
  change (state, applied config version, last rejection, a model's queue becoming
  non-empty or empty — not its depth changing in between, which the 10 s report
  carries: status traffic stays low under load — and a deployment's circuit opening
  or closing; these routing changes are spaced by a 1 s minimum gap: one inside the
  gap is sent at its end with the state as it is then, so a gateway sends at most one
  such report a second — `GATEWAY.md`, Status minimum gap), and every 10 s (settled
  2026-09-24: a failed report is simply retried by the next one; nothing waits for
  it): instance ID, protocol version, state
  (starting / ready / draining), start time, applied config version, last rejection,
  in-flight counts and caps per backend, queued requests per model, circuit state
  per deployment (exact fields: Messages, Status).
- A gateway silent for 30 s is dropped from the live set, and the live-gateway count
  is pushed again.

## Status intake (settled 2026-09-24)

How the control plane takes `POST /v1/status`, as `kaiak-control` implements it.

- **Checks, in order**, after the request checks: the status message (schema, then
  the message rules) — a schema violation answers `400 status-invalid`, a rule
  violation `400` with the rule's code (`timestamp-invalid`; the first one found,
  every issue in `detail`); the status's instance equal to the
  `Kaiak-Instance` header, else `400 instance-mismatch`. Accepted: `204`, no body.
- **Accepted before any config is published**: a starting gateway reports with
  nothing applied, and the live set must know it before its first batch.
- The latest status per instance is kept with its receipt time (the control plane's
  clock); a newer receipt replaces it whatever it says.
- **Live set**: an instance joins with its first accepted status and leaves when an
  expiry sweep finds it silent for 30 s (configurable). The live-gateway count is the
  set's size. A **draining** gateway stays live until it stops reporting: it still
  serves its in-flight requests under its per-minute share, and dropping it early
  would raise the other gateways' shares while it still spends. Rejected: leaving the
  set on `draining`.
- **Expiry sweep**: an invocable run (manual, or the core's timer every 5 s by
  default, so a gateway leaves between 30 and 35 s after its last status); each run
  reports its trigger, time and result (expired and forgotten instances, or the
  failure) to the host. A gateway expired and still silent after an hour
  (configurable) is forgotten — instance names churn with pods, and the list must not
  grow without bound. A forgotten gateway that reports again simply joins again.
- **Batch cursor retention** (settled 2026-09-25): an instance's last counted batch
  (the de-duplication cursor) is kept for 7 days (configurable) after it was counted,
  whether or not its gateway is still remembered, and the same sweep drops cursors
  past that (reporting the instances). A gateway partitioned for longer than the
  forget delay that resends a batch counted before its silence (an ack lost just
  before the partition) is acknowledged without counting; only a resend after the
  retention is counted again. The state stays bounded by the instances that counted
  a batch in the last 7 days. Rejected: dropping the cursor with the gateway's record
  at the forget delay — an hour-long partition then counted a batch twice.
- **Two processes under one instance name** (misconfiguration — `KAIAK_INSTANCE_ID`
  copied between replicas): each reports its own `started_at`, so the stored start
  time alternates. The rule: a status whose `started_at` differs from the stored one
  **and equals the start time that one replaced, last seen less than the live timeout
  ago**, flags the instance (`conflict`, reason `started-at-alternating`). A restart
  moves to a new start time once and never back, so it does not flag. The flag clears
  once no alternation is seen for the live timeout. It is made visible (the host's
  gateway list; a warning log when raised), not resolved: the two processes also
  share one usage de-duplication slot, whose alternating epochs each count as a fresh
  spool. Rejected: flagging any start-time change while the old one is fresh — every
  restart within 30 s would flag.

## Control-plane processes (settled 2026-09-25, D7)

- **One control-plane process per store.** The config versions' order, the totals
  revision and its consistent snapshots, the subscriptions that feed the streams and
  the expiry sweep's timer all live in one process: a second process on the same store
  would assign clashing versions, push totals out of order and miss the other's
  publishes. Several replicas behind a load balancer need a store (and a core) built
  for it — not this one.
- **Enforced by a store lease**: `kaiak-control`'s core takes the store's lease when it
  starts (the Fastify plugin starts it when the app is ready), renews it every third
  of its time to live (default 30 s) and gives it up when it stops. A core that finds
  the lease held by another, unexpired, refuses to start (`store-lease-held`, naming
  the holder and the lease's end) — a startup error, not a silent second writer. A
  lease a crashed process left behind expires, so a replacement starts once it has
  run out. A renewal that fails (another holder took it after this process stalled
  past the lease, or the store failed) is reported to the host, which by default
  stops the process. The in-memory store implements the lease too; it is one per
  process by construction, so the lease matters for stores that outlive a process.
- **Trust model**: gateways share one token (Shape → Auth). A token holder can report
  usage and status under **any** instance ID — the body's instance is checked against
  the header, not against the caller's identity. The token is an operator secret like
  a provider key: whoever holds it can report usage that spends any budget, and
  statuses that change the live-gateway count (so every gateway's per-minute share),
  under any gateway name. Per-instance tokens are in the backlog.

## Control-plane outage

- Gateways keep serving on their current config and last pushed totals, hold usage
  (in the spool, or in memory without a data directory),
  and reconnect with backoff; per-minute limits keep being enforced locally.
- **Priced models with a money limit fail closed** once the outage exceeds a configured grace
  period — `global.control_outage_grace_ms` in the config, whole milliseconds, 0 or
  more, default `900000` (15 minutes; a policy decision, so it lives with the policy;
  ignored in file mode) — requests to them get `503` until the
  control plane is back. Other models keep serving — unpriced models included, whatever
  USD limit names them: they cost nothing (settled 2026-09-25, D6; `GATEWAY.md`,
  Limits → Unpriced models).
- **What counts as an outage** (settled 2026-09-24): contact is a snapshot fetched,
  bytes on the config stream (heartbeats included) or a usage ack, and an open
  stream is contact while it stays open; the outage is no open stream and no
  contact for longer than the grace, counted from the gateway's start when it never
  reached the control plane (a last-known-good or seed boot). A request is refused when its
  model has a price in force and any limit that applies to it — any of its scopes, a
  model set covering its model — is a `usd_per_month` limit: `503 budget_unavailable` (`GATEWAY.md`, Client API). The
  first contact ends it; status reports are not counted as contact (they are
  best-effort and carry nothing back). While usage batches wait for an answer, the
  gateway is also in outage once they have waited past the grace, an open stream
  notwithstanding (settled 2026-09-25, M16; `GATEWAY.md`, Limits → Usage acks count
  for money limits).
- **Not an outage, same refusal**: while the newest totals are for another config
  than the one the gateway applied (typically a config it rejected) for longer than
  the grace, the same models get the same `503 budget_unavailable` — the control
  plane is reachable but counts other limits (`GATEWAY.md`, Limits → Control-plane
  mode: totals follow their config). It shows in `kaiak_control_config_mismatch`,
  not in `kaiak_control_outage` (settled 2026-09-25, N-P3): the remedy is the
  config, not the control plane.
- **Not an outage, same refusal: no totals yet** (settled 2026-09-25, D8): a gateway
  that has not yet applied totals for its config since it started (nor restored
  them from its data directory) does not know the spend, and refuses the same
  models the same way until they arrive. Its readiness waits for them within the
  boot wait (`GATEWAY.md`, Control-plane mode → Readiness waits for the first
  totals), so this shows only when they are late; the totals that follow the
  config on every stream connect (Config stream) end it.
- On reconnect: snapshot, resume the stream, send the held batches in order.
