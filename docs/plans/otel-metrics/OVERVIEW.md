# Plan: OTel metrics

## Goal

Export the gateway's metrics over OTLP beside `/metrics`, the way the logs layer was
added (`docs/plans/otlp-logs/`), with every metric named in OpenTelemetry's
vocabulary: the semantic conventions' name, unit and attributes where one matches,
`kaiak.*` for the rest. One definition per metric serves both outputs: `/metrics`
writes the Prometheus form that Prometheus's own OTLP ingestion would make of the
OTLP one.

On the way, the metrics-side findings of the structure review land, so each metric
changes once (`docs/BACKLOG.md` → OpenTelemetry export → Metrics;
`docs/reviews/2026-10-07-structure/`: `modules/gateway-observability.md` F3, F4, F5,
F8, F10 and `STRUCTURE.md` T12), and the OpenTelemetry code is laid out as a
package tree that traces (the next plan) build on and that can leave the repo as a
library when a second project needs it.

## Scope

- **A telemetry tree**, `gateway/internal/telemetry/`, importing nothing from kaiak
  but the standard library and `netfail` (itself standard-library only):
  - `otlp` — the OTLP/HTTP connection every signal shares: `OTEL_*` settings per
    signal, the resource, the HTTP client, one export request and what came of it
    (delivered, partly rejected, failed, retried), with the delivery rules the logs
    settled (no redirects, no remote text, what counts as delivered, retries and
    `Retry-After`);
  - `otlplog` — the log exporter, moved here and onto `otlp`;
  - `metric` — the registry as OpenTelemetry instruments, a collect step producing
    the data model, and the Prometheus text writer with the OpenTelemetry →
    Prometheus name translation;
  - `otlpmetric` — the periodic OTLP metric exporter reading the registry.
- **The metric list renamed** to the target table below — names, units, attributes —
  on `/metrics` and OTLP alike.
- **The structure findings:** producer-owned label lists (F3), one key-label helper
  (F4), per-config gauges prepared in one place (F5), the build version owned by
  `main` (F8), the log-export count mirror gone (F10), one routing picture for the
  status report and the gauges (T12).
- **The dependency ruling** relaxed to "minimal, maintained, worth their cost", with
  the OpenTelemetry SDK's exporters recorded as rejected and why.
- Spec, DEPLOYMENT (upgrade notes with every old → new name), README, architecture,
  the live-test kit and image smoke test; e2e with a fake collector; a live check
  against an OpenTelemetry Collector and against Prometheus's OTLP receiver.

## Out of scope

- **Traces** — the next plan. It takes `go.opentelemetry.io/otel` + `sdk/trace` (the
  ruling in step 1 allows it) with a span exporter of our own on `telemetry/otlp`.
- **Moving logs onto the SDK** — the SDK's log pipeline goes against three of our
  rules (drop policy, the `slog` bridge's mapping, remote text in errors; step 1).
- **Go runtime and process metrics** (`go.*`, `process.*`), `http.server.active_requests`,
  body-size histograms: kaiak emits none today (backlog entry, step 12).
- **`target_info` and `otel_scope_*` labels on `/metrics`** (decision 17).
- **Extracting the telemetry tree** into its own repo or module: when a second project
  needs it (decision 13).
- protobuf and gRPC encoding, compression, exemplars, exponential histograms, TLS
  client certificates (decision 8).

## Decisions

Settled with the user (2026-10-08):

1. **Our own exporters, not the SDK's.** A spike (Go SDK v1.47.0) found the
   SDK's OTLP exporters link the gRPC client stack and protobuf (+13.5 MB on a 12.7 MB
   binary) and break settled rules: export errors are strings carrying the
   collector's text (and a malformed headers variable prints the header value);
   a `200` login page and a JSON partial success count as delivered; a refused
   connection is not retried; redirects are followed with the credential headers
   unless a custom client is passed; malformed `OTEL_*` values are ignored silently;
   a histogram series cannot exist before its first observation; the log pipeline
   drops the oldest records and the `slog` bridge nests groups, writes times as
   integers and drops the error attribute's key. The trace SDK core (`otel`,
   `sdk/trace`) is small — four third-party modules, no gRPC — and is the traces
   plan's to take.
