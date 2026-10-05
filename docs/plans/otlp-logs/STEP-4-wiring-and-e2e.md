# Step 4 — wiring and e2e

**Status:** done (2026-10-05)

## Intent

Turn the exporter on in the gateway when configured, flush it in the drain, expose
its metric, and prove it end to end — against a fake collector in the suite and a
real OpenTelemetry Collector once by hand. This step ends the phase.

## Files likely touched

- `gateway/cmd/kaiak/main.go`: read the OTLP settings with the other settings;
  build the logger after them (a start failure still prints to stderr), wrapping the
  stderr handler when export is on; one `kaiak starting` attribute saying whether
  export is on and to which endpoint host (never headers).
- The drain: flush the exporter after usage, within the flush reserve.
- `gateway/internal/metrics`: `kaiak_log_export_records_total{outcome}`, read from
  the exporter's counts.
- `gateway/e2e`: a gateway with a fake collector — request lines (success, a limit
  refusal, a `401`) and boot events arrive with the stderr attributes and the
  resource; with no endpoint, the collector sees nothing.
- `docs/plans/otlp-logs/OVERVIEW.md`: verification status.

## Decisions made during planning

- The live check uses the `otel/opentelemetry-collector` image with the `otlp`
  receiver (HTTP, port 4318) and the `debug` exporter (`verbosity: detailed`), run on
  the user's Docker context; its output is pasted into this step's Result.

## Acceptance criteria

- e2e as above; the metric moves with exports and drops.
- `scripts/check-all.sh` green three times in a row, Go test cache cleared before
  each.
- The live check shows request lines with their attributes and the resource
  (`service.name`, `service.instance.id`); container and scratch files removed after.
- Status recorded in every step file; the phase is committed.

## Result

**What changed**

- `gateway/cmd/kaiak/main.go`:
  - `readSettings` reads the export settings (`otlplog.ReadSettings`) with the
    others: a malformed `OTEL_*` value fails the start, on stderr.
  - `main` still builds the stderr logger first from `KAIAK_LOG_FORMAT` (a bad value
    prints `kaiak: …` and exits 1, as before). `run` builds the exporting logger
    once the settings are read: with export on it starts `otlplog.New` (resource:
    `metrics.Version()`, the instance ID; problem reports to the stderr logger
    alone), wraps the stderr handler with `Exporter.Handler`, and `kaiak starting`
    carries `kaiak.log_export.endpoint` (`EndpointHost()`); off, the attribute is
    absent.
  - `run` writes `kaiak stopped with an error` itself (moved from `main`, which now
    only exits 1), so it is exported; then, as its last act after every other
    deferred step, `finishLogExport`: `Flush` until the drain's deadline (grace +
    timeout from the drain's start) or 1 s from now, whichever is later — the 1 s
    alone after a start failure or a second stop signal — then `Close`.
  - Registers the metric from `Counts()` when export is on.
- `gateway/internal/metrics`: `Registry.CounterFunc` (a counter read at scrape
  time, as `GaugeFunc` is for gauges); `logexport.go` — `LogExportCounts` and
  `RegisterLogExport` (`kaiak_log_export_records_total{outcome}`, the three series
  from startup), fed through an adapter in `cmd/kaiak`, as `ControlState` is;
  `Version()` — the build version `kaiak_build_info` reports, now also
  `service.version`. Test: `TestLogExportMetrics`.
- `gateway/cmd/kaiak/logexport_test.go`: the settings (off, on, three malformed
  variables named in the error); a rejected config at startup exports `kaiak
  starting`, `config rejected` and `kaiak stopped with an error` before `run`
  returns; a settings error reaches stderr; the error line written once.
