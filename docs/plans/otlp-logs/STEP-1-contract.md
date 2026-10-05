# Step 1 — contract

**Status:** done (2026-10-05)

## Intent

Write down the gateway's log vocabulary and the OTLP log export as contract: the
field table every log line follows, the variables, the record mapping, delivery and
its limits, the metric, and the no-dependency choice. Steps 2–4 implement what this
step writes.

## Files likely touched

- `docs/specs/GATEWAY.md`:
  - Observability → Logs: **the field table** — every attribute the gateway logs
    today (the request line's and the operational events'), its new name, and its
    meaning where it differs from today's (token counts: `gen_ai.usage.input_tokens`
    is all input; cache read and creation are its parts). Standard names from the
    OpenTelemetry conventions where the meaning matches — HTTP (`http.request.method`,
    `url.path`, `http.response.status_code`, `server.address`), error (`error.type`),
    GenAI (`gen_ai.request.model`, `gen_ai.response.model`, `gen_ai.request.stream`,
    `gen_ai.operation.name`, `gen_ai.provider.name` if a well-known value fits each
    backend type, `gen_ai.usage.*`) — and `kaiak.*` for the rest. The conventions
    version followed is named (HTTP from `semantic-conventions`, GenAI from
    `semantic-conventions-genai`, checked on the day). The request line's message,
    and whether the OTLP record carries an `eventName`, are settled here;
  - Configuration sources: the `OTEL_*` variables accepted, their fallbacks and
    defaults, `http/json` as the only protocol, start failures;
  - Observability: a new *OTLP log export* bullet beside *Logs* — every line, the
    same content as stderr, the resource, the mapping, delivery (queue, batches,
    retries, drop-newest, one export in flight), drain behaviour after usage, export
    problems on stderr only;
  - the metrics table: `kaiak_log_export_records_total{outcome}`.
- `docs/DEPLOYMENT.md`: how to point a gateway at a collector (sidecar or node agent),
  the variables, a minimal Collector config with the `otlp` receiver on HTTP, the
  drop metric to alert on.
- `docs/TECH-STACK.md`: the OTLP/HTTP JSON exporter is hand-written; the OTel Go SDK
  rejected as the gateway's first third-party module (dated).
- `docs/BACKLOG.md`: the *OpenTelemetry export* entry's *Logs* layer now names this
  export as built (the entry stays for traces and the `traceparent` test).

## Decisions made during planning

- All decisions in OVERVIEW 5–12 are recorded in the spec, dated 2026-10-05.
- Before naming a field, read its definition in the conventions: a standard name is
  used only where the meaning matches; otherwise `kaiak.*`.
- Inventory the current fields from the code (every `slog` call in `gateway/`), not
  only the spec: the table must cover every attribute a log line can carry.

## Acceptance criteria

- The field table covers every attribute the gateway logs (checked against the
  code), each standard name checked against its convention's definition.
- The spec states every variable, default and failure; the mapping; delivery and drop
  rules; the metric; drain order — each dated.
- No code change. Suite run and recorded (green: docs only).

## Result

**What changed**

- `docs/specs/GATEWAY.md`:
  - Observability → **Logs** rewritten around **the field tables** (one for the
    request line, one for operational events), each row `Attribute | Was | …`, so
    the old → new mapping is in the spec itself. Around them, dated 2026-10-05: the
    one-vocabulary decision (rejected: exporter-only translation, keeping kaiak's
    names), the conventions followed, the naming rules for `kaiak.*`, the units
    rule, and the request line's message and `eventName`.
  - Observability → new **OTLP log export** bullet: content, resource, record
    mapping, delivery (queue 10 000, batches 512 / 1 s, one export in flight,
    retries, drop-newest, the metric's outcomes, `log export failing` on stderr
    only), and at-exit behaviour.
  - Configuration sources → new **OTLP log export** bullet: every `OTEL_*`
    variable read, its fallback and default, on/off rules, `http/json` only with
    the explicit refusal of `grpc` / `http/protobuf`, start failures, the
    variables not read.
  - Metric list: `kaiak_log_export_records_total{outcome}`.
  - Lifecycle → Draining step 5 and the Kubernetes sizing note: the export's final
    flush after usage and `kaiak stopped`.
  - Every other mention of a log field in the spec moved to the new names
    (`kaiak.relay_end=…`, `kaiak.retry_refused`, `kaiak.upstream.error.*`,
    `kaiak.queue.wait_duration`, `kaiak.circuit.*`, `error.type`, `kaiak.limit.*`, the
    outage line, the config-load line, the drain line, `kaiak.reason`, the
    `kaiak_circuit_open` note, Errors by key, Config load cost).
