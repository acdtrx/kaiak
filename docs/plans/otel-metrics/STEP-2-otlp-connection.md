# Step 2 — the OTLP connection

**Status:** not started

## Intent

One OTLP/HTTP connection for every signal, holding the delivery rules the logs
settled, and the log exporter moved onto it inside the telemetry tree. No behaviour
change.

## Files likely touched

- New `gateway/internal/telemetry/otlp/`:
  - settings: the `OTEL_*` reading of `otlplog/settings.go`, made per signal — a
    signal's endpoint (`OTEL_EXPORTER_OTLP_<SIGNAL>_ENDPOINT` as is, else the shared
    one plus `/v1/<signal>`), headers, timeout, protocol (`http/json` only), the
    signal's `OTEL_<SIGNAL>_EXPORTER`, `OTEL_SDK_DISABLED`; `OTEL_SERVICE_NAME` and
    `OTEL_RESOURCE_ATTRIBUTES` once for all;
  - the resource and its JSON encoding;
  - the client (no redirects) and one export: `POST` a body, read the answer by the
    rules (empty or the signal's `Export…ServiceResponse`, the partial-success count
    under the signal's field name — `rejectedLogRecords`, `rejectedDataPoints`,
    `rejectedSpans`), retry with backoff and `Retry-After` within the timeout, and
    report the outcome in the gateway's own words (status, rejected count, failure
    class) — never the collector's text;
  - the shared JSON encoding helpers (`anyValue`, 64-bit integers as strings,
    doubles, `keyValue`).
- `gateway/internal/otlplog/` → `gateway/internal/telemetry/otlplog/`: the handler,
  queue, batching and problem reports stay; settings, client, `post`,
  `readExportResponse`, `retryAfter`, `backoff` and the encoding helpers move to
  `otlp`. `Flush`/`Close` become `ForceFlush(ctx)`/`Shutdown(ctx)` (decision 12),
  same semantics.
- `gateway/cmd/kaiak/main.go` — wiring of the moved package and the per-signal
  settings (logs only so far).
- `scripts/check-gateway.sh` — the boundary check: no package under
  `internal/telemetry/` imports `kaiak/…` other than `kaiak/internal/telemetry/…`
  and `kaiak/internal/netfail` (`go list -deps`).
- Tests move with their code: `otlplog/{settings,exporter,handler}_test.go` split
  between `otlp` and `otlplog`; what they assert is unchanged.

## Decisions made during planning

- `otlp` knows nothing of batching or queues: each signal's exporter batches its own
  way (logs queue records; metrics collect on an interval) and hands `otlp` one body.
- The `log export failing` report stays in `otlplog`: its attributes are the logs'.
  `otlp` returns what the report needs.
- An unknown signal name is a programming error (panic), not a setting.

## Acceptance criteria

- Every test of `otlplog` before the step has a counterpart after it, asserting the
  same (count tests before and after; a dropped one named with its reason).
- The boundary check fails on a planted `kaiak/internal/config` import in
  `telemetry/` (tried once, recorded, reverted).
- `go test ./internal/telemetry/...` passes on its own.
- `scripts/check-all.sh` green. Suite recorded.

## Result

