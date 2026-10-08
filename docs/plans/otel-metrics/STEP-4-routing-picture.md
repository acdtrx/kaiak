# Step 4 — the routing picture

**Status:** not started

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

