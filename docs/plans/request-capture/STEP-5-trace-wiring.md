# Step 5 — wiring the trace export

**Status:** done

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
- `docs/specs/GATEWAY.md`, Observability → Metric list: the rows of
  `otel.sdk.processor.span.processed` and `otel.sdk.exporter.span.exported`, after
  `otel.sdk.exporter.metric_data_point.exported` — they land with the families that
  answer them (`TestFullScrapeIsTheSpecsMetricList` reads the table). Step 1 wrote
  them and parked them here; the rest of the counts' contract (Exporters' own counts,
  Naming → named deviations, Series at 0, Request capture) is in the spec already.

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

## Result

### What changed

- `gateway/cmd/kaiak/settings.go`: `traceExport` (`otlp.ReadSettings(otlp.Traces, …)`,
  nil when off); `captureMemory` from `KAIAK_CAPTURE_MEMORY_BYTES` (the `*_BYTES`
  loop: whole bytes, 1 or more; default `defaultCaptureMemory`, 128 MiB), read
  whether or not trace export is on. Nothing reads `captureMemory` yet: step 6 builds
  the budget from it.
- `gateway/cmd/kaiak/main.go`:
  - `exports` — the three exporters, each nil when off — replaces the two locals;
    `startExports` starts the log export (its problems to stderr alone) and then the
    trace export, whose report logger is the process logger (wrapped by the log
    export when on: `trace export failing` is an ordinary line); the metric export
    still starts after `newGraph`, as `exp.metrics`;
  - `kaiak starting` names `kaiak.trace_export.endpoint` (after the log and metric
    endpoints) when trace export is on;
  - `exports.finish` holds the exit order, each step within `exitBound` as before:
    the trace flush, the final metric export, the last line, the log flush; run's
    defer calls it. Comments of `run`, `exitExportFloor`, `finishExport` name the
    trace flush;
  - `newGraph` takes `exports` and registers `metrics.RegisterTraceExport` when
    traces are on, with a zero drop source (`metrics.CaptureDrops{}`) — step 6
    replaces it with the capture's counts;
  - the applier callback warns `groups capture requests but trace export is off:
    nothing is captured` (`kaiak.config.capture_groups`, how many groups capture)
    after each applied config with a capturing group while `s.traceExport` is nil.
- `gateway/internal/metrics/traceexport.go` (new): `RegisterTraceExport(reg, counts
  func() otlptrace.Counts, drops func() CaptureDrops)` —
  `otel.sdk.processor.span.processed` (`capture_span_processor`: handed, `memory`,
  `too_large`, `shutdown`) and `otel.sdk.exporter.span.exported`
  (`otlp_http_json_span_exporter`: accepted, failed by `error.type`), read at collect;
  `CaptureDrops{Memory, TooLarge}` is the input the capture stage feeds.
- `docs/specs/GATEWAY.md`: the two Metric list rows, after
  `otel.sdk.exporter.metric_data_point.exported`; removed from this file.
- **No startup warning for a body cap above the capture budget**: not built, as
  STEP-1 decided (such a capture is dropped `memory`, which the line and the counts
  show); the spec has no field for one. The bullet above that says "extended if it
  applies" is settled: it does not.
- No dependency added.

### Decisions made while implementing

- **The exit order is one function, `exports.finish`**, and the test seam: nothing in
  `run` makes spans before step 6, so the exit test drives `startExports`, `newGraph`
  and `finish` — the code `run` calls — with real exporters and a queued span,
  rather than `run` itself. The span through the whole binary, enqueued by a real
  request before SIGTERM, is step 9's e2e ("the drain flushing captures at exit").
- **The warning reads the setting** (`s.traceExport == nil`), the operator's switch,
  as the `export_off` of step 6 will.
- **The default lives in `cmd/kaiak`** (`defaultCaptureMemory`) for now: the other
  budgets' defaults live with their owners (`server`, `control`), so step 6 may move
  it to `internal/capture` with the budget.

### Tests

- `cmd/kaiak/traceexport_test.go` (7 tests):
  - `TestTraceExportSettings` — off unset, off with the general endpoint alone, on with
    the traces endpoint, on with `OTEL_TRACES_EXPORTER=otlp` and the general one, off
    with `none`; five malformed trace values fail naming the variable, never echoing
    a header value;
  - `TestCaptureMemorySetting` — default 134217728, a set value, five malformed values
    failing naming the variable — with trace export off and on;
  - `TestStartingLineNamesTheTraceExportEndpoint` — `run` off / general endpoint alone
    / on: the endpoint named only when on; off, the warning (WARN, 1 group) after
    `config applied`; on, none;
  - `TestTraceExportCountsFromTheStart` — `run` with metric export on, traces on and
    off: `/metrics` (the admin listener, mid-run) holds exactly the five zero series
    when on and none when off; the final OTLP metric export the same, with their
    component type and name; no trace export request; no goroutine left after `run`
    returns;
  - `TestExitFlushesTracesFirst` — a queued span, then `finish` with a 10 s deadline:
    the trace export, then the final metric export (counting the span handed over and
    accepted, or failed `400`), then the log export carrying `kaiak stopped`; the
    span released; refused, `trace export failing` on stderr and in the log export;
  - `TestExitTraceFlushIsBounded` — a collector that never answers traces: exit takes
    the 1 s floor (≤ 3 s), the export cut short counts `timeout` in the final metric
    export and is reported, the log flush still carries the report and the last
    line;
  - `TestCaptureWithTraceExportOffIsWarned` — `newGraph`'s applier, each config applied
    twice then a rejected one: one warning per applied config naming 1 (minimal) or 3
    (three of four groups); none for a config with no capturing group, with trace
    export on, or after a rejection.
- `internal/metrics`: `TestTraceExportMetrics` (the five zero series at startup; then
  the drops from the capture input, the rest from the exporter); the full scrape
  registers the trace export, so `TestFullScrapeIsTheSpecsMetricList` reads the two
  new rows.
- Mutation checks (reverted): finishing metrics before traces fails both exit tests
  (`exports in the order traces 1, metrics 0`; the cut span missing from the final
  metric export); giving the trace exporter the stderr-only logger fails both
  (`trace export failing on stderr true, in the log export false`).
- `go test -race -count=8` over the new `cmd/kaiak` tests and `-count=5` over the
  metrics ones: green.

### Suite

- `scripts/check-gateway.sh`: green — gofmt, vet, staticcheck and the telemetry
  boundary clean; `go test -race -count=1` passes every package (924 tests, examples
  and fuzz targets listed — 916 at step 4, plus the 8 above); the live-test kit's
  lint is clean and its self-test passes for vllm, llama-server, openai,
  azure-openai, anthropic, azure-anthropic, and vllm with two backends.
- `scripts/check-all.sh`: green — phase 2 ends here. Control `npm test`: 679 tests,
  678 pass, 0 fail, 1 skipped (the store contract's reconnect test, as before). `npm
  run lint`: tsc clean, "boundaries ok". The cross-half e2e passes. Final lines:

  ```
  ==> npm run lint (control)
  > tsc -p . && node scripts/check-boundaries.ts
  boundaries ok
  ==> cross-half e2e (sample control plane + two gateways; two cores over one store)
  ok  	kaiak/e2e	80.407s
  all checks passed
  ```
