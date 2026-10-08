# Step 12 — review and green

**Status:** not started

## Intent

The plan is checked by an independent review and closed out.

## Files likely touched

- Fixes from the review, each with its test.
- `docs/BACKLOG.md` — OpenTelemetry export → Metrics removed (built); Traces updated
  (the dependency ruling allows the trace SDK core; the shared connection exists); a
  new entry for Go runtime and process metrics with its revisit trigger.
- `docs/reviews/2026-10-07-structure/STRUCTURE.md` → Outcome: F3, F4, F5, F8, F10
  and T12 marked done with their steps.
- `docs/plans/otel-metrics/OVERVIEW.md` — verification status.

## Decisions made during planning

- The independent review runs as before (a plain copy, Codex), over the branch diff
  against the anchor tag.

## Acceptance criteria

- Every review finding fixed or answered in the review file.
- `scripts/check-all.sh` green 3× in a row. Suite recorded.

## Result


### Fixes

- **`http.response.status_code` typed int in the OTLP metric export** (step 11,
  finding 1). It went out as `stringValue "200"`; the HTTP convention and the log
  export type it as an int, so a collector joining logs and metrics saw two types.
  - `gateway/internal/telemetry/metric/registry.go` — `Definition.AttributeTypes`
    (`map[string]AttributeType`; `StringAttribute` the default, `IntAttribute`),
    carried in the snapshot's `Family`. Values stay strings on the recording path
    and in the collect. Registration panics on a type for a key not among the
    attributes or an unknown type. An int key's value must be "" or a canonical
    decimal integer (`strconv.FormatInt`'s form, so label and typed value agree),
    checked when a recorded value first makes a series (the slow path, once per
    series) and at every observation; anything else panics, as a wrong number of
    values already does. Chosen over checking at encode: the error shows where the
    bad value is recorded, and the encoder never has to pick between dropping a
    point and sending the wrong type. Safe on the request path: the status code is
    `strconv.Itoa` of the status the gateway answered, never client input.
  - `gateway/internal/telemetry/otlpmetric/encode.go` — an `IntAttribute` value is
    written with `otlp.IntValue` (`intValue`, a decimal string); the rest
    `stringValue`. Prometheus output unchanged.
  - `gateway/internal/metrics/ops.go` — `http.server.request.duration` types
    `http.response.status_code` as `IntAttribute`. Checked against the conventions
    and the log export's typed keys (`slog.Int`/`Int64`/`Bool`/`Float64`/…): every
    other metric attribute (`http.request.method`, `url.scheme`, `http.route`,
    `error.type`, `gen_ai.*`, `service.version`, `process.runtime.version`,
    `otel.component.*`, every `kaiak.*`) is a string in both; no boolean or double
    attribute on any metric.
  - `docs/specs/GATEWAY.md` → OTLP metric export, data model: attributes have the
    log export's types — `http.response.status_code` an `intValue`, every other a
    `stringValue`.
  - Tests: `metric.TestAttributeTypesAreValidated` (bad definitions; "", "200",
    "-1" taken; "2xx", "+200", "0200", " 200", "1e3", an overflow panic; an existing
    series not rechecked; a string key takes any value; an observed int value
    checked; types and string values in the collect). `otlpmetric`: the round-trip
    decoder reads `intValue` and compares which keys were sent as ints with the
    family's types (`everyKind`'s counter gains a status code, one point without
    it); new `TestIntAttributeIsIntValue` (status code `intValue` "200"/"504",
    route `stringValue`, the empty one left out). Mutation (encoder writing
    strings) fails both. `e2e/metricexport_test.go`: the pushed-vs-scraped
    comparison requires `intValue` for `http.response.status_code` and
    `stringValue` for every other attribute. `fakeotlp` already decoded `intValue`:
    unchanged.
  - Suite: `go test -race ./internal/telemetry/... ./internal/metrics/... ./e2e/...`
    all ok (`kaiak/e2e` 114.6 s). `scripts/check-all.sh`: exit 0 — gofmt, vet,
    staticcheck, telemetry boundary, race tests (`kaiak/e2e` 119.2 s), live-test kit
    lint and self-test, control `npm test` (629 tests, 628 pass, 1 skipped, 0 fail),
    lint (`boundaries ok`), cross-half e2e `ok kaiak/e2e 65.412s`, "all checks
    passed".
