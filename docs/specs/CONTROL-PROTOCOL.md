# Control Protocol

> The contract between gateways and a control plane. The control-plane side is
> implemented once, in `kaiak-control`; the sample control plane and the real one both
> use it. Machine-readable form: JSON Schemas and fixtures in `protocol/` — a change here
> updates both, and both halves, in the same change (`AGENTS.md`, Project-Specific
> Rules). Decisions settled 2026-09-24 unless dated otherwise; message shapes are
> settled (Messages), endpoint paths stay provisional until the control-plane plan
> implements them.

## Shape

- **Control plane → gateway**: one **SSE stream** per gateway — the current config,
  then the usage totals, then every change of either (Config stream).
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

- `GET /v1/stream` → SSE events (behavior: Config stream; settled 2026-10-07, no
  parameters):
  - `config` — the current config with its `config_hash` (Current config);
  - `totals` — the control plane's usage totals per scope and type (global and every
    group with usage) for hourly and monthly windows, the number of live gateways, and
    the last of this gateway's batches they include in each epoch (Budgets; Messages →
    Totals).
- `POST /v1/usage` → one usage batch; the answer is the ack, which names the batch and
  nothing else (Usage batches).
- `POST /v1/status` → gateway status; `204` with no body when accepted (Status
  intake).
- Every body above is one of the messages below (Messages).
- Rejected (2026-10-07): a config snapshot endpoint beside the stream
  (`GET /v1/config`) — the stream's first event is the current config, and two ways to
  get it are two paths to keep consistent.

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
  `usage-batch-invalid` and `instance-mismatch` (400, Usage intake),
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

## Current config (settled 2026-10-07)

- **The app owns the config.** The host app built on `kaiak-control` is the source of
  truth: it composes the config from its own data (keys, groups, models, backends),
  keeps whatever history and audit it wants, and decides who may change what and
  when. `kaiak-control` validates the document the app publishes and broadcasts the
  **current config** to the gateways — it keeps no history and assigns no versions.
- **Publishing replaces the current config.** A config that fails validation (schema,
  semantic rules, the parents rule below) is refused with its issues and the current
  config stays. Before the first publish there is no config: streams stay open and
  send it when it is published.
- **`config_hash`**: the lowercase hex SHA-256 of the config's JSON text exactly as
  the control plane sends it (the `config` member of the `config` event). It
  identifies content, so a gateway can skip a config it already runs or already
  rejected and can report which config it applied or rejected. It carries no order:
  two hashes are equal or not, never older or newer. **The store keeps that text**
  (settled 2026-10-07): a publish writes the config's JSON once, hashes it, and every
  process sends the stored text as it is. Rejected: storing the document as a JSON
  value and writing it out again for each stream — a database JSON type (Postgres
  `jsonb`) does not keep member order, and the text sent then no longer matches its
  hash.
- **One current config across processes**: a publish is stored only if the config it
  was checked against is still the current one — a condition inside the store,
  invisible on the wire. Two publishes racing (in one control-plane process, or in
  two over one store — Control-plane processes) never interleave: the one that loses
  is checked again against the winner, the parents rule included, and replaces it or
  is refused. Concurrent editing is the app's to coordinate; the last publish to land
  is the current config. A publish depends on no usage: counted batches never refuse
  it, and it never refuses them (Usage intake → Counted whatever the config).
- **Parents never change** (settled 2026-09-27): a publish that gives a group the
  current config defines another `parent` than it has there — a top-level group
  given a parent and a child made top-level included — is refused like a config that
  fails validation, code `group-parent-changed` (control plane only; the issue names
  the group). A move is a delete and a create: the group's ancestors are what its
  usage records name and its spend was counted toward, so they never change under
  it. The check compares with the current config only — the first publish has
  nothing to compare, and a group deleted in one publish may come back under another
  parent in a later one. The gateway does not check it: it is stateless and may boot
  with no previous config (a seed, a fresh start), so it has nothing to compare
  with.
- **The gateway applies what the control plane sends.** The control plane is the
  authority on which config is current; the gateway applies every `config` event it
  receives, skipping one whose hash equals the config it runs or the one it last
  rejected (Config stream). Keeping the order of what goes out on a stream is the
  sending process's job (Config stream → Order), not the gateway's.
- Rejected (2026-10-07):
  - config versions with a bounded history, a resume position (`since`) and `resync`
    (settled 2026-09-24) — every config is a whole document, so a gateway only ever
    needs the current one; replaying a history sends configs the app has already
    replaced, and a restored store's history no longer matches the positions its
    gateways resume from;
  - a gateway that only applies a config newer than the one it runs — it protects
    against nothing the sender cannot prevent on its own stream, and after a store
    restore (an older config current again) it kept gateways on a config the control
    plane no longer has;
  - a config epoch naming the store a version counts in (settled 2026-09-25) — a
    store restored without taking a new one had its configs and totals ignored by
    every gateway, and with no versions there is nothing for it to name.

## Config stream (settled 2026-09-24; the current config only, 2026-10-07)

- `GET /v1/stream` takes no parameters. The response is `text/event-stream`,
  `Cache-Control: no-cache`, `X-Accel-Buffering: no` (proxies must not buffer it),
  never compressed, carrying `Kaiak-Protocol` like every response.