2. **One vocabulary: `/metrics` is renamed too.** Each metric is defined once in
   OpenTelemetry form; `/metrics` writes its Prometheus translation. No dual names.
3. **Request duration is `http.server.request.duration`** with the HTTP convention's
   attributes (`http.request.method`, `url.scheme`, `http.route`,
   `http.response.status_code`, `error.type`) plus `gen_ai.request.model`. The status
   code replaces `status_class`; a client that left before any answer has none, as
   on the log line.
4. **Buckets: ours where the convention's are too short** — the HTTP advice stops at
   10 s; generations run minutes. A named deviation.
5. **Usage tokens are the GenAI usage counters**
   (`gen_ai.client.inference.usage.{input_tokens, output_tokens,
   cache_read.input_tokens, cache_write.input_tokens, reasoning.output_tokens}`),
   counted as the log line counts them (input includes cache reads and writes, output
   includes reasoning), with `gen_ai.operation.name`, `gen_ai.provider.name` (absent
   for the self-hosted types, as on the log line) and `gen_ai.token.modality`
   (`unknown`). Records, cost and clamped records stay `kaiak.usage.*`.
6. **Time to first token and decode speed keep `kaiak.*` names** — the convention's
   `time_to_first_chunk` counts any first chunk, `time_per_output_chunk` measures each
   chunk. Everything without a convention is `kaiak.*`.
7. **Temporality:** `OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE` —
   `cumulative` (default), `delta`, `lowmemory`, as the specification defines them.
8. **Settings read:** endpoint, headers, timeout (per signal, then shared),
   `OTEL_METRICS_EXPORTER`, `OTEL_METRIC_EXPORT_INTERVAL` (60 s),
   `OTEL_METRIC_EXPORT_TIMEOUT` (30 s), the temporality preference. Not read,
   documented: protocol other than `http/json`, compression, exemplars, exponential
   histograms, certificates. A malformed value fails the start.
9. **The exporters' own counts are the SDK self-observability metrics**
   (`otel.sdk.exporter.log.exported`, `otel.sdk.processor.log.processed`,
   `otel.sdk.exporter.metric_data_point.exported`; development-stage conventions).
10. **GenAI conventions pinned at the commit of 2026-10-06** (`4f85037`, the
    `gen_ai.client.inference.*` metric names) for metrics and log attributes alike.
11. **Dependency rule** (replaces "zero third-party dependencies"): minimal,
    maintained dependencies that are clearly worth what they cost; the binary stays
    usable standalone, depending on no service but the control plane; storage and
    management stay with the app built on `kaiak-control`. A dated ruling in
    `TECH-STACK.md` and `AGENTS.md`.
12. **Keep OpenTelemetry's philosophy, not its API's shape.** Code records through
    domain methods and never knows about export; definitions follow the data model
    (name, unit, description, instrument kind, attributes; start times; resource and
    scope apart); aggregation, collection and export are separate, each reader with
    its own temporality; exporters have `ForceFlush(ctx)` and `Shutdown(ctx)`.
13. **The telemetry tree stays in the repo, built to leave:** no kaiak imports
    (checked by `scripts/check-gateway.sh`), its own tests with no kaiak fixtures.
    Extracted when a second project wants it.

Made while planning (confirm in review):

14. **`error.type` on the request duration is kaiak's error code** — the log line's
    `error.type` — for every request that ended in an error, `4xx` included. The HTTP
    convention sets it for server `5xx` only (a `4xx` is not a server error) and uses
    the status as its value; kaiak's codes say which refusal, at the same
    cardinality. A named deviation.
15. **`url.scheme` is always `http`**: the API listener speaks plain HTTP; TLS, where
    there is any, ends before it.
16. **The circuit gauges merge into `kaiak.circuit.state`**, one series per deployment
    and state (`closed`, `open`, `half_open`), 1 for the current one — the
    conventions' state-attribute pattern (`k8s.node.condition`, `hw.status`) — instead
    of one 0/1 gauge per non-closed state. The cooldown stays its own 0/1 gauge.
