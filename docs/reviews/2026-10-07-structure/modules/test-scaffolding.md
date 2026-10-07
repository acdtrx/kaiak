# test-scaffolding — structure review

Scope covered: `gateway/internal/fakebackend` (+ `cmd/fakebackend`, `captures/`),
`gateway/internal/fakecontrol`, `gateway/e2e` (harness and file shapes), test helpers
inside gateway packages (server, control, config, auth, otlplog, cmd/kaiak), control
test helpers (kaiak-control `fastify/`, `control-plane/`, `config/`, `messages/`,
`usage/`; sample tests), `protocol/fixtures` and both halves' loaders,
`scripts/check-*.sh`, `scripts/live` (structure only). Lighter pass: structure, not
coverage.

## Module summaries

### gateway/internal/fakebackend
- Responsibilities: a scriptable model server on loopback serving every endpoint
  (chat, completions, embeddings, Messages + count_tokens, Responses + input_tokens,
  models list) under three layouts (`/v1/`, `/openai/v1/`, `/anthropic/v1/`); records
  every request (`Requests`, `Arrivals`, `ModelsRequests`); injects faults per `Reply`.
  `cmd/fakebackend` runs it as a process (flags: profile, auth, cached-tokens, models)
  for `scripts/live -self-test`.
- Exported surface used outside: `New`, `NewAt` (cmd only), `Backend.{URL, Close,
  SetReply, QueueReplies, SetModels, SetModelsStatus, Requests, Arrivals,
  ModelsRequests}`, `Reply`, `Usage`, `Request.Canceled`, `DefaultChunks`,
  `DefaultModels`. Importers: `e2e` (18 files), `server` (26 test files), `provider`
  (5 test files), `accounting` (1), `cmd/fakebackend`; `scripts/live` builds the
  command.
- Dependencies: stdlib only; imports nothing from the gateway.
- Domain concepts encoded: API protocol wire shapes (`fakebackend.go` OpenAI chunks and
  usage, `messages.go` Anthropic events and usage split, `responses.go` Responses
  events and usage); backend URL layouts per type (`layouts`, `URL()` doc: azure types
  take the root, others `/v1`); output-limit cut per API (`HonorMaxTokens`: `length` /
  `max_tokens` / `incomplete`).
- `captures/` (recorded vLLM and llama-server answers) live here but fakebackend never
  reads them; `provider` and `accounting` tests do.

### gateway/internal/fakecontrol
- Responsibilities: a control plane for tests speaking the protocol raw (no gateway
  imports, so `internal/control`'s in-package tests can use it): config stream (replay,
  then totals, then changes), usage intake with de-duplication by per-epoch cursor and
  scripted faults (`UsageFault`: refuse, drop ack, ack other), status intake, request
  checks in kaiak-control's order, scripted totals (windows, live count) with a
  per-stream changes-only diff, outage/restart/protocol scripting.
- Exported surface used outside: `New`, `Server.{URL, Close, Publish, Restart,
  PushTotalsOnChange, HoldTotalsOnConnect, SetWindows, SetLiveGateways,
  PushCurrentTotals, PushTotals, CloseStreams, SetDown, SetProtocol, Requests, Gets,
  FailUsage, SetUsageFault, CountedRecords, UsageEvents, Statuses, StatusEvents,
  Connected, Opened}`, `Stream.{Send, SendConfig, Comment, Close, Done, Seq,
  Instance}`, `UsageFault`, `UsageEvent`, `Outcome*`. Importers: `e2e` (5 files),
  `control` (4 test files), `server/usage_path_test.go`, `cmd/kaiak` (2 test files).
  Exported but unused outside: `Totals`, `Hash`, `ConfigEvent`, `Counted`.
- Domain concepts mirrored from kaiak-control (drift surface): protocol version
  (`fakecontrol.go:158`), instance pattern (`:547`, same regex as
  `control/schema.go:15`), totals changes-only rule incl. "a dropped window still in
  its window is listed at 0" (`streamTotalsLocked`, `:335`), window currency
  (`totalsWindow.current`, `:319`), intake order and error codes (`checked`,
  `serveUsage`, `checkRecords`), body limits (`:652`). Only the crosshalf e2e checks the
  real counterpart.

