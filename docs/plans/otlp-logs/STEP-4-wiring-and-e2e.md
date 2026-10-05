# Step 4 — wiring and e2e

**Status:** not started

## Intent

Turn the exporter on in the gateway when configured, flush it in the drain, expose
its metric, and prove it end to end — against a fake collector in the suite and a
real OpenTelemetry Collector once by hand. This step ends the phase.

## Files likely touched

- `gateway/cmd/kaiak/main.go`: read the OTLP settings with the other settings;
  build the logger after them (a start failure still prints to stderr), wrapping the
  stderr handler when export is on; one `kaiak starting` attribute saying whether
  export is on and to which endpoint host (never headers).
- The drain: flush the exporter after usage, within the flush reserve.
- `gateway/internal/metrics`: `kaiak_log_export_records_total{outcome}`, read from
  the exporter's counts.
- `gateway/e2e`: a gateway with a fake collector — request lines (success, a limit
  refusal, a `401`) and boot events arrive with the stderr attributes and the
  resource; with no endpoint, the collector sees nothing.
- `docs/plans/otlp-logs/OVERVIEW.md`: verification status.

## Decisions made during planning

- The live check uses the `otel/opentelemetry-collector` image with the `otlp`
  receiver (HTTP, port 4318) and the `debug` exporter (`verbosity: detailed`), run on
  the user's Docker context; its output is pasted into this step's Result.

## Acceptance criteria

- e2e as above; the metric moves with exports and drops.
- `scripts/check-all.sh` green three times in a row, Go test cache cleared before
  each.
- The live check shows request lines with their attributes and the resource
  (`service.name`, `service.instance.id`); container and scratch files removed after.
- Status recorded in every step file; the phase is committed.

## Result

_Not started._