17. **No `target_info`, no `otel_scope_*` labels on `/metrics`.** Scrape configs set
    `job`/`instance`, which the resource would repeat, and the scope is one constant;
    Prometheus's OTLP ingestion adds no scope labels by default. The compatibility
    spec's exporter rule (both by default) is the deviation, named.
18. **Attribute names are the log vocabulary's**; where the log line has no name, a
    new `kaiak.*` one: `kaiak.key.root_group`, `kaiak.error.class`,
    `kaiak.attempt.outcome`, `kaiak.circuit.state`, `kaiak.probe.result`,
    `kaiak.config.result`, `kaiak.usage.status`, `kaiak.usage.batch.result`,
    `kaiak.usage.drop_reason`.
19. **Usage metrics name the public model**: `gen_ai.request.model` is the model the
    client asked for on every metric, as on the log line; attempt metrics name the
    backend's model as `kaiak.deployment.model`. Attempts are not
    `gen_ai.client.inference.duration`: its `gen_ai.request.model` would be the
    backend's name, the opposite of every other metric.
20. **Delta exports every series every time** — an unchanged one as 0 — so a series
    created at 0 never disappears; the SDK drops unchanged series, which breaks
    "series at 0" after the first export.
21. **The final export runs at exit** after the usage flush and the final status and
    before the log flush, bounded as the log flush is (Lifecycle → Draining).
22. **The usage record carries its operation** gateway-locally (`json:"-"`, like
    `Generation`): the usage metrics need `gen_ai.operation.name`, and the protocol
    has no field for it. `gen_ai.provider.name` comes from the deployment's backend
    type in the live config.

## The metric list (target)

Prometheus names as Prometheus's OTLP ingestion translates them
(`UnderscoreEscapingWithSuffixes`, its default): dots to `_`, the unit's suffix
(`s` → `_seconds`, `By` → `_bytes`, `/s` → `_per_second`; a `{…}` unit adds none),
`_total` on counters. Label names translate the same way (`kaiak.backend.id` →
`kaiak_backend_id`). The usage labels are `kaiak.key.group`, `kaiak.key.root_group`,
`kaiak.key.id`, `gen_ai.request.model`, `gen_ai.operation.name`,
`gen_ai.provider.name`, `kaiak.usage.status`.

