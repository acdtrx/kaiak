# Step 6 — the rename

**Status:** done (2026-10-08)

## Intent

Every family but the usage ones takes its name, unit and attributes from the
`GATEWAY.md` table (step 1), and everything that reads metric names follows.

## Files likely touched

- `gateway/internal/metrics/{ops,delivery,control,logexport}.go` and the build info.
- `gateway/internal/server/metrics.go` — the request duration's attributes: method
  (the log line's `methodAttrs` rule; `_OTHER`), `url.scheme`, `http.route` (the
  matched route: a body endpoint's `route()`, `/v1/models`, `/v1/models/{model}`,
  `/v1/models/{model}/props`; absent for an unknown path), the status code (absent
  when none was sent), `error.type` (decision 14).
- The circuit state family (decision 16) from the routing picture.
- The log exporter's counts as `otel.sdk.exporter.log.exported` and
  `otel.sdk.processor.log.processed` (decision 9), with `otel.component.*`.
- Tests that read metrics: `server/*_test.go`, `e2e/*_test.go`, `cmd/kaiak`,
  `metrics_test.go`; `scripts/live/{checks,twobackends}.go`;
  `scripts/smoke-images.sh`, `scripts/build-images.sh`.
- Docs naming metrics outside the spec: `docs/DEPLOYMENT.md` (alerts, dashboards),
  `docs/testing/LIVE-BACKENDS.md`, `docs/ARCHITECTURE.md`,
  `docs/architecture/gateway.html`, `docs/TECH-STACK.md`.

## Decisions made during planning

- The route attribute is the route as the client API documents it, not the request
  path: `{model}` stays literal.