- **The export's bound covers its collect and encoding** (independent review,
  finding 1). Mechanism confirmed: `export` collected, applied the temporality and
  encoded every request before starting `context.WithTimeout`, and neither
  encoding nor collect looked at a context, so a final export's ForceFlush
  returned on time but `Shutdown` waited for the encoding to end. Measured at
  100,000 series (no race / `-race`): Collect 30 ms / 170 ms, delta transform
  10 ms / 60 ms, encoding 75 ms / 1.5 s.
  - `gateway/internal/telemetry/otlpmetric/exporter.go` — the export's timeout
    (and ctx: ForceFlush's, Shutdown's stop) starts before the collect. An
    encoding cut by it counts every point of the collect failed with
    `error.type=timeout` ("export cut short"), as an export whose time ran out
    during delivery does; an encoding error is still `_OTHER`.
  - `gateway/internal/telemetry/otlpmetric/encode.go` — `encodeRequests` takes
    ctx and looks at it at each stream's start and every 1,024 points (about a
    millisecond of work). Requests after a cut need nothing: `otlp.Client.Export`
    on an ended ctx fails at once as a timeout.
  - Not cut: Collect (the `metric` package's, shared with the scrape; tens of ms
    at 100,000 series, the floor at exit is 1 s) and the temporality transform —
    so the delta state moves on whole before encoding, and an export cut while it
    encodes loses its deltas exactly as any failed delta export does, the next
    delta starting at its time (consistent with `TestDeltaLostWhenAnExportFails`).
    Recording unchanged.
  - `docs/specs/GATEWAY.md` → OTLP metric export, interval and timeout: the bound
    names the collect, the encoding, the requests and their retries.
  - Tests (`otlpmetric/exporter_test.go`, 100,000 series): ported
    `TestExportTimeoutCoversEncoding` (a 10 ms export timeout: no request past
    100 ms, all 100,000 points failed as `timeout`) and new
    `TestExitCutsAnExportWhileItEncodes` (a 20 ms ForceFlush then Shutdown, as at
    exit, end in under half a whole export's time; points failed as `timeout`).
    Both failed before the fix in both modes (first request at 113 ms / 1.65 s;
    exit 98 ms of 111 ms / 1.65 s of 1.68 s). After: exit 27 ms of 110 ms / 165 ms
    of 1.68 s — what remains is Collect.
- **No point starts after its collect's time** (independent review, finding 2).
  Mechanism confirmed: `Collect` took its time before the callbacks and the
  series-map copies, so a series created in between was carried with a later
  start (the ported test: 3.4 µs after). `gateway/internal/telemetry/metric/collect.go`
  takes the snapshot's time once every family's series are read. Chosen over
  clamping: a clamped start would move between exports (a reset to a cumulative
  backend) and misdate the series; over excluding late series: more code for the
  same result. Delta intervals stay contiguous: the reader uses one snapshot time
  as both an export's end and the next delta's start, wherever in the collect it
  is taken. `docs/specs/GATEWAY.md` → data model says so. Test: ported
  `metric.TestSeriesCreatedDuringACollectStartsBeforeItsTime` (a callback
  barrier creates the series mid-collect), failed before, passes.
- **Histogram count/sum coherence while recording** (independent review,
  inherited limitation): accepted, no change — metrics are approximate by design
  (docs/kaiak.md, principle 7) and a coherent read would add a lock or a hot/cold
  swap to every observation.
- Suite: ported tests `-race -count=5` ok (and `-count=5` without race);
  `go test -race ./internal/telemetry/... ./cmd/kaiak/...` all ok.
- `scripts/check-all.sh` 3× in a row, each exit 0 — gofmt, vet, staticcheck,
  telemetry boundary, race tests, live-test kit, control `npm test` (629 tests,
  628 pass, 1 skipped, 0 fail), lint (`boundaries ok`), cross-half e2e, "all
  checks passed":
  - run 1: `ok kaiak/e2e 121.708s`, cross-half `ok kaiak/e2e 65.337s`;
  - run 2: `ok kaiak/e2e 117.289s`, cross-half `ok kaiak/e2e 66.154s`;
  - run 3: `ok kaiak/e2e 119.750s`, cross-half `ok kaiak/e2e 65.354s`.
