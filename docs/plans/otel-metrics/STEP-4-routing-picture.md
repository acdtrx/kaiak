# Step 4 — the routing picture

**Status:** done (2026-10-08)

## Intent

Routing produces one picture of what it serves against a config snapshot (T12);
`main` formats it as the status report's `serving`, `metrics` reads it for the
per-backend, per-model and per-deployment gauges (F5). Per-config series are
prepared in one place. No output change.

## Files likely touched

- `gateway/internal/routing/` — a `Picture` (name is the step's): every backend,
  model and deployment of the snapshot plus those still active from a dropped
  config; per backend its in-flight count and cap share; per model its queue depth;
  per deployment its circuit state (with its opening time) and cooldown. Built under
  the router's locks once per call, from the getters it replaces.
- `gateway/cmd/kaiak/controlplane.go` — `servingStatus` formats the picture.
- `gateway/internal/metrics/ops.go` — the read-at-collect families read the
  picture: one helper for the per-deployment gauges, one for the zero-filled
  per-backend and per-model counts; `NewOps` takes the `Circuits`, and
  `ConfigLoaded` prepares both their series, so `main` drops its `PrepareSeries`
  call (F5).
- Tests: `cmd/kaiak/controlplane_test.go`, `metrics_test.go`, routing tests for the
  picture.

## Decisions made during planning

- One picture per collect: the gauges of one scrape agree with each other (today
  each family reads the router separately).
- The router getters the picture replaces go when nothing else reads them
  (removal discipline: grep checklist).

## Acceptance criteria

- `servingStatus`'s tests and the status fixtures unchanged in what they assert.
- `/metrics` byte-identical on a scenario with an open, a half-open and a cooling
  deployment and a backend removed with requests in flight.
- `git grep -n "PrepareSeries" gateway/cmd` finds nothing.
- Phase 1 end: `scripts/check-all.sh` green. Suite recorded.

## Result

### What changed

- `gateway/internal/routing/serving.go` (new) — the picture is `routing.Serving`
  (routing's own word: `OnServingChange`, the status report's `serving`), taken by
  `Router.Serving(s *config.Snapshot)`: `Backends` (`BackendServing`: the
  snapshot's `MaxInFlight`, this gateway's `Share`, `InFlight`), `Models`
  (`ModelServing`: `Queued`), `Deployments` (`DeploymentServing`: `Circuit`
  closed/open/half_open, `OpenedAt`, `CoolingUntil` + `CoolingDown()`). Every
  backend, model and deployment of `s` at zero/closed, plus backends and models a
  reload dropped that still run or wait; with `s` nil only those. Locking: the
  snapshot walk and zero-fill run before taking `r.mu`; one `r.mu` hold then
  copies the sparse live maps (backend load, queue lengths, circuits, cooldowns)
  and computes shares — the router's only mutex, so no ordering issue, and the
  hold is proportional to active entries plus the snapshot's backends.
- `gateway/internal/routing/routing.go`, `circuit.go` — removed
  `InFlightByBackend`, `MaxInFlightByBackend`, `QueuedByModel`, `CoolingDown`,
  `Circuits` and `CircuitReport`. `IDOf` stays (server's attempts use it).
- `gateway/cmd/kaiak/controlplane.go` — `servingStatus(routing.Serving)` formats
  the picture (configured cap, deployments under their backend, opening time in
  UTC).
- `gateway/cmd/kaiak/main.go` — `NewOps(reg, router, circuits, holder)`; the
  `PrepareSeries` call is gone.
- `gateway/internal/metrics/registry.go`, `text.go` — `GaugeFuncs([]GaugeDesc,
  collect)`: gauge families read together; `collect` gets one emit per family.
  `WriteText` runs each group's collect once before writing, keeps the samples in
  a map local to that write, and writes each family in its name order.
- `gateway/internal/metrics/ops.go` — the six routing gauges are one `GaugeFuncs`
  group over one `router.Serving(holder.Current())` per scrape;
  `deploymentGauge(emit, serving, is)` serves open, half-open and cooling. The
  per-backend/per-model zero-fill helper F5 proposed is not needed: the picture
  is zero-filled by routing, so the in-flight, cap and queue gauges are plain
  loops. `Ops` holds the `*Circuits`; `prepareSeries` (construction and
  `ConfigLoaded`) prepares the circuit counters too; `Circuits.PrepareSeries` is
  now the unexported `prepareSeries`.
- `docs/BACKLOG.md` — F5 and T12 sub-items of OpenTelemetry export → Metrics
  removed (resolved).
- Tests: new `routing/serving_test.go` (`TestServingCoversTheSnapshotAndWhatADroppedOneLeftRunning`
  and the helpers `inFlight`, `queuedIn` over the picture, `coolingDown` over
  internal state); routing `notClosed` reads `r.circuits` (the tests check what the
  router tracks, including for deployments outside the config); server tests read
  the picture (`inFlight(g)`, `notClosed(g)`, `coolingDown(g)`); the cap-share
  test reads `Share`. `TestServingStatusCoversTheAppliedConfig` builds its input
  from an idle router's `Serving(s)` with live values set by hand; every
  assertion unchanged. New `TestGaugeFuncsReadOncePerWrite` and
  `TestRoutingGauges` in `metrics_test.go`.

### Per-scrape design

A registry group, not a cache: one collect callback for several families, run
once per `WriteText`, its samples held in that write's local map. Concurrent
scrapes each read their own picture; no shared mutable state, no scrape counter,
no time-based reuse. It mirrors OpenTelemetry's multi-instrument callback
(`RegisterCallback(f, instruments...)`), so step 5's collect step can carry it
over as is.

### Byte-identity

A test in `internal/metrics` built the registry as `main` does (`NewCircuits` as
the router's observer, `NewOps`, then per apply: holder swap, `Configure`,
`PrepareSeries`, `ConfigLoaded`) and set up: backends a (cap 4), b, c (cap 2),
d (cap 1), gone (cap 1); b's deployment open, c's half-open (`ProbeNow`), a's
cooling (a 429), two models sharing a's deployment; d full with a request queued;
gone's slot held and a request queued on its model `old`; then a reload without
gone and old; two requests in flight on a. Full scrape before any change: 906
lines, 65 253 bytes (deterministic over three runs). After: `cmp` identical, three
runs. The test stays as `TestRoutingGauges`, asserting the exact sample lines of
the six routing families (HELP/TYPE left out, as steps 5–6 rewrite them).

### Notes

- Deployments outside the snapshot are not in the picture. Before, the circuit and
  cooling gauges also emitted circuits/cooldowns the router held outside the
  holder's snapshot, and `kaiak_backend_max_in_flight` read the router's last
  `Configure` rather than the holder. The router drops circuits and cooldowns of
  deployments its applied config lacks, so these differ only in the instant
  between the holder's swap and `Configure` — not in a steady state.
- `NewOps` with a holder that already has a config now also prepares the circuit
  counters; `main` builds it before any config, so its output is unchanged.

### Greps

- `git grep -n "PrepareSeries" gateway/cmd` — nothing.
- `InFlightByBackend`, `MaxInFlightByBackend`, `QueuedByModel`, `CoolingDown()`
  (the router's), `.Circuits()`, `CircuitReport`, `PrepareSeries`: nothing outside
  `docs/plans/` and `docs/reviews/`.

### Suite

`scripts/check-all.sh`: exit 0 — gofmt, vet, staticcheck, telemetry boundary,
race tests (`kaiak/e2e` 108.8 s; `cmd/kaiak`, `metrics`, `routing`, `server` ok),
live-test kit, control `npm test` (629 tests, 628 pass, 1 skipped, 0 fail), lint
(`boundaries ok`), cross-half e2e `ok kaiak/e2e 65.373s`, "all checks passed".
Phase 1 ends green.