### gateway/e2e
- Responsibilities: builds `kaiak` once (`TestMain`), runs it as a subprocess, waits on
  its JSON log; per-feature scenario files against fakebackend + fakecontrol; under
  build tag `crosshalf`, the real sample control plane (`sample_test.go`), a control
  proxy (`proxy_test.go`) and replicas (`replicas_test.go`).
- Harness surface (`harness_test.go`): `logLines` (`wait`, `waitCount`, `count`,
  `text`; `find` lives in `startup_test.go`), `msg` matcher, `gateway` (`get`, `post`,
  `settled`, `metric`, `waitMetric[Within]`, `stop`, `waitExit`, `signal`; `exitCode`,
  `exitError` live in `stateless_test.go`, `postMessages` in `messages_test.go`),
  env builders, `startGateway → startGatewayEnv → startGatewayIn → startProcess`,
  `newKey`, `writeJSON`, `events`.
- Config construction: `map[string]any` builders, one per scenario — `testConfig`
  (base), derived by map surgery (`groupTreeConfig`, `freeConfig`, `crossHalfConfig`,
  inline edits in `shared_test.go`), or spelled from scratch (`typesConfig`,
  `messagesConfig`, `responsesConfig`, `reliabilityConfig`).
- Domain concepts: backend type → credential header / base-URL layout / key env
  (three tables: `typedBackends`, `messagesBackends`, `responsesBackends`); log
  vocabulary (`msg("listening", "kaiak.listener.name", …)`, `gen_ai.usage.*`).

### Gateway in-package test helpers
- `server`: `testGateway` + `buildTestGateway` (`server_test.go:196`) wiring the
  whole pipeline (providers, router, recorder, limiter, `NewAPI`); variants
  `newTestGatewayWith`, `newTestGatewayBodies`, `newCircuitGateway`,
  `newCappedGateway`, `newRetryGateway`, and `newControlledGateway`
  (`usage_path_test.go:45`) which re-wires everything. Config is a string document
  (`testDocWith`, `server_test.go:78`) edited by `strings.Replace`/regex (~63 edits in
  17 files).
- `config` / `control`: shared-fixture runners (`fixtures_test.go` in each).
- `otlplog`, `cmd/kaiak`, `e2e`: one fake OTLP collector each.
- `auth`, `server`, `e2e`: one key-hash helper each (`hashOf`, `newKey`).

### control test helpers
- kaiak-control has no shared test-support module. `fastify/sse-client.ts` is the one
  shared test helper (SSE client, used by four fastify test files). Every other helper
  — fixture loading, gateway headers, `startApp`, `openStream`, `nextTotals`,
  `configNumbered`, usage-batch builders — is per file.
- `store-contract/` (public export `kaiak-control/store-contract`): the store contract
  as tests plus negative-control stores; consumers in tests build ad-hoc failing stores
  by spreading `createMemoryStore()` — idiomatic, no finding.

### protocol/fixtures and loaders
- Layout: `config/{valid,invalid,resolved}`, `messages/<kind>/{valid,invalid}`,
  `duplicate-members/` (with `cases.json`), `keys/`. `invalid/cases.json` names each
  file's kind (schema | semantic) and code.
- Go loaders: `config/fixtures_test.go`, `control/fixtures_test.go` (runners);
  `auth/keyfixture_test.go`; ad-hoc reads of `valid/minimal.json` / `full.json` in
  `cmd/kaiak`, `provider`, `control/client_test.go`. Paths hard-coded relative
  (`../../../protocol/fixtures…`).
- TS loaders: runners in `config/config.test.ts`, `messages/messages.test.ts`,
  `messages/duplicate-members.test.ts`, `config/limits.test.ts`; 15+ other test files
  compute their own `FIXTURES` path and their own reader (`readFixture`, `fixture`,
  `readJson`).

### scripts
- `check-all.sh` → `check-gateway.sh` (gofmt, vet + staticcheck incl. crosshalf tag,
  `go test -race -count=1 ./...`, then the same lint + self-test for `scripts/live`) →
  `npm test`, `npm run lint` → crosshalf e2e. Clean; no finding.