- `gateway/e2e/logexport_test.go` (fake OTLP collector on `httptest`, decoding the
  OTLP JSON into typed values):
  - `TestLogExport`: boot, a success, a `401`, a `rate_limit_exceeded` refusal, the
    drain and `kaiak stopped` — every stderr line and every exported record compared
    one to one (message, level, time to the nanosecond, every attribute and its
    type, severity number, observed time), plus spot checks of the request
    attributes; resource `service.name=kaiak`, `service.version` = the
    `kaiak_build_info` version, `service.instance.id=e2e` (winning over the one in
    `OTEL_RESOURCE_ATTRIBUTES`), the variable's other attribute; scope `kaiak`; the
    path under a base endpoint (`/base/v1/logs`), the configured `Authorization`
    header sent and absent from stderr, as is the endpoint URL;
    `kaiak.log_export.endpoint` on `kaiak starting`; `failed`/`dropped` at 0 from
    startup; `exported` equal to the records the collector accepted.
  - `TestLogExportOff`: no endpoint (with `OTEL_EXPORTER_OTLP_PROTOCOL=grpc` and a
    service name set), and an endpoint with `OTEL_LOGS_EXPORTER=none` — no export
    reaches the collector, no metric series, no endpoint attribute.
  - `TestLogExportRefusedBatch`: the first batch answered `400` — counted `failed`
    (metric = the report's count), `log export failing` on stderr with the status
    and the collector's message, never exported; refused + accepted records equal
    stderr's other lines.
  - `TestLogExportStalledCollectorAtExit`: a collector that never answers — exit
    within the flush floor (drain timeout 0.5 s), the held batch reported failed,
    `cut short`; nothing exported.
  - The harness keeps the test process's `OTEL_*` variables out of every gateway, as
    it does `KAIAK_*`.
- `docs/specs/GATEWAY.md` (Observability → OTLP log export: at exit): after a second
  stop signal the 1 s alone bounds the final flush; a failed start's flush comes
  after `kaiak stopped with an error`.
- `docs/ARCHITECTURE.md`: `otlplog` in the package list, the collector in the data
  flow, scrape-time counters in `metrics`. `docs/architecture/gateway.html`: the
  `otlplog` row, the Observability bullet, the footer.
- `OVERVIEW.md`: verification status.

**Decisions made during the step**

- The spec over the step file in two places: `kaiak.log_export.endpoint` is absent
  when export is off (the step file had one attribute saying on or off), and the
  final flush is bounded by the drain's deadline with a 1 s floor (the step file and
  OVERVIEW decision 11 had "within the flush reserve").
- **A second stop signal leaves the final flush its 1 s floor only** — the spec did
  not say; "skips whatever waiting remains" (Lifecycle) decided it, and the spec now
  says so.