- `docs/DEPLOYMENT.md`: Environment table rows for the `OTEL_*` variables; the
  proxy note covers the collector; Draining and Resources mention the export's
  final flush and queue; Observability → new *Log export to an OpenTelemetry
  collector* bullet (sidecar or node agent with `status.hostIP`, a minimal
  Collector config with the `otlp` receiver on HTTP `0.0.0.0:4318` and the `debug`
  exporter, picking request lines by body, querying by standard names, the loss
  metric); a *Log export losing records* starter alert.
- `docs/TECH-STACK.md`: the Logging bullet points at the vocabulary; new
  *OTLP log export: hand-written OTLP/HTTP with JSON encoding* ruling (settled
  2026-10-05; rejected: the OTel Go SDK as the first third-party module, protobuf
  encoding, gRPC).
- `docs/BACKLOG.md`: *OpenTelemetry export* → the *Logs* layer is planned by this
  plan; the revisit trigger now speaks for traces only.

**The mapping in short** (the full tables: `GATEWAY.md` → Observability: Logs)

- Request line, standard names: `method` → `http.request.method` (`_OTHER` for an
  unknown method, then `http.request.method_original`); `path` → `url.path`;
  `status` → `http.response.status_code`; `error_code` → `error.type`; `model` →
  `gen_ai.request.model`; `stream` → `gen_ai.request.stream`; `tokens_in` →
  **computed** `gen_ai.usage.input_tokens` = in + cached + cache_write;
  `tokens_cached` → `gen_ai.usage.cache_read.input_tokens`; `tokens_cache_write` →
  `gen_ai.usage.cache_write.input_tokens`; `tokens_out` →
  `gen_ai.usage.output_tokens`; `tokens_reasoning` →
  `gen_ai.usage.reasoning.output_tokens`.
- Request line, added: `gen_ai.operation.name` (`chat`, `text_completion`,
  `embeddings`), `kaiak.backend.type`, `gen_ai.provider.name` (`openai`,
  `azure.ai.openai` only).
- Request line, `kaiak.*`: `request_id` → `kaiak.request.id`; `latency_ms` →
  `kaiak.request.duration` (seconds); `key_id` → `kaiak.key.id`; `group` →
  `kaiak.key.group`; `auth_failure` → `kaiak.auth.failure`; `limit_scope|limit_id|
  limit_type|limit|limit_configured|used|requested` → `kaiak.limit.scope|id|type|
  enforced|configured|used|requested`; `backend` → `kaiak.backend.id`;
  `deployment_model` → `kaiak.deployment.model`; `attempts`, `tried`,
  `retry_refused`, `relay_end` → `kaiak.` + the same name; `queue_wait_ms` →
  `kaiak.queue.wait_duration`, `ttft_ms` → `kaiak.time_to_first_token` (seconds); `upstream_error|_code|_type` → `kaiak.upstream.error.message|code|type`;
  `cost_usd|estimated|partial` → `kaiak.usage.cost_usd|estimated|partial`.
- Operational events: `error` → `exception.message`; `pid` → `process.pid`;
  `instance_id` → `service.instance.id`; `file` → `file.name` (data-directory
  files) or `file.path` (paths); `addr` → `server.address` + `server.port`; the
  batch-refused line's `status`/`code` → `http.response.status_code`/`error.type`;
  everything else under subject namespaces — `kaiak.config.*`, `kaiak.control.*`,
  `kaiak.totals.*`, `kaiak.usage.*`, `kaiak.limit.*`, `kaiak.model.*`,
  `kaiak.circuit.*`, `kaiak.drain.*`, `kaiak.listener.*`, `kaiak.data_file.*`,
  `kaiak.log_export.*`, with shared `kaiak.reason`, `kaiak.trigger`,
  `kaiak.backend.id`, `kaiak.deployment.model`, `kaiak.duration`,
  `kaiak.lasted`. Every duration is seconds as a double, no unit in its name
  (amended 2026-10-05, below).
- **Durations, old → new** (all seconds, doubles):

  | Old | New | Line |
  |---|---|---|
  | `latency_ms` | `kaiak.request.duration` | request (to the µs) |
  | `queue_wait_ms` | `kaiak.queue.wait_duration` | request (to the µs) |
  | `ttft_ms` | `kaiak.time_to_first_token` | request (to the µs) |
  | `duration_ms` | `kaiak.duration` | config applied / rejected (to the µs), probe |
  | `lasted_ms`, `lasted` (string) | `kaiak.lasted` | config stream ended, outage over |
  | `wait_ms`, `waited_ms` | `kaiak.control.totals_wait`, `kaiak.control.totals_waited` | first totals |
  | `boot_wait_ms` | `kaiak.control.boot_wait` | config snapshot not fetched at startup |
  | `delay_ms` | `kaiak.control.delay` | control plane reconnect scheduled |
  | `since_contact`, `grace`, `usage_waiting` (strings) | `kaiak.control.since_contact`, `kaiak.control.outage_grace`, `kaiak.control.usage_waiting` | control plane outage |
  | `open_ms` | `kaiak.circuit.open_duration` | circuit half-open, circuit closed |
  | `grace`, `timeout`, `flush_reserve`, `cut_after` (strings) | `kaiak.drain.grace`, `kaiak.drain.timeout`, `kaiak.drain.flush_reserve`, `kaiak.drain.cut_after` | draining |
  | `timeout` (string) | `kaiak.listener.shutdown_timeout` | shutdown timed out |
