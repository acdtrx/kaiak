# Step 3 — circuit breaker and probes

**Status:** done (2026-09-24) — suite green (no expected reds)

## Intent

Take failing deployments out of rotation and probe them back in.

## Files likely touched

- `gateway/internal/routing/` — circuit state per deployment, eligibility
- `gateway/internal/provider/` — the `GET …/models` probe (only providers talk to
  backends)
- `gateway/internal/server/` — failure classification fed from relay outcomes
- `gateway/internal/metrics/`

## Decisions made during planning

- Consecutive failures (connect, timeouts, 5xx) ≥ threshold → open; success resets
  the count; 429 and caller 4xx are neutral.
- Open deployments are skipped by routing. All of a model's deployments open → `503`
  (existing "no healthy backend" semantics; pick the code and add it to the table).
- A prober per open deployment (owned goroutine, stops on close/reload removal)
  probes every `probe_interval_ms`; success → closed. Probe results logged.
- State per deployment outlives config reloads for deployments that still exist.

## Acceptance criteria

- Tests: opens at threshold, not before; neutral classes don't count; routing skips
  open; probe closes; all open → 503; prober lifecycle (no leaks on reload/shutdown).

## Decisions made during implementation

- **Classification** — one function, `server.circuitOutcome`, run in the routing
  finisher when the request is over, reported on the slot (`Slot.Report`, before
  `Release`). Failure: connect error / lost before the first event, first-byte
  timeout, backend 5xx, backend 401/403 (`upstream_auth_failed` — the gateway's
  credential; every request would fail the same way, so the circuit opens and stops
  hammering), a response broken off upstream after the first event (the backend
  broke). Neutral: backend 429, other backend 4xx, client gone (before or after the
  first event), the drain's cut, a gateway fault building the request. Success: a
  response relayed to its end with a status below 400. Table in `GATEWAY.md`.
- **Open circuits** ignore request outcomes (requests in flight when it opened may
  still report); only a probe closes. Outcomes on deployments the applied config does
  not have are not counted.
- **All open** → new requests `503 no_healthy_deployment` (type `server_error`,
  error class `no_healthy_deployment`, platform-side) at once, never queued, no
  record. Queued waiters keep waiting until their timeout; the dispatcher serves them
  when a probe closes a circuit.
- **Probers per backend** (the probe is per backend): started when a backend's first
  circuit opens; a success closes every circuit of the backend open when the probe
  started, then the prober stops; also stops when a reload drops the backend's last
  open deployment and at shutdown. `Router.RunProbers(ctx)` owns the goroutines and
  returns once they stopped; `cmd/kaiak` runs it with the other background work
  (stopped after the drain). `Router.ProbeNow(ctx, backend, trigger)` is the
  mechanism; the timer calls it with trigger `interval`. Probe cut short by shutdown:
  neither counted nor logged.
- **Probe** (`provider.Registry.Probe`): `GET <API prefix>/models` with the backend's
  credential over its pool, bounded by connect timeout + 5 s; success = 2xx.
  `routing` gets it as a `ProbeFunc` in `routing.Options` — routing never talks to a
  backend.
- **API**: `routing.New(Options{Probe, Observer, Logger})`; `OnQueueChange` became
  `OnServingChange` (queue empty↔non-empty, circuit open/closed) → `ServingChanged`;
  `DeploymentID` exported (the key of counts and circuits); `OpenCircuits()`.
  Metrics get transitions and probe results through `routing.Observer`
  (`metrics.Circuits`); `kaiak_circuit_open` is read at scrape time.
- **Metric labels**: `kaiak_circuit_open` and `kaiak_circuit_transitions_total` carry
  the deployment's identity — `model` is the model name on the backend (a backend
  model may serve under several public names).
- **Logs**: `circuit opened` (warn, with `last_error`), `circuit closed`,
  `probe succeeded` (info), `probe failed` (info for the first after opening, debug
  for repeats).
- **fakebackend**: `QueueReplies` (a scripted sequence before the standing reply),
  models list at `…/models` with `SetModelsStatus`, `ModelsRequests` (kept apart
  from `Requests`).

## Result

- Tests: `routing/circuit_test.go` — opens exactly at the threshold (observer and
  serving-change hook told once), outcomes on an open circuit change nothing, success
  resets, neutral neither counts nor resets, 1000 rounds of 2-failures-then-success
  under threshold 3 never open, routing skips open, all open → `ErrNoHealthyDeployment`
  without queueing, probe failure keeps / success closes every circuit of the backend
  (not the other backend's), a queued waiter keeps waiting and is served once the probe
  closes, state (open and counts) survives a reload, unconfigured deployments not
  tracked, prober lifecycle through the timer (failures keep it, success stops it,
  reload removal stops it, shutdown stops it mid-probe and `RunProbers` waits, no
  prober after shutdown), cut-short probe not counted. `server/circuit_test.go` —
  the classification table end to end (threshold 1: 500, 503, 401, 403, connect
  refused, first-byte timeout, cut mid-stream open; 429, 400, 404, client gone before
  / mid-stream, success stay closed), threshold then `503 no_healthy_deployment` with
  log lines, no record, metrics; load moves to the healthy deployment, probe failure
  keeps / success closes, back in rotation, log lines and metrics; probe credentials
  (Bearer, Azure `api-key`, paths). `provider` probe test (paths, credentials,
  non-2xx and refused). `cmd/kaiak` `servingStatus` with an open circuit (UTC
  `opened_at`); the existing goroutine-leak checks cover `run`'s `RunProbers`.
- `go test -race -count=5 ./internal/routing ./internal/server`: ok.
- `scripts/check-all.sh`: gofmt, vet, staticcheck, `go test -race` (all packages,
  e2e included), live-test kit, `npm test` (379 pass), lint, cross-half e2e —
  `all checks passed`.
