# Step 11 — docs and live checks

**Status:** done with findings (2026-10-08)

## Intent

Operators can find, configure and read the metrics; the names hold against real
OpenTelemetry and Prometheus software.

## Files likely touched

- `docs/DEPLOYMENT.md` — metric export setup (the variables, one endpoint for both
  signals, opting out), alerts and dashboard queries in the new names, Upgrades:
  the 0.12 note with every renamed metric and label (old → new, from the spec's
  "Was" column).
- `README.md`, `docs/ARCHITECTURE.md`, `docs/architecture/gateway.html`,
  `docs/testing/LIVE-BACKENDS.md` — wherever metrics or the telemetry layout
  appear.
- `scripts/live/` — any check reading metrics follows the names (done in step 6;
  re-run here live).

## Decisions made during planning

- Live checks run on the user's test setup (`docs/testing/LIVE-BACKENDS.md`), with
  containers removed afterwards.

## Acceptance criteria

- Live: an `otel/opentelemetry-collector` (OTLP/HTTP receiver, `debug` exporter)
  shows every family with its unit and attributes under the resource.
- Live: a Prometheus 3.x with `--web.enable-otlp-receiver` ingests the gateway's push
  while scraping its `/metrics`: for every family, the pushed series' name and label
  names equal the scraped ones (`job`/`instance` aside).
- Live: the live-test kit's DGX checks pass in the new names.
- `scripts/check-all.sh` green. Suite recorded.

## Result

### Docs

- `docs/DEPLOYMENT.md`:
  - Topology diagram: the gateways' OTLP push (logs, metrics) to a collector.
  - Draining: the final metric export in the exit sequence.
  - Environment: the `OTEL_*` rows for both signals — shared and per-signal endpoint,
    exporter, headers, timeout and protocol variables; `OTEL_METRIC_EXPORT_INTERVAL`,
    `OTEL_METRIC_EXPORT_TIMEOUT` (the shorter of it and the OTLP timeout),
    `OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE`; the proxy sentence names the
    OTLP export.
  - vLLM `--enable-prompt-tokens-details`: cache reads in the metrics, not
    `tokens_cached`.
  - Observability: new bullet **Metric names are OpenTelemetry's** (the translation,
    examples); Restrict the admin port also covers the metric export's collector;
    the log export bullet became **OTLP export to an OpenTelemetry collector** — one
    endpoint for both signals, opting one out (`OTEL_METRICS_EXPORTER=none`,
    `OTEL_LOGS_EXPORTER=none`, `OTEL_SDK_DISABLED`), where the collector runs, one
    resource for both, the minimal collector with a metrics pipeline, the two log
    bullets unchanged, and four metric bullets: the same series as the scrape (the
    Prometheus OTLP receiver's full path, the push's `job`/`instance`, Prometheus's
    `le` formatting — from the live check), temporality (Prometheus refuses delta —
    from the live check), interval and size (the 4 MiB split), and what
    `metric export failing` means.
  - Starter alerts: reviewed against the spec and a live scrape (every metric and
    label name in Observability and the other sections exists in the scrape, checked
    by script); a new ticket alert **Metric export failing**.
  - Secrets: the per-signal headers.
  - Upgrades: **Next release** (unreleased; protocol 5 and config format 5
    unchanged), after 0.11.5 — the section lists versions oldest first, so the new
    note goes last: the renamed metrics (table, old → new), the renamed labels
    (table), the request duration's attributes (`status_class` → status code +
    `error_type`, `endpoint` → `http_route`, client-closed without a status), input
    tokens' meaning change, the circuit gauges merge, the log-export counter →
    `otel.sdk.*`, the shared endpoint now turning metric export on (and how to keep
    logs only), the dependency rule (no new dependency).
- `README.md`: the admin port line — metrics in OpenTelemetry's names; the OTLP push
  of logs and metrics.
- `docs/ARCHITECTURE.md`: data-flow diagram (logs and metrics to the collector);
  `gateway/e2e` lists the log and metric export tests.
