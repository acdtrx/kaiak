# Architecture

> Module boundaries, data flow and deployment shape. Principles in `docs/kaiak.md`;
> subsystem contracts in `docs/specs/`; stack choices in `docs/TECH-STACK.md`. Update
> this file when the shape changes.
>
> Illustrated overviews: `docs/architecture/gateway.html` (gateway internals) and
> `docs/architecture/control-plane.html` (gateway ↔ control plane). Building a control
> plane on `kaiak-control`: `control/kaiak-control/GUIDE.md`.

## Two halves and a contract

- **`gateway/`** — the product: the `kaiak` binary (Go, standard library only). Serves
  client traffic from a config file or from a control plane, never waiting on one.
- **`control/`** — the control-plane side: `kaiak-control` (the reusable library) and
  `sample` (a thin app on it). Never in the request path.
- **`protocol/`** — JSON Schemas and shared fixtures for the config document and every
  protocol message. It is the contract: both halves run the same fixtures in their test
  suites, so neither can drift alone.

## Data flow

```mermaid
flowchart LR
    client[Clients<br/>OpenAI chat/completions/embeddings,<br/>Anthropic Messages, OpenAI Responses] -->|requests, SSE streams| gw[kaiak gateway<br/>N replicas]
    gw -->|passthrough: the same API<br/>at both ends| be[Backends<br/>vLLM, llama-server, SGLang,<br/>OpenAI, Azure OpenAI,<br/>Anthropic, Claude in Foundry]
    cp[Control plane<br/>kaiak-control] -->|SSE stream: current config,<br/>usage totals, live count| gw
    gw -->|usage records, status| cp
    cp -.->|backend verify: GETs,<br/>when the app calls it| be
    file[(Config file)] -.->|file mode| gw
    prom[Prometheus] -->|scrape admin port| gw
    gw -.->|log lines, OTLP/HTTP JSON,<br/>when configured| otel[OpenTelemetry<br/>collector]
```