- **On connect** the control plane subscribes to changes *before* it reads the current
  config, then sends a `config` event with the current config, then a `totals` event
  from a totals read issued after the stream connected (Totals, below).
  With no config published yet the stream stays open (heartbeats only) and sends both
  when the first config is published. Totals go out only once the stream has sent a
  config.
- **After that**, every change of the current config — published by any control-plane
  process (Control-plane processes) — sends a `config` event with the config current
  when it is read; a quick run of publishes may reach the stream as one event with the
  newest. Events carry no `id:`.
- **Order** (settled 2026-10-07): a process sends on a stream what it read last, by
  when it **issued** each read — never by a value the store returns. A config read
  issued before a config already sent on that stream is dropped when it completes,
  and likewise for totals. The two kinds are ordered apart: totals do not depend on
  the config (Messages → Totals), so a config and a totals read may go out in either
  order. Rejected: ordering by a sequence the store moves with each change, and
  comparing what each read returns — notifications, concurrent reads and a config's
  publish all report it out of order under a database store, so ordinary traffic
  read as the store going back.
- **A config that cannot be read** (settled 2026-10-07): after a change, a process
  reads the current config to send it; a read that fails is tried again with growing
  delays (`kaiak-control`: 100 ms doubling, five retries, configurable), and one that
  still fails is reported to the host and closes every stream the process holds, so
  its gateways reconnect and read the current config on connect. A totals read that
  fails is retried at the push interval; a stream sends no totals it has not got, so
  a lasting failure shows on the gateways as usage that stays uncounted, which is an
  outage past the grace (Control-plane outage → What counts as an outage). Rejected:
  dropping the failed config read until the next change — a published config (a
  revoked key) would stay off that process's gateways with nothing reported; closing
  the streams once totals reads keep failing (settled 2026-10-07) — a second
  mechanism for what the gateways' rule already covers, and blind to totals that stop
  for other causes (a lost notification channel).
- **A restored store is the current state** (settled 2026-10-07): after a restore from
  a backup, or a failover to a standby that was behind, the store's config and totals
  are what processes read, and they go out like any other — the restored config is
  sent to every gateway not running it, and the totals' windows are the restored
  ones. Nothing in the protocol tells a restore apart; how to restore is in
  Control-plane processes → Restoring the store. Rejected: detecting a rollback
  (a store sequence going back) and closing every stream — the sequence arrives out of
  order on ordinary traffic, so the check fires with no restore, and a gateway
  reconnecting gets the same current state that a push carries; requiring a store
  that rolls back to take a new identity (a config epoch) — it depends on whoever
  restores the store remembering to.
- **Heartbeat**: a comment line (`: heartbeat`) every 15 s, so idle proxies and load
  balancers keep the connection. The gateway may treat a much longer silence as a dead
  connection and reconnect.
- **Totals** (settled 2026-09-24; on the stream only, complete then changes,
  2026-10-07): a `totals` event (`data:` the totals on one line) follows the config on
  connect, **complete** — every window with usage (Messages → Totals). After that it
  goes out whenever the totals change (a counted batch, made by whichever
  control-plane process — Control-plane processes — or a gateway joining or leaving
  the live set), **at most one per second per stream**, listing **only the windows
  that changed** since the stream's last totals: the first change after a quiet
  second is pushed at once, later ones within the second become one trailing push.
  A counted batch that changes no window (it cost and counted nothing) still moves
  its instance's `counted_through`, and is pushed for that. Each push carries the
  newest values its process has read. A publish changes no
  totals. **The first totals on a stream come from a totals read issued after the
  stream connected** (settled 2026-10-07), so they include every batch counted
  before it — whichever process counted it, and whatever another process already
  sent the gateway; until that read succeeds the stream sends no totals. Rejected:
  starting a new stream from the process's latest read — after a reconnect to
  another process it can be older than the totals the gateway applied, whose usage
  the gateway has already stopped counting as its own, so that usage is in neither
  count until the next read; and a process whose reads fail would hand a new stream
  its last good read as complete. The stream is the only way totals reach a gateway (Usage batches: the ack
  carries none). Rejected: the complete totals on every push — with every scope's
  windows listed, a deployment with thousands of groups sends megabytes to every
  gateway every second.