| OpenTelemetry name | Kind | Unit | Attributes | Prometheus | Was |
|---|---|---|---|---|---|
| `http.server.request.duration` | histogram | `s` | `http.request.method`, `url.scheme`, `http.route`, `http.response.status_code`, `error.type`, `gen_ai.request.model` | `http_server_request_duration_seconds` | `kaiak_request_duration_seconds` |
| `kaiak.time_to_first_token` | histogram | `s` | `gen_ai.request.model`, `kaiak.backend.id` | `kaiak_time_to_first_token_seconds` | same |
| `kaiak.output_token_rate` | histogram | `{token}/s` | `gen_ai.request.model`, `kaiak.backend.id` | `kaiak_output_token_rate_per_second` | `kaiak_output_tokens_per_second` |
| `kaiak.errors` | counter | `{request}` | `kaiak.error.class` | `kaiak_errors_total` | same, `class` |
| `kaiak.request.errors` | counter | `{request}` | key labels, `gen_ai.request.model`, `error.type` | `kaiak_request_errors_total` | same, `code` |
| `kaiak.limit.rejections` | counter | `{request}` | `kaiak.limit.scope`, `kaiak.limit.type` | `kaiak_limit_rejections_total` | same, `scope_kind`, `type` |
| `kaiak.backend.active_requests` | up-down counter (read at collect) | `{request}` | `kaiak.backend.id` | `kaiak_backend_active_requests` | `kaiak_backend_in_flight_requests` |
| `kaiak.backend.active_requests_limit` | up-down counter (read at collect) | `{request}` | `kaiak.backend.id` | `kaiak_backend_active_requests_limit` | `kaiak_backend_max_in_flight` |
| `kaiak.queue.size` | up-down counter (read at collect) | `{request}` | `gen_ai.request.model` | `kaiak_queue_size` | `kaiak_queued_requests` |
| `kaiak.queue.wait_duration` | histogram | `s` | `gen_ai.request.model` | `kaiak_queue_wait_duration_seconds` | `kaiak_queue_wait_seconds` |
| `kaiak.queue.rejections` | counter | `{request}` | `gen_ai.request.model`, `error.type` (`queue_full`, `queue_timeout`) | `kaiak_queue_rejections_total` | same, `reason` (`full`, `timeout`) |
| `kaiak.retries` | counter | `{attempt}` | `gen_ai.request.model`, `kaiak.backend.id`, `kaiak.attempt.outcome` | `kaiak_retries_total` | same, `reason` |
| `kaiak.upstream.attempts` | counter | `{attempt}` | `kaiak.backend.id`, `kaiak.deployment.model`, `kaiak.attempt.outcome` | `kaiak_upstream_attempts_total` | same |
| `kaiak.upstream.attempt.duration` | histogram | `s` | `kaiak.backend.id` | `kaiak_upstream_attempt_duration_seconds` | same |
| `kaiak.request.attempts` | histogram | `{attempt}` | `gen_ai.request.model` | `kaiak_request_attempts` | same |
| `kaiak.circuit.state` | up-down counter (read at collect) | `{deployment}` | `kaiak.backend.id`, `kaiak.deployment.model`, `kaiak.circuit.state` | `kaiak_circuit_state` | `kaiak_circuit_open`, `kaiak_circuit_half_open` |
| `kaiak.deployment.cooling_down` | up-down counter (read at collect) | `{deployment}` | `kaiak.backend.id`, `kaiak.deployment.model` | `kaiak_deployment_cooling_down` | same |
| `kaiak.circuit.transitions` | counter | `{transition}` | `kaiak.backend.id`, `kaiak.deployment.model`, `kaiak.circuit.state` (entered) | `kaiak_circuit_transitions_total` | same, `to` |
| `kaiak.probes` | counter | `{probe}` | `kaiak.backend.id`, `kaiak.probe.result` | `kaiak_probes_total` | same, `result` |
| `kaiak.config.loads` | counter | `{load}` | `kaiak.trigger`, `kaiak.config.result` | `kaiak_config_loads_total` | same, `trigger`, `result` |
| `kaiak.config.last_applied_timestamp` | gauge | `s` | — | `kaiak_config_last_applied_timestamp_seconds` | same |
| `kaiak.config.size` | gauge | `By` | — | `kaiak_config_size_bytes` | same |
| `kaiak.config.apply.duration` | histogram | `s` | `kaiak.trigger`, `kaiak.config.result` | `kaiak_config_apply_duration_seconds` | same |
| `kaiak.limits.sync.duration` | histogram | `s` | — | `kaiak_limits_sync_duration_seconds` | same |
| `kaiak.connections.refused` | counter | `{connection}` | — | `kaiak_connections_refused_total` | same |
| `kaiak.build.info` | gauge | — | `service.version`, `process.runtime.version` | `kaiak_build_info` | same, `version`, `go_version` |
| `kaiak.usage.batch.sends` | counter | `{batch}` | `kaiak.usage.batch.result` | `kaiak_usage_batch_sends_total` | same, `result` |
| `kaiak.usage.queue.batches` | up-down counter | `{batch}` | — | `kaiak_usage_queue_batches` | same |
| `kaiak.usage.queue.records` | up-down counter | `{record}` | — | `kaiak_usage_queue_records` | same |
| `kaiak.usage.queue.size` | up-down counter | `By` | — | `kaiak_usage_queue_size_bytes` | `kaiak_usage_queued_bytes` |
| `kaiak.usage.dropped_records` | counter | `{record}` | `kaiak.usage.drop_reason` | `kaiak_usage_dropped_records_total` | same, `reason` |
| `kaiak.usage.last_ack_timestamp` | gauge | `s` | — | `kaiak_usage_last_ack_timestamp_seconds` | same |
| `kaiak.control.connected` | gauge (read at collect) | — | — | `kaiak_control_connected` | same |
| `kaiak.control.last_contact_timestamp` | gauge (read at collect) | `s` | — | `kaiak_control_last_contact_timestamp_seconds` | same |
| `kaiak.control.totals_applied_timestamp` | gauge (read at collect) | `s` | — | `kaiak_control_totals_applied_timestamp_seconds` | same |
| `kaiak.control.outage` | gauge (read at collect) | — | — | `kaiak_control_outage` | same |
| `kaiak.control.config_rejected` | gauge (read at collect) | — | — | `kaiak_control_config_rejected` | same |
| `kaiak.usage.records` | counter | `{record}` | usage labels | `kaiak_usage_records_total` | same |
| `kaiak.usage.clamped_records` | counter | `{record}` | — | `kaiak_usage_clamped_records_total` | same |
| `kaiak.usage.cost_usd` | counter | `{USD}` | usage labels | `kaiak_usage_cost_usd_total` | same |
| `gen_ai.client.inference.usage.input_tokens` | counter | `{token}` | usage labels, `gen_ai.token.modality` | `gen_ai_client_inference_usage_input_tokens_total` | `kaiak_usage_tokens_total{unit}` — all input now |
| `gen_ai.client.inference.usage.cache_read.input_tokens` | counter | `{token}` | as above | `…_cache_read_input_tokens_total` | `unit="tokens_cached"` |
| `gen_ai.client.inference.usage.cache_write.input_tokens` | counter | `{token}` | as above | `…_cache_write_input_tokens_total` | `unit="tokens_cache_write"` |
| `gen_ai.client.inference.usage.output_tokens` | counter | `{token}` | as above | `…_output_tokens_total` | `unit="tokens_out"` |
| `gen_ai.client.inference.usage.reasoning.output_tokens` | counter | `{token}` | as above | `…_reasoning_output_tokens_total` | `unit="tokens_reasoning"` |
| `otel.sdk.exporter.log.exported` | counter | `{log_record}` | `otel.component.type`, `otel.component.name`, `error.type` (failed) | `otel_sdk_exporter_log_exported_total` | `kaiak_log_export_records_total{outcome}` exported, failed |
| `otel.sdk.processor.log.processed` | counter | `{log_record}` | `otel.component.type`, `otel.component.name`, `error.type` (`queue_full`, `shutdown`) | `otel_sdk_processor_log_processed_total` | its `dropped` |
| `otel.sdk.exporter.metric_data_point.exported` | counter | `{data_point}` | `otel.component.type`, `otel.component.name`, `error.type` (failed) | `otel_sdk_exporter_metric_data_point_exported_total` | new |

