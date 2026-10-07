# Step 1 — server test harness

**Status:** done (2026-10-08)

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

## Result

**What changed**

- `gateway/internal/server/server_test.go` — one `buildTestGateway(t, testOptions{…})`
  with options `edit` (the starting config document), `limiter`, `bodyMemory` and
  `attach` (control-plane mode: called with the document, the logger and the
  limiter before the rest of the pipeline is wired, it returns the recorder's
  batcher). `newTestGateway(t)` is `buildTestGateway(t, testOptions{})`;
  `newTestGatewayWith` and `newTestGatewayBodies` are gone, their call sites
  (`limits_test.go`, `visibility_test.go`, `bodies_test.go`) pass
  `testOptions{limiter: …}` / `testOptions{bodyMemory: …}`. New: `replaceOnce(t,
  doc, old, repl)` and its regex twin `replaceMatch(t, doc, re, repl)`, both failing
  the test unless the anchor occurs exactly once; `g.apply(t, edit)` (swap +
  `router.Configure`); `testLookupEnv`, the one fake environment the builder and the
  control client's applier read.
- `gateway/internal/server/usage_path_test.go` — `newControlledGateway` keeps only
  the fake control plane, the shared limiter's contact closure and the control
  client (built, run and connected inside `attach`) on top of the builder.
  `testLimitsTotals` and the contact closure are untouched (step 15).
- Every edit of the test config document in the server tests goes through
  `replaceOnce`/`replaceMatch`: 61 `strings.Replace(doc, …, 1)` edits and 3 regex
  edits (`"local-b"` in `retry_test.go` and `circuit_test.go`, `localEntry` in
  `pathmissing_test.go`) in 17 files (api, bodies, circuit, endpoints, estimate,
  keylimit, limits, messages, metrics, pathmissing, queue, responses, retry,
  routing, server, usage_path, visibility; `retrybudget_test.go` only passes `t` to
  `withGlobal`). The four
  `strings.Replace` calls left in `upstream_test.go` and `routing_test.go` edit
  expected response bodies, not the config.
- `g.apply` replaces the swap + configure sequence in `newCircuitGateway`,
  `newCappedGateway`, `newRetryGateway`, `withAnthropicModels`,
  `withMessagesModels`, `withResponsesModels`, `TestResponsesLimitRefusals`,
  `withKeyLimit` and `pathmissing_test.go`'s "no deployment left". Sites that swap
  without `router.Configure` keep doing so (no behaviour change).
- Edit helpers take the test's `t`: `withGlobal(t, …)`, `withLargeBodies(t, …)`,
  `workloadLimits(t, …)`, `localAs(t, …)`; `cappedDoc(t, doc)`, `capLocal(t, doc)`,
  `fiveDownDeployments(t, doc)`. `keyLimitDoc` folded into `withKeyLimit`, whose
  edit is `func(t, doc)`.
- `gateway/internal/auth/auth.go` — exported `KeyHash(key)` (`"sha256:"` + hex
  SHA-256), used by `Authenticate`, by `auth_test.go` (its `hashOf` gone), by
  `server_test.go` and `accounting_test.go` (`hashOf` gone) and by
  `e2e/harness_test.go`'s `newKey`. `scripts/live/config.go` keeps its own copy.

**Decisions made during the step**

- `replaceOnce` fails on more than one occurrence too, not only on none: with
  `strings.Replace(…, 1)` an ambiguous anchor silently edits whichever comes first.
  No current anchor is ambiguous.
- Table-driven tests must not capture the parent's `t` in an edit that runs inside
  a subtest (a `Fatal` there would be called from the wrong goroutine). So
  `api_test.go`'s `setup` takes the subtest's `t`; `TestPerKeySlotIsReleasedOnEveryPath`'s
  `edit` field is `func(t, doc)`; `TestMaxAttempts`'s table became data (`global`,
  `down` attempt counts) with the edit built inside the subtest. The edit text and
  order, and every assertion, are the same.
- The builder does not `router.Configure` the starting snapshot (as before):
  configuring every test gateway's router could change tests that swap later
  without configuring. The controlled gateway likewise stays unconfigured, as
  before.
- In `newControlledGateway` the fake backend is now created by the builder after
  the fake control plane (cleanup order: client, backend, control plane; was
  client, control plane, backend). Nothing depends on it.

**Edits that matched nothing:** none. With `replaceOnce` in place every one of the
64 anchors matches exactly once (confirmed by a throwaway test that the helpers do
fail on a missing and a repeated anchor, then removed).

**Report vs code** (034329e line numbers): the code had three `newTestGateway*`
constructors (`newTestGateway`, `…With`, `…Bodies`) plus `buildTestGateway`; the
"~63 edits" are 61 text plus 3 regex edits; the live kit's copy is at
`scripts/live/config.go:132`. Otherwise as reported.

**Test counts** (before → after, identical)

- `internal/server`: 184 → 184 top-level tests; 432 → 432 `=== RUN` (with subtests).
- `internal/auth`: 5 → 5 tests; 14 → 14 runs.
- `e2e`: 40 → 40 tests (42 → 42 with `crosshalf`); 120 runs before, green after in
  the suite.

**Suite** — `scripts/check-gateway.sh`, green:

```
==> gofmt / go vet / staticcheck 2026.2.1 (gateway)
==> go test -race -count=1 (gateway): ok cmd/kaiak, e2e, accounting, auth, clip,
    config, control, limits, logattr, metrics, netfail, otlplog, provider, routing,
    schemacheck, server, sse
==> gofmt / go vet / staticcheck (live-test kit)
==> live-test kit self-test: passed for vllm, llama-server, openai, azure-openai,
    anthropic, azure-anthropic, vllm with two backends
gateway checks passed
```
