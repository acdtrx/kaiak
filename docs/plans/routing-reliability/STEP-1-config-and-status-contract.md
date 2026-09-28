# Step 1 — config and status contract

**Status:** done (2026-09-24) — phase 1 complete, suite green

## Intent

Add the P3 settings to the config document and the reliability fields to the status
message, on both halves, so the gateway steps build on a settled contract.

## Files likely touched

- `protocol/schema/config.schema.json`, `status.schema.json`, fixtures + `cases.json`
- `control/kaiak-control` config and messages types/validators; gateway `config` and
  `control` packages
- `docs/specs/CONTROL-PROTOCOL.md`, `docs/specs/GATEWAY.md`

## Decisions made during planning

- Backend: optional `max_in_flight` (absent = no cap). Queue (one per model, settled
  2026-09-24): global `queue: { size, timeout_ms }` (defaults 100 and 30000), model
  optional `queue` override.
- Global: `retries: { max_attempts }` (default 3, min 1); model: optional
  `retries: { max_attempts }` override. Global: `circuit: { failure_threshold,
  probe_interval_ms }` (defaults 5 and 10000).
- Status: per backend `{ in_flight, max_in_flight? }` replacing today's in-flight map,
  per model `{ queued }`; per
  deployment `{ backend, model, circuit: "closed" | "open" }` (+ opened_at when open).
  Exact shape settles here.
- Semantic rules for new fields where the schema cannot express them (if any).

## Acceptance criteria

- Fixtures pass/fail in both halves; defaults applied at load on the gateway; spec
  records the fields; `scripts/check-all.sh` green.

## Decisions made during implementation

- **Config** (recorded in `GATEWAY.md`, Routing and reliability → Reliability
  settings): backend `max_in_flight` ≥ 1, omitted = no cap. `global.queue
  { size ≥ 0, timeout_ms ≥ 1 }` (100 / 30000); `size` 0 allowed = never queue, a
  request finding no free slot is refused at once with `queue_full`. A model's
  `queue` overrides field by field (at least one field). `global.retries
  { max_attempts }` 1..10 (3); a model's `retries` must set `max_attempts`.
  `global.circuit { failure_threshold ≥ 1, probe_interval_ms ≥ 100 }` (5 / 10000),
  global only. No semantic rules: the schema expresses every bound.
- **Gateway snapshot**: defaults resolved at load — `Backend.MaxInFlight` (0 = no
  cap), `Model.Queue { Size, Timeout }` and `Model.MaxAttempts` already merged over
  the global values, `Snapshot.Circuit { FailureThreshold, ProbeInterval }`. Steps
  2–4 read only the resolved per-model values.
- **Status shape** (recorded in `CONTROL-PROTOCOL.md`, Messages → Status):
  `backends: { <id>: { in_flight, max_in_flight?, deployments: { <backend model
  name>: { circuit, opened_at? } } } }` and `models: { <name>: { queued } }`, both
  required, empty before the first config. Deployments are **nested under their
  backend**, keyed by the backend-side model name, instead of the planned flat
  `deployments` array: a deployment is backend + backend-side model name (routing's
  identity), so nesting makes each unique by construction and follows the
  "collections keyed by ID" convention — the array would have needed a duplicate
  rule in both halves. Every backend of the applied config is listed (idle ones with
  0, so caps show) plus backends a reload dropped while requests still run; every
  model is listed with `queued` 0 when empty. `opened_at` present exactly when
  `open` (schema if/then/else; the Go walker mirrors it) and checked by
  `timestamp-invalid`.
- **Gateway reporter**: `control.Options.Serving func() control.Serving` replaces
  `InFlight`; `cmd/kaiak` builds it (`servingStatus`) from
  `router.InFlightByBackend()` and the applied snapshot. For now every model reports
  `queued` 0 and every deployment `closed` — the honest state, as no queue or
  circuit breaker exists yet: **step 2 feeds real queue depths, step 3 real circuit
  states** (through routing, read by the same adapter).
- **Sample page**: the gateways section's "In flight" column reads
  `backends[*].in_flight`, listing busy backends only ("idle" when none); the rest
  of the new fields are step 5.

## Result

- Fixtures: config `full.json` exercises every new field (global queue/retries/
  circuit, a cap, a partial and a full model queue override, a retries override);
  13 new invalid config fixtures; status valid fixtures moved to the new shape
  (`ready.json` carries a cap, an open circuit, a queue; `rejected.json` a dropped
  backend); status invalid fixtures replaced/added (16 schema cases, one
  `timestamp-invalid` for `opened_at`). Both halves pass all of them.
- Tests added: gateway config defaults and overrides (`TestReliabilitySettings`,
  cap saturation), status reporter with the new hook, `servingStatus` in
  `cmd/kaiak`; page/events tests moved to the new shape.
- `scripts/check-all.sh`: gofmt, vet, staticcheck, `go test -race` (all packages,
  e2e included), live-test kit, `npm test` (379 pass, 0 fail), lint, cross-half e2e
  — `all checks passed`.