- `scripts/live`: separate module (`kaiak-live`), stdlib only. Re-implements e2e harness
  pieces (`logLines`, `msg`, `listenerURL`, `newKey`, a config builder) and the
  gateway's per-type endpoint table (`apis.go` `kindEndpoints`). Duplication is forced
  by the module boundary and is an independent statement of expectations; leave as is.

## Findings

### F1 — Server test harness re-wires the pipeline twice and carries a copy of cmd/kaiak's totals conversion that has already drifted
- **Kind** — cross-module.
- **Where** — `gateway/internal/server/server_test.go:196` `buildTestGateway`;
  `gateway/internal/server/usage_path_test.go:45` `newControlledGateway`, `:108`
  `testLimitsTotals`; `gateway/cmd/kaiak/main.go:790` `limitsTotals`, `:412`.
- **Now** — The pipeline (providers registry, router with circuits observer, usage
  sink fan-out, recorder, limiter, drain, `NewAPI(...)` with 9 positional arguments)
  is wired three times: `main.go`, `buildTestGateway`, and `newControlledGateway`
  (which differs only in the limiter, the recorder's `Batcher`, and the doc coming from
  the control plane). `testLimitsTotals` "converts the control plane's totals for the
  limiter, as the binary's wiring does" — but the binary's version is
  `limitsTotals(t control.Totals, complete bool)` setting `Complete: complete`; the
  test copy never sets `Complete`. So every controlled-gateway test feeds the limiter
  changes-only totals, and the complete-totals branch of
  `limits/shared.go:70-80` (reset of pushed windows, re-apply of every counter) never
  runs on that path.
