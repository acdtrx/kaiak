# Step 8 — metric settings

**Status:** done (2026-10-08)

## Intent

The metrics signal's settings read and checked at start (decision 8), on the
per-signal reading of step 2.

## Files likely touched

- `gateway/internal/telemetry/otlp/settings.go` — `OTEL_METRICS_EXPORTER`
  (`otlp`, `none`), the metrics endpoint, headers, timeout and protocol;
  `OTEL_METRIC_EXPORT_INTERVAL` (ms, default 60000), `OTEL_METRIC_EXPORT_TIMEOUT`
  (ms, default 30000), `OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE`
  (`cumulative`, `delta`, `lowmemory`, case-insensitive; default `cumulative`).
- `gateway/cmd/kaiak/main.go` — read with the other settings; `kaiak starting`
  carries the metric export endpoint (`kaiak.metric_export.endpoint`, host:port) as
  it does the log one.
- Tests: every variable, its fallback, its malformed form, empty = unset.

## Decisions made during planning

- One endpoint variable enables both signals; each signal opts out on its own.
- A zero or negative interval or timeout is malformed (the specification's values
  are positive).
- The export timeout bounds one export with its retries — the batch's timeout, as
  for logs; `OTEL_METRIC_EXPORT_TIMEOUT` and the OTLP timeout: the shorter wins
  (recorded in the spec by step 1 or here).

## Acceptance criteria

- A malformed value of each variable fails the start with a line naming it, never
  printing a header value (test).
- `scripts/check-all.sh` green. Suite recorded.

## Result

### What changed

- `gateway/internal/telemetry/otlp/signal.go` — the signal description gains
  `periodic *periodicVariables` (interval, export timeout, temporality variable
  names): set for `Metrics`, nil for `Logs`. Reading branches on that data, never
  on the signal.
- `otlp/settings.go` — `readPeriodic` reads `OTEL_METRIC_EXPORT_INTERVAL` (default
  60 s), `OTEL_METRIC_EXPORT_TIMEOUT` (default 30 s) and
  `OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE` (any case; default
  `cumulative`). Durations share one parser with the OTLP timeout (`milliseconds`:
  whole ms above 0; zero, negative, units, fractions, overflow are errors naming the
  variable). Typed `Temporality` (`Cumulative` — the zero value — `Delta`,
  `LowMemory`, `String` as the variable spells it). Accessors `Interval()`,
  `ExportTimeout()`, `Temporality()`. As with every other export setting, the
  periodic variables are read only when the signal's export is on.
- `gateway/cmd/kaiak/settings.go` — `metricExport` read beside `logExport`; a
  malformed value fails the start through the same error path.
- `gateway/cmd/kaiak/main.go` — `kaiak starting` carries
  `kaiak.metric_export.endpoint` (host:port) when metric export is on. No exporter
  yet (step 10).

### Decisions settled while implementing

- **`ExportTimeout()` is the effective bound**, computed at read: the shorter of
  `OTEL_METRIC_EXPORT_TIMEOUT` and the OTLP request timeout (signal's, else
  general, else 10 s) — 10 s with the defaults, as the spec says. For logs it is the
  request timeout (the batch's bound), so step 9 and `otlplog` can read one name.
  The raw `OTEL_METRIC_EXPORT_TIMEOUT` is not exposed.
- `Interval()` is 0 and `Temporality()` `Cumulative` for logs (no such settings).
- `String()` unchanged (no new fields printed).
- No spec edit: Configuration sources → OTLP export and Observability → OTLP
  metric export already state these variables, defaults and the timeout rule.

### Tests

- `otlp`: `TestMetricSettings` — defaults, empty = unset, the interval, each
  combination of export and request timeout (shorter wins; metrics request timeout
  over the general one), each preference and any case; logs read none of them
  (even malformed); metrics off reads none of them; malformed interval (0,
  negative, unit, fraction, overflow), export timeout (0, negative, text),
  temporality (unknown, misspelled). `TestTemporalityString`. The per-signal
  variables (exporter, endpoint, headers, timeout, protocol), their fallbacks and
  malformed forms for metrics were already covered by `TestSettingsPerSignal`.
- `cmd/kaiak`: `TestMetricExportSettings` — nothing set, one shared endpoint
  enabling both, metrics on / logs off, logs on / metrics off, metrics endpoint
  alone; each metric variable malformed fails `readSettings` naming it, a header
  value never echoed. `TestStartingLineNamesTheMetricExportEndpoint` — present
  (host:port) when on, absent when off.

### Suite

`scripts/check-all.sh`: exit 0 — gofmt, vet, staticcheck, telemetry boundary, race
tests (`kaiak/e2e` 112.6 s, `telemetry/otlp`, `cmd/kaiak` ok), live-test kit lint
and self-test, control `npm test` (629 tests, 628 pass, 1 skipped, 0 fail), lint
(`boundaries ok`), cross-half e2e `ok kaiak/e2e 65.867s`, "all checks passed".
