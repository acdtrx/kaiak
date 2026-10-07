# Step 3 — e2e harness and passthrough scenarios

**Status:** not started

## Intent

Adding a backend type or a client API to the e2e is one row, not a new file of copied
tables, switches and loops; the harness has one way to run a logged process and one way
to wait.

## Findings

- test-scaffolding F2: one table keyed by backend type (credential header and value,
  key env, URL layout); one `backendEntry(typ, fakeURL)`; one scenario config builder;
  one per-backend passthrough check parameterised by an API spec (path, body, stream end
  marker, API-specific assertions). `typedBackends`, `messagesBackends`,
  `responsesBackends` and their switches become that table; `messagesConfig` and
  `responsesConfig` become one builder.
- test-scaffolding F6: a `process` type (cmd, logs, exit; `start`, `stop`, `waitExit`)
  under `gateway` and the sample process; a `changes` type (`notify`, `wait`) for
  `logLines`, `totalsWatch` and the control proxy; `poll` moved to the harness; harness
  methods spread over feature files moved into `harness_test.go`.
- test-scaffolding bug: the misplaced doc comments at `e2e/sample_test.go` (`used`,
  `mergeWindows`).

## Files likely touched

- `gateway/e2e/{backendtypes,messages,responses,harness,sample,proxy,startup,stateless,shared}_test.go`.

## Decisions made during planning

- API-specific assertions (`anthropic-version`, `store: false`, service tier) stay in
  each API's spec, not as flags on the shared loop.
- The backend-type table here is the e2e's own statement of expectations; step 4's
  shared fixture is what ties the provider and backend-verify to each other.

## Acceptance criteria

- One backend-type table in `e2e`; adding a type is one row.
- One process type, one wait primitive; no fixed sleeps introduced.
- Every scenario that ran before still runs, the crosshalf ones included (count
  subtests before/after, both build tags).
- `scripts/check-all.sh` green (the crosshalf e2e is touched).
