# Step 9 — the metric exporter

**Status:** done (2026-10-08)

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

### What changed

- New `gateway/internal/telemetry/otlpmetric/` (standard library + `telemetry/metric`,
  `telemetry/otlp`):
  - `exporter.go` — `Exporter`: the periodic reader, delivery through
    `otlp.Client.Export`, its counts, `metric export failing` reports, `ForceFlush`,
    `Shutdown`.
  - `temporality.go` — the preference mapped per kind, the delta state.
  - `encode.go` — `ExportMetricsServiceRequest` JSON, assembled from separately
    encoded metrics and points, and the size split.
- `gateway/internal/metrics/metricexport.go` — `RegisterMetricExport(reg, func()
  otlpmetric.Counts)`: `otel.sdk.exporter.metric_data_point.exported`
  (`{data_point}`, read at collect) with `otel.component.type`/`.name`
  `otlp_http_json_metric_exporter[/0]`, the accepted series at 0, a failure's series
  from its first failure — as `RegisterLogExport`. New `TestMetricExportMetrics`;
  `fullScrape` registers it; `pendingFamilies` was left empty and is gone with its
  two uses (the full scrape now compares every family both ways).
- `gateway/internal/telemetry/fakeotlp` — decodes metric exports too (`Export`
  gains `ResourceMetrics`; `Metric`, `Sum`, `Gauge`, `Histogram`, the two point
  types, `Received.Metrics()`), with `DisallowUnknownFields` for both signals so a
  misspelled member fails the test. Shared `Resource`/`Scope` types. Still no kaiak
  import.
- `docs/ARCHITECTURE.md` — the `fakeotlp` entry names metric exports.
- `gateway/internal/telemetry/metric/collect.go` — `Family.Divisor` (a scaled
  counter's divisor, 0 otherwise) and `Point.SubUnits` (its exact count of whole
  sub-units, which `Double` divides), so a reader that subtracts values subtracts
  integers; `TestCollect` asserts both. `/metrics` output unchanged.

### API

```go
func New(s *otlp.Settings, svc otlp.Service, reg *metric.Registry, report *slog.Logger) *Exporter
func (e *Exporter) Counts() Counts                 // Counts{Exported uint64; Failed map[error.type]uint64}
func (e *Exporter) ForceFlush(ctx context.Context) error
func (e *Exporter) Shutdown(ctx context.Context)
```

- **Reader**: a ticker at `Interval()` (first export one interval after `New`). A
  tick carries the time it was due; one due before the last export ended is
  skipped, so an overrun never queues an export. Export and flushes run on the one
  sender goroutine: one export in flight.
- **ForceFlush(ctx)**: collect and export now (after the export in flight); the
  export's own context ends with ctx, so a final export is cut at its deadline (its
  undelivered points count `timeout`) rather than outliving it. Returns nil once
  the export ended, ctx's error, or `errShutdown` after Shutdown.