- Client → gateway → backend is the only request path. A request reaches only a
  backend that speaks its client API natively (Anthropic Messages, OpenAI Responses or
  OpenAI's chat, completions and embeddings): the gateway passes it through with the
  edits it owns and never translates between APIs (settled 2026-10-06; translation is
  in `docs/BACKLOG.md`). Each backend type fixes the endpoints it serves
  (`docs/specs/GATEWAY.md`, Providers → Endpoint support).
- The control plane feeds the gateways config and receives usage and status in the
  background; a gateway with no control plane reachable keeps serving (file mode; in
  control-plane mode once booted, or from the seed config at boot) — except models
  under a USD limit, refused once the outage passes its grace.
- The control plane reaches a backend only when the app verifies one
  (`verifyBackend`, `docs/specs/BACKEND-VERIFY.md`): a few `GET`s to read what it
  reports about its models, off the request path, never on a schedule.
- Budgets are shared through totals, not allowances: each gateway enforces the
  control plane's pushed totals plus its own usage not yet counted, and divides
  per-minute limits by the live-gateway count (`docs/specs/CONTROL-PROTOCOL.md`,
  Budgets).
- Metrics and usage records are separate paths (`docs/kaiak.md`, principle 7).
- Logs go to stderr always and, with an OTLP endpoint in the `OTEL_*` variables, to
  an OpenTelemetry collector too — queued and sent in the background, never on the
  request path, flushed at exit after usage (`docs/specs/GATEWAY.md`, Observability
  → OTLP log export).

## Deployment shape

- **Gateways**: N identical, stateless replicas behind one Service (a Deployment on
  Kubernetes): read-only filesystem, no volume, only the control plane's URL and
  token required. Usage not yet acknowledged waits in memory; the drain delivers it.
  A seed config (free local models) covers a boot while the control plane is
  unavailable; with no config at all a gateway exits and is restarted. A gateway
  writes nothing to disk (settled 2026-10-07; `docs/specs/GATEWAY.md`, Configuration
  sources).
- **Control plane**: any number of processes over one store whose implementation holds
  the store contract (conditional writes, consistent reads, change notification with a
  catch-up — `docs/specs/CONTROL-PROTOCOL.md`, Control-plane processes); one process
  with the in-memory store. Never in the request path.
- **Admin port** (`9090`: probes, `/metrics`) stays inside the cluster; only the API
  port (`8080`) is exposed to clients.
- Details, sizing and alerts: `docs/DEPLOYMENT.md`.

## Gateway packages

The binary is `gateway/cmd/kaiak`; it builds the dependency graph and owns shutdown.
Subsystems are a flat list of packages under `gateway/internal/`, each added by the
step that first needs it. Go's `internal/` visibility and its ban on import cycles
enforce the boundaries.

- `config` — the config document: strict decoding, validation, resolution into an
  immutable snapshot (each group's path, effective allowed models and limits —
  `child_defaults` merged, `"*"` resolved), the holder that swaps
  it atomically, the one apply path every source uses (validate, swap, log, count),
  and the file-mode loader (startup and SIGHUP reload).
- `server` — the API and admin listeners (health, readiness, `/metrics`), the drain
  (admission, in-flight count, the shutdown sequence `cmd/kaiak` triggers on
  SIGTERM/SIGINT) and the request pipeline: a fixed ordered list of stages over one
  per-request struct (admission → auth → key concurrency → inbound → model access →
  model parameters → limits → attempts), plus the model endpoints' answers. Each
  endpoint has a client API format; the format decides the owned fields the inbound
  stage reads, the output-limit key, and the error shape of every answer the gateway
  gives on that route. The attempts stage is
  routing → accounting → provider run once per attempt: an attempt that failed before
  anything reached the client is retried — through all three again, the queue
  included — on another deployment when there is one; limits reserve once per client
  request and settle the sum of its attempts' records. Work that must happen however
  a request ends (the last attempt's slot release and circuit report, usage
  settlement, ops metrics, the log line) runs in request finishers.
- `auth` — key hash lookup (key → key ID, group and its path) and the one
  model-access check.
- `routing` — model name → deployment: least in flight among the eligible
  deployments (circuit closed) whose backend has a free slot, ties taking turns;
  owns the per-deployment and per-backend in-flight counts, the per-backend caps and
  the per-deployment circuit breakers (all of which outlive config reloads that keep
  the deployment), and one FIFO queue per model for requests that find no free slot.
  A single dispatcher under routing's lock hands each freed slot to the
  longest-waiting request that can use it; a waiting request blocks on its own
  channel under its request context (no goroutine of its own). A retry passes what
  its earlier attempts rule out (`Avoid`: deployments tried, deployments refused) and
  routing chooses — and dispatches — around them. `server` classifies each attempt's
  outcome and reports it on the attempt's slot (and, by the same classification, to
  the ops metrics); a circuit that
  opens starts one prober per backend (goroutines owned by `RunProbers`, which
  `cmd/kaiak` runs until shutdown) calling the probe function it was given —
  `provider`'s models-list probe, so routing never talks to a backend itself.
  `ProbeNow` is the probe mechanism; the prober's timer is one trigger of it; a
  successful probe (listing the deployment's model) makes a circuit half-open, and
  the one trial request it admits closes it at its first data event or re-opens it
  when it fails first; the prober keeps probing half-open circuits (a failed probe
  re-opens them). Its `ModelChecker`
  (goroutine run by `cmd/kaiak`) probes each applied config's backends once, off the
  request path, and warns about deployments whose model is not listed.
  `cmd/kaiak` hands it each applied config's caps and circuit setting, the
  live-gateway count of each totals message (caps split among live gateways), wires its
  serving-change hook (queue empty/non-empty, circuit open/closed) to the status
  reporter, and its circuit events to `metrics` through an observer interface
  `routing` defines.
- `provider` — the only code that talks to backends: a module per backend type
  (openai, azure-openai, vllm, llama-server, anthropic, azure-anthropic, and the
  generic openai-compatible), each self-contained over a shared wire core, the one
  table of which endpoints each type serves, passthrough body edits per client API,
  when a stream is whole per format, per-backend connection pools, the circuit
  breaker's probe (`GET …/models`; none for azure-anthropic, which has no models
  list); it reads backend streams with `sse`. It returns response events in the
  client's format; `server` relays them to the client, and observers (accounting)
  read them on the way.
