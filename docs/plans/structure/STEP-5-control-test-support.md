# Step 5 — kaiak-control test support; tests filed by subject

**Status:** not started

## Intent

`kaiak-control` tests share one support module, so a protocol bump or a new usage unit
touches no test file, and a behaviour's tests live in one file named for that
behaviour on both halves — not in files named after the review round that found them.

## Findings

- test-scaffolding F4, edges F4: `kaiak-control/src/test-support/index.ts` (an ordinary
  subsystem for the boundary lint, not in package `exports`): `fixture(rel)`,
  `gatewayHeaders(instance)` from `PROTOCOL_VERSION`, `startApp(core, opts)`,
  `openStream(base, instance)`, `nextConfig` / `nextTotals`, `configNumbered`,
  `usageBatch(overrides)` built on a `protocol/fixtures` usage record; `sse-client.ts`
  moves there.
- Fold `fastify/round-2.test.ts`, `fastify/round-3.test.ts`,
  `control-plane/{review,round-3}.test.ts` into the files of their subjects (stream
  ordering → `fastify.test.ts`; totals → `status-totals.test.ts`; status receipt →
  control-plane tests; …).
- Gateway side, same rule: the `review_test.go` files in `accounting`, `provider`,
  `server` and `server/inbound_review_test.go` fold into their subjects' test files.
- control-core hint: test literals `"5"` / `format_version: 5` read the exported
  constants.

## Files likely touched

- `control/kaiak-control/src/**/*.test.ts` (fastify, control-plane, usage, messages,
  config), `control/sample/src/main.test.ts`, `sample/src/app/app.test.ts`.
- `control/scripts/check-boundaries.ts` only if the new subsystem needs declaring.
- `gateway/internal/{accounting,provider,server}/*review*_test.go` and their targets.

## Decisions made during planning

- Fold per subject, one commit per subject if that keeps the diff readable.
- A test's name may change to say what it checks; its assertions do not.

## Removal checklist (clean at phase end)

- `git ls-files 'control/**/round-*.test.ts' 'control/**/review.test.ts' 'gateway/**/*review*_test.go'`
  → none.
- `git grep -n '"kaiak-protocol": "5"' control/` → none outside test-support.

## Acceptance criteria

- One test-support module; every fastify test file uses it.
- Test counts per half unchanged (record `npm test` and `go test` counts before/after;
  each moved test findable by name in the Result).
- `scripts/check-all.sh` green. **Phase 1 ends here.**