- **Shutdown(ctx)**: as `otlplog`'s — cancels the export in flight (counted
  failed), waits for the sender, closes idle connections, writes the last report
  whatever the once-a-minute limit says; exports nothing itself (the final export is
  `ForceFlush`'s, step 10). Once stopped no tick or flush starts an export.
- **Reports**: `report` is a `*slog.Logger` chosen by the caller. The spec says
  `metric export failing` is an ordinary line (stderr, and the log export when on),
  unlike `log export failing` — so step 10 passes the process logger, not
  stderr-only (the brief's "stderr only" read against the spec). Attributes:
  `kaiak.metric_export.failed`, `http.response.status_code`, `exception.message`
  (`otlp.Outcome`'s own words).

### Temporality as implemented

- `cumulative`: every sum and histogram 2; `delta`: counters (recorded and read at
  collect) and histograms 1, up-down counters 2; `lowmemory`: as delta but a counter
  read at collect (`Family.Observed`) stays 2. Gauges carry none and no start.
- Cumulative start: the snapshot's `StartTime` (series creation, or the registry's
  creation for a series read at collect) — stable across exports.
- Delta: the reader keeps per family and series the last collected cumulative value
  and that collect's time; every series is sent every time (unchanged → 0); start
  is the previous collect that carried the series, or the series' start for its
  first point (a late series starts at its creation). Histograms: per bucket, count
  and sum. A scaled counter (the cost) subtracts its integer sub-units and divides
  the change once by the divisor, so each delta is exact and the deltas sum to the
  cumulative count (the spec's exact integer sum of nano-dollars). A value below the last (never, by construction) is taken as a restart:
  its whole value is the change.
- **Failure** (the spec: "A delta export that fails is not sent again: its points
  are counted failed and lost"): the state moves on at every collect whatever the
  outcome, so the sum of *delivered* deltas is the cumulative value less the failed
  export's change; the next delta starts at the failed export's time. The acceptance
  line "sum of the exported deltas equals the cumulative value" holds counting the
  lost delta; `TestDeltaLostWhenAnExportFails` asserts 5 + 2 delivered, 7 lost, 14
  in all, `Failed{"400": 1}`.
- State lives as long as the registry's series (recorded series never go away); a
  series read at collect that disappears and returns continues from its last value.

### Encoding and size split

- `resourceMetrics` → one resource (`Client.Resource()`, shared with logs) → scope
  `kaiak` → metrics with `name`, `description`/`unit` (left out when empty), `sum`
  (`aggregationTemporality`, `isMonotonic`), `gauge`, `histogram` (`count`, `sum`,
  `bucketCounts`, `explicitBounds`); `asInt` as a decimal string, `asDouble` per the
  family's `Number`; `timeUnixNano` the collect's time on every point;
  `startTimeUnixNano` on sums and histograms. Empty attribute values left out.
  Families with no series are left out; a collect with no series sends nothing.
- Split at 4 MiB: points and metric frames encoded separately; whole metrics are
  packed into a request while they fit, a metric too large for one request alone is
  cut between points (each part repeats name, unit, description, temporality). A
  single point over the bound would go alone, over it (not reachable: a point is
  well under 1 KB). Requests go one after the other within one context bounded by
  `ExportTimeout()`; each request's points count by its own outcome.
- **4 MiB basis checked** (collector `main` at `1b185fe`, read-only via the GitHub
  API): `config/confighttp/server.go` — `defaultMaxRequestBodySize = 20 * 1024 *
  1024 // 20MiB`; `config/configgrpc/configgrpc.go` passes `grpc.MaxRecvMsgSize`
  only when `max_recv_msg_size_mib` is set, so grpc-go's
  `defaultServerMaxReceiveMessageSize = 1024 * 1024 * 4` applies. The spec's
  reason ("the collector's default message limit over gRPC and well under its HTTP
  receiver's default") is right; number unchanged.

### Tests (`otlpmetric/exporter_test.go`, 21 top-level)

`TestExportRoundTripsEveryFamily` (a test-side decoder of the OTLP JSON shape over
`fakeotlp`: every family with series — counter, scaled counter, up-down counter,
gauge, histogram, and the three read-at-collect kinds — compared with the
registry's collect for name, unit, description, kind, temporality, attributes,
start, value and int/double; resource, scope, headers), `TestExportGoesToTheMetricsPath`,
`TestTemporalityByKind` (3 preferences × 8 families), `TestCumulativeStartTimes`
(stable, late series), `TestDeltaStream` (every series every time, zeros, starts,
late series, scaled counter, histogram buckets/count/sum, up-down stays
cumulative), `TestDeltaLostWhenAnExportFails`, `TestCumulativeCarriesAFailedExport`,
`TestScaledCounterDeltasAreExact` (20 delta exports of 1000 increments of 1–7
nano-dollars over a total of 123 456 789 012: each delta equals its change ÷ 1e9
exactly, the deltas sum to the cumulative sub-units),
`TestLowMemoryKeepsObservedCountersCumulative`, `TestSubtractStartedOver`,
`TestOwnCountsShowInTheNextExport` (own count on the exported registry: 0, then the
previous export's), `TestPartialSuccess` (`rejectedDataPoints`, report without
collector text), `TestStalledCollectorIsBoundedByTheExportTimeout` (each timeout the
shorter), `TestTickDuringAnExportIsSkipped`, `TestExportsOnTheInterval` (real
ticker), `TestSizeSplit` (bound held per request, small metrics whole, large one
split, round-trip, a failed request fails only its points), `TestNothingToSend`,
`TestProblemReportsAreRateLimited`, `TestCollectorTextIsNeverReported`,
`TestForceFlushEndsWithItsContext`, `TestShutdownCutsTheExportAndReports`.
Mutations tried and caught (then reverted): no tick skip; delta start = series
start; state not advanced on failure; no split; lowmemory treating observed
counters as delta; flush ctx not bound to the export; a scaled counter's delta by
double subtraction (fails at the second export: 3.996999993205463e-06 for 3.997e-06). `-race -count=8` clean.

### Suite

- `go test -race ./internal/telemetry/...` alone: `metric`, `otlp`, `otlplog`,
  `otlpmetric` ok.
- `go test -race ./internal/telemetry/... ./internal/metrics/...`: all ok.
- `scripts/check-all.sh`: exit 0 — gofmt, vet, staticcheck, telemetry boundary, race
  tests (`kaiak/e2e` 106.2 s; `metrics`, `telemetry/metric`, `telemetry/otlpmetric`
  and the rest ok), live-test kit lint and self-test, control `npm test` (629 tests,
  628 pass, 1 skipped, 0 fail), lint (`boundaries ok`), cross-half e2e `ok kaiak/e2e
  65.567s`, "all checks passed".