- `accounting` — meters each attempt's response as it is relayed — reading usage as
  each client API reports it (OpenAI, Messages, Responses) into the same units —, settles usage and
  cost into usage records (each naming the key's group path) — one per routed
  request, plus one per retried attempt whose request reached the backend and got
  no answer — clamps them to the
  protocol's bound, hands each in control-plane mode to the control client's batch
  sender (the batcher, which tags it with its batch's usage generation), then to a
  fan-out of sinks (usage metrics).
- `limits` — a sliding-minute counter per per-minute limit, and a UTC-hour token count
  and a UTC-month cost count for global and every group whether limited or not, kept
  until its window ends whatever the config, each limit a check over its scope's
  count; a request is checked against global and every group
  on its key's path, following the live config; check-and-reserve before routing,
  settlement from the request's usage records in a finisher. In control-plane mode: hour and month windows over the pushed totals plus
  the gateway's own usage not yet counted (tagged by the usage generation each
  record carries), totals applied by scope and type whatever the config runs (the
  first after each connect complete, later ones the changed windows only), per-minute
  shares from the live-gateway count, the outage refusal for priced
  money-limited models (no stream contact, usage batches unanswered, or acknowledged
  batches no totals have shown counted, past the grace). Nothing outlives the
  process. It knows nothing of the
  protocol: `cmd/kaiak` converts the client's totals updates and contact.
- `metrics` — a small registry (counters, gauges, fixed-bucket histograms, gauges and
  counters read at scrape time) and its Prometheus text exposition, served on the admin port; the
  ops metrics the `server` pipeline feeds when a request is over; the usage-metrics
  sink on accounting's fan-out. The registry is built in `cmd/kaiak` and passed in.
- `clip` — bounds a client-controlled string (path, method, model name) before it is
  logged or echoed in an error message; `server` and `auth` use it.
- `logattr` — log attribute values in the units the log vocabulary fixes (a duration
  in seconds); every package that logs a duration uses it.
- `netfail` — names a failed exchange's class (timed out, connection refused, TLS
  failure, malformed response, …) in the gateway's own words, so a log line
  reporting a transport failure never carries the bytes Go quoted from the remote
  party (`docs/specs/GATEWAY.md`, Logs: no remote text); `provider`, `control` and
  `otlplog` use it.
- `otlplog` — OTLP log export: reads the `OTEL_*` settings, and its `slog` handler
  hands every record to the stderr handler and queues a copy; one sender goroutine
  posts the queue as OTLP/HTTP JSON batches with retries, dropping the newest when
  full, and flushes on demand. It imports nothing of the gateway's but `netfail`.
  `cmd/kaiak` wraps the process logger with it when export is on, gives it a
  stderr-only logger for its own problem reports (no feedback loop), reads its counts
  into `metrics` (`kaiak_log_export_records_total`) and runs its final flush as the
  process's last act.
- `sse` — the server-sent events reader (WHATWG format): splits a stream into blocks
  with their raw bytes, data, event name and ID, bounded in size. The provider relays
  backend streams with it; `control` follows the config stream with it.
- `schemacheck` — the parts every document walker is built from: strict JSON decoding
  and checks mirroring the JSON Schemas in `protocol/` (the standard library has no
  schema validator), real-date and real-instant checks. `config` and `control` build
  their walkers on it.