- **`run` writes `kaiak stopped with an error`**, not `main`: the line must go out
  before the final flush, which `run` owns (the drain's deadline is `run`'s). One
  deferred function at the top of `run` writes it and then flushes, so it runs after
  every other deferred step (the data directory's lock, background goroutines).
- The exporter starts inside `run`, not in `main`: `run` already reads the settings,
  owns the registry and the drain's deadline, and every `cmd/kaiak` test keeps
  calling it unchanged.
- The metric is pulled at scrape time (`CounterFunc`) rather than pushed: the
  exporter already keeps atomic counts and must not import `metrics`.
- Seen in `TestLogExportStalledCollectorAtExit`: records dropped at exit right after
  a reported failure are counted but not reported — `Close`'s report falls inside
  the once-a-minute limit (step 3's decision, the spec's limit), and the metric dies
  with the process. Kept as specified; worth a look if exit drops ever need to be
  visible (a last report at exit exempt from the limit would cover it).

**Live check** (2026-10-05)

- The current Docker context (`default`) had no daemon; the user's `dev` context
  (`ssh://acdtrx@dev.local`, Docker 29.4.0, linux/amd64) ran
  `otel/opentelemetry-collector:0.162.0` (latest stable; config: `otlp` receiver,
  HTTP on `0.0.0.0:4318`; `debug` exporter, `verbosity: detailed`; copied in with
  `docker cp`), port 4318 published; `dev.local:4318` was reachable from this machine.
- The gateway ran locally in file mode — a `go build` of `./cmd/kaiak` (so SIGTERM
  reaches the gateway itself, not `go run`), the fake backend
  (`internal/fakebackend/cmd/fakebackend`), one model, one key in a group with a
  1-request-per-minute limit, `OTEL_EXPORTER_OTLP_ENDPOINT=http://dev.local:4318`,
  `OTEL_RESOURCE_ATTRIBUTES=deployment.environment.name=live-check`.
- A first start with a config missing `global` failed: the collector received
  `kaiak starting`, `config rejected`, `kaiak stopped with an error`.
- Then a success, a `401` and a `429`; `kaiak_log_export_records_total` read
  `exported 7, failed 0, dropped 0` before the stop; after SIGTERM the collector had
  all 12 lines of the run, `kaiak stopped` last. The request batch as the collector
  printed it:

```
ResourceLog #0
Resource SchemaURL:
Resource attributes:
     -> service.name: Str(kaiak)
     -> service.version: Str((devel))
     -> service.instance.id: Str(live-mac)
     -> deployment.environment.name: Str(live-check)
ScopeLogs #0
ScopeLogs SchemaURL:
InstrumentationScope kaiak
LogRecord #0
ObservedTimestamp: 2026-10-05 19:06:36.472243 +0000 UTC
Timestamp: 2026-10-05 19:06:36.472243 +0000 UTC
SeverityText: INFO
SeverityNumber: Info(9)
Body: Str(request)
Attributes:
     -> kaiak.request.id: Str(live-ok)
     -> http.request.method: Str(POST)
     -> url.path: Str(/v1/chat/completions)
     -> http.response.status_code: Int(200)
     -> kaiak.request.duration: Double(0.000613)
     -> kaiak.key.id: Str(k-live)
     -> kaiak.key.group: Str(live)
     -> gen_ai.request.model: Str(chat)
     -> gen_ai.request.stream: Bool(false)
     -> gen_ai.operation.name: Str(chat)
     -> kaiak.backend.id: Str(fake)
     -> kaiak.backend.type: Str(openai-compatible)
     -> kaiak.deployment.model: Str(fake)
     -> kaiak.attempts: Int(1)
     -> gen_ai.usage.input_tokens: Int(7)
     -> gen_ai.usage.cache_read.input_tokens: Int(0)
     -> gen_ai.usage.cache_write.input_tokens: Int(0)
     -> gen_ai.usage.output_tokens: Int(49)
     -> gen_ai.usage.reasoning.output_tokens: Int(0)
     -> kaiak.usage.cost_usd: Double(0)
     -> kaiak.usage.estimated: Bool(false)
     -> kaiak.usage.partial: Bool(false)
Trace ID:
Span ID:
Flags: 0
LogRecord #1
…
     -> kaiak.request.id: Str(live-401)
     -> http.response.status_code: Int(401)
     -> error.type: Str(missing_api_key)
     -> kaiak.auth.failure: Str(missing_key)
LogRecord #2
…
     -> kaiak.request.id: Str(live-429)
     -> http.response.status_code: Int(429)
     -> error.type: Str(rate_limit_exceeded)
     -> kaiak.limit.scope: Str(group)
     -> kaiak.limit.id: Str(live)
     -> kaiak.limit.type: Str(requests_per_minute)
     -> kaiak.limit.enforced: Int(1)
     -> kaiak.limit.configured: Int(1)
     -> kaiak.limit.used: Int(1)
```

- `service.version` is `(devel)`: what `kaiak_build_info` reports for that build
  (no link-time version). The container, the image, the processes and the scratch
  config were removed afterwards.

**Suite ×3** — `scripts/check-all.sh`, `go -C gateway clean -testcache` before each,
all three green:

| Run | Time | Gateway (`go test -race`) | Control | Cross-half e2e |
|---|---|---|---|---|
| 1 | 157 s | 17 packages ok (`e2e` 105.4 s, `otlplog` 3.9 s) | 574 tests, 574 pass | ok 43.9 s |
| 2 | 155 s | 17 packages ok (`e2e` 104.5 s, `otlplog` 3.9 s) | 574 tests, 574 pass | ok 43.9 s |
| 3 | 181 s | 17 packages ok (`e2e` 105.5 s, `otlplog` 3.8 s) | 574 tests, 574 pass | ok 64.0 s |

Each run also: gofmt, go vet, staticcheck 2026.2.1 (gateway and live-test kit), the
live-test kit's self-test (vllm, llama-server, openai, azure-openai, vllm with two
backends), `npm run lint` — all passed; `all checks passed`.
