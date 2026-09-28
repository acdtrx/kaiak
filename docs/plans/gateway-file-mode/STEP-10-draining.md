# Step 10 — draining

**Status:** done (2026-09-24) — phase 4 continues with step 11; suite green, no expected reds.

## Intent

SIGTERM shuts down without dropping requests or usage: the four-stage drain from
`GATEWAY.md` (control-plane status notice arrives with P2).

## Files likely touched

- `gateway/cmd/kaiak/main.go`, `gateway/internal/server/` — lifecycle
- config: grace period and drain timeout (env, with defaults)

## Decisions made during planning

- Sequence: `/readyz` fails → accept new requests for the grace period (default 5 s)
  → refuse new requests (`503`) → wait for in-flight up to the drain timeout (default
  60 s; streams can be long) → write the usage snapshot → exit 0.
- Every goroutine has an owner stopped in this sequence; the test checks none leak.
- A second SIGTERM/SIGINT exits immediately after writing the snapshot.

## Acceptance criteria

- Tests: an in-flight stream finishes during drain; requests during grace succeed,
  after grace get 503; snapshot written; drain timeout cuts off a hung stream.

## Result

Commands run (2026-09-24):

- `scripts/check-gateway.sh` — gofmt, `go vet`, staticcheck 2026.2.1 clean;
  `go test -race ./...` **pass**. Drain, stop-signal and listener tests also at
  `-count=10` — pass, no flakes.
- `npm test` in `control/` — 87 tests **pass**; `npm run lint` — `tsc` clean,
  `boundaries ok`.
- Manual, built binary + a throwaway slow-streaming backend (16 events, 500 ms apart;
  scratch, deleted), grace 3 s, timeout 30 s: stream started, SIGTERM → `/readyz`
  `503 draining`, `/healthz` 200, `/metrics` answering; request during grace → 200 with
  `Connection: close`; after grace a new connection → refused (curl exit 7), a request
  on a keep-alive connection opened before the drain → `503 server_shutting_down`,
  `Connection: close`; the stream ran to `[DONE]` (record `partial=false`, 16 tokens
  out); log `draining` → `draining: refusing new requests in_flight=1` → `drained` →
  `limits snapshot written trigger=shutdown` → `kaiak stopped`; `limits.json`
  written; exit code 0.

Delivered:

- `server.Drain` — phase (serving / draining / refusing) and the in-flight count
  (API handler entry to return, after finishers). `Run(listener, DrainTimes, hurry,
  logger)` is the mechanism: `begin` (readiness fails, `Connection: close` on
  responses) → grace → `refuse` (listening socket closed; `admission` stage answers
  `503 server_shutting_down`) → `finish` (wait for idle up to the timeout, else cut
  off: request contexts cancelled with a drain cause, then `http.Server.Close`; wait
  for the cut handlers; else `Shutdown` for connections finishing their last writes).
  `hurry` closed skips the remaining waits and cuts off.
- `Listener`: `Serve()` (no ctx; returns nil when stopped by the drain or Shutdown),
  `Shutdown(timeout)` (admin), request contexts derived from a per-listener base
  context the cut cancels.
- Pipeline: `admission` is stage 0. Upstream: a drain cut ends a relay as
  `relay_end=shutdown` with the connection cut (never a clean end), a not-yet-started
  response as `503 server_shutting_down`; both count in the new error class
  `shutting_down`.
- `cmd/kaiak`: SIGTERM and SIGINT on one channel registered for the process's life;
  `run(ctx, logger, env, reload, stop)`: first stop value (or a listener failure)
  drains, a second hurries; ctx cancel = hurried drain. After the drain: background
  goroutines stopped and waited, snapshot written, admin listener shut down (5 s),
  listeners waited. `KAIAK_DRAIN_GRACE_MS` (5000), `KAIAK_DRAIN_TIMEOUT_MS` (60000).
- Docs: `GATEWAY.md` — Lifecycle rewritten with the exact sequence, env vars, error
  table row, error class, `relay_end=shutdown`, partial cause, Kubernetes grace-period
  note; `ARCHITECTURE.md` — `server` description.

Decisions beyond the plan:

- **SIGINT drains like SIGTERM** (Ctrl-C and `docker stop` expect it).
- **Keep-alive handling**: the listener's socket is closed instead of calling
  `http.Server.Shutdown` at grace end — `Shutdown` (and `SetKeepAlivesEnabled(false)`)
  close idle keep-alive connections at once, so a client reusing one would see a reset
  instead of an answer. Idle kept-alive connections therefore stay open through the
  drain and get a clean `503` + `Connection: close`; `Shutdown` runs only once the
  handlers are done. During grace every response carries `Connection: close` so
  clients move to fresh connections while the endpoint is still reachable.
- **Refusal is a pipeline stage** (`admission`, first) — no side door; it runs before
  auth, so refusing reads nothing.
- **Second signal exits 0**, like a timed-out drain: the sequence completed, cut
  requests are logged and settled as partial. A listener failure still exits 1.
- **ctx stays on `run`** as the hard stop (hurried drain): existing tests and callers
  keep a cancellation primitive; the signals are the production trigger.
- **No `kaiak_draining` gauge**: `/readyz` already reports it; recorded in GATEWAY.md.
- The goroutine check inspects stacks for this module's frames right after `run`
  returns (on the test goroutine) — deterministic, no polling; verified to catch a
  background goroutine left running.

Deviations: none from the acceptance criteria.

Open doubts:

- A request racing the socket close (connected but not yet accepted) is reset by the
  OS — inherent to closing a listener; clients see a connection error.
- The cut path waits for the cut handlers without a bound of its own; it relies on
  every blocking point honouring the context or the closed connection (upstream call,
  body read, client write) — true today, a future stage that blocks otherwise would
  stall the drain.
- `KAIAK_DRAIN_TIMEOUT_MS=0` cuts in-flight requests right after the grace period —
  allowed deliberately (a "no drain" setting), not guarded.
