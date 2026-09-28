# Step 4 — retries

**Status:** done (2026-09-24) — suite green; **phase 2 complete** (steps 2–4: the
gateway caps, queues, breaks circuits, probes and retries, all tested)

## Intent

Retry failed attempts before the first byte on another deployment of the same model,
with exact accounting across attempts.

## Files likely touched

- `gateway/internal/server/` (routing/provider stages, relay), `routing/`,
  `accounting/`, `limits/`

## Decisions made during planning

- Attempt loop inside the pipeline's routing+provider stages (no side door): attempts
  up to `max_attempts` (model override or global); retry on connect error, first-byte
  timeout, backend 5xx, backend 429; each retry excludes deployments already tried when
  another is eligible, and never retries a deployment that answered 429.
- Each attempt takes its own slot/in-flight count and feeds the circuit breaker.
- Usage: one reservation for the client request; a first-byte-timeout attempt emits
  its own record (estimated input tokens, no output, `estimated` + `partial`, same
  request ID, attempt number in the log); the answering attempt's record as today;
  connect/5xx/429 attempts emit none. Limits settle the sum.
- The final failure (all attempts failed) answers with the last attempt's error.
- Log line: attempt count and the deployments tried.

## Acceptance criteria

- Tests: success on second deployment; single-deployment retry; 429 not retried on the
  same deployment; caller 4xx not retried; no retry after first byte; timeout attempt
  record + answer record, limits settled to their sum; all fail → last error;
  client disconnect mid-retry.
- Phase 2 end: `scripts/check-all.sh` green.

## Decisions made during implementation

- **Circuit adjustments (with this step)**: a client gone — or the drain's cut —
  *after* the first event leaves the backend's status to speak: a response under way
  is a success (the backend proved it serves), a relayed `5xx` stays a failure;
  before the first event it stays neutral. The drain's cut is treated like the client
  leaving (same reasoning; the brief named only the client). Metric label: the
  backend-side model on `kaiak_circuit_open` and `kaiak_circuit_transitions_total`
  is `deployment_model`, as on the log line.
- **Attempt loop**: the pipeline's routing, accounting and provider stages became one
  stage, `attempts` (`server.sendAttempts`): per attempt `Acquire` → meter → `Send`
  → `retryReason`. Limits stay one stage before it (one reservation, counted once).
  A backend error status is **held** (the provider has read its first event, nothing
  is written) while the retry looks for a slot: relayed as is when none is found, so
  "the last attempt's error" is always exact, `Retry-After` included. A retried
  attempt reports its outcome and frees its slot before the next `Acquire` (a
  capped single deployment can then retry on itself); the last attempt is ended by
  one request finisher (settle, report, release).
- **Retry decision** (`server.retryReason`): connect/lost before first event,
  first-byte timeout, `5xx`, `429`, and `401`/`403` — the last refusing every
  deployment of the model on that backend for the request (the credential is per
  backend; another backend may serve; a single-backend model answers at once).
  Not: a relayed response (`2xx`, other `4xx`), client gone, drain cut, gateway fault.
- **Routing API**: `Acquire(ctx, m, avoid routing.Avoid)` — `Avoid{Tried, Refused}`
  (the zero value is a first attempt). Usable = eligible, not refused, untried when
  any untried usable exists; recomputed at each dispatch. A retry queues like a new
  request (back of the queue, same size/timeout, wait counted) but never for a
  deployment it may not use: nothing usable → `ErrNoHealthyDeployment` at once. The
  dispatcher now serves the longest-waiting request *that can use* a free slot
  (scanning past a queued retry that avoids it), so no usable slot stays free.
- **Retry without a slot** (queue full/timeout, no deployment left): the last
  attempt answers; logged `retry_refused` (`queue_full`, `queue_timeout`,
  `no_deployment_left`); queue refusals still count in
  `kaiak_queue_rejections_total`. Client gone / drain cut between attempts or in the
  retry's queue wait: `client_closed` / `server_shutting_down`, no further attempt.
- **Accounting**: `Meter.TimedOut()` — a first-byte-timeout attempt settles to the
  estimated input, no output, `estimated` + `partial`, **retried or not** (the
  settled "usage across attempts" rule replaces "no units" for the timeout
  everywhere, so a final timeout bills the same as a retried one). Records: one per
  retried timed-out attempt plus the last attempt's (always one). `rq.records` holds
  them; `limits.Limiter.Settle(res, recs...)` settles the sum (variadic; no record
  = release). The usage-metrics sink gets each record, so
  `kaiak_usage_requests_total` counts records. CONTROL-PROTOCOL notes a request ID
  may appear on several records (no wire change).
- **Log line**: `attempts` (routed requests), `tried` when > 1
  (`backend/deployment_model:outcome,…`), `retry_refused`; token and cost fields are
  the sum of the request's records, `estimated`/`partial` the last record's.
- **Metrics**: `kaiak_retries_total{model,reason}` (reasons `unavailable`,
  `timeout`, `server_error`, `rate_limited`, `auth_failed`; counted per retry sent),
  `kaiak_request_attempts{model}` histogram (buckets 1–10).
- **Existing tests adapted to retries** (behavior changed by design, not weakened):
  circuit tests run with `max_attempts: 1` (each request one outcome); the
  first-byte-timeout test expects 3 attempts, each cancelled; the "slot handed on a
  backend error" queue test now sees the holder's retry queue behind the waiting
  request and succeed (order holder, queued, holder).

## Result

- Tests: `server/retry_test.go` — success on the second deployment after connect
  refused / 500 / 429 / first-byte timeout / 401 on the first (log `tried`, records,
  request counted once, reservation settled, retry and attempts metrics); timed-out
  attempt record (estimated input, partial, its deployment) + answer record, limits
  settled to their sum, request count 1, usage metrics per record; single deployment
  retried on 500 and on connect errors (3 attempts, one record); all fail → the third
  attempt's 503 relayed with its `Retry-After`; not retried: single-deployment 429
  (`retry_refused=no_deployment_left`), single-backend 403, caller 400, cut after the
  first event; max attempts global 2, per-model 4, 1; circuit fed per attempt
  (threshold reached through retries, then one attempt); a tried deployment reused
  when the other is open; retry queued for a capped backend (served when the slot
  frees; client gone while waiting → 499, one zero-unit record, nothing in flight;
  queue timeout → the first attempt's 500 relayed, `retry_refused=queue_timeout`,
  rejection counted); drain cut while a retry waits. `server/circuit_test.go` —
  classification table now tells failure / neutral / success apart (one failure
  primed, threshold 2). `routing/retry_test.go` — untried preferred, refused never
  used (all refused → `ErrNoHealthyDeployment` without queueing), tried reused when
  the others are open, a queued retry lets a request behind it take a slot it may
  not use. `accounting` timed-out meter; `limits` settlement over several records;
  `metrics` retries and attempts.
- `go test -race -count=5 ./internal/routing ./internal/server ./internal/accounting
  ./internal/limits`: ok.
- `scripts/check-all.sh`: gofmt, vet, staticcheck, `go test -race` (all packages,
  e2e included), live-test kit, `npm test` (379 pass), lint, cross-half e2e —
  `all checks passed`.
