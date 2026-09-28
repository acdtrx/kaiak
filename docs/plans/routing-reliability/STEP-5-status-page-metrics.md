# Step 5 — status, page and metrics

**Status:** done

## Intent

Report reliability state to the control plane and show it; complete the metrics.

## Files likely touched

- `gateway/internal/control/` status reporter; `control/kaiak-control` status storage;
  `control/sample/src/page/` gateways section; `gateway/internal/metrics/`

## Decisions made during planning

- Status carries the step-1 fields; a circuit change or a queue appearing/emptying
  triggers a status send (on change), queue depth otherwise on the 10 s cadence.
- Page: per gateway, backends with in-flight/queued and cap, deployments with circuit
  state (open highlighted, opened_at).
- Metrics: `kaiak_circuit_open{backend,deployment_model}`, queue depth per backend, queue wait
  histogram, queue rejections by reason, attempts per request / retries total by reason.

## Acceptance criteria

- Tests on both halves; page renders the new fields escaped; metric names in the
  GATEWAY.md table.

## Decisions made during implementation

- **Status minimum gap** (GATEWAY.md, Status minimum gap): `ServingChanged` (queue
  empty↔non-empty, circuit open/close) sends at once when the last report is ≥ 1 s old
  (`Options.StatusMinGap`, default 1 s), else arms one timer to the gap's end; that one
  report reads the state then. Other triggers (start, connect, config applied or
  rejected, drain, interval) are never held, and any report disarms a pending gap —
  it already carries the latest state. The gap counts from the last report of any
  trigger. Routing-change reports log `trigger=serving`. The reporter's clock and gap
  timer are unexported options, replaced in tests.
- **`kaiak_usage_requests_total` renamed `kaiak_usage_records_total`**: with retries a
  request can settle several records. Client requests are counted by
  `kaiak_request_duration_seconds_count` (observed once per request in
  `observeRequest`, whatever its attempts) — now stated in the metrics table and
  asserted in the retry test (two records, one request count).
- **Page**: the gateways table's "In flight" column became "Serving" — a one-line
  summary (in flight, queued, circuits open; "idle" when all zero). Under the table,
  per gateway: a backends table (ID, `in flight / cap` with `—` when uncapped,
  deployments sorted, an open circuit flagged with when it opened, relative and
  absolute; a closed one plain; "none in the applied config" for a backend a reload
  dropped) and a "Queued" line listing only models with requests waiting. Everything
  derived in the page; `kaiak-control` unchanged.
- Metrics checklist: every reliability family already existed with the settled names
  (`kaiak_circuit_open` / `kaiak_circuit_transitions_total` with `deployment_model`,
  `kaiak_probes_total`, `kaiak_queued_requests`, `kaiak_queue_wait_seconds`,
  `kaiak_queue_rejections_total`, `kaiak_backend_max_in_flight`, `kaiak_retries_total`,
  `kaiak_request_attempts`) and in the GATEWAY.md table; the admin-port test now
  checks each family is exposed.

## Result

- Tests: `control/status_test.go` — a burst of five routing changes inside the gap
  arms one timer for the gap's remainder, sends nothing until it ends, then one report
  with the latest queue depth; a change after the gap goes at once without a timer;
  three reports in all. A drain inside an hour-long gap is reported at once and
  carries the pending routing change. `server/retry_test.go` — the timed-out attempt
  and the answer are two usage records, one `kaiak_request_duration_seconds_count`.
  `server/metrics_test.go` — every reliability family exposed; no
  `kaiak_usage_requests_total`. Sample `page.test.ts` — backends with caps (`3 / 4`,
  `1 / —`), sorted, an open circuit with `opened_at` relative and absolute, closed
  plain, queued models only when non-zero, a dropped backend, the summary line;
  escaping of a backend ID, a deployment model and a model name. `events.test.ts` —
  a status with an open circuit and a queue, then a recovered status, each pushed live
  to the gateways section. Live-test kit metrics check reads
  `kaiak_usage_records_total` (self-test green for all three kinds).
- `go test -race -count=20 -run 'Status|Serving|Drain' ./internal/control`: ok.
- `scripts/check-all.sh`: gofmt, vet, staticcheck, `go test -race` (all packages,
  e2e included), live-test kit self-test (13 passed ×3), `npm test` (380 pass), lint,
  cross-half e2e — `all checks passed`. No expected reds.