The exact `error.type` values for the exporters' failures, the up-down vs gauge choice
for each read-at-collect family, and the remaining names are fixed by step 1 in
`GATEWAY.md`; a change there is a change to this table.

## Constraints

- The request path never waits on export: collection reads atomics and live state;
  the exporter runs in the background.
- Nothing sensitive in metrics: label values stay config names and fixed vocabularies
  (`GATEWAY.md` → Observability), never client input, a key or content.
- The telemetry tree imports only the standard library and `netfail`.
- No new Go dependency in this plan; no new npm dependency.
- Specs change in the same step as the contract they describe.

## Risks

- **Breadth of the rename.** Metric names appear in ~45 files (tests, e2e, the
  live-test kit, smoke tests, docs). Mitigation: the rename is one step against the
  table, with a repo-wide grep checklist (`git grep -P`) of every old name.
- **Translation drift**: our Prometheus names must equal Prometheus's own
  translation. Mitigation: a table test of the translation rules from the
  compatibility spec's examples, and the live check (step 11) comparing a scraped and
  a pushed copy in one Prometheus.
- **Delta state** (decision 20): last-sent values per series per exporter.
  Mitigation: tests over counter resets, histograms and late-created series.
- **Moving `otlplog`** could lose a delivery rule. Mitigation: its tests move
  unchanged in what they assert; the rules live in `otlp` with their tests.

