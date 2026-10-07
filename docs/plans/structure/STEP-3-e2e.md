# Step 3 — e2e harness and passthrough scenarios

**Status:** done (2026-10-08)

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

## Result

**What changed**

- `gateway/e2e/passthrough_test.go` (new) — the passthrough scenarios' shared parts:
  - `backendTypes`, one table keyed by backend type (`openai-compatible`, `vllm`,
    `llama-server`, `openai`, `azure-openai`, `anthropic`, `azure-anthropic`): credential
    header and value, key env and key, `resourceRoot` (base_url is the resource root,
    not `/v1`) and `apiPrefix` (`/v1`, `/openai/v1`, `/anthropic/v1`). `backendEntry(typ,
    fakeURL)` builds a backend's config entry from it; `backendKeysEnv()` sets every
    keyed type's variable.
  - `scenarioBackend{name, typ, model, deployed}`; `scenarioConfig(fakeURL, hash,
    backends)` (one model per backend, group `w`, key `k`); `apiScenarioConfig(fakeURL,
    oldURL, hash, backends)` adds what the Messages and Responses scenarios share: the
    `old` vllm backend, models `pair` and `rpm`, group `tight` and `tightKey`.
  - `passthrough[R apiRow]`, a client API's spec: client path, backend path under the
    type's prefix, how it is sent (`(*gateway).post` / `postMessages`), the body, the
    cases, the stream end marker, the settled line's usage attributes, and the API's own
    `check(t, row, case, sent, got)`. `run` makes the shared checks per backend × case:
    200; public model name in the answer, deployed name absent; stream end marker when
    streamed; settled on the backend with the API's usage; exactly one backend request;
    path and deployed model; the type's credential header; no header carrying the
    client's key; then the API's check.
- `backendtypes_test.go`, `messages_test.go`, `responses_test.go` — `typedBackends`,
  `messagesBackends`, `responsesBackends` and their three switches, `typesConfig`,
  `messagesConfig`, `responsesConfig` and the key-env constants are gone. Each API keeps
  its row type embedding `scenarioBackend` with its own flags (`chatBackend.forcesTier`,
  `messagesBackend.standardOnly`, `responsesBackend.{forcesTier, countsTokens}`) and its
  spec (`chatPassthrough`, `messagesPassthrough`, `responsesPassthrough`) holding its
  assertions: the tier rules, `anthropic-version` on Anthropic's types only and no
  `Authorization` on a Messages backend, `store: false`. The backend that does not serve
  the API (`chatOnly`, `claudeOnly`) is one more `scenarioBackend` in the config.
- `harness_test.go`:
  - `changes` (`notify`, `end(why)`, `wait(t, what, limit, check)`), the one wait
    primitive: used by `logLines.wait`/`waitCount`, `totalsWatch.wait` and the control
    proxy's `waitRecord`/`waitStatus`.
  - `process` (`name`, `cmd`, `logs`, `exited`, `err`; `start`, `signal`, `stop`,
    `waitExit`, `exitCode`), embedded by `gateway` and `sampleProcess`. `startProcess`
    became `startKaiak` (a gateway not yet waited on).
  - `poll`/`pollUntil` moved here (`metricPoll` → `pollEvery`); `waitMetricWithin`
    polls through `poll`.
  - Moved in: `logLines.find` (from `startup_test.go`), `exitCode` (now on `process`)
    and `gateway.exitError` (from `stateless_test.go`), `gateway.postMessages` (from
    `messages_test.go`).
- `sample_test.go` — `startSampleReplicas` starts through `process.start`; its own
  `stop` is gone (the process's); `totalsWatch` holds a `changes`; a goroutine wakes its
  waits at the top of each hour (what the wait's own hour timer did); `poll`/`pollUntil`
  gone. The doc comment of `used` sits on `used` again, `mergeWindows` keeps its own.
- `proxy_test.go` — `statusesChange`/`recordsChange` became one `changes`.

**Decisions made during the step**

- Rows stay per API with a generic spec (`passthrough[R apiRow]`), so a flag lives on
  the API's row type and the API's `check` gets its typed row; the shared loop reads no
  flag.
- The shared loop makes the union of what the three tests checked. New for some
  backends, all passing: the public-model and client-key checks now run for chat too;
  the type's credential header is checked on every backend, so Messages' and
  Responses' vllm / llama-server rows now also assert no `Authorization`. The client-key
  check is "no header carries the key" (Responses checked `Authorization`, Messages
  `X-Api-Key`); Messages keeps "no `Authorization` header at all" in its spec.
- One key per type: Responses' openai key was `sk-e2e`, now `sk-e2e-openai` like
  `TestBackendTypes`. Every scenario gets every type's key variable.
- Model metadata is one shape for all scenarios (`tools: true`, `reasoning: true`); it
  only feeds `/v1/models` metadata, which no passthrough test reads.
- Request IDs are `<backend>-<case>` with `=` replaced (request IDs allow no `=`):
  `vl-stream-false` where it was `vl-false`. Subtest names are unchanged.
- One pipe carries a process's stdout and stderr for both processes (kaiak writes
  nothing to stdout); the inherited environment drops `KAIAK_`, `OTEL_` and `INIT_CWD`
  for both (the gateway dropped the first two, the sample the first and last). The
  sample's `stop` now also checks for a race report, as the gateway's did.
- Failure messages of a wait are `<what> not seen within <limit>` and `<why ended>
  before <what>`. `waitCount` no longer prints how many lines it saw (the process log is
  printed at failure); the totals watch logs its latest totals at cleanup when the test
  failed, instead of in the wait's message.
- No fixed sleep added; `startup_test.go`'s deliberate one-second hold (a readiness
  probe's window) is untouched.

**Report vs code** (034329e line numbers): as reported, give or take a line in
`harness_test.go` (step 1 added an import). The report understates how the two
processes differed (stderr pipe vs one pipe for both streams; different environment
filters), and the totals wait also woke at the top of each hour, which `changes` alone
does not cover (the hour goroutine above). The three tests also differed in their keys,
their client-key check, and the checks they made at all (chat: no public-model or key
check).

**Test inventory** (before → after, names diffed, identical)

- `go test -list`: 40 → 40 tests; with `crosshalf` 42 → 42.
- `=== RUN` with subtests: default 120 → 120; crosshalf `^TestAcrossHalves` 19 → 19.

**Suite** — `scripts/check-all.sh`, green:

```
==> gofmt / go vet / staticcheck 2026.2.1 (gateway)
==> go test -race -count=1 (gateway): ok cmd/kaiak, e2e, accounting, auth, clip,
    config, control, limits, logattr, metrics, netfail, otlplog, provider, routing,
    schemacheck, server, sse
==> gofmt / go vet / staticcheck (live-test kit)
==> live-test kit self-test: passed for vllm, llama-server, openai, azure-openai,
    anthropic, azure-anthropic, vllm with two backends
gateway checks passed
==> npm test (control)
==> npm run lint (control)
==> cross-half e2e (sample control plane + two gateways; two cores over one store)
ok  	kaiak/e2e	65.349s
all checks passed
```
