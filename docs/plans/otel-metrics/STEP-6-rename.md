# Step 6 — the rename

**Status:** not started

## Intent

Every family but the usage ones takes its name, unit and attributes from the
`GATEWAY.md` table (step 1), and everything that reads metric names follows.

## Files likely touched

- `gateway/internal/metrics/{ops,delivery,control,logexport}.go` and the build info.
- `gateway/internal/server/metrics.go` — the request duration's attributes: method
  (the log line's `methodAttrs` rule; `_OTHER`), `url.scheme`, `http.route` (the
  matched route: a body endpoint's `route()`, `/v1/models`, `/v1/models/{model}`,
  `/v1/models/{model}/props`; absent for an unknown path), the status code (absent
  when none was sent), `error.type` (decision 14).
- The circuit state family (decision 16) from the routing picture.
- The log exporter's counts as `otel.sdk.exporter.log.exported` and
  `otel.sdk.processor.log.processed` (decision 9), with `otel.component.*`.
- Tests that read metrics: `server/*_test.go`, `e2e/*_test.go`, `cmd/kaiak`,
  `metrics_test.go`; `scripts/live/{checks,twobackends}.go`;
  `scripts/smoke-images.sh`, `scripts/build-images.sh`.
- Docs naming metrics outside the spec: `docs/DEPLOYMENT.md` (alerts, dashboards),
  `docs/testing/LIVE-BACKENDS.md`, `docs/ARCHITECTURE.md`,
  `docs/architecture/gateway.html`, `docs/TECH-STACK.md`.

## Decisions made during planning

- The route attribute is the route as the client API documents it, not the request
  path: `{model}` stays literal.
- Series at 0 unchanged in scope (the spec's list), in the new names; the request
  duration still created on first use.

## Acceptance criteria

- Grep checklist, outside `docs/plans` and `docs/reviews`, `git grep -P` for each old
  name and label that changed (`kaiak_request_duration_seconds`,
  `kaiak_output_tokens_per_second`, `kaiak_backend_in_flight_requests`,
  `kaiak_backend_max_in_flight`, `kaiak_queued_requests`, `kaiak_queue_wait_seconds`,
  `kaiak_circuit_open`, `kaiak_circuit_half_open`, `kaiak_log_export_records_total`,
  `kaiak_usage_queued_bytes`, `\bstatus_class\b`, `\bscope_kind\b`,
  `\bdeployment_model\b` as a label, `\bgo_version\b`): no hit where a metric is
  meant.
- A test over a full scrape: every family and label in the `GATEWAY.md` table and
  nothing else (usage families excepted until step 7).
- `scripts/check-all.sh` — usage-metric reds only, cleared in step 7; named. Suite
  recorded.

## Result