## Tag

`v0.11.6` on `main`, "before the OTel metrics plan", immediately before step 1
starts (not pushed to `github`: an anchor, not a release).

## Branch and worktree

Branch `otel-metrics`, worktree `.claude/worktrees/otel-metrics`. It rebases onto
`main` before the ff merge. Steps are implemented by subagents (the same model as
the main session), one step per brief; the main session reviews each against its
acceptance criteria and commits at the step boundary.

## Phases and steps

- **Phase 1 — Contract and ground** (steps 1–4). The spec says what the plan builds;
  the shared connection and the structure findings land with no metric output
  change.
  1. `STEP-1-contract.md` — the ruling, the metric list and OTLP metric export in
     the spec.
  2. `STEP-2-otlp-connection.md` — `telemetry/otlp`, `otlplog` moved onto it, the
     boundary check.
  3. `STEP-3-label-lists-and-version.md` — F3, F4, F8, F10.
  4. `STEP-4-routing-picture.md` — T12 and F5.
- **Phase 2 — The data model and the names** (steps 5–7). `/metrics` ends in the
  target names.
  5. `STEP-5-metric-data-model.md` — `telemetry/metric`: instruments, collect,
     Prometheus translation; output unchanged where the translation allows.
  6. `STEP-6-rename.md` — every family but usage to the target table.
  7. `STEP-7-usage-metrics.md` — the GenAI usage counters and the usage labels.
- **Phase 3 — OTLP metric export** (steps 8–10).
  8. `STEP-8-metric-settings.md` — the metrics signal's settings.
  9. `STEP-9-metric-exporter.md` — `telemetry/otlpmetric`: encoding, temporality,
     delivery, its own counts.
  10. `STEP-10-wiring-and-e2e.md` — `main`, the exit order, e2e with a fake
      collector.
- **Phase 4 — Docs, live and review** (steps 11–12).
  11. `STEP-11-docs-and-live.md` — DEPLOYMENT, README, architecture, live-test kit;
      live checks against a Collector and Prometheus's OTLP receiver.
  12. `STEP-12-review-and-green.md` — independent review, fixes, backlog and review
      outcome updates, `check-all` green.

## Verification

- Unit: the translation rules; every instrument kind collected and written; series
  at 0 for every config-determined family; each temporality across resets and
  late series; every setting and its malformed form; the delivery rules once, in
  `otlp`.
- e2e: a gateway with a fake collector receives every family under the resource,
  with the same names (translated), units, attributes and values as `/metrics` at
  the same moment; nothing is sent with no endpoint or `OTEL_METRICS_EXPORTER=none`;
  the final export arrives at exit; logs and metrics share one endpoint variable.
- Live: an `otel/opentelemetry-collector` (`debug` exporter) shows the metrics; a
  Prometheus 3.x with its OTLP receiver ingests the push while scraping `/metrics`,
  and the two copies have the same series names and labels.
- `scripts/check-all.sh` green 3× in a row.

**Verification status:** done (2026-10-08); every phase green.

- [x] Unit: the translation rules (checked against Prometheus's `otlptranslator`
  v1.0.0); every instrument kind; series at 0; each temporality, late series,
  exact scaled-counter deltas; every setting and its malformed form; the delivery
  rules in `otlp` (steps 2, 5, 8, 9).
- [x] The spec's metric list compared both ways with a full scrape
  (`metrics/spec_test.go`, steps 6–9).
- [x] e2e: push and scrape agree family by family in file and control-plane mode;
  nothing sent when off; the final export at exit; one endpoint for both signals;
  delta end to end (step 10).
- [x] Live: Collector 0.162.0 shows every family; Prometheus 3.15.0 ingests the push
  and scrapes `/metrics` with the same 409 series, 62 names and label names; the
  DGX live-test kit passes (step 11).
- [x] Independent review: two findings fixed, one accepted
  (`docs/reviews/2026-10-08-otel-metrics/`, step 12).
- [x] `scripts/check-all.sh` green 3× in a row (step 12).