- **Slow readers** (settled 2026-09-24): config events wait in the control plane's
  send buffer until the gateway reads them; a heartbeat is skipped while earlier
  events are still waiting. Totals never queue: while the buffer waits to drain, one
  push is held and sent on drain with every window that changed since the last
  totals sent, at its newest value. A stream whose socket
  takes nothing for **30 s** (the plugin's `stalledStreamTimeoutMs`) is closed; the
  gateway reconnects. Rejected: queueing every totals event behind a slow reader —
  frequent totals would grow the buffer with values already out of date.
- The stream ends when the gateway disconnects (the control plane drops its
  subscription), when it stalls (Slow readers), when a config cannot be read (above),
  or when the control plane shuts down — the gateway reconnects (to another replica, or with backoff) and
  takes the current config and totals again.

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
| config event (`config-event`) | `{ config_hash, config }` — `config` is a full config document |
| totals (`totals`) | `{ live_gateways, counted_through, windows: [window…] }` |
| totals window | `{ group?, type, window_start, used }` |
| usage record (`usage-record`) | the record (Usage records) |
| usage batch (`usage-batch`) | `{ batch: { instance, epoch, sequence }, records: [record…] }` |
| usage ack (`usage-ack`) | `{ batch: { instance, epoch, sequence } }` |
| status (`status`) | `{ instance, protocol_version, state, started_at, applied_config_hash, last_rejection, backends, models }` |
| status backend | `{ in_flight, max_in_flight?, deployments: { <backend model name>: { circuit, opened_at? } } }` |
| status model | `{ queued }` |

- **Stream events**: SSE `event:` names the event, `data:` holds one JSON message on
  one line: `config` → a config event, `totals` → totals. The event name picks the
  schema; the data carries no type field of its own. A `totals` event the gateway
  cannot decode ends the stream (settled 2026-10-07; `GATEWAY.md`, Control-plane
  mode → Stream): a later totals event lists only what changed, so the reconnect's
  complete totals are the only way back to what the control plane holds. Other
  malformed events are logged and skipped.
- **Config event**: valid only if its config passes the config schema and the
  semantic rules. The gateway reads the envelope first and hands the config to its
  config loader: a config it rejects is a *config rejection* — kept out of force and
  reported in status with its hash and codes — not a malformed message.
- **Usage batch**: `epoch` is 32 lowercase hex digits (128 random bits, new with each
  fresh spool); `sequence` starts at 1 and increases by one per batch within the
  epoch. A batch holds **1 to 500 records**: a gateway with nothing to report sends
  nothing, and a gateway with more queues further batches behind the outstanding one.
  Every record's `gateway_instance` is the batch's instance, and record IDs are unique
  within a batch.
- **Usage ack** (settled 2026-10-07): the batch ID it acknowledges — counted now or
  before, the answer is the same — and nothing else. The gateway never sends the
  batch again; its own usage leaves its counters only through stream totals (Totals:
  `counted_through`), and with a data directory the batch stays in the spool until
  totals covering it are saved (`GATEWAY.md`, Usage spool). Rejected: fresh totals in every ack (settled 2026-09-24) — acks
  and stream pushes travel on different connections and cross, so the gateway has to
  order totals by a revision, which a store restored from a backup sets back.
- **Totals** — the `totals` event's data, made for the stream's gateway:
  - **No order on the wire** (settled 2026-10-07): totals travel only on the stream,
    and the process that holds the stream sends them in order (Config stream →
    Order). The gateway applies each one as it comes. Rejected: a `revision` on every
    totals message, ordered by the gateway — totals on one stream need none, and
    totals from two connections are what makes one necessary.
  - `live_gateways` — the live-gateway count. Each gateway enforces its share of
    every per-minute limit (limit ÷ count, rounded down, at least 1 unless the limit
    is 0) and of every backend `max_in_flight` cap (cap ÷ count, rounded up, at least
    1), treating 0 as 1 (Totals → Per-minute windows).
  - `counted_through` — a list of `{ epoch, sequence }`: for the recipient's
    instance, the last usage batch the control plane has counted **in each epoch** it
    still keeps a batch cursor for (Status intake → Batch cursor retention), one entry
    per epoch, in no particular order; empty before its first (settled 2026-09-24;
    per epoch 2026-10-07). Every totals message carries the whole list, changes-only
    ones included. When the gateway applies a totals message it stops counting as its
    own usage every batch at or below its epoch's entry: those are inside the windows
    it now applies. Between an ack and the next push the gateway keeps counting the
    acknowledged batch as its own — about a second, a conservative over-count, never
    a double count (settled 2026-10-07). Rejected: one `{ epoch, sequence }`, the
    batch counted last in any epoch — a gateway process that died with a batch write
    stalled is replaced under the same instance name with a new epoch; when the old
    write commits after the new process's first batch, the one cursor names the old
    epoch from then on, and the new process can never retire its own usage.
  - **Consistent snapshot**: a message's windows include exactly the batches the
    control plane had counted when it read them — `counted_through` among them. The
    store guarantees it (settled 2026-10-06, Control-plane processes): a batch's
    counting is one store write, and the windows and every instance's cursors are
    read from one store snapshot, so it holds whichever process counted and whichever
    reads. A changes-only message lists the windows that changed between two such
    snapshots, with the later one's `counted_through`, so what the gateway holds after
    applying it is the later snapshot. The windows read are the ones current once the read is over (settled
    2026-10-07): a read that crossed an hour or month boundary is made again with the
    new windows, since a batch counted past the boundary meanwhile is in a window the
    first read did not ask for. Rejected: a read without that guarantee, where a
    message could show a batch counted that its windows lack — usage dropped from
    enforcement until the next push.
  - `windows` — **every scope and type with usage, whatever the config** (settled
    2026-10-07): global and every group usage has counted toward in the control
    plane's current window (Usage intake → Counted toward), whether or not a config
    limits it, one entry per scope and type. The first totals on a stream are
    **complete**: a window not listed has used nothing in the control plane's current
    window. Each later one lists **only the windows that changed** since the stream's
    last totals: a window not listed keeps the value it last had. Within a window usage
    only grows, so a listed window carries its full `used`, which replaces the old
    value — a repeated or coalesced push is harmless. A window the last totals listed
    that a later read lacks within the same window (a store restored to less) is
    listed with `used` `"0"`; one that ended with its hour or month is not listed —
    the gateway starts the next window itself. Per-minute limits are never
    listed. Rejected: listing only the windows of the current config's limits — a
    gateway still running a config it was moved off (it rejected the new one) enforces
    limits the new config dropped, and read them as unspent.
  - A window names its scope and type (settled 2026-09-27, model set removed
    2026-10-06): `group` (absent for global) and `type` (`tokens_per_hour` or
    `usd_per_month`) — the identity the gateway keeps its counts by (`GATEWAY.md`,
    Limits).
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
- **Matching totals to limits** (enforced by the gateway, `GATEWAY.md`, Limits;
  settled 2026-10-07): the gateway keeps an hour and a month count for every scope and
  type, limited or not, and a limit is a check over its scope's count. A window's
  `used` is the pushed base of the count with the same group (or global) and type,
  **whatever config the gateway runs** — windows are counted per scope and type
  whatever the config (Usage intake → Counted toward), so they mean the same under
  every config. A window newer than the count's current one starts that window. The
  first totals on a stream set every base (a scope not listed: 0); later ones replace
  the bases they list. Rejected: applying totals only when they were computed under
  the gateway's config (settled 2026-09-25, H3), with a mismatch state refusing
  budgets after a grace — a gateway that rejects a config keeps stale bases; keeping
  the last base of a limit the totals stop listing — it misses the other gateways'
  new spend for that limit.
- **Status**: `state` is `starting`, `ready` or `draining`; `started_at` is when the
  gateway process started; `applied_config_hash` is the `config_hash` of the applied
  config, `null` until a config from the control plane (or its last-known-good copy)
  is applied — a gateway serving its seed config reports `ready` with `null` (settled
  2026-09-25: `ready` means a config is in force); `last_rejection` is
  `{ config_hash, codes }` when **the latest config the gateway received from the
  control plane was rejected**, else `null` (`codes`: the rejection's issue codes, as
  the gateway logs them) — set on a rejection, cleared when a later config is applied
  or when the config received is the one the gateway runs (settled 2026-09-24; the
  running config, 2026-10-07: republishing the running config is how an operator
  backs out a bad publish, and the latest config received is then the one in force). A last-known-good boot does not clear it (that copy is not a
  config the control plane sent). Routing state (settled 2026-09-24), both
  collections empty before the first config is applied:
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
  - `totals-window-duplicate` — two windows for one scope and type;
  - `counted-through-epoch-duplicate` — two `counted_through` entries for one epoch;
  - `record-instance-mismatch` — a record's `gateway_instance` is not the batch's
    instance;
  - `record-id-duplicate` — two records in a batch share a record ID.

  A config event also reports its config's own codes. Schema violations have no
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
- **Strict everywhere** (settled 2026-10-06): every object but the ID-keyed
  collections and a group's `labels` (an open map whose keys follow a pattern) has a
  closed set of fields. A model names no request parameters: the one its config sets
  is the output limit (`GATEWAY.md`, Model metadata → no model defaults).
- **Integers are bounded** (settled 2026-09-25): every config integer (and every limit
  `value`) is at most 2^53 − 1, so both halves read it exactly — `kaiak-control` as a
  JavaScript number, the gateway as an `int64`. An integer may be spelled with a
  fraction or exponent (`4096.0`, `1e3`), as JSON Schema counts it.
- **Public model names** (the `models` keys, and every `allowed_models` entry) must
  not end in `/props` (settled 2026-09-24): the gateway serves
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
    must pass every limit of each scope, and its usage counts toward each of them.
  - **Limits** (settled 2026-10-06): `{ type, value }`, at most one per type in a
    scope (global, a group, a group's `child_defaults`), covering all of the scope's
    usage — every model, counted together. A limit's **identity** is (group, type); a
    global limit has no group. Totals windows, the gateway's counters and the
    file-mode snapshot all key limits by it. An unpriced model costs nothing, so a
    `usd_per_month` limit already counts only priced models' spend. Rejected:
    - limits on a set of models (the `models` member, settled 2026-09-24): budgets
      are set per group, a model set made one scope hold several limits of a type,
      and an edited set made a new identity whose spend had to be carried over at
      publish — tying publishes to usage counting across control-plane processes;
    - an `id` per limit to keep identity through edits — one limit per type in a
      scope needs none.

    A money budget across a provider's backends is a separate concern (backlog:
    provider budgets).
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
    limits replaces the default limit of the same type, in place,
    and default limits it does not override still apply. A group's **effective
    limits** are that merge: the parent's `child_defaults.limits` in their order,
    each replaced in place by the group's limit of the same type, then the
    group's other limits in their order. `child_defaults` on a group without
    children has no effect and is not an error.
  - **`child_defaults` is a default, not a ceiling** (settled 2026-09-27): since a
    child's own list or same-type limit replaces the default, a child can be
    given more than the default — more models, a higher value. A hard restriction
    for a whole subtree goes on the parent's **own** `allowed_models` and `limits`,
    which bind every group below it (allowed models intersect down the path, and
    every scope's limits apply). For the `users` example: the models and budget no
    person may exceed are `users`' own list and limits; its `child_defaults` are
    what each person gets unless their own group says otherwise.
  - **Counters are bounded** (settled 2026-09-27; counted by what is allocated,
    2026-10-07): the counters a config allocates on every gateway are at most
    **50 000** (`counters-exceeded`): two for global and two for every group (the
    hour and month counts every scope keeps, limited or not — `GATEWAY.md`, Limits →
    Every scope is counted), plus one for each effective per-minute limit
    (`global.limits`, and each group's own limits merged with its parent's
    `child_defaults.limits`). A per-minute counter holds a minute of one-second
    buckets (about 1.4 KB), an hour or month counter a few hundred bytes, so the
    bound keeps counter memory under about 70 MB. Groups and `child_defaults` multiply
    — D default per-minute limits on a group with N children are D × N — so a config a
    few hundred KB long could otherwise take gigabytes on every gateway, and a gateway
    killed by it refetches it on restart: a fleet-wide crash loop. The value is a
    constant in both halves. Rejected: bounding the effective limits only — every
    scope is counted whether or not it has limits, so groups without limits passed
    the bound however many there were.
  - **Parents never change** once published: a move is a delete and a create.
    `kaiak-control` refuses a publish that changes one (`group-parent-changed`,
    Current config); the gateway does not check it. A group created again under an
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
  - `counters-exceeded` — the counters the config allocates (two per scope, global
    and every group, plus one per effective per-minute limit) are more than 50 000
    (Config → The group tree). Reported once, at the document root (path `""`). The
    count needs only each group's direct parent, so it is made whatever the other
    tree rules find; a group whose `parent` has no entry counts its own per-minute
    limits alone.
  - `deployment-backend-unknown` — a deployment's `backend` has no entry.
  - `allowed-model-unknown` — an `allowed_models` entry (a group's or a
    `child_defaults`') names no model.
  - `allowed-models-wildcard-mixed` — `"*"` listed alongside model names (a group's
    list or a `child_defaults` list).
  - `limit-duplicate` — two limits in one list (`global.limits`, one group's
    `limits`, one group's `child_defaults.limits`) share a type. A group's limit with
    the type of one of its parent's defaults is an override, not a duplicate.
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
  (its `config_hash` and codes) in its status.
- Provider credentials appear only as environment-variable names. A backend's
  `base_url` carries no userinfo (`user:password@`; settled 2026-09-25): a URL
  credential would travel in config events, last-known-good copies and host pages.
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
  previous one (Usage intake → Counted in its own window). Several control-plane
  processes need clocks kept in step (settled 2026-10-07): NTP-synchronized, within
  a second of each other. Two processes on either side of an hour boundary serve
  windows of different hours for as long as they disagree, and the expiry sweep
  compares one process's clock with a receipt time another wrote (Status intake).

## Usage batches (settled 2026-09-24)

- Each gateway fills one batch of usage records and sends it; it has **at most one
  batch outstanding**. Records settled meanwhile go into the next batch.
- A batch carries an ID: the gateway's instance ID, an **epoch** (random, created with
  a fresh spool, so a replaced pod reusing an instance name is never mistaken for the
  old one) and a **sequence number** increasing per batch within the epoch.
- The control plane remembers the last batch ID it counted per instance and epoch. A
  batch it has already counted (a resend after a lost ack) is acked again without
  counting — delivery is at-least-once, counting exactly-once, with state bounded by
  the epochs each gateway counted a batch in within the cursor retention.
  Record IDs stay in the records for audit and storage.
- **The ack names the batch and nothing else** (settled 2026-10-07): on ack the
  gateway stops sending the batch. It keeps counting the batch as its own until
  stream totals show it counted (Messages → Totals: `counted_through`), so its own
  usage is never counted twice or missed in between, and with a data directory keeps
  it in the spool until totals covering it are saved, so a crash in between loses
  none of it from its budgets (`GATEWAY.md`, Usage spool).
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
    `5xx`, a malformed ack or an ack naming another batch, on the reconnect backoff
    (Config stream).
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
  instance-mismatch` (the code status reports use too). A batch is counted whether or
  not a config has been published (settled 2026-10-07): counting does not depend on
  the config (Counted whatever the config), and the ack carries nothing a config is
  needed for. Rejected: `503 config-unavailable` before the first publish (settled
  2026-09-24) — it holds every gateway's usage back on a control plane with nothing
  published yet, although counting needs no config.
- **De-duplication** by the last counted batch ID per instance **and epoch** (settled
  2026-10-07): nothing counted for the instance yet → counted; a sequence above the
  last of its epoch → counted (a skipped sequence is counted too and logged: records
  lost on the gateway side are its loss, and the control plane must not stall on
  them); a sequence at or below the last of its epoch → acked again, not counted (a
  resend, or a straggler behind a later batch); nothing counted yet in its epoch
  after batches of another → counted and logged as a new epoch (a fresh spool).
  Batches from one instance are taken one at a time within a control-plane process;
  across processes the store's conditional write (below) decides, so two copies racing
  (a retry beside the original) are counted once either way — also when the gateway
  moved to a new epoch while one copy's write stalled. The totals' `counted_through`
  lists the instance's last counted batch of each epoch kept (Messages → Totals).
  Rejected: one last batch per
  instance (settled 2026-09-24) — a write of an earlier epoch's batch, stalled while
  its resend was counted elsewhere and the gateway started a new spool, read the new
  epoch's batch as "a different epoch" and counted the batch again.
- **Counted atomically**: the batch ID becomes the instance's last of its epoch, its
  amounts join the totals and its records join the recent records in one store write — a batch is counted and
  remembered, or neither. Rejected: separate writes, where a crash between them loses
  a batch or counts it twice.
- **Counted whatever the config** (settled 2026-10-06): a batch's amounts do not
  depend on the config — every record counts toward its scopes' windows (Counted
  toward) — so a publish never refuses a batch's write, and a batch never refuses a
  publish. Rejected: counting toward the limits of the config in force, with the
  write conditional on that version — every publish races usage, and a limit's spend
  depends on which config a batch's write happened to see.
- **Exactly once in the store** (settled 2026-09-25, D7): the write is conditional on
  the last batch ID of the batch's epoch the decision was made against — the store
  compares and writes in one atomic operation, and when another writer counted a batch
  of the instance in that epoch in between, it writes nothing and returns where the
  instance's counting stands, and the batch is decided again (typically a duplicate
  now). So the store, not one process's
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
- **Counted toward** (Budgets; settled 2026-09-27, by scope 2026-10-06): a record's
  scopes are global and every group its `groups` lists — the path as recorded, never
  re-derived. In each scope, the record adds
  `tokens_in + tokens_cache_write + tokens_out` (written tokens count, settled
  2026-10-02; input read from the cache does not, settled 2026-10-05 — `GATEWAY.md`,
  Limits → Settle) to the scope's `tokens_per_hour` window and `cost_nano_usd` to its
  `usd_per_month` window for the record (Counted in its own window), **whether or not
  the scope has a limit of that type** in any config: windows are kept per scope and
  type, and totals list every one with usage (Messages → Totals). Per-minute limits
  are not counted (they stay local to gateways). Rejected: counting only toward the
  limits of the config in force — it ties counting to publishes.
- **Groups the config no longer defines** (deleted after the gateway settled the
  record) still count toward their windows, which no limit checks; the listed groups
  that remain and global count as always — so usage settled just before a delete
  counts toward the ancestors that remain, and records stay readable after the tree
  is reorganized. Parents never change (Current
  config), so a listed group that still exists has the ancestors it had when the
  record was made — unless it was deleted and created again under the same ID,
  which counting cannot tell apart (the record counts toward what the ID names
  now, as its windows do: Totals). Rejected: dropping such records, which would
  under-count the surviving scopes' budgets on every config edit that races
  traffic.
- **Totals** list every scope's windows whatever the config (Messages → Totals), so
  an edited limit keeps its spend, a removed limit's scope keeps its count, and a
  limit added within a window checks the scope's usage counted in that window so far
  — a monthly budget added mid-month counts the month (settled 2026-10-06). Rejected:
  a new limit starting at 0 — a scope's spend does not depend on whether it has a
  limit. **A group ID used
  again resumes its window's spend** (settled
  2026-09-27): a group deleted and created again with the same ID within the same
  hour or month — a move included, whatever its new parent — gets that window's
  amounts back for each limit of its types; it is a new group in the tree,
  but the ID's budget in the current window carries on. A fresh budget takes a new
  ID. Rejected: dropping a deleted group's windows at publish — every other limit
  that returns within its window comes back with its spend, and a group's limits
  are no exception. Windows with nothing used are not listed. A
  window past the 18-digit ceiling of `used` is reported at the ceiling.
- **Sums are exact**: amounts are summed as `BigInt` from record to wire.
- **Past windows** older than the previous hour or month are no longer needed (the
  previous one still takes late records); each process's expiry sweep lets the store
  drop them once an hour (settled 2026-10-07; a manual sweep run included). Totals
  only ever read current windows, so a store that keeps old ones (for audit) answers
  the same. A store that fails the drop fails that sweep run, reported to the host
  like any sweep failure. Rejected: dropping them while counting the first batch of
  an hour — a failing drop answered an already counted batch with an error, and the
  gateway resent it.
- **Recent records**: the last 100 received records (configurable) are kept with their
  receipt time, for a host's view of recent traffic; duplicates add none.

## Budgets

- The control plane aggregates usage per group and global, per hourly and monthly
  limit window: its clock picks the current windows, each record counts
  in its own window when that is the current or previous one (Usage intake).
- **Totals, not allowances** (settled 2026-09-24): it pushes each scope's used amount
  per window; limits come from the config both sides hold. Each gateway enforces
  `pushed totals + its own usage not yet counted` against the limit — not yet counted
  meaning above its epoch's `counted_through` entry in the totals it has seen, in-flight requests
  included. Pushing a remaining
  allowance instead would let each of N gateways spend all of it. The overshoot is
  bounded by the other gateways' unreported usage — about one batch interval each
  (principle 6).
- Totals are pushed on the stream after each batch that changes them, at most once a
  second per gateway — the complete set on connect, then the windows that changed
  (Messages, Totals). The push rules — on connect and on live-set changes too,
  coalescing, order, slow readers — are in Config stream. Acks carry no totals (Usage batches).
- **Editing a limit keeps its spend** (settled 2026-10-06): a limit is identified by
  its scope and type, so a new value applies to the window's spend so far, on both
  sides, with nothing to carry. Rejected: a model-set carry-over (settled 2026-09-25,
  D5), where a publish that changed a limit's model set raised the new limit's
  window to its predecessor's, and needed no batch counted between reading the spend
  and storing the version — a coupling of publishing and counting across processes
  that limits without model sets remove.
- **Per-minute windows** stay local to each gateway; the control plane pushes only the
  number of live gateways, and each gateway enforces limit ÷ live gateways, rounded
  down, 0 counting as 1, never below 1 unless the limit is 0 (`GATEWAY.md`, Limits →
  Control-plane mode). The same count splits backend `max_in_flight` caps: each
  gateway enforces cap ÷ live gateways, rounded up (settled 2026-09-25, D3;
  `GATEWAY.md`, Routing and reliability → Concurrency cap).

## Gateway status

- Sent on connect (when the client starts, and whenever a config stream connects), on
  change (state, applied config, last rejection, a model's queue becoming
  non-empty or empty — not its depth changing in between, which the 10 s report
  carries: status traffic stays low under load — and a deployment's circuit opening
  or closing; these routing changes are spaced by a 1 s minimum gap: one inside the
  gap is sent at its end with the state as it is then, so a gateway sends at most one
  such report a second — `GATEWAY.md`, Status minimum gap), and every 10 s (settled
  2026-09-24: a failed report is simply retried by the next one; nothing waits for
  it): instance ID, protocol version, state
  (starting / ready / draining), start time, applied config hash, last rejection,
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
  clock); a newer receipt replaces it whatever it says. The write is conditional on
  the record the status was judged against (settled 2026-10-06): a status another
  control-plane process took in between makes it fail, and the status is judged
  again against that one, so joining the live set and the conflict rule below hold
  across processes. A status's receipt time is read once (settled 2026-10-07): when
  the record that won holds a later receipt time, this status is older than the
  stored one and is dropped (accepted, not written) — written late, it would replace
  the newer status and, after a restart, read as the replaced process coming back.
  A gateway record's revision, which these writes compare, never repeats for an
  instance, a forgotten and recreated one included, nor across a restore of the store
  (Control-plane processes → Restoring the store): a write computed from a record
  that is gone never matches the record there now.
- **Live set**: an instance joins with its first accepted status and leaves when an
  expiry sweep finds it silent for 30 s (configurable). The live-gateway count is the
  set's size, the same for every control-plane process, read apart from the totals
  snapshot (nothing needs the two read together). A **draining**
  gateway stays live until it stops reporting: it still serves its in-flight requests
  under its per-minute share, and dropping it early would raise the other gateways'
  shares while it still spends. Rejected: leaving the set on `draining`.
- **Expiry sweep**: an invocable run (manual, or each control-plane process's timer
  every 5 s by default, so a gateway leaves between 30 and 35 s after its last
  status); each run reports its trigger, time and result (expired and forgotten
  instances, or the failure) to the host. **Every process sweeps** (settled
  2026-10-06): each expiry or forgetting is a write conditional on the record the
  sweep read, so a status taken meanwhile by any process keeps its gateway live, and
  two sweeps racing expire a gateway once; it forgets gateways in instance order, so
  two sweeps never take a database's records in opposite orders. Each run also drops
  past batch cursors (below) and, once an hour, past windows (Usage intake → Past
  windows). A gateway expired and still silent after an hour
  (configurable) is forgotten — instance names churn with pods, and the list must not
  grow without bound. A forgotten gateway that reports again simply joins again.
- **Batch cursor retention** (settled 2026-09-25; per epoch 2026-10-07): each epoch's
  last counted batch of an instance (the de-duplication cursor) is kept for 7 days
  (configurable) after it was counted,
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
  share one status record and one place in the live set, so the live-gateway count is
  one short and each one's per-minute shares are too large. Rejected: flagging any start-time change while the old one is fresh — every
  restart within 30 s would flag.

## Control-plane processes (settled 2026-10-06)

- **Any number of control-plane processes over one store.** Gateways may reach any of
  them — through a load balancer, one connection to one process and the next to
  another — and see one control plane. What makes them agree lives in the store, not
  in a process: the store is where processes coordinate, and a deployment that runs
  several processes provides a store that holds the guarantees below across them.
  `kaiak-control` holds every protocol rule on top of its store contract; its
  in-memory store serves one process (`control/kaiak-control/GUIDE.md`, the store).
- **What the store guarantees**, to every process:
  - **Conditional writes**: every write that changes the totals, the current config
    or the live set is made only if what it was computed from is still the store's
    state, else nothing is written and the process computes again — a counted batch
    on its instance's last batch (Usage intake), a publish on the config it was
    checked against (Current config), a status or an expiry on the gateway record it
    read (Status intake). Publishes and batches never condition on each other (Usage
    intake → Counted whatever the config).
  - **Consistent reads**: the windows and every instance's batch cursors come from one
    store snapshot (Messages → Totals: consistent snapshot); one snapshot can serve
    every stream a process holds.
  - **The config as text**: the store returns the config's JSON text exactly as
    published (Current config → `config_hash`).
  - **Every call settles**: a store call resolves or rejects; one that never does
    holds that process's config deliveries (one at a time) behind it with nothing
    reported, so a database store bounds every statement (a statement timeout).
  - **Change notification**: every process hears of every publish, totals change and
    live-set change any process made, so each sends to the streams it holds (Config
    stream). **A read after a notification sees the change** (settled 2026-10-07): a
    process that hears of a publish reads the current config, of a live-set change the
    gateway records, of a counted batch the totals, and each read sees what it was
    told of — a database store reads them from its primary, never from a replica that
    may lag the notification. A read that missed the change would send the old config
    (a revoked key still usable) until the next publish. A store whose change channel can drop notifications (a database's
    listening connection) announces a **catch-up** to every process each time the
    channel is back (settled 2026-10-07), after which a read sees every change made
    while it was down; the process then reads the current config, the totals and the
    live set again. A notification missed without a catch-up leaves that process's
    streams on an old config until the next publish, and their totals until the next
    change.
  - **No order of its own** (settled 2026-10-07): the store keeps no sequence. A
    process orders what it sends by when it issued each read (Config stream → Order).
- **Restoring the store** (settled 2026-10-07): stop every control-plane process;
  restore; move the gateway-record revisions past every value issued before the
  restore (a database sequence set ahead of anything it handed out); start the
  processes, each of which reads the current config, totals and live set on start
  (`start()` catches up). Rejected: restoring under running processes — an expiry or
  status write a process computed before the restore can match a restored revision
  and overwrite or expire a record written after it, and with the change channel up
  nothing tells the processes to read again, so their streams keep the old state.
- **Processes start and stop freely**: no process owns the store, so rolling updates,
  restarts and replicas need no ordering. The expiry sweep runs on every process
  (Status intake). What a process keeps for itself — the streams it holds, the
  batches of one instance it takes one at a time — needs no agreement with the others.
- Rejected:
  - one process per store, enforced by a store lease (settled 2026-09-25, D7) — it
    kept deployments from running replicas, and a process that stalled past the
    lease had to stop;
  - a leader for the sweep — conditional writes make running it everywhere safe;
  - a revision carried on totals for gateways to order (`{ control_plane, sequence }`,
    or one sequence per store) — totals travel on one stream, ordered by their sender
    (Config stream → Order);
  - a store-wide sequence moved by every change, for processes to order reads and to
    detect a restore — reads and notifications report it out of order under a
    database store (Config stream → Order, A restored store is the current state);
  - the core polling the store for changes made elsewhere — change notification is
    the store's guarantee, and a second mechanism for the same concern would stack a
    safety net on it.
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
  USD limits their scopes have: they cost nothing (settled 2026-09-25, D6; `GATEWAY.md`,
  Limits → Unpriced models).
- **What counts as an outage** (settled 2026-09-24; stream only 2026-10-07): contact
  is bytes on the config stream (heartbeats included), and an open
  stream is contact while it stays open; the outage is no open stream and no
  contact for longer than the grace, counted from the gateway's start when it never
  reached the control plane (a last-known-good or seed boot). A request is refused when its
  model has a price in force and any limit that applies to it — any scope on its
  path — is a `usd_per_month` limit: `503 budget_unavailable` (`GATEWAY.md`, Client API). The
  first contact ends it; status reports and usage acks are not counted as contact
  (they carry no totals back, so they prove nothing about the bases — a stream broken
  behind a proxy while acks still flow would otherwise never become an outage, and
  each gateway would spend the remaining budget on its own). While its usage waits,
  the gateway is also in outage once it has waited past the grace, an open stream
  notwithstanding (settled 2026-09-25, M16; `GATEWAY.md`, Limits → Usage acks count
  for money limits): a batch waits **for an answer** until it is acknowledged or
  refused, and an acknowledged batch of the gateway's own instance waits **to be
  shown counted** until stream totals whose `counted_through` covers it are applied
  (settled 2026-10-07). Totals that stop coming while the stream stays open — a
  process whose change channel died, or whose totals reads keep failing — leave a
  spending gateway's acknowledged batches uncovered, so the gateway that spends
  enters outage; one that spends nothing risks nothing. Heartbeats are contact, but
  they prove the connection, not the bases. Rejected: the control plane closing its
  streams when its totals reads keep failing — one rule on the gateway covers every
  cause, a dead change channel included, which the control plane cannot see.
- **Not an outage, same refusal: no totals yet** (settled 2026-09-25, D8): a gateway
  that has not yet applied totals since it started (nor restored them from its data
  directory) does not know the spend, and refuses the same
  models the same way until they arrive. Its readiness waits for them within the
  boot wait (`GATEWAY.md`, Control-plane mode → Readiness waits for the first
  totals), so this shows only when they are late; the totals that follow the
  config on every stream connect (Config stream) end it.
- On reconnect: the stream sends the current config and totals; the held batches go
  in order.
