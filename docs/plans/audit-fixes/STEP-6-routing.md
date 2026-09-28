# Step 6 — routing

**Status:** done (2026-09-25). Phase 2 complete: `scripts/check-all.sh` green.

## Items

- **H8** backend 404 naming the model = deployment failure (retry, circuit); probe
  checks the model is listed in `/models`; background warning at config apply.
- **D3 (M1)** control-plane mode: each gateway enforces `ceil(max_in_flight ÷ live)` per
  backend from the pushed live count; file mode unchanged; spec + guide.
- **M3** dispatch: cache `canServe` per model per round; benchmark before/after.
- **L1** retry budget per model (retries ≤ 20% of recent attempts), relay Azure
  `retry-after-ms`, short jittered delay before a same-deployment retry.
- **L10** prune small maps in `Configure`; **L11** clamp the injected output default to
  context − input estimate (floor 256); **L12** half-open: one trial request before
  full close; **L15** one circuit sample per distinct deployment.

## Acceptance criteria

- Tests per item; phase 2 end: `scripts/check-all.sh` green.

## Result

Each regression test was run before its fix and failed (or did not compile against
the missing API), then passed.

- **M3** (`c50cdfb`): the dispatcher asks `canServe` once per model and round for
  first attempts (empty avoid set); retries keep per-waiter checks.
  `BenchmarkDispatchFullScan` (`routing/dispatch_bench_test.go`: 5 models × 20
  deployments on 20 shared backends, all full, 500 first attempts waiting per model —
  the no-free-slot round every release ends with), Apple M5 Max:
  **before ~615 µs/op** (619 948, 608 905, 619 074 ns), **after ~7 µs/op**
  (10 057, 7 285, 6 823 ns; unchanged at the end of the step).
- **L15, L10** (`021fb54`): `kaiak_circuit_open` emits one sample per distinct
  deployment (`TestCircuitSampleOncePerDeployment`: 2 samples before). `Configure`
  forgets the turn counters of removed models (`TestConfigureForgetsRemovedModelsTurns`);
  `provider.Registry.Retain` drops the pools of removed backends, called on every
  apply (`TestRetainDropsRemovedBackendsPools`).
- **D3** (`76a6ed0`): `Router.SetLiveGateways` (fed from `Limiter.LiveGateways()` on
  every totals message and after the restore at boot; file mode never calls it),
  share `max((cap+live−1)/live, 1)`; a raised share dispatches at once.
  `kaiak_backend_max_in_flight` shows the share (`Router.MaxInFlightByBackend`);
  status keeps the configured cap. Tests: `TestCapIsSplitAmongLiveGateways` (4÷2 → 2,
  5÷2 → 3, 1÷2 → 1, a waiter served when the share rises), e2e
  `TestSharedLimitsAcrossGateways` subtest (2 live gateways, cap 4 → metric 2 on both).
  Found while wiring: the split was applied after the totals file's fsync; it now
  applies before the save.
- **L12** (`c43b2df`): a successful probe makes a circuit half-open (still reported
  `open` in status and `kaiak_circuit_open`); one trial at a time (a trial id on the
  slot and circuit); trial success closes (`circuit closed`, trigger `trial`), failure
  re-opens at once and restarts the prober, neutral or a release without a report
  lets the next request try; `kaiak_circuit_transitions_total` gains `to="half_open"`.
  Tests: `TestHalfOpenAdmitsOneTrialWhoseSuccessCloses`,
  `TestHalfOpenTrialFailureReopens`, `TestHalfOpenTrialWithoutAVerdictLetsAnotherTry`,
  `TestQueuedRequestGetsTheTrialSlot`; updated as the behavior requires:
  `TestProbeSuccessMakesEveryOpenCircuitOfTheBackendHalfOpen` (was “…Closes…”),
  `TestProberLifecycle`, server `TestOpenDeploymentIsSkippedAndProbedBackIn`, e2e
  `TestRetryOnAnotherDeploymentAndCircuit` flow (waits for `circuit half-open`, then the trial's
  `circuit closed`), the live kit's `failover` check and `LIVE-BACKENDS.md`.