- `control` — the only code that talks to the control plane (a capability fence, as
  `provider` is for backends: no other package opens a connection to it). The
  control-protocol messages: Go types, strict decoding and validation against
  `protocol/schema/`, fixture parity with `kaiak-control` (the usage record is
  `accounting`'s type). The client: boot from the stream's first config or, with the
  control plane unavailable, the seed config — else an error `cmd/kaiak` exits on; the
  config stream (read with `sse`) with reconnect backoff, every config handed to
  `config`'s apply path unless its hash is the running or last rejected config; the
  usage batch sender — accounting's batcher, which only appends in memory and
  returns the filling batch's usage generation, a goroutine sealing batches into a
  queue in memory (at most `KAIAK_USAGE_MEMORY_BYTES` of encoded records, default
  64 MiB) — another sending them one outstanding at a time; the status reporter (on start,
  on change, on a stream connecting, every 10 s; routing changes at most one a
  second). Totals come from stream events only and go to one consumer (the
  limiter), in stream order, with the usage generations they show counted (an ack
  only stops a batch being sent, and a malformed totals event ends the stream); the client also tracks contact with the control
  plane (read by the limiter's outage check and the metrics). `cmd/kaiak` wires the
  usage flush and the final status into the drain; `metrics` gets the delivery
  metrics through an observer interface `control` defines, and the connection state
  through one `metrics` defines.
- `fakebackend` — a test backend speaking OpenAI's chat, completions and embeddings,
  Anthropic Messages and OpenAI Responses (with recorded answers of real servers in
  `captures/` for tests that need their exact bytes), that can stream, stall, hang, cut,
  fail (also for a scripted sequence of requests) and omit usage on demand, serves a
  models list whose status the test sets, and records every request it receives; used by tests
  only. `fakebackend/cmd/fakebackend` runs it as a process (listen address, behavior
  profile, credential check) for scripts and the live-test kit's self-test.
- `fakecontrol` — a control plane for tests: the stream of the current config the
  test publishes, the request checks, and scripted events, outages, restarts and
  protocol versions; takes usage batches (de-duplicated by batch ID as the protocol
  settles, with scripted failures: error answers, acks dropped after counting) and
  status reports; scripted totals (windows, live count) with per-instance
  `counted_through`, optionally pushed on every change; records every
  request. Tests only; it imports nothing from the gateway, so `control`'s own tests
  use it.

Test tooling outside the binary:

- `gateway/e2e` — the end-to-end test: builds `kaiak`, runs it as a subprocess with a
  generated config against the fake backend, drives it over HTTP and signals (reload,
  restart, drain); in control-plane mode against `fakecontrol` (boot, pushed config,
  usage batches delivered, the drain flushing the last records), two
  gateways sharing one `fakecontrol` (per-minute shares, a budget spent through one
  enforced on the other, the outage refusal and recovery), and routing reliability
  with one model on two fake backends (`reliability_test.go`: retry on the other
  deployment after a refused connection, a `500`, a `429`, a stream's first-event
  timeout; a non-stream response timeout answered at once;
  the circuit opening, a probe half-opening it and its trial closing it; capped
  backends queueing, `queue_full`, `queue_timeout`, a queued request served during
  the drain), the binary's timeouts on real sockets (`timeouts_test.go`: body-read,
  idle and write deadlines from the environment, a stalled stream counted toward the
  circuit), every metric series at 0 from startup and each config apply
  (`zeroseries_test.go`), and the stateless
  gateway (`stateless_test.go`: the minimal setup — control plane URL and token
  only, a read-only working directory — the boot order with the seed and the exit
  without a config, a priced seed refused, the drain's flush reserve delivering a
  cut stream's record). Part of `go test ./...`.
  Built only with the `crosshalf` tag, `TestAcrossHalves` (`sample_test.go`) is the
  cross-half e2e: the real sample control plane (`node control/sample/src/main.ts`)
  and two `kaiak` processes, the control-plane traffic through an in-test proxy
  (`proxy_test.go`: a fixed address while the sample stops and restarts on another
  port, one usage ack it can lose after the sample counted the batch, and every
  status report it forwarded with the sample's answer). It reads the sample's totals
  from the protocol's own stream (an observer instance decoding `totals` events),
  what the sample received as status (an open circuit, a queued model) from the
  reports the proxy forwarded and the sample accepted, and the gateways' state from
  logs, metrics and headers. `TestAcrossHalvesReplicas` (`replicas_test.go`) runs two
  gateways against two control-plane cores over one store (the sample with a protocol
  replica, each gateway behind a proxy of its own): usage counted once in totals both
  cores serve, a publish reaching the other core's gateway, a gateway moved to the
  other core carrying on with the current config. Needs Node and `control/`'s dependencies, so
  `go test ./...` leaves them out and `scripts/check-all.sh` runs them.
- `scripts/live` — the live-test kit, a separate Go module (standard library only,
  imports nothing from the gateway): generates a config for a real vLLM,
  llama-server, Azure OpenAI or OpenAI backend — or two backends serving one model
  (load spread, the cap, a failover the user drives) — runs the built binary and checks it end to end
  (`docs/testing/LIVE-BACKENDS.md`).

## Control-plane packages

- npm workspaces under `control/`: `kaiak-control` and `sample`. Each package exposes
  one entry (`src/index.ts`, set by its `exports`).
- Inside a package, a subsystem is a top-level folder under `src/`. Code outside a
  subsystem imports it only through its `index.ts`, and the import graph is acyclic —
  enforced by `control/scripts/check-boundaries.ts` in `npm run lint`.
- `kaiak-control` subsystems:
  - `schemas` — loads every JSON Schema of the package's `schema/` (a byte-for-byte
    copy of `protocol/schema/`, `npm run sync-schemas`) into one validator
    (cross-file `$ref`s resolve by `$id`) and turns violations into issues.
  - `calendar` — real-date and real-instant checks, the part of a timestamp a schema
    pattern cannot check.
  - `config` — validates a config document: the shared schema from `protocol/`, then
    the semantic rules (`docs/specs/CONTROL-PROTOCOL.md`, Config — the group tree
    included); resolves each scope — global and every group — to its path,
    effective limits and allowed models (`child_defaults` merged, down the path),
    as the gateway's config does.
  - `messages` — validates each control-protocol message: its schema, then the
    message rules (`docs/specs/CONTROL-PROTOCOL.md`, Messages); one validator per
    message.
  - `storage` — the storage interface every piece of control-plane state goes
    through (async, so a database implements it) and the in-memory store, its
    reference implementation and the sample's store. The store is where
    control-plane processes agree: the current config's JSON text and its hash,
    conditional writes
    (publish on the hash of the config it was checked against, batch on the
    instance's last batch of its epoch, gateway records on a revision that never
    repeats), the consistent totals snapshot, and `subscribe()`, which tells every
    core of every change and announces a catch-up when the store's change channel
    reconnects. The contract ships as tests (`store-contract`, a second package
    entry, with a lossy channel that runs the catch-up test), with deliberately
    broken stores as their negative controls.
  - `config-publishing` — publishing (validate, refuse a changed group parent, then
    replace the current config conditionally), the current config and its hash, and
    the delivery of each new current config to the core's listeners — a failed read
    retried, then announced (the Fastify plugin ends its streams).
  - `usage` — usage intake and totals (`docs/specs/CONTROL-PROTOCOL.md`, Usage
    intake): validates a batch, de-duplicates it by the instance's last counted batch
    of its epoch (one batch at a time per instance, the store's write conditional on
    that batch),
    stamps it with the receipt time, adds each record to the hour and month windows
    of global and each group of its path, whatever the config (the record's
    `gateway_time` window when that is the current or previous one), and answers an
    ack naming the batch; recent records; a subscription to counted batches for
    pushes. Totals list every scope and type with usage in its current window,
    whatever the config, from one store snapshot of the windows and every instance's
    cursors, taken once the read's windows are current; `counted_through` is the
    recipient's cursor of each epoch.
  - `gateways` — gateway status and the live set (`docs/specs/CONTROL-PROTOCOL.md`,
    Status intake): validates a status, keeps the latest per instance with its
    receipt time, joins the instance to the live set, flags two processes sharing an
    instance name; the expiry sweep (an invocable run plus its scheduling timer)
    drops silent gateways from the set, forgets long-silent ones and drops batch
    cursors past their retention (7 days by default); a subscription to
    changes, telling whether the live count moved.
  - `keys` — key generation and hashing (Config, key format).
  - `backend-verify` — `verifyBackend`, the only code in `control/` that talks to a
    backend (`docs/specs/BACKEND-VERIFY.md`): checks that it answers and accepts the
    credential, and reads what it reports about its models (vLLM's list, llama-server's
    `/props`) into a report the app turns into declared config. Input checked with the
    config schema's rules; answers with lenient schemas of its own. Called by the app
    only — not by `control-plane` or `fastify`.
  - `protocol` — the checks every gateway request passes (token, protocol version,
    instance) as framework-agnostic functions returning structured errors with the
    status an adapter answers with; the protocol version and header names.
  - `control-plane` — the core the host app builds (`createControlPlane`): store,
    token, clock, recent-records size, live-set timings in; the
    operations of the subsystems above out, the live set's size wired into totals;
    `start`/`stop` take and release the store subscription (`start` announces a
    catch-up) and run and stop the expiry sweep (every core sweeps; its writes are
    conditional). Listener events come from the store's notifications.
    HTTP adapters and the host app use it; it knows nothing of HTTP.
  - `fastify` — the HTTP adapter: a Fastify plugin (`controlProtocolPlugin`) the host
    registers with a core instance. It mounts the gateway endpoints (default under
    `/v1`), runs the request checks and adds the protocol header on every response,
    and writes the stream straight to the socket (subscribe, the current config, live
    config pushes ordered by when each read was issued and skipped by the stream's
    last-sent hash, one totals read per push for every stream — complete on connect,
    then the changed windows, held for slow readers —, heartbeat, the stall bound, a
    synchronous teardown); takes
    usage batches and statuses;
    starts the core with the app and stops it on close. Routes only — logging and the rest of
    the app are the host's.

```mermaid
flowchart LR
    fastify --> cp
    fastify --> cpub
    fastify --> protocol
    fastify --> messages
    cp[control-plane] --> cpub[config-publishing]
    cp --> protocol
    cp --> storage
    cp --> usage
    cp --> gateways
    gateways --> messages
    gateways --> storage
    usage --> messages
    usage --> storage
    storage --> messages
    sc[store-contract] --> storage
    sc --> messages
    cpub --> config
    cpub --> schemas
    cpub --> storage
    keys --> schemas
    bv[backend-verify] --> config
    bv --> schemas
    protocol --> schemas
    config --> schemas
    config --> calendar
    messages --> config
    messages --> schemas
    messages --> calendar
```

- `sample` subsystems (the app wiring lives in its process entries, `src/main.ts`,
  `src/keygen-cli.ts` and `src/verify-cli.ts`):
  - `settings` — the process settings read from the environment.
  - `logging` — the Fastify logger option per log format.
  - `config-file` — the config source: reads the file and publishes it through the
    core; the reload is an invocable run (startup and a debounced watch on the file's
    directory are its triggers — the file's own entry, and the `..`-prefixed entries a
    Kubernetes ConfigMap volume swaps atomically), recording each run and keeping the
    latest failure until a run succeeds.
  - `app` — `createSampleApp`: the Fastify instance, the core on the in-memory store,
    the plugin mounted, the status page registered, the config file read on ready and
    watched until close (each run also told to the page), the core's listener
    failures and expiry sweeps logged; a warn line at startup says the state is in
    memory (budgets, totals and batch de-duplication reset on restart).
  - `page` — the read-only status page: `GET /` renders four sections (gateways,
    config with the group tree, totals vs limits per scope, recent usage by group
    path) from the core and the config file's
    state, server-side, escaped by default (`html` template tag); `GET /events` is
    one SSE stream per browser — every section on connect, then each changed section
    re-rendered and pushed as `event: section` (`{id, html}`), coalesced to at most
    one push per second, applied by a small inline script. At most 32 page streams
    are open at once (they share the port with the gateways' config streams and take
    no key); one more is refused `503 page-streams-full` (settled 2026-09-25, N-S6).
    The page subscribes to
    the core only while a browser is connected; `preClose` ends its streams. No
    authentication (local and demo use); key IDs only; backend URLs shown without
    userinfo; a notice leads the page: state is in memory and resets on restart, not
    a billing system.
  - `keygen` — the keygen command: arguments in, the key, its ID, its hash and the
    `keys` entry (`{ hash, group }`) out, through `kaiak-control`'s `createKey`.
  - `verify` — the verify command: arguments in (the API key read from the variable
    `--api-key-env` names), `kaiak-control`'s `verifyBackend` report out as JSON on
    stdout, the metadata to merge and what is left to decide on stderr.

```mermaid
flowchart LR
    main[main.ts] --> app
    main --> settings
    main --> logging
    cli[keygen-cli.ts] --> keygen
    vcli[verify-cli.ts] --> verify
    app --> cf[config-file]
    app --> logging
    app --> page
    page --> cf
    page --> kc
    logging --> settings
    app --> kc[kaiak-control]
    cf --> kc
    keygen --> kc
    verify --> kc
```