- `docs/architecture/gateway.html`: package map (metrics box "OTel names · scrape ·
  push", Prometheus "or a collector", edge "scrape · push"); `metrics` row
  rewritten; new row `telemetry/metric`, `telemetry/otlpmetric`; the tree's rule on
  the `telemetry/otlp` row; Observability → Metrics (one definition, OpenTelemetry
  names, the Prometheus translation, the OTLP push, its count); footer.
- `docs/testing/LIVE-BACKENDS.md`: the `metrics` check row (what `checks.go` reads,
  in the new names), the Azure 429 counter, the manual client check's grep.

### Live check 1 — OpenTelemetry Collector (2026-10-08)

- `dev` context (`ssh://acdtrx@dev.local`, Docker 29.4.0, linux/amd64):
  `otel/opentelemetry-collector:0.162.0` (latest release), config copied in with
  `docker cp`: `otlp` receiver HTTP `0.0.0.0:4318`, `debug` exporter
  `verbosity: detailed`, `logs` and `metrics` pipelines; port 4318 published.
- Locally: `go build ./cmd/kaiak` and `./internal/fakebackend/cmd/fakebackend`
  (`-addr 127.0.0.1:18000`); file mode, one `openai-compatible` backend
  (`max_in_flight` 4), one priced model `chat`, one key in group `live`
  (1 request per minute); `OTEL_EXPORTER_OTLP_ENDPOINT=http://dev.local:4318`,
  `OTEL_RESOURCE_ATTRIBUTES=deployment.environment.name=live-check`,
  `OTEL_METRIC_EXPORT_INTERVAL=3000`, `KAIAK_INSTANCE_ID=live-mac`.
- Traffic: a streamed success (200), no key (401), a second request (429).
- Every export: `"resource metrics": 1, "metrics": 37, "data points": 106` — the 37
  families file mode has (the 11 control-plane and usage-delivery families exist
  only in control-plane mode; check 2 covers them). Resource and scope:

```
Resource attributes:
     -> service.name: Str(kaiak)
     -> service.version: Str((devel))
     -> service.instance.id: Str(live-mac)
     -> deployment.environment.name: Str(live-check)
InstrumentationScope kaiak
```

  Each family as the collector printed it (name | unit | type, monotonic,
  temporality | attribute keys seen):

```
gen_ai.client.inference.usage.cache_read.input_tokens | {token} | Sum mono=true Cumulative | kaiak.key.group kaiak.key.root_group kaiak.key.id gen_ai.request.model gen_ai.operation.name kaiak.usage.status gen_ai.token.modality
gen_ai.client.inference.usage.cache_write.input_tokens | {token} | Sum mono=true Cumulative | (same)
gen_ai.client.inference.usage.input_tokens | {token} | Sum mono=true Cumulative | (same)
gen_ai.client.inference.usage.output_tokens | {token} | Sum mono=true Cumulative | (same)
gen_ai.client.inference.usage.reasoning.output_tokens | {token} | Sum mono=true Cumulative | (same)
http.server.request.duration | s | Histogram Cumulative | http.request.method url.scheme http.route http.response.status_code gen_ai.request.model error.type
kaiak.backend.active_requests | {request} | Sum mono=false Cumulative | kaiak.backend.id
kaiak.backend.active_requests_limit | {request} | Sum mono=false Cumulative | kaiak.backend.id
kaiak.build.info |  | Gauge | service.version process.runtime.version
kaiak.circuit.state | {deployment} | Sum mono=false Cumulative | kaiak.backend.id kaiak.deployment.model kaiak.circuit.state
kaiak.circuit.transitions | {transition} | Sum mono=true Cumulative | kaiak.backend.id kaiak.deployment.model kaiak.circuit.state
kaiak.config.apply.duration | s | Histogram Cumulative | kaiak.trigger kaiak.config.result
kaiak.config.last_applied_timestamp | s | Gauge |
kaiak.config.loads | {load} | Sum mono=true Cumulative | kaiak.trigger kaiak.config.result
kaiak.config.size | By | Gauge |
kaiak.connections.refused | {connection} | Sum mono=true Cumulative |
kaiak.deployment.cooling_down | {deployment} | Sum mono=false Cumulative | kaiak.backend.id kaiak.deployment.model
kaiak.errors | {request} | Sum mono=true Cumulative | kaiak.error.class
kaiak.limit.rejections | {request} | Sum mono=true Cumulative | kaiak.limit.scope kaiak.limit.type
kaiak.limits.sync.duration | s | Histogram Cumulative |
kaiak.output_token_rate | {token}/s | Histogram Cumulative | gen_ai.request.model kaiak.backend.id
kaiak.probes | {probe} | Sum mono=true Cumulative | kaiak.backend.id kaiak.probe.result
kaiak.queue.rejections | {request} | Sum mono=true Cumulative | gen_ai.request.model error.type
kaiak.queue.size | {request} | Sum mono=false Cumulative | gen_ai.request.model
kaiak.queue.wait_duration | s | Histogram Cumulative | gen_ai.request.model
kaiak.request.attempts | {attempt} | Histogram Cumulative | gen_ai.request.model
kaiak.request.errors | {request} | Sum mono=true Cumulative | error.type kaiak.key.group kaiak.key.root_group kaiak.key.id gen_ai.request.model
kaiak.retries | {attempt} | Sum mono=true Cumulative | gen_ai.request.model kaiak.backend.id kaiak.attempt.outcome
kaiak.time_to_first_token | s | Histogram Cumulative | gen_ai.request.model kaiak.backend.id
kaiak.upstream.attempt.duration | s | Histogram Cumulative | kaiak.backend.id
kaiak.upstream.attempts | {attempt} | Sum mono=true Cumulative | kaiak.backend.id kaiak.deployment.model kaiak.attempt.outcome
kaiak.usage.clamped_records | {record} | Sum mono=true Cumulative |
kaiak.usage.cost_usd | {USD} | Sum mono=true Cumulative | kaiak.key.group kaiak.key.root_group kaiak.key.id gen_ai.request.model gen_ai.operation.name kaiak.usage.status
kaiak.usage.records | {record} | Sum mono=true Cumulative | (as cost)
otel.sdk.exporter.log.exported | {log_record} | Sum mono=true Cumulative | otel.component.type otel.component.name
otel.sdk.exporter.metric_data_point.exported | {data_point} | Sum mono=true Cumulative | otel.component.type otel.component.name
otel.sdk.processor.log.processed | {log_record} | Sum mono=true Cumulative | otel.component.type otel.component.name error.type
```

  No `gen_ai.provider.name`: the backend is self-hosted (`openai-compatible`), as
  the spec says. The request duration's three points:

```
     -> http.request.method: Str(POST)
     -> url.scheme: Str(http)
     -> http.route: Str(/v1/chat/completions)
     -> http.response.status_code: Str(200)
     -> gen_ai.request.model: Str(chat)
Count: 1
…
     -> http.response.status_code: Str(401)
     -> error.type: Str(missing_api_key)
…
     -> http.response.status_code: Str(429)
     -> error.type: Str(rate_limit_exceeded)
     -> gen_ai.request.model: Str(chat)
```

- **At exit** (SIGTERM, `KAIAK_DRAIN_GRACE_MS=0`): the final export's points carry
  `Timestamp: 16:12:22.134903` — after `drained` (`.134882`) and before
  `kaiak stopped` (`.141899`); the collector received it at `.153`, then the log
  batch at `.157` (`kaiak stopping`, `draining`, `draining: refusing new
  requests`, `drained`, `kaiak stopped`). Its own count read 1566 data points
  accepted.
- **A collector with no metrics pipeline** (the same container, logs pipeline only):
  `POST /v1/metrics` → `404 page not found`, `/v1/logs` → 200. A gateway pushing to
  it logged `metric export failing` (`kaiak.metric_export.failed` 94,
  `http.response.status_code` 404, `exception.message` "collector answered 404 Not
  Found") at the first failure and again at exit, and `/metrics` showed
  `otel_sdk_exporter_metric_data_point_exported_total{…,error_type="404"} 189`.
  DEPLOYMENT's upgrade note says `404`.

### Live check 2 — Prometheus OTLP receiver and scrape (2026-10-08)

- `prom/prometheus:v3.15.0` (latest release; go1.27.1) on `dev`,
  `--web.enable-otlp-receiver`, port 9090; scrape job `kaiak-scrape` every 5 s on
  `10.79.1.50:19090` — the Mac's LAN address (`acidbook.local` from dev.local);
  `curl` from dev.local to the gateway's `/healthz` answered 200, so the gateway
  stayed local with `KAIAK_ADMIN_ADDR=0.0.0.0:19090`.
- To get every family, control-plane mode: the sample control plane
  (`npm start -w sample`, `KAIAK_SAMPLE_LISTEN=127.0.0.1:18090`, the same config),
  the gateway with `KAIAK_CONTROL_URL`, the fake backend, and
  `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT=http://dev.local:9090/api/v1/otlp/v1/metrics`,
  `OTEL_METRIC_EXPORT_INTERVAL=5000` (default temporality, cumulative; no log
  export). Traffic: a streamed success, a 401, a 429, `GET /v1/models`.
- Prometheus: target `kaiak-scrape` `up`; `count by (job, instance)` → scrape
  `{job="kaiak-scrape", instance="10.79.1.50:19090"}` 414, push
  `{job="kaiak", instance="live-mac"}` 410.
- Comparison (`/api/v1/series` per job, `job` and `instance` dropped; left out:
  the scrape's `up`, `scrape_*` (5) and the push's `target_info` (1)): **409 series
  each, 62 metric names each, 46 families — the same names, and for every name the
  same label-name sets.** With `le` written as a number, the same 409 label sets,
  and an instant query of both at one moment gave the same value for all 409.
  The 46 families and their labels (both copies):

```
gen_ai_client_inference_usage_{cache_read_input,cache_write_input,input,output,reasoning_output}_tokens_total {gen_ai_operation_name,gen_ai_request_model,gen_ai_token_modality,kaiak_key_group,kaiak_key_id,kaiak_key_root_group,kaiak_usage_status}
http_server_request_duration_seconds {error_type,gen_ai_request_model,http_request_method,http_response_status_code,http_route,url_scheme}
kaiak_backend_active_requests, kaiak_backend_active_requests_limit {kaiak_backend_id}
kaiak_build_info {process_runtime_version,service_version}
kaiak_circuit_state, kaiak_circuit_transitions_total {kaiak_backend_id,kaiak_circuit_state,kaiak_deployment_model}
kaiak_config_apply_duration_seconds, kaiak_config_loads_total {kaiak_config_result,kaiak_trigger}
kaiak_config_last_applied_timestamp_seconds, kaiak_config_size_bytes, kaiak_connections_refused_total {}
kaiak_control_{config_rejected,connected,last_contact_timestamp_seconds,outage,totals_applied_timestamp_seconds} {}
kaiak_deployment_cooling_down {kaiak_backend_id,kaiak_deployment_model}
kaiak_errors_total {kaiak_error_class}
kaiak_limit_rejections_total {kaiak_limit_scope,kaiak_limit_type}
kaiak_limits_sync_duration_seconds {}
kaiak_output_token_rate_per_second, kaiak_time_to_first_token_seconds {gen_ai_request_model,kaiak_backend_id}
kaiak_probes_total {kaiak_backend_id,kaiak_probe_result}
kaiak_queue_rejections_total {error_type,gen_ai_request_model}
kaiak_queue_size, kaiak_queue_wait_duration_seconds, kaiak_request_attempts {gen_ai_request_model}
kaiak_request_errors_total {error_type,gen_ai_request_model,kaiak_key_group,kaiak_key_id,kaiak_key_root_group}
kaiak_retries_total {gen_ai_request_model,kaiak_attempt_outcome,kaiak_backend_id}
kaiak_upstream_attempt_duration_seconds {kaiak_backend_id}
kaiak_upstream_attempts_total {kaiak_attempt_outcome,kaiak_backend_id,kaiak_deployment_model}
kaiak_usage_batch_sends_total {kaiak_usage_batch_result}
kaiak_usage_clamped_records_total, kaiak_usage_last_ack_timestamp_seconds, kaiak_usage_queue_{batches,records,size_bytes} {}
kaiak_usage_cost_usd_total, kaiak_usage_records_total {gen_ai_operation_name,gen_ai_request_model,kaiak_key_group,kaiak_key_id,kaiak_key_root_group,kaiak_usage_status}
kaiak_usage_dropped_records_total {kaiak_usage_drop_reason}
otel_sdk_exporter_metric_data_point_exported_total {otel_component_name,otel_component_type}
```

  (46 of the spec's 48: the two `otel.sdk.*` log families need log export, which
  this run left off — check 1 showed them.)
- **The `le` values differ** for whole-number bounds — 105 bucket series: the
  scrape stores `le="1.0"`, `"10.0"`, `"300.0"`, the push `le="1"`, `"10"`,
  `"300"`; fractional bounds (`0.005` … `2.5`) and `+Inf` agree. The gateway
  writes `le="1"` on `/metrics` (as before this plan); Prometheus 3 rewrites a
  scraped `le` to its float form, and its OTLP path does not. Prometheus's two
  paths, not kaiak's two outputs: neither form on `/metrics` would make them agree.
  Names, label names and values hold; a `sum by (le)` mixing a scraped and a pushed
  copy of one histogram splits those buckets. Named in DEPLOYMENT (Observability:
  the same series as the scrape).
- **Delta to Prometheus**: a second gateway with
  `OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE=delta` got `500` on every
  export (Prometheus: `invalid temporality and type combination for metric
  "gen_ai.client.inference.usage.cache_read.input_tokens"` …), nothing stored
  under its instance; `metric export failing` with status 500, the count's
  `error_type="500"`. Named in DEPLOYMENT (keep cumulative for Prometheus).

### Live check 3 — the live-test kit on the DGX (2026-10-08)

The DGX was not swapped: the kit ran against what was serving, ordinary requests
only. vLLM 0.30.0, `unsloth/Qwen3.8-27B-NVFP4` on `dgx.local:11434`; embeddings on
llama-server b9917 at `llama-embed.local:11435`
(`/models/qwen3-embedding-0.6b-q8_0.gguf`):

```sh
go -C scripts/live run . -kind vllm -base-url http://dgx.local:11434/v1 \
  -model unsloth/Qwen3.8-27B-NVFP4 -max-output 4096 \
  -chat-params '{"chat_template_kwargs":{"enable_thinking":false}}' \
  -messages-params '{"chat_template_kwargs":{"enable_thinking":false}}' \
  -responses-params '{"chat_template_kwargs":{"enable_thinking":false}}' \
  -embeddings-base-url http://llama-embed.local:11435/v1 \
  -embeddings-model /models/qwen3-embedding-0.6b-q8_0.gguf
```

**29 passed, 0 failed, 1 skipped** (`messages-cache`: the server reported no cache
read). The metric check: `PASS metrics live-chat: 10 usage records, 207 output
tokens, 0.020504 USD; 1 rate-limited; 14 successful upstream attempts` — it reads
`kaiak_usage_records_total`, `gen_ai_client_inference_usage_output_tokens_total`,
`kaiak_usage_cost_usd_total`, `kaiak_errors_total{kaiak_error_class=…}`,
`kaiak_upstream_attempts_total`, `kaiak_upstream_attempt_duration_seconds_count`
and `http_server_request_duration_seconds_count{http_route=…}` for the messages and
responses routes. Not run live: `llama-server` (the DGX serves vLLM; a `cria` swap
was not needed for the metric checks) and the two-backend run (`capacity` and
`failover` read `kaiak_backend_active_requests_limit` and `kaiak_circuit_state`;
one vLLM process is serving) — both run in the kit's self-test in `check-all`
against the fake backend.

### Findings

1. **`http.response.status_code` is a string in the OTLP metric export**
   (`Str(200)`); the HTTP convention types it as an int, and the log export sends
   it as one (`Int(200)`). `otlpmetric/encode.go` writes every attribute value with
   `otlp.StringValue`: the registry keeps attribute values as strings. No effect on
   Prometheus (labels are strings) — the comparison above holds — but a collector
   or backend joining a gateway's logs and metrics, or filtering on the
   convention's type, sees two types for one attribute. Not a deviation the spec
   names. For step 12: type the attribute (an int attribute in the definition, or
   the encoder writing this key as `intValue`), or name the deviation.
2. **Prometheus's `le` formatting** (check 2): a Prometheus behavior, documented, no
   kaiak change possible.

### Cleanup

Containers `kaiak-otelcol` and `kaiak-prom` removed, and their images
(`docker --context dev ps -a`: none); local gateways, the fake backend and the
sample control plane stopped (`pgrep`: none); scratch configs, binaries and logs
deleted.

### Suite

`scripts/check-all.sh`: exit 0 — gofmt, vet, staticcheck, telemetry boundary, race
tests (every package ok; `kaiak/e2e`, `telemetry/metric`, `otlp`, `otlplog`,
`otlpmetric`, `metrics`, `server` ok), the live-test kit's lint and self-test (all
kinds and the two-backend run, 0 failed), control `npm test` (629 tests, 628 pass,
1 skipped, 0 fail), lint (`boundaries ok`), cross-half e2e `ok kaiak/e2e 65.433s`,
"all checks passed". Docs-only step: no expected reds.
