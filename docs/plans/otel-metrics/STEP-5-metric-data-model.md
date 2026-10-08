# Step 5 — the metric data model

**Status:** done (2026-10-08)

## Intent

The registry becomes OpenTelemetry instruments in `telemetry/metric`: each family
defined by name, unit, description, kind and attribute keys; a collect step producing
the data model; the Prometheus writer translating names. `internal/metrics` registers
its families on it under OpenTelemetry names that translate to today's Prometheus
names wherever the translation allows, so the output barely moves; step 6 renames.

## Files likely touched

- New `gateway/internal/telemetry/metric/` from `internal/metrics/{registry,text}.go`:
  - instruments: counter (monotonic; the scaled counter kept for exact sums of
    sub-units), up-down counter, gauge, histogram (explicit bounds), and the
    read-at-collect forms of counter, up-down counter and gauge;
  - definitions validated at registration (name syntax per the metrics API, unit
    ASCII, attribute keys, buckets);
  - `Collect` → a snapshot in the data model: per family its definition and points
    (attributes, start time, value or histogram counts and sum); the start time is
    the registry's creation, or the series' creation for a series made later;
  - the Prometheus text writer over a snapshot, with the translation: name (chars
    outside `[a-zA-Z0-9_:]` → `_`, runs collapsed), unit suffix (the spec's unit map;
    `{…}` units add none; `x/y` → `_per_<y>`), `_total` on monotonic sums; attribute
    keys the same way; HELP from the description; an empty attribute value left
    out, as today.
- `gateway/internal/metrics/*.go` — every family registered with an OpenTelemetry
  name, unit and kind. Families whose Prometheus name the translation cannot
  reproduce (e.g. `kaiak_output_tokens_per_second` from `kaiak.output_token_rate`)
  change name here, ahead of step 6 — listed in the Result.
- `gateway/internal/server/admin.go` — `/metrics` collects and writes.
- Tests: `metrics_test.go` split — registry and writer tests into
  `telemetry/metric` (no kaiak fixtures), family tests stay.

## Decisions made during planning

- Label keys in code are the OpenTelemetry attribute keys from this step on, even
  where step 6 has not yet renamed them (today's `backend` is registered as
  `backend`; step 6 makes it `kaiak.backend.id`).
- Histograms keep per-bucket counts; the snapshot carries what OTLP needs
  (`bucketCounts`, `explicitBounds`, `count`, `sum`) and the writer accumulates for
  `le`.
- A collect never blocks a recording for more than a map copy: the read-at-collect
  callbacks run once per collect, on the collecting goroutine.

## Acceptance criteria

- A table test of the translation over the compatibility spec's examples and every
  unit the target table uses (`s`, `By`, `{token}/s`, `{request}`, none).
- `/metrics` identical to before but for the families listed in the Result.
- `go test ./internal/telemetry/...` passes on its own; the boundary check holds.
- `scripts/check-all.sh` — expected reds only where a test names a family this step
  renamed early, cleared in step 6; named in the Result. Suite recorded.

## Result

### What changed

- New `gateway/internal/telemetry/metric/` (standard library only):
  - `registry.go` — `Registry`, `Definition`, `Kind`, `Number`; the recorded
    instruments and their validation.
  - `collect.go` — the instruments read at collect, `Callback`/`Observer`, `Collect`
    and the snapshot types.
  - `prometheus.go` — the OpenTelemetry → Prometheus translation and
    `WritePrometheus`.
  - `registry_test.go`, `prometheus_test.go` — the registry and writer tests moved
    from `internal/metrics` (golden exposition, validation, callbacks once per
    collect, concurrency), new: `TestPrometheusName` (table), `TestPrometheusLabel`,
    `TestCollect` (kinds, numbers, start times, histogram buckets),
    `TestCallbacksAreValidated`, `TestPrometheusOrderIsByPrometheusName`; benchmarks.
    No kaiak fixtures.
- `gateway/internal/metrics/registry.go`, `text.go` — removed. The package doc moved
  to `ops.go`.