- **H8** (`0b2a9b3`): provider classifies a `404` whose error message names the
  deployment's backend-side model as a whole word, or whose code is
  `model_not_found` / `DeploymentNotFound`, as `upstream_model_missing` (502, not
  relayed); other 404s are relayed whole (the read part is put back). Server: retry
  reason `model_missing`, that deployment refused for the request, circuit failure,
  no usage record (meter `Refused`), error class `upstream_error`. Probe returns
  which models the backend lists (`data[*].id`; azure-openai unchecked; a 2xx without
  a models list fails); `ProbeNow` half-opens only listed deployments, warns once per
  open period for an unlisted one, and the prober keeps going. `routing.ModelChecker`
  (latest config wins, 8 backends at once, run by `cmd/kaiak`) warns per deployment
  whose model is not listed. fakebackend `SetModels` / `-models`; the kit's fakes list
  its self-test models. Tests: `TestProbeReportsTheListedModels`,
  `TestModelMissingAnswerIsAnError` (vLLM new and old shapes, OpenAI code, Ollama;
  not matched: another 404, a longer model name, non-JSON), `TestProbeKeepsUnlistedDeploymentsOpen`
  (with the listing check off: the prober stopped, “no probe”), `TestModelCheckWarnsPerMissingModel`,
  server `TestRetrySucceedsOnTheOtherDeployment/wrong model on the host`,
  `TestNotRetried` (single deployment → 502 once; unrelated 404 relayed),
  `TestOutcomeClassification` row, `TestOpenDeploymentIsSkippedAndProbedBackIn`
  (a probe not listing pair-b keeps it out), e2e `TestGatewayEndToEnd` subtest (the
  apply-time warning; timed out with the check off).
- **L1** (`4b37f84`): per-model retry budget (10 s sliding window of 1 s buckets,
  retries ≤ max(10, 20% of attempts); past it `retry_refused: "retry_budget"`, the
  attempt answers); `Retry-After-Ms` relayed; a retry landing on the deployment that
  just failed pauses 100–300 ms (random, holding its slot; client gone during it =
  canceled). Tests: `TestRetryBudgetWindow`, `TestRetryBudgetStopsRetries`,
  `TestRetryOnTheSameDeploymentPausesFirst` (failed with the pause off),
  `TestNotRetried` 429 subtest checks `retry-after-ms` (failed before).
- **L11** (`4d9b833`): injected default = `min(default, max(context_length − input
  estimate, 256))`; client values untouched. `TestInjectedOutputDefaultFitsTheContext`
  (300 / 275 / 256 floor / client 300 untouched; failed before).
- Docs: `GATEWAY.md` (wrong model on a host, Client API row, retry and outcome rows,
  retry budget, same-deployment pause, headers, caps across gateways, half-open,
  probe reads the list, metrics), `CONTROL-PROTOCOL.md` (live count splits caps;
  status `max_in_flight` is the configured value; half-open reported `open`),
  `ARCHITECTURE.md`, `LIVE-BACKENDS.md`.

Suite: `GOFLAGS=-count=1 scripts/check-all.sh` → `all checks passed` (gofmt, vet,
staticcheck, gateway race tests incl. e2e, live-kit lint + self-test — `failover`
passes with half-open —, control 436/436, lint, cross-half e2e). No expected reds.
Phase 2 is complete.

## Decisions for the user to confirm

1. **H8 client answer**: `502 upstream_model_missing` (new code, class
   `upstream_error`), the backend's `404` not relayed — parallel to
   `upstream_auth_failed`. Alternative: relay the backend's `404` when no retry is
   left.
2. **H8 matching**: message naming the model as a whole word, or code
   `model_not_found` / `DeploymentNotFound`. The code match goes beyond “message names
   the model” (OpenAI's message names the requested model, Azure's names none).
3. **H8 probe on azure-openai is not model-checked** — Azure's `/openai/v1/models`
   lists models, not deployments, so checking would keep every Azure circuit open.
   A 2xx probe answer without a models list now fails the probe (openai-compatible).
4. **llama-server** (ignores the request's model name) must be configured with a name
   its `/models` lists, or its circuit never leaves open after a failure; the
   apply-time warning shows the mismatch.
5. **D3 status**: no `max_in_flight_share` field; status keeps the configured cap, the
   metric shows the share (no protocol change).
6. **L12 status**: half-open reported as `open` (no `half_open` in the status
   protocol); the transitions metric and the log carry it. A half-open deployment
   with its trial under way is ineligible, so a model whose other deployments are open
   answers `503 no_healthy_deployment` meanwhile.
7. **L1 budget numbers**: 10 s window, 20%, minimum 10 retries per window, per model
   per gateway. Only `retry-after-ms` relayed (not Azure's `x-ratelimit-*`, which
   would collide with the gateway's own headers).
8. **L1 pause**: 100–300 ms, taken after the slot is acquired (the slot is held
   during the pause).
9. **L11 floor**: 256, and the lowered value is the effective output limit the
   reservation counts.

## Deviations

- The e2e and kit flows around circuit recovery changed (half-open before closed):
  tests updated as the new behavior requires, not weakened.
- The retry-heavy server tests now take longer (≈ +9 s for `internal/server`) because
  single-deployment retries pause 100–300 ms.