- **How it got here** — Both copies started at 0.7.3. `2a8590c` ("totals complete then
  changes", 2026-10-07) added `Complete` to the production function; the test copy was
  last touched in `d5c45cd` and was not updated. The wiring copy in
  `newControlledGateway` exists because `buildTestGateway`'s options grew one
  constructor at a time (`newTestGatewayWith` for the limiter, `newTestGatewayBodies`
  for bodies) and there was no slot for a batcher.
- **Proposed shape** — (a) One conversion, importable by both: e.g. a method on
  `control.TotalsUpdate` returning `limits.Totals` (control may import limits; neither
  imports the other today), used by `main.go:412` and the test. Better still, see the
  cross-module hint on the `TotalsWindow`/`PushedWindow` mirror. (b) One
  `buildTestGateway(t, testOptions{limiter, bodies, batcher, holder})`; the four
  `newTestGateway*` constructors become one-line option sets, `newControlledGateway`
  keeps only the control-client setup.
- **Payoff** — removes the drifted copy (one bug class gone); ~25 lines of wiring;
  a `NewAPI`/`RecorderOptions` change touches 2 places instead of 3.
- **Cost / risk** — small; test-only plus one exported helper in `control` (or
  `limits`). Fixing `Complete` may surface a test that relied on changes-only
  behaviour.
- **Confidence** — high (both functions read; history confirms).

### F2 — Three parallel per-API passthrough scenarios in e2e, each with its own backend-type table and type switch
- **Kind** — cross-function.
- **Where** — `gateway/e2e/backendtypes_test.go:21` `typedBackends`, `:47`
  `typesConfig`, `:81` `TestBackendTypes`; `messages_test.go:31` `messagesBackends`,
  `:45` `messagesConfig`, `:126` `TestMessages`; `responses_test.go:25`
  `responsesBackends`, `:40` `responsesConfig`, `:98` `TestResponses`.
- **Now** — Each file declares a struct table `{name, typ, model, deployed, path,
  header, credential, <per-API flag>}` and a config builder that loops it with the same
  switch: `openai`/`anthropic` → `api_key_env`, `azure-*` → `base_url` without `/v1`
  plus `api_key_env`. `messagesConfig` and `responsesConfig` are the same function
  (same `model(...)` closure, `"old"` backend, `pair`/`rpm` models, `w`/`tight`
  groups, `k`/`k-tight` keys) differing in one backend and one capability flag. The
  test bodies `TestMessages` (lines 140–185) and `TestResponses` (112–160) are the
  same loop: post per backend × stream, check public model name, settled log line,
  one backend request, path, deployed model, credential header, client key not
  leaked, service tier.
- **How it got here** — `TestBackendTypes` came with backend types (v0.9.0);
  `messages_test.go` and `responses_test.go` came together in the messages-responses
  plan (2026-10-06), the second copied from the first.
- **Proposed shape** — One table keyed by backend type: credential header, credential
  value, key env, URL layout (`/v1` vs root) — the facts that belong to the type. One
  `backendEntry(typ, fakeURL)` and one scenario config builder taking the backends to
  include. One shared per-backend passthrough check parameterised by an API spec
  (`path`, request body, stream end marker, extra assertions such as
  `anthropic-version` or `store:false`); per-API flags (`forcesTier`,
  `standardOnly`, `countsTokens`) stay with the API's row.
- **Payoff** — ~120–150 lines. Adding a backend type: 3 tables + 3 switches → 1 row.
  Adding an API protocol: a new file of ~250 lines → one spec plus its specific
  checks.
- **Cost / risk** — medium, test-only, three files. Risk of over-sharing the loop;
  keep API-specific assertions in the spec rather than flags.
- **Confidence** — high on the duplication; medium on how far to share the loop.

### F3 — The shared-fixture runner convention is implemented four times, plus a kind switch duplicating the decoder map
- **Kind** — cross-module.
- **Where** — Go: `gateway/internal/config/fixtures_test.go:24-75`
  (`invalidCase`, `fixtureFiles`, `readCases`) and `:158` `TestInvalidFixtures`;
  `gateway/internal/control/fixtures_test.go:74-125` (identical helpers) and `:173`
  `TestInvalidMessageFixtures`; duplicate-member runners in both files with different
  local case types; `control/fixtures_test.go:366` `decodeByKind` vs `:53` `decoders`.
  TS: `control/kaiak-control/src/config/config.test.ts:25-50` and
  `messages/messages.test.ts:36-65` (identical `readJson`, `fixtureFiles`,
  `readCases`, `InvalidCase`).
- **Now** — `fixtureFiles` and `readCases` are byte-identical across the two Go
  packages and across the two TS files; the "cases.json entries equal the files, then
  each invalid file yields exactly `[schema]` or `[code]`" loop is written twice per
  half. In `control/fixtures_test.go`, adding a message kind means a `decoders` entry
  and a `decodeByKind` case (the latter exists because `decoders` returns codes, not
  the error whose `Issues[0].Path` the duplicate-member test needs);
  `TestEveryFixtureKindHasADecoder` guards only the map.
- **How it got here** — the config runner came first; the message runner was cloned
  from it when message fixtures arrived; duplicate-member fixtures later added a third
  runner split across both packages.
- **Proposed shape** — Go: a small test-only package (non-`_test` file, like
  `fakebackend`), e.g. `internal/fixturetest`, with `Files(t, dir)`,
  `InvalidCases(t, dir)`, `RunInvalid(t, dir, codes func([]byte) []string,
  schemaCode string)` and `DuplicateCases(t)`; `decoders` holds error-returning
  decoders so codes and issue paths come from one place and `decodeByKind` goes. TS:
  the same three helpers in the test-support module of F4. Optional, larger: treat
  `config` as one more kind under the messages layout so one runner per half covers
  all.
- **Payoff** — ~80 Go lines, ~40 TS lines; adding a message kind 2 places → 1; the
  cases.json rules stated once per half.
- **Cost / risk** — small; test-only; no fixture moves unless the optional layout
  change is taken.
- **Confidence** — high.

### F4 — kaiak-control tests: no shared test support; protocol version literal ×17, usage-record builders ×6, files named after review rounds
- **Kind** — cross-module (control).
- **Where** — `control/kaiak-control/src/fastify/{fastify,round-2,round-3,
  status-totals,usage-route}.test.ts` (`startApp` ×5, `openStream` ×4, `nextTotals`
  ×4, `configNumbered` ×2, fixture reader ×5, gateway headers ×5);
  `control-plane/{control-plane,review,round-3}.test.ts`, `usage/usage.test.ts`,
  `fastify/round-{2,3}.test.ts` (usage batch/record builders spelling every field and
  every unit); `"kaiak-protocol": "5"` in 9 files / 17 sites although
  `PROTOCOL_VERSION` is exported (`protocol/index.ts:11`).
- **Now** — Every fastify test file re-creates Fastify + plugin + listen + address
  assertion + cleanup list, the SSE open with gateway headers, and a "skip to next
  config/totals" loop. Usage records are built inline six times with all
  `units` keys. Regression tests are filed by review round (`round-2.test.ts`,
  `round-3.test.ts` ×2, `review.test.ts`; gateway side `review_test.go` in
  accounting, provider, server, `inbound_review_test.go`), so the tests of one
  behaviour (e.g. totals pushing) sit in `status-totals`, `round-2` and `round-3`.
- **How it got here** — each review round (docs/reviews/2026-10-07/AUDIT-2/3) landed
  its regression tests in a new file with its own copy of the helpers. Protocol bumps
  were applied by hand (`7d51838` edited the literal in 7 control test files).
- **Proposed shape** — `kaiak-control/src/test-support/index.ts` (an ordinary
  subsystem for the boundary lint, not in package `exports`): `fixture(rel)`,
  `gatewayHeaders(instance)` built from `PROTOCOL_VERSION`, `startApp(core, opts)`,
  `openStream(base, instance)`, `nextConfig`/`nextTotals`, `usageBatch(overrides)`
  based on `protocol/fixtures/messages/usage-record/valid/one-group.json`. Move
  `sse-client.ts` there. Then fold the round-N/review tests into the files of their
  subject (gateway: same for the `review_test.go` files).
- **Payoff** — ~200 lines; protocol bump 17 test sites → 0; a new usage unit 6
  builders → the fixture; one place to find a behaviour's tests.
- **Cost / risk** — medium, test-only; the folding step is mechanical but touches
  ~1,100 lines of tests (do it per subject, not in one go).
- **Confidence** — high.

### F5 — Server tests edit a string config with unguarded text anchors
- **Kind** — cross-function.
- **Where** — `gateway/internal/server/server_test.go:78` `testDocWith`; edits in 17
  files (e.g. `retry_test.go:52` `withGlobal`, `circuit_test.go:23`,
  `retry_test.go:26` regex on `"local-b": \{[^}]*\}`, `limits_test.go:22,25,158,233`).
- **Now** — ~63 `strings.Replace(doc, anchor, …, 1)` edits anchored on exact text
  (`"max_request_body_bytes": 1024 }`, `"research": {}`, `"pair": {`). A missing
  anchor leaves the document unchanged without failing; chained edits on the same
  anchor (`newCircuitGateway` rewrites `"max_request_body_bytes": 1024 }`, so a
  `withGlobal` edit after it would match nothing) are order-sensitive. The
  `newTestGateway` + `testSnapshotWith` + `holder.Swap` + `router.Configure` sequence
  repeats in `newCircuitGateway`, `newCappedGateway`, `newRetryGateway`. The e2e uses
  a second idiom (maps with type-asserted surgery, e.g. `shared_test.go:33-34`).
- **How it got here** — each feature added its anchor edit to the one base document.
- **Proposed shape** — cheapest: `replaceOnce(t, doc, old, new)` that fails when the
  anchor is absent, and a `g.apply(t, edit)` method for the swap+configure sequence.
  Larger, optional: edit a decoded map (`edit func(cfg map[string]any)`) in both
  server and e2e, sharing small builders (`chatModel(deployments...)`,
  `backendEntry`) — overlaps F2.
- **Payoff** — removes a silent-failure mode; ~15 lines of repeated apply code.
  The larger option removes formatting coupling from ~63 edits.
- **Cost / risk** — cheap option small; map option medium (17 files).
- **Confidence** — medium: no edit was found that matches nothing today; raise by
  running the suite with `replaceOnce` in place.

### F6 — e2e harness primitives duplicated inside the e2e package
- **Kind** — cross-function.
- **Where** — `gateway/e2e/harness_test.go:257` `startProcess` vs
  `sample_test.go:541` `startSampleReplicas` (+ `:605` `stop`); wait loops
  `harness_test.go:98` `logLines.wait`, `:124` `waitCount`, `sample_test.go:706`
  `totalsWatch.wait`, `proxy_test.go:202` `waitRecord`, `:232` `waitStatus`; polls
  `harness_test.go:422` `waitMetricWithin` vs `sample_test.go:836` `poll`/`pollUntil`.
- **Now** — Starting a logged child process (env filtered of `KAIAK_*`, log read into
  `logLines`, `exited` + `err`, cleanup kill and log dump on failure, SIGTERM + wait
  within `waitLimit`) is written twice. Five waits hand-roll the same "closed-and-
  replaced change channel + deadline timer" loop over four separate change channels.
  Two polling loops exist because `poll` sits in a `crosshalf`-tagged file the default
  build cannot see. Harness methods are spread over feature files (`logLines.find` in
  `startup_test.go`, `gateway.exitCode/exitError` in `stateless_test.go`).
- **How it got here** — the sample process and the proxy arrived with the crosshalf
  test and copied the gateway pattern; later features added their helpers where first
  needed.
- **Proposed shape** — a `process` type (cmd, logs, exited, err; `start`, `stop`,
  `waitExit`) embedded by `gateway` and `sampleProcess`; a `changes` type
  (`notify()`, `wait(t, what, limit, check func() bool)`) used by `logLines`,
  `totalsWatch` and `controlProxy`; `poll` moved to the harness and used by
  `waitMetricWithin`; harness methods moved into `harness_test.go`.
- **Payoff** — ~90 lines; one place for the wait/timeout discipline the harness
  comment promises ("never fixed sleeps").
- **Cost / risk** — small–medium, test-only.
- **Confidence** — high.

### F7 — Three fake OTLP collectors
- **Kind** — cross-module.
- **Where** — `gateway/internal/otlplog/exporter_test.go:31` `collector`;
  `gateway/cmd/kaiak/logexport_test.go:52` `fakeCollector`;
  `gateway/e2e/logexport_test.go:149` `otlpCollector` (+ decode types `:31-71`).
- **Now** — three httptest servers decoding OTLP/HTTP JSON, answering request n as
  scripted, keeping records or bodies; three different decode shapes (otlplog's own
  `exportRequest`, an inline anonymous struct, e2e's `otlpExport`).
- **How it got here** — one per layer as the OTel log export plan reached it.
- **Proposed shape** — `internal/fakeotlp` beside `fakebackend`/`fakecontrol`:
  `New(t, answer func(n int, r *http.Request) int)`, `Records()`, `Bodies()`,
  `Next(t)`, with its own independent decode types (e2e's), not otlplog's encoder
  types — the independence is what makes it a check.
- **Payoff** — ~60 lines; one OTLP decode shape to maintain.
- **Cost / risk** — small; otlplog's tests that assert on `exportRequest` fields would
  switch to the fake's types.
- **Confidence** — medium (worth it only if a fourth user appears or the OTLP shape
  changes).

### F8 — fakebackend `Reply`: fault knobs grown one field per feature
- **Kind** — in-function.
- **Where** — `gateway/internal/fakebackend/fakebackend.go:25-96` `Reply`; `:332-352`,
  `:527` `interrupt`; `messages.go:71-74, 86-89`; `responses.go:97-100, 113-116`.
- **Now** — 22 fields. Three mutually exclusive before-body bools
  (`StallBeforeFirstByte`, `CutBeforeBody`, `StallBeforeBody`); four mid-stream faults
  with their own index fields (`HangAfter` + `PingEvery`, `CutAfter`, `EndAfter`,
  `ErrorEvent` + `ErrorEventAfter` + `ErrorEventCode`) with inconsistent zero
  meaning (`HangAfter: 0` = off, `ErrorEventAfter: 0` = before the first event). The
  error event is handled outside `interrupt`, twice in each of the Messages and
  Responses writers.
- **How it got here** — each reliability/timeout/passthrough feature added its knob.
- **Proposed shape** — `Before BeforeFault` (enum: stall first byte, cut body, stall
  body) and `Fault *StreamFault{At int; Kind: Hang|Cut|End|ErrorEvent; Code string;
  PingEvery time.Duration}`; `interrupt(i)` handles every kind including the error
  event (with a per-API error payload func).
- **Payoff** — 10 fields → 2; ~15 lines in the writers; one zero rule.
- **Cost / risk** — ~60 call sites in tests change; no behaviour change.
- **Confidence** — medium; low payoff — do it only when the next fault is added.

### F9 — Recorded captures live in fakebackend but are read by two other packages through copied path helpers
- **Kind** — cross-module.
- **Where** — `gateway/internal/fakebackend/captures/`;
  `gateway/internal/provider/messages_test.go:20` `captured`;
  `gateway/internal/accounting/messages_usage_test.go:16` `recordedStreamMeter`
  (same `filepath.Join("..", "fakebackend", "captures", server, name)`).
- **Now** — fakebackend never reads the captures (its answers are synthetic); the
  readers know its directory layout by relative path.
- **Proposed shape** — `fakebackend` embeds them (`//go:embed captures`) and exports
  `Captured(server, name string) []byte`; both tests use it. This also makes a later
  "fake shape vs capture" test cheap to write.
- **Payoff** — two path helpers → one; directory knowledge in its owner.
- **Cost / risk** — trivial.
- **Confidence** — high; payoff small.

### F10 — fakecontrol: dead exported surface
- **Kind** — in-module.
- **Where** — `gateway/internal/fakecontrol/fakecontrol.go:261` `Totals` (no caller
  anywhere), `:508` `Hash`, `:514` `ConfigEvent`, `:435` `Counted` (callers only
  inside the package).
- **Proposed shape** — delete `Totals`; unexport the other three.
- **Payoff** — 4 exported names gone. **Cost** — trivial. **Confidence** — high
  (grep over `gateway/` and `scripts/`).
- Leave as is: the duplicated `protocolVersion = "5"` (`:158`) — fakecontrol cannot
  import `control` (control's in-package tests import fakecontrol), and a mismatch
  fails every control test at once, so it is self-detecting.

Modules judged clean enough to leave: `scripts/check-*.sh`; `scripts/live` (duplication
forced by its own module); fakecontrol's overall shape (four totals delivery switches
are each used and map to distinct protocol situations); store-contract consumers.

## Cross-module hints

- `control.TotalsWindow{Group, Type, WindowStart, Used}` and
  `limits.PushedWindow{Group, Type, Start, Used}` mirror each other field by field,
  converted in `cmd/kaiak/main.go:790` (and copied in tests, F1). One type would drop
  the conversion — for the control/limits reviewers.
- Key-hash rule `"sha256:" + hex(sha256(key))`: `internal/auth/auth.go:66` plus test
  copies `auth/auth_test.go:14`, `server/server_test.go:35`, `e2e/harness_test.go:487`
  and `scripts/live/config.go:128`. An exported `auth.KeyHash` would serve the gateway
  module's three.
- "Azure types take the resource root, others `/v1`" base-URL rule is stated in
  `fakebackend.URL()` doc, three e2e switches (F2), `server_test.go` doc,
  `scripts/live/config.go`, and implemented in the provider modules.
- `scripts/live/apis.go` `kindEndpoints` restates the gateway's endpoint support per
  type (GATEWAY.md, Providers → Endpoint support); the self-test catches drift.
- fakecontrol restates kaiak-control's totals changes-only rule and intake order
  (`fakecontrol.go:331`, `:657`); `e2e/sample_test.go:807` `mergeWindows` restates the
  gateway's changes-only merge. Only the crosshalf e2e ties them to the real thing.
- SSE reading: production `internal/sse`; ad-hoc parsers in `e2e/harness_test.go:474`
  `events`, `control/sample/src/page/events.test.ts:118` `openPageStream`; test client
  `kaiak-control/src/fastify/sse-client.ts`.
- Log vocabulary (`listening` + `kaiak.listener.name`, `request` + `kaiak.request.id`,
  `gen_ai.usage.*`) is matched in e2e, `scripts/live/process.go` and
  `cmd/kaiak` tests: a rename touches all three.

## Bugs noticed in passing

- `gateway/internal/server/usage_path_test.go:108` `testLimitsTotals` drops
  `Complete` (see F1): controlled-gateway tests never exercise complete totals.
- `gateway/e2e/sample_test.go:802-803`: the doc comment of `used` sits above
  `mergeWindows`'s own comment (`:807`); `used` (`:825`) is left without one.
