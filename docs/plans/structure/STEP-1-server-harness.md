# Step 1 — server test harness

**Status:** not started

## Intent

The server tests build the gateway in one place and fail loudly when a config edit
misses. Today the pipeline is wired three times (`main.go`, `buildTestGateway`,
`newControlledGateway`), there are four `newTestGateway*` constructors, and ~63
`strings.Replace` edits leave the document unchanged without failing when their anchor
is gone.

## Findings

- test-scaffolding F1(b): one `buildTestGateway(t, testOptions{…})`.
- test-scaffolding F5: `replaceOnce` that fails; `g.apply(t, edit)` for the repeated
  swap + configure sequence.
- test-scaffolding hint: one exported `auth.KeyHash` for the gateway module's test
  copies of `"sha256:" + hex(sha256(key))`.

## Files likely touched

- `gateway/internal/server/server_test.go`, `usage_path_test.go`, and the ~17 test files
  with anchor edits (`retry_test.go`, `circuit_test.go`, `limits_test.go`, …).
- `gateway/internal/auth/auth.go` (`KeyHash`), `auth_test.go`, `server/server_test.go`,
  `gateway/e2e/harness_test.go`.

## Decisions made during planning

- `testLimitsTotals` and the test contact closure are **not** fixed here: step 15
  removes them with the adapters they copy. `newControlledGateway` keeps only its
  control-client setup on top of the shared builder.
- The larger option of F5 (edit a decoded map instead of text) is not taken: it would
  restyle 17 files for a formatting coupling `replaceOnce` already makes loud.
- `scripts/live` keeps its own key-hash copy (separate module, by design).

## Acceptance criteria

- One builder; each former `newTestGateway*` is a one-line option set.
- Every anchor edit in server tests goes through `replaceOnce`; running the suite with
  it in place finds no edit that matched nothing (or fixes the ones that did, named in
  the Result).
- One key-hash function in the gateway module, used by its tests.
- Test count in `internal/server` unchanged (record before/after).
- `scripts/check-gateway.sh` green.