- `gateway/internal/metrics/{ops,usage,delivery,control,logexport,buildinfo}.go` —
  every family a `metric.Definition` (OpenTelemetry name, unit, description, kind;
  attribute keys still today's labels). The routing group is one `Callback` over six
  `ObservableUpDownCounter`s (`deploymentGauge` → `deploymentState`); the control
  gauges are one callback over five `ObservableGauge`s (`Contact()` read once per
  collect instead of twice); log export one callback over an `ObservableCounter`.
  The usage queue depths are `UpDownCounter`s: `UsageQueueDepth` still reports
  absolute depths, moved by the change from the last report (an atomic swap, so the
  changes add up to the last value reported).
- `gateway/internal/server/admin.go` — `/metrics` is `scrapeHandler(reg)`: one
  `Collect`, `WritePrometheus`, `PrometheusContentType`.
- `gateway/cmd/kaiak/{main,controlplane}.go` — `*metric.Registry`,
  `metric.NewRegistry()`.
- Tests: `internal/metrics/metrics_test.go` keeps the family tests (`text()` collects
  and writes); `server` tests (`server_test`, `admin_test`, `metrics_test`,
  `drain_test`, `queue_test`) and `cmd/kaiak/main_test` on the new API;
  `server/metrics_test.go` and `e2e/zeroseries_test.go` name
  `kaiak_output_token_rate_per_second`.

### API of telemetry/metric

```go
type Definition struct{ Name, Unit, Description string; Attributes []string; Buckets []float64 }
func NewRegistry() *Registry
func (r *Registry) Counter(Definition) *Counter              // Add(uint64, attrs...), Inc(attrs...)
func (r *Registry) ScaledCounter(Definition, divisor float64) *Counter
func (r *Registry) UpDownCounter(Definition) *UpDownCounter  // Add(int64, attrs...)
func (r *Registry) Gauge(Definition) *Gauge                  // Set(float64, attrs...)
func (r *Registry) Histogram(Definition) *Histogram          // Observe(float64, attrs...), Prepare(attrs...)
func (r *Registry) ObservableCounter(Definition) *ObservableCounter             // Observe(o, uint64, attrs...)
func (r *Registry) ObservableUpDownCounter(Definition) *ObservableUpDownCounter // Observe(o, int64, attrs...)
func (r *Registry) ObservableGauge(Definition) *ObservableGauge                 // Observe(o, float64, attrs...)
func (r *Registry) Callback(observe func(*Observer), instruments ...Observable)
func (r *Registry) Collect() Snapshot
type Snapshot struct{ Time time.Time; Families []Family }            // families by OpenTelemetry name
type Family struct{ Definition; Kind Kind; Number Number; Observed bool; Points []Point }
type Point struct{ Attributes []string; StartTime time.Time; Int int64; Double float64; Histogram HistogramValue }
type HistogramValue struct{ BucketCounts []uint64; Count uint64; Sum float64 }
const PrometheusContentType
func WritePrometheus(w *bytes.Buffer, s Snapshot)
```

- Kinds `KindCounter`, `KindUpDownCounter`, `KindGauge`, `KindHistogram`; `Number`
  `Int` (counters, up-down counters) or `Double` (gauges, the scaled counter,
  histogram sums) — what step 9 writes as `asInt` / `asDouble`. `Observed` marks
  the read-at-collect families (the `lowmemory` rule needs it).
- Start times: a recorded series' creation (taken in the slow path that creates it);
  the registry's creation for every series read at collect (its value is counted
  since before any collect saw it); zero for gauges. Stable across collects.
- Validation (panics): name per OpenTelemetry's syntax (letter, then
  `[A-Za-z0-9_./-]`, ≤255); unit printable ASCII without spaces, ≤63; attribute keys
  letter then `[A-Za-z0-9_.]`, no two written as the same label, none written as `le`
  on a histogram; buckets finite and increasing, histograms only; duplicate
  OpenTelemetry name or duplicate Prometheus translation; wrong number of attribute
  values (recording or observing); an instrument read by two callbacks, from another
  registry, or observed outside its callback.
- `Callback` is the step-4 group generalised: one callback, any read-at-collect
  kinds, run once per collect on the collecting goroutine.

### Translation verified

Against `github.com/prometheus/otlptranslator`: its source read on `main`, then every
row of `TestPrometheusName` and `TestPrometheusLabel` run through the released
v1.0.0 (`MetricNamer` with `UnderscoreEscapingWithSuffixes`, `LabelNamer{}`) in a
throwaway module outside the repo — all equal. What that settled:

- `{token}/s` → `_per_second`; `By` → `_bytes`; a unit in braces adds nothing;
  `1/s` → `_per_second`; `By/{batch}` → `_bytes`.
- A unit word is skipped when the name has it as a word **anywhere**, not only at
  the end (`seconds.spent` [s] → `seconds_spent_total`) — Prometheus's rule, stricter
  than the specification's "ends with".
- The per-unit word is never found in the name: words are split at `_`, so
  `tokens_per_second` [`{token}/s`] → `tokens_per_second_per_second`.
- `total` (counters) and `ratio` (gauges with unit `1` only — not up-down counters)
  are moved to the end if the name has them elsewhere.
- Unit map as released: `TiBy` → `tibibytes`, `kBy` unmapped (written `kBy`).
  Unreleased `main` changes both (`tebibytes`, `kBy` → `kilobytes`); kaiak uses
  neither.
- Label names: runs of characters outside `[a-zA-Z0-9]` → one `_` (so `a__b` →
  `a_b`), a trailing one kept.

### Families renamed early

- `kaiak_output_tokens_per_second` → `kaiak_output_token_rate_per_second`
  (`kaiak.output_token_rate`, `{token}/s`). The only one: every other family keeps
  today's Prometheus name, under the target OpenTelemetry name where it translates
  identically and a temporary one otherwise (`kaiak.request.duration`,
  `kaiak.queue.wait`, `kaiak.backend.in_flight_requests`,
  `kaiak.backend.max_in_flight`, `kaiak.queued_requests`, `kaiak.circuit.open`,
  `kaiak.circuit.half_open`, `kaiak.usage.queued_bytes`, `kaiak.log_export.records`,
  `kaiak.usage.tokens`) for step 6 and 7 to replace. (`kaiak.output_tokens` with
  `{token}/s` would also have kept the old name; the brief chose the target.)
- No HELP text changed. TYPE lines unchanged (up-down counters write `gauge`).

### Byte-identity

A temporary test built the registry as `main` does (build info, log export, control
state, circuits + router observer, ops, usage, usage delivery) over a two-backend,
two-model config and fed every family (config loads applied/rejected, limits sync,
requests, first token, output rate, errors, request errors with and without a key,
limit rejection, queue wait and rejection, retry, attempts, probe, an opened
circuit, a slot in flight, usage records complete/partial, clamped, batch send,
two queue depths, drops). Scrape before: 645 lines, deterministic over two runs.
After: `diff` shows only the 41 lines of the output-rate family, renamed in place;
everything else identical. The test was removed.

### Benchmarks (`-count 5`/interleaved ×9, Apple M5 Max, ns/op, allocs/op)

| | before | after |
|---|---|---|
| Counter Inc, 3 attributes | 24.4–25.4, 1 alloc (16 B) | 25.3–26.3, 1 alloc (16 B) |
| Counter Inc, none | 6.25–6.45, 0 | 6.34–6.44, 0 |
| Histogram Observe, 2 attributes | 23.2–24.4, 1 alloc (8 B) | 23.8–24.6, 1 alloc (8 B) |
| Histogram Observe, parallel | 98–102, 1 alloc | 99–108, 1 alloc |

Same mechanics (join key, read-locked map lookup, atomics), no allocation added.
Inc with attributes measures about 1 ns slower in the interleaved run, within the
run-to-run spread; a field reorder made no difference. The allocation is the
`strings.Join` key, as before.

### Suite

- `go test -race ./internal/telemetry/...` alone: `metric`, `otlp`, `otlplog` ok.
- `scripts/check-all.sh`: exit 0 — gofmt, vet, staticcheck, telemetry boundary,
  race tests (`kaiak/e2e` 104.3 s; `metrics`, `server`, `cmd/kaiak`,
  `telemetry/metric` ok), live-test kit, control `npm test` (629 tests, 628 pass,
  1 skipped, 0 fail), lint (`boundaries ok`), cross-half e2e `ok kaiak/e2e
  69.338s`, "all checks passed". No expected reds: the two tests naming the output
  rate were updated here.