- Series at 0 unchanged in scope (the spec's list), in the new names; the request
  duration still created on first use.

## Acceptance criteria

- Grep checklist, outside `docs/plans` and `docs/reviews`, `git grep -P` for each old
  name and label that changed (`kaiak_request_duration_seconds`,
  `kaiak_output_tokens_per_second`, `kaiak_backend_in_flight_requests`,
  `kaiak_backend_max_in_flight`, `kaiak_queued_requests`, `kaiak_queue_wait_seconds`,
  `kaiak_circuit_open`, `kaiak_circuit_half_open`, `kaiak_log_export_records_total`,
  `kaiak_usage_queued_bytes`, `\bstatus_class\b`, `\bscope_kind\b`,
  `\bdeployment_model\b` as a label, `\bgo_version\b`): no hit where a metric is
  meant.
- A test over a full scrape: every family and label in the `GATEWAY.md` table and
  nothing else (usage families excepted until step 7).
- `scripts/check-all.sh` — usage-metric reds only, cleared in step 7; named. Suite
  recorded.

## Result

### What changed

- **Ops families** (`gateway/internal/metrics/ops.go`): every definition takes the
  table's OpenTelemetry name, unit and attribute keys; shared keys are constants
  (`gen_ai.request.model`, `kaiak.backend.id`, `kaiak.deployment.model`,
  `kaiak.attempt.outcome`, `kaiak.circuit.state`). Renamed: `kaiak.queue.wait` →
  `kaiak.queue.wait_duration`, `kaiak.backend.in_flight_requests` →
  `kaiak.backend.active_requests`, `kaiak.backend.max_in_flight` →
  `kaiak.backend.active_requests_limit`, `kaiak.queued_requests` → `kaiak.queue.size`,
  `kaiak.request.duration` → `http.server.request.duration`. `QueueReason` values are
  the error codes (`queue_full`, `queue_timeout`) under `error.type`.
- **Request duration**: `ObserveRequest(method, route, status, errorType, model, d)`;
  `url.scheme` the constant `http`; status absent when 0. `server/metrics.go` passes
  `knownMethod` (the method rule, factored out of `methodAttrs` in `requestlog.go`),
  `endpoint.route()` (now also the model endpoints': a `path` field holding
  `/v1/models`, `/v1/models/{model}`, `/v1/models/{model}/props`), `statusSent` (the
  log line's no-status-for-a-client-that-left rule, now one function for both), and one
  `errorType` (error code, else `kaiak.relay_end`) shared with `kaiak.request.errors`.
  `statusClass` gone.
- **Circuit state**: one `kaiak.circuit.state` up-down counter, three points per
  deployment (closed/open/half_open, 1 on the current) from the routing picture,
  beside `kaiak.deployment.cooling_down`; `deploymentState` gone.
- **Key labels**: `keyLabelNames` → `keyAttributes` = `kaiak.key.group`,
  `kaiak.key.root_group`, `kaiak.key.id`, shared by `kaiak.request.errors` and the
  usage families (so usage series now carry these three plus the old `model`,
  `status`, `unit` until step 7 — the families' structure is untouched).
- **Build info**: `service.version`, `process.runtime.version`. **Delivery**:
  `kaiak.usage.batch.result`, `kaiak.usage.drop_reason`, `kaiak.usage.queue.size`.
- **Log export counts**: `otlp.Outcome.ErrorType` — the status as a string (an
  unretried one, the last when retries ran out, a redirect), `malformed_response`,
  `timeout`, or the connection class (`connection_refused`, `name_not_resolved`,
  `host_unreachable`, `connection_closed`, `tls_failure`, else `connection_failed`)
  mapped from `netfail.Class`; constants `otlp.ErrorRejected`,
  `ErrorMalformedResponse`, `ErrorTimeout`. `otlplog.Exporter.Counts()` returns
  `otlplog.Counts{Handed, QueueFull, Shutdown, Exported, Failed map[error.type]}`
  (handed counted at `take`; the old `dropped` split into full queue vs. closed).
  `metrics.RegisterLogExport(reg, func() otlplog.Counts)` writes
  `otel.sdk.processor.log.processed` and `otel.sdk.exporter.log.exported` with
  `otel.component.type`/`.name` (`batching_log_processor[/0]`,
  `otlp_http_json_log_exporter[/0]`); accepted + the queue's three series at 0,
  failure series on first use. The telemetry tree still imports no kaiak package.
- **Tests**: every metric-reading test follows (`server`, `e2e`, `cmd/kaiak`,
  `metrics`, `otlp`, `otlplog`). New: `metrics/spec_test.go`
  `TestFullScrapeIsTheSpecsMetricList` — parses the GATEWAY.md table (Prometheus
  name, kind, attributes with "usage labels"/"as above" expanded and translated by
  the Naming rule) and compares it with a full scrape of every family driven to show
  every attribute: same families, TYPE and label sets both ways (checked to fail on a
  planted attribute rename). Pending, both directions: `kaiak_usage_records_total`,
  `kaiak_usage_cost_usd_total`, `kaiak_usage_tokens_total`, the five
  `gen_ai_client_inference_usage_*` (step 7 removes these from `pendingFamilies`),
  `otel_sdk_exporter_metric_data_point_exported_total` (step 9). Also
  `TestRequestDurationRoutesAndMethods` (model routes, `{model}` literal, `_OTHER`, no
  route for an unknown path or refused method), the client-closed series without a
  status (`queue_test`), `TestConnectionErrorType`,
  `TestRefusedConnectionErrorType`, `error.type` asserted on every `otlp` failure
  test, and the `otlplog` count tests compare `Counts` whole.
- **Scripts**: `scripts/live/{checks,twobackends,apis}.go`,
  `scripts/smoke-images.sh` (`kaiak_build_info{service_version=…`).
  `scripts/build-images.sh` names no metric label (its `go_version` is a shell
  variable): unchanged.
- **Docs**: `DEPLOYMENT.md` (alerts, cardinality, errors-per-team query, version,
  config load cost, log export loss — names and labels current; the log-loss alert is
  now an `or` of the two families, since the exporter's failure series do not exist
  before a failure), `docs/testing/LIVE-BACKENDS.md`, `docs/architecture/gateway.html`,
  `docs/BACKLOG.md` (a revisit trigger naming the in-flight metric). `ARCHITECTURE.md`,
  `TECH-STACK.md`, README needed nothing. Code comments naming old metrics brought
  current (`config/snapshot.go`, `cmd/kaiak/main.go`, `server/attempts.go`).

### Spec readings (smallest reading taken; flag for review)

- **`http.route` on a refused method** (`405`, e.g. `GET /v1/chat/completions`): absent.
  The refusal handler sets no endpoint, so the route counts as unmatched — consistent
  with the Cardinality bullet ("a refusal before the route or model is known has
  neither"). The HTTP convention would set the route when the path matched; say so if
  the route should be present.
- **A 2xx whose body could not be read** (connection broke mid-body): `malformed_response`
  — "an answer that could not be read as the signal's response … truncated JSON" — not
  `connection_closed`; `timeout` when the export's time ran out while reading. A 2xx
  over 4 MiB is `malformed_response`; a non-2xx over 4 MiB its status.
- **netfail classes not in the list**: "timed out" and "cancelled" (an attempt cut by
  the export's context, Shutdown included — "the bound at exit") → `timeout`;
  "malformed response" (a malformed HTTP answer) → `malformed_response`.
- **A batch that could not be encoded** (not reachable in practice): `error.type`
  `_OTHER`, the convention's fallback — the spec's list has no value for it.
- **Records logged after Shutdown** count as `shutdown`, like those still queued then.
- **Status 0** (a handler that wrote nothing, as for an aborted response before the
  header): no `http.response.status_code` on the metric; the log line keeps its
  existing behaviour (it writes the 0).

### Grep checklist

`git grep -nP '<each old name/label>' -- ':!docs/plans' ':!docs/reviews'` for
`kaiak_request_duration_seconds`, `kaiak_output_tokens_per_second`,
`kaiak_backend_in_flight_requests`, `kaiak_backend_max_in_flight`,
`kaiak_queued_requests`, `kaiak_queue_wait_seconds`, `kaiak_circuit_open`,
`kaiak_circuit_half_open`, `kaiak_log_export_records_total`,
`kaiak_usage_queued_bytes`, `\bstatus_class\b`, `\bscope_kind\b`,
`\bdeployment_model\b`, `\bgo_version\b`: remaining hits are GATEWAY.md's renames
line and Was columns (metric list and the log field tables), the `kaiak.tried` log
format `backend/deployment_model:outcome` (spec and `requestlog.go`), and the shell
variable `go_version` in `release.yml` and `build-images.sh` — none a metric.

### Suite

`scripts/check-all.sh`: exit 0 — gofmt, vet, staticcheck, telemetry boundary, race
tests (`kaiak/e2e` 109.9 s; `cmd/kaiak`, `metrics`, `server`, `telemetry/metric`,
`telemetry/otlp`, `telemetry/otlplog` ok), live-test kit lint and self-test, control
`npm test` (629 tests, 628 pass, 1 skipped, 0 fail), lint (`boundaries ok`),
cross-half e2e `ok kaiak/e2e 65.357s`, "all checks passed". No expected reds: the
usage families are left out of the full-scrape comparison until step 7 instead.
