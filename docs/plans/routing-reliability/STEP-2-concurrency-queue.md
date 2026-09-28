# Step 2 — concurrency cap and queue

**Status:** done (2026-09-24) — suite green (no expected reds)

## Intent

Enforce `max_in_flight` per backend with a gateway-held, bounded, timed queue, and
make routing prefer deployments whose backend has a free slot.

## Files likely touched

- `gateway/internal/routing/` — slots per backend, queue, choice with capacity
- `gateway/internal/server/` — error table (`queue_full`, `queue_timeout`), drain
- `gateway/internal/metrics/` — queue depth, wait time, rejections

## Decisions made during planning

- Slots belong to the backend (endpoint) and outlive config reloads for backends that
  still exist; a lowered cap applies to new admissions.
- Choice: among the model's healthy deployments (circuit step 3; all healthy for now),
  least in flight with a free slot; if none has a slot, the request waits in its
  **model's queue** (FIFO; settled 2026-09-24) and is served by whichever of the
  model's backends frees a slot first. A freed slot on a backend serving several
  models goes to the longest-waiting request across those models' queues.
- A queued request holds its limits reservation (taken before routing) and releases it
  on queue rejection; client disconnect while queued removes it at once.
- Drain: queued requests are served if slots free within the drain timeout, else cut
  with the drain's refusal.

## Acceptance criteria

- Tests: cap respected under concurrency; queue order; full → 429 at once; timeout →
  429; disconnect leaves the queue; slots released on every relay end; drain with a
  queue; reload keeps slots.

## Decisions made during implementation

- **Dispatch**: one dispatcher in `routing`, run under the router's mutex whenever a
  slot may have freed (a release, `Configure`): it gives each free slot to the
  longest-waiting queue head whose model can use it (arrival numbers across all
  models), taking the slot *for* that waiter and sending it the deployment on a
  1-buffered channel. Waiters block on that channel, a timer (the model's
  `Queue.Timeout`) and the request context — no goroutine per request, no polling,
  no herd, no lost wake-up (a grant racing a timeout is served; a grant racing a
  disconnect is released and dispatched on). New requests never jump waiters: after
  every dispatch no waiter can use a free slot.
- **API**: `Router.Acquire(ctx, m) (Slot, Wait, error)` replaces `Acquire(m)`;
  `Slot.Release` is the finisher; `Wait{Queued, Duration}` feeds the log and
  metrics; errors `ErrQueueFull`, `ErrQueueTimeout`, or the context's. The existing
  routing tests keep their behavior; their call sites go through a small helper.
  Eligibility seam for step 3: `Router.eligible(d)` (every deployment for now), used
  by both the choice and the dispatcher.
- **Caps in force**: `Router.Configure(snapshot)` takes each applied config's caps
  (wired in `cmd/kaiak` through the applier's load observer); a backend the last
  config does not name keeps the cap of the snapshot its request carries. A raised
  cap dispatches at once. Queues are keyed by public model name, so waiters from an
  older snapshot share the queue.
- **Errors**: `429 queue_full` / `429 queue_timeout`, type `server_error`
  (platform capacity, not the caller's `requests`/`tokens` limits); `queue_full`
  with `Retry-After: 1`, `queue_timeout` without. Error class `queue_rejected`
  (platform-side). Recorded in `GATEWAY.md`.
- **Metrics**: `kaiak_backend_max_in_flight{backend}` (capped backends only),
  `kaiak_queued_requests{model}` (every configured model, scrape time),
  `kaiak_queue_wait_seconds{model}` (waits that got a slot),
  `kaiak_queue_rejections_total{model,reason=full|timeout}`.
- **Status**: `servingStatus` reports real queue depths (models a reload dropped while
  requests wait are listed too); `Router.OnQueueChange` fires when a model's queue
  goes empty ↔ non-empty and calls the new `control.Client.ServingChanged`, which
  requests a `change` report (coalesced like the others). Recorded in
  `CONTROL-PROTOCOL.md`.
- **Log**: `queue_wait_ms` on every request that entered a queue.
- **Drain**: nothing new needed — the drain's in-flight count covers the whole
  handler, so queued requests keep it waiting, are served if slots free, and at the
  cut leave with `503 server_shutting_down` (cause `errDrainCut`), no record,
  reservation released.

## Result

- Tests: `routing/queue_test.go` — cap under concurrency (10 requests, cap 3, peak
  exactly 3, each release admits one), FIFO, longest-waiting across two models on a
  shared backend (and a slot on a backend only one model uses skips the other),
  size 0 and full refused at once, timeout, leaving the queue, lowered/raised cap via
  `Configure`, empty↔non-empty hook. `server/queue_test.go` — queue order through
  the pipeline, `queue_full` with `Retry-After`, `queue_timeout` after the model's
  timeout, no records and reservations released for refused/left requests, log
  field, metrics, slot handed on success / client disconnect / upstream cut
  mid-stream / backend error (first-byte timeout), drain served and drain cut.
  Metrics, `servingStatus` and `ServingChanged` tests extended. Existing routing and
  server tests unchanged in behavior and green.
- `scripts/check-all.sh`: gofmt, vet, staticcheck, `go test -race` (all packages,
  e2e included), live-test kit, `npm test` (379 pass), lint, cross-half e2e —
  `all checks passed`.
