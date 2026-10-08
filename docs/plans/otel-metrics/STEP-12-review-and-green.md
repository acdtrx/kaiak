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
