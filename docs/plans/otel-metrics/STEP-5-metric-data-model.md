# Step 5 — the metric data model

**Status:** not started

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

