# Step 8 — `starting` off both halves; control-side leftovers

**Status:** not started

## Intent

Remove the unreachable `starting` status from the protocol on both halves (decision 4),
and the control-side leftovers: a totals API shaped like a gateway, read by one page
with a fake instance, and a sample package entry nothing imports.

## Findings

- control-main F5 / decision 4 — `starting`, both halves in one change:
  - gateway: `currentStatus` reports `ready` or `draining`; the `Applier.Loaded()` read
    and `TestStatusIsStartingUntilAConfigIsApplied` go; `StateStarting` and its walker
    value go;
  - `protocol/schema/status.schema.json` (and the synced `kaiak-control/schema/` copy),
    `protocol/fixtures/messages/status/valid/starting.json`;
  - `kaiak-control` `GatewayState`; the sample's `.state-starting` CSS;
  - `docs/specs/CONTROL-PROTOCOL.md` ("Accepted before any config is published: a
    starting gateway…").
- control-core F2, edges F3(a) — `ControlPlane.totals(instance)`, `Usage.totals` and
  `countedThrough` go. The sample page reads `core.readTotals()`: `windowStarts[type]`
  for the start and `liveGateways` for the count; `PAGE_READER`, `HOUR_MS` and
  `monthStart` go. The ~21 test call sites read `readTotals()` or the stream. `GUIDE.md`
  §9 says `readTotals()` (step 26 trims the rest).
- edges F5 — `control/sample/src/index.ts` and the `exports` field go.
- control-main small item — `currentStatus` and `servingStatus` both guard
  `Deployments != nil` and copy `Serving` field by field: keep one guard (with step 19
  in mind, which moves `servingStatus`).

## Files likely touched

- `gateway/internal/control/{status,messages,schema}.go`, `status_test.go`.
- `protocol/schema/status.schema.json`, `protocol/fixtures/messages/status/`,
  `control/kaiak-control/schema/` (sync script), `kaiak-control/src/messages/types.ts`.
- `kaiak-control/src/{usage,control-plane}/index.ts` and tests; `fastify/gateway-stream.ts`
  (keeps the one `counted_through` filter).
- `control/sample/src/page/{sections,format}.ts`, CSS, `src/index.ts`, `package.json`.
- `docs/specs/CONTROL-PROTOCOL.md`, `control/kaiak-control/GUIDE.md` §9.

## Decisions made during planning

- No protocol version bump (decision 9): no current gateway sends `starting`, and both
  halves move together.
- `readTotals` keeps its name; renaming it is not worth a second API change.

## Removal checklist (clean at phase end)

- `git grep -nwi 'starting' -- protocol/ gateway/internal/control control/ docs/specs/CONTROL-PROTOCOL.md`
  → no status-state use (other meanings of the word are fine; list what remains).
- `git grep -nE '\.totals\(|countedThrough|PAGE_READER' control/` → none.
- `git ls-files control/sample/src/index.ts` → none.

## Acceptance criteria

- Status schema, fixtures, both decoders and the spec agree on `ready | draining`; the
  shared fixtures pass on both halves.
- The sample page shows the same spend and window starts as before (its tests).
- `scripts/check-all.sh` green, or reds named with the step that clears them.
