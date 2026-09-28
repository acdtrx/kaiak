# Step 1 — protocol messages

**Status:** done (2026-09-24)

## Intent

Settle every message's exact shape as JSON Schema with fixtures, and add
`global.control_outage_grace_ms` to the config document — on both halves.

## Files likely touched

- `protocol/schema/` — `config-snapshot.schema.json`, `stream-event.schema.json`
  (`config`, `totals`, `resync`), `usage-batch.schema.json`, `usage-ack.schema.json`,
  `status.schema.json`, the usage record as a shared `$defs`; `config.schema.json`
  (outage grace)
- `protocol/fixtures/messages/<kind>/{valid,invalid}/` + `cases.json` per kind
- `kaiak-control` — message validation entry, fixture-driven tests
- Gateway: config package accepts and defaults `control_outage_grace_ms`; a Go test
  decodes every message fixture into the types step 6/7 will use (types may live in a
  new `gateway/internal/control` package created here, with decode + validate only)
- `docs/specs/CONTROL-PROTOCOL.md` — field names

## Decisions made during planning

- Usage record schema = the record fields already settled (accounting, P1).
- Batch ID = `{ instance, epoch, sequence }`; epoch is 32 hex random; sequence starts
  at 1 per epoch.
- `totals` payload: config version it refers to, per scope (`global`, `team:<id>`,
  `workload:<id>`, `user:<id>`) per limit (type + model set) the used amount and
  window start; plus `live_gateways`. Exact shape settles here.
- Status: instance, protocol version, state (`starting` | `ready` | `draining`),
  applied config version, last rejection `{ version, codes }`, in-flight per backend,
  gateway start time.

## Acceptance criteria

- Valid/invalid fixtures pass/fail in both halves; spec lists the fields.
- Config fixtures updated for the new field; both suites green.

## Result

- `scripts/check-gateway.sh` — gofmt, vet, staticcheck 2026.2.1, `go test -race ./...`
  (all packages ok, new `internal/control` included), live-test kit self-test:
  `gateway checks passed`.
- `control/`: `npm test` — 178 tests, 178 pass, 0 fail; `npm run lint` — `tsc` clean,
  `boundaries ok`.
- No expected reds.

### Decisions made while implementing

- One schema file per message: `config-snapshot`, `totals`, `resync`, `usage-record`,
  `usage-batch`, `usage-ack`, `status`, plus `common.schema.json` (instance ID,
  config version, safe count, amount). No `stream-event.schema.json`: the SSE event
  name picks the data's schema (`config` → config snapshot, `totals` → totals,
  `resync` → resync). Every schema (config included) has an `$id` under
  `https://kaiak.invalid/protocol/schema/` so the files `$ref` each other.
- Totals: `{ config_version, live_gateways, windows: [{ scope, id?, type, models?,
  window_start, used }] }`, complete (a limit not listed used 0 in the current window),
  hour/month limit types only, `window_start` aligned by schema. The ack carries the
  same shape, `live_gateways` included. `used` is a string of at most 18 digits (see
  CONTROL-PROTOCOL.md, Messages → Totals, for the reasoning).
- Batch: 1–500 records (empty batches refused; step 7 queues sealed batches rather
  than growing one past 500). Message rules: `record-instance-mismatch`,
  `record-id-duplicate`, `totals-window-duplicate`, `rejection-not-newer`,
  `timestamp-invalid`.
- Status carries `protocol_version` (the one body that does, so it can be stored and
  shown); `last_rejection` only while newer than the applied version.
- Instance ID shape: `^[A-Za-z0-9][A-Za-z0-9._-]{0,252}$`; the body's instance must
  equal the `Kaiak-Instance` header.
- Gateway: the config snapshot's config is returned raw and validated by
  `config.Parse`, so a bad config is a config rejection (version + codes), not a
  malformed message. The parity test counts either as the fixture's rejection.
- Gateway decoding: walker → rules on the generic tree (timestamps must be checked
  before a typed decode into `time.Time`) → strict typed decode; integers written as
  `4.0`/`4e0` are accepted as the schemas accept them.
- `control_outage_grace_ms`: schema default 900000, `Snapshot.ControlOutageGrace`
  defaulted at load; fixtures `full.json` (600000) and three invalid ones.
- Shared key fixture `protocol/fixtures/keys/example.json`; `auth` test authenticates
  the key against the fixture hash verbatim. Step 2's keygen test uses the same file.

### Deviations from the plan

- Kit API is one purpose-named validator per message (`validateUsageBatch`, …) in a
  new `messages` subsystem, not `validateMessage(kind, doc)` (CODING-RULES §1).
- Refactors needed to avoid copying helpers into a second place (CODING-RULES §2):
  - gateway: new `internal/schemacheck` (walker parts, decode, real-date checks)
    extracted from `config`; `config` now builds on it; `config` exports `IsID`,
    `IsModelName`, `IsPublicModelName`, `IsTimestamp` for the message walkers.
  - kit: new `schemas` subsystem (one Ajv instance for all schema files, issue
    shape, JSON Pointer) replacing `config/schema.ts`; new `calendar` subsystem
    (real date / instant) moved out of `config/semantic.ts`. `ConfigIssue` is now an
    alias of `ValidationIssue`.

### For later steps

- Step 4/5: check the body's instance against the `Kaiak-Instance` header.
- Step 4: totals are complete per push/ack (decided here, not per touched scope);
  `used` is sent as a string (BigInt on the kit side).
- Step 6: validate `KAIAK_INSTANCE_ID` against the instance shape at startup in
  control-plane mode — a hostname outside it would make every batch invalid.
- Step 6/7: encoding `Totals.Windows`, `Status.InFlight`, `Rejection.Codes` needs
  non-nil collections (nil encodes as `null`, which the schemas refuse).
- Step 8: `live_gateways` 0 is treated as 1.
