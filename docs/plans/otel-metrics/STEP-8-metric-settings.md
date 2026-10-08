# Step 8 — metric settings

**Status:** not started

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

