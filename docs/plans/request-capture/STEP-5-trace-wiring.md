# Step 5 — wiring the trace export

**Status:** not started

## Intent

Start and stop the trace exporter with the gateway, expose its counts, and warn when
the config captures but this gateway exports no traces.

## Files likely touched

- `gateway/cmd/kaiak/settings.go` — `otlp.ReadSettings(otlp.Traces, …)`;
  `KAIAK_CAPTURE_MEMORY_BYTES` (default 128 MiB, the `*_BYTES` pattern).
- `gateway/cmd/kaiak/main.go`:
  - the exporter built when traces are on; the `kaiak starting` line names the traces
    endpoint as it names the others;
  - at exit, the trace exporter finished with the others (`finishExport`, bounded by
    the drain deadline) — order: traces, metrics, the stopped line, logs (so a trace
    export's failure report still reaches the log export);
  - the startup warning when a config cap exceeds its budget, extended if it applies
    (a body cap above the capture budget means such bodies are never captured).
- `gateway/internal/metrics/` — a `RegisterTraceExport` twin of `RegisterLogExport`:
  `otel.sdk.processor.span.processed` (component type `capture_span_processor`) and
  `otel.sdk.exporter.span.exported` (`otlp_http_json_span_exporter`), read at
  collect (decision 28). The capture drops (`memory`, `too_large`) are wired here as
  inputs the capture stage will feed (step 6).
- The config-applied line: when the applied config has groups with `capture: true`
  and traces are off, a warning naming how many (decision 26).
- Tests: `cmd/kaiak/traceexport_test.go` (as `logexport_test.go`), the warning, the
  setting.
- `docs/specs/GATEWAY.md`, Observability → Metric list: the two rows below, after
  `otel.sdk.exporter.metric_data_point.exported` — they land with the families that
  answer them (`TestFullScrapeIsTheSpecsMetricList` reads the table). Step 1 wrote
  them and moved them here; the rest of the counts' contract (Exporters' own counts,
  Naming → named deviations, Series at 0, Request capture) is in the spec already:

  ```
  | `otel.sdk.processor.span.processed` | counter, read at collect | `{span}` | `otel.component.type`, `otel.component.name`, `error.type` | `otel_sdk_processor_span_processed_total` | With OTLP trace export on: request captures the capture stage and the export's queue are done with — handed to the exporter (no `error.type`), or dropped: `memory` (the capture budget could not hold it), `too_large` (its span exceeds the export limit), `shutdown` (still queued when the final flush ended) (Request capture; Exporters' own counts) | — (added 2026-10-13) |
  | `otel.sdk.exporter.span.exported` | counter, read at collect | `{span}` | `otel.component.type`, `otel.component.name`, `error.type` | `otel_sdk_exporter_span_exported_total` | With OTLP trace export on: spans the exporter sent — accepted by the collector (no `error.type`), or failed, by how (Exporters' own counts) | — (added 2026-10-13) |
  ```

## Decisions made during planning

- **The budget lives where captures are made** (`internal/capture`, step 6); this
  step only parses the setting and passes it on.

## Acceptance criteria

- The gateway starts with and without traces; malformed trace settings fail the start;
  `kaiak starting` names the endpoint.
- The counts appear on `/metrics` and in OTLP metric export, with zero series present
  from the start, as the log exporter's.
- The exit order is tested: a span enqueued before SIGTERM is exported within the
  drain deadline.
- The warning is written once per applied config that captures with traces off.
- `scripts/check-all.sh` green (phase 2 ends here).