- Conventions: `semantic-conventions` v1.44.0 (2026-08-04; the attributes used
  are unchanged on `main` 2026-10-05); `semantic-conventions-genai` `main` at
  `e07f4eb` (2026-10-02; no release yet).

**Decisions made during the step**

- The request line keeps the message `request`; no OTLP `eventName` (rejected
  `kaiak.request`: a second way to say what the body says).
- `error` (a Go error string) → `exception.message`: `error.type` must be
  low-cardinality, `error.message` is deprecated, and `exception.message` is what
  OTel's Go `RecordError` writes. `error_code` → `error.type` (a low-cardinality
  class the request ended with — matches).
- **Durations in seconds** (amended 2026-10-05 after the user's review, replacing
  this step's first choice of `_ms` milliseconds): every duration attribute is
  seconds as a double with no unit in its name — the request line's to the
  microsecond, the others to the millisecond. Seconds match the conventions
  (`http.server.request.duration`, `gen_ai.response.time_to_first_chunk`) and the
  gateway's Prometheus metrics (`_seconds`). Rejected: integer milliseconds under
  `_ms` names; Go duration strings. `KAIAK_*_MS` variables and the config's
  `*_ms` fields are configuration, not log fields, and keep their names and units
  (said in the spec's Units bullet). Sizes keep `_bytes`, money `_usd`.
- Not standard on purpose: `deployment_model` (not `gen_ai.response.model`, which
  is what the backend's answer reports), `ttft_ms` (not
  `gen_ai.response.time_to_first_chunk`: that counts any first chunk, a role-only
  one included), `backend` (not
  `server.address`: a config ID, not an address), `latency_ms` (no attribute
  standard for a log record's duration).
- `gen_ai.provider.name` only where a well-known value fits (`openai`,
  `azure.ai.openai`); `kaiak.backend.type` always, so one field answers "which
  type" for every backend.
- `http.request.method` follows the convention's `_OTHER` rule with
  `http.request.method_original` (clipped) for unknown methods.
- No name is also a namespace (`kaiak.backend.id`, `kaiak.limit.enforced` for the
  old `limit`) — stores keeping attributes as nested objects reject a value and an
  object at one path.
- Variables: `OTEL_LOGS_EXPORTER=otlp` with no endpoint turns export on to the
  specification's default `http://localhost:4318/v1/logs` (a fleet-wide injected
  `otlp` should not fail the start); `OTEL_LOGS_EXPORTER` takes only `otlp`/`none`,
  `OTEL_SDK_DISABLED` only `true`/`false`; compression, certificate, `*_INSECURE`,
  `OTEL_BLRP_*` and attribute-limit variables are not read.
- Delivery details the plan left open: backoff 0.5 s doubling to 5 s with jitter;
  a `Retry-After` beyond the batch's remaining timeout fails it; response bodies
  read up to 4 MiB; partial-success rejections count as `failed`; `log export
  failing` at most once a minute; queue and batch sizes fixed (not
  configurable); scope `kaiak`, no `schemaUrl`; `service.version` and
  `service.instance.id` win over `OTEL_RESOURCE_ATTRIBUTES`.
- At exit: the final flush runs after `kaiak stopped`, bounded by the drain's
  deadline or 1 s from its start, whichever is later; a start failure after the
  exporter started gets the same flush, bounded by 1 s.
- `kaiak_log_export_records_total` series exist at 0 from startup when export is
  on, and are absent when it is off.

**Against OVERVIEW** (reported, not silently changed there):

- OVERVIEW's Goal says the content is exactly today's: the request line gains three
  fields (`gen_ai.operation.name`, `kaiak.backend.type`, `gen_ai.provider.name`),
  and `kaiak starting` gains `kaiak.log_export.endpoint` (step 4's). Every
  duration becomes seconds as a double — the operational ones were Go duration
  strings or integer milliseconds.
- Decision 6: export is also on with `OTEL_LOGS_EXPORTER=otlp` and no endpoint
  (default endpoint), beyond "on when either endpoint is set".
- Decision 9 says "default 10 000": the queue is fixed, not configurable.
- Decision 11 says the flush is within the reserve: it is after usage and bounded
  by the drain's deadline, but always gets at least 1 s, so `kaiak stopped` and the
  drain's last lines reach the collector even when usage took the whole reserve.
- The GenAI cache-write attribute is `gen_ai.usage.cache_write.input_tokens` (not
  `cache_creation`, which the registry no longer has).
- Decision 5's example `kaiak.key_id` is `kaiak.key.id` (the key's ID and group
  are one namespace, `kaiak.key.*`).

**Old log field names outside the spec — for step 2** (`git grep` at this commit,
outside `docs/plans`, `docs/reviews`; protocol fixtures and schemas name usage
record fields, not log fields, and stay):

- Code that writes them: `gateway/internal/server/api.go` (request line),
  `internal/server/drain.go`, `internal/server/listener.go`,
  `internal/config/loader.go`, `internal/control/{client,lastknowngood,spool,spooldisk,status,stream,usage}.go`,
  `internal/limits/{limits,shared}.go`, `internal/routing/{circuit,modelcheck}.go`,
  `internal/accounting/accounting.go`, `internal/state/state.go`,
  `cmd/kaiak/main.go`.
- Values that change, not only names — every duration to seconds as a double:
  `internal/server/api.go` (latency, queue wait, time to first token),
  `internal/config/loader.go` (`durationMS`), `internal/control/client.go` (boot
  wait, reconnect delay, stream lasted), `internal/limits/shared.go` (outage
  times, as duration strings today), `internal/routing/circuit.go` (open time,
  probe duration), `internal/server/drain.go` and `listener.go` (duration
  strings today), `cmd/kaiak/main.go` (first-totals wait).
- Comments naming them: `internal/server/metrics.go:95-97` (`error_code`,
  `relay_end`), `internal/server/upstream.go:611,622` (`relay_end`),
  `internal/metrics/ops.go:396-397`.
- Tests reading them (narrow grep for the distinctive names; step 2 needs its own
  grep for the generic ones — `status`, `backend`, `model`, `trigger`, `file`,
  `addr`, …): `cmd/kaiak/main_test.go`, `e2e/{control,e2e,grouptree,harness,media,reliability,timeouts}_test.go`,
  `internal/accounting/accounting_test.go`, `internal/config/loader_test.go`,
  `internal/control/{client,usage}_test.go`,
  `internal/server/{accounting,backend_errors,circuit,drain,keylimit,limits,listener,metrics,pathmissing,queue,retry,retrybudget,server,timeouts,upstream,visibility}_test.go`.
  The e2e harness and the live kit read the `listening` line's `addr`: they must
  join `server.address` and `server.port`.
- Live-test kit: `scripts/live/checks.go` (`request_id`, `status`, `backend`,
  `tokens_*`, `cost_usd`, `estimated`, `partial`, `relay_end`, `error_code`,
  `upstream_error`), `scripts/live/twobackends.go` (`request_id`, `attempts`,
  `queue_wait_ms`, `backend`, `trigger`), `scripts/live/process.go` (`listener`,
  `addr`).
- Docs: `docs/DEPLOYMENT.md:180-183` (the drain line's `grace`, `timeout`,
  `flush_reserve`, `cut_after`), `:492-496` (limit fields), `:615` (`duration_ms`,
  `bytes`), `:641-648` (the Logs bullet); `docs/testing/LIVE-BACKENDS.md:151`,
  `:161`, `:284`, `:303`, `:338-341` (`estimated`, `backend`, `error_code`,
  `upstream_error`, `tokens_cache_write`, `tokens_cached`); `docs/BACKLOG.md:318`
  (`error_code` in a settled entry's history); `docs/architecture/*.html` name no
  field.
- Not for step 2: `docs/ARCHITECTURE.md` gains the exporter package when step 3/4
  adds it.

**Suite** — `scripts/check-all.sh`, green (docs only):

```
==> gofmt / go vet / staticcheck 2026.2.1 (gateway)
==> go test -race (gateway): ok cmd/kaiak, e2e, accounting, auth, clip, config,
    control, limits, metrics, provider, routing, schemacheck, server, sse, state
==> gofmt / go vet / staticcheck (live-test kit); self-test passed for vllm,
    llama-server, openai, azure-openai, vllm with two backends
==> npm test (control): tests 574, pass 574, fail 0
==> npm run lint (control): boundaries ok
==> cross-half e2e: ok kaiak/e2e 43.879s
all checks passed
```

**Suite after the seconds amendment** — `scripts/check-all.sh`, green: gofmt, vet,
staticcheck; `go test -race` ok for every package (cached: no Go file changed);
live-test kit self-test passed; control `npm test` 574 pass, 0 fail; lint
`boundaries ok`; cross-half e2e `ok kaiak/e2e 59.159s`; `all checks passed`.
