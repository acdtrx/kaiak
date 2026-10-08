# Step 9 — the metric exporter

**Status:** not started

## Intent

`telemetry/otlpmetric`: on each interval, collect the registry, apply the
temporality, encode `ExportMetricsServiceRequest` JSON and send it through `otlp`;
count data points exported and failed.

## Files likely touched

- New `gateway/internal/telemetry/otlpmetric/`:
  - the reader: a ticker at the interval, one export in flight; a collect that
    overruns the interval skips the tick, never queues exports;
  - temporality: cumulative passes the snapshot; delta and lowmemory keep the last
    exported value per series and kind (decision 7) and send every series every
    time (decision 20); a counter seen to go back (never, by construction) is
    reported as a reset with a new start time;
  - encoding: `resourceMetrics` → `scopeMetrics` (scope `kaiak`) → `metrics`; sums
    with `isMonotonic` and `aggregationTemporality` (integers), gauges, histograms
    with `bucketCounts`, `explicitBounds`, `count`, `sum`; 64-bit integers as
    strings; `startTimeUnixNano` per decision; units and descriptions;
  - delivery through `otlp`; a batch is one collect, sent whole (the size bound is
    the specification's: a collect too large for one request is split by metric);
  - its own counts: `otel.sdk.exporter.metric_data_point.exported` registered on the
    registry it exports;
  - `ForceFlush(ctx)` (collect and export now) and `Shutdown(ctx)`.
- Problem reports to stderr only, as for logs: `metric export failing`, at most one
  a minute, with the data points failed and the last status — never the
  collector's text; the field table in `GATEWAY.md` gains its attributes.
- Tests with a fake collector: each kind and temporality, a reset, a late series,
  the self counts, partial success, a stalled collector bounded by the timeout.

## Decisions made during planning

- The registry is collected for OTLP and for `/metrics` independently; neither
  reader's state affects the other.
- No queue: a failed export's points are not kept for the next one — cumulative
  values carry them, and delta loses them (the specification's behaviour), counted
  as failed.

## Acceptance criteria

- Decoding an export with the OTLP JSON shape (a test-side decoder) yields every
  family of the registry with its unit, kind, attributes and values.
- Delta: the sum of the exported deltas equals the cumulative value, across a failed
  export in between (counted failed, lost).
- The boundary check holds; `go test ./internal/telemetry/...` passes on its own.
- `scripts/check-all.sh` green. Suite recorded.

## Result

