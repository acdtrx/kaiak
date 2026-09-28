# Step 5 — circuits and metrics

**Status:** done (2026-09-25) — suite green (phase 3 continues with step 6)

## Items

- **E4** half-open trial decided at the first event (a 2xx whose first event arrived
  closes; later failures count normally).
- **N-C2** non-stream hung backends: a response timeout counts as a failure when it was
  the trial or when several arrive in a row (decide N); keep probing while half-open.
- **E8** only SSE data events reset the stall timer.
- **E5** every counter with a known label set pre-created at 0 at startup and on config
  apply (attempts per configured deployment × outcome, config loads per trigger ×
  result, queue rejections, circuit transitions, retries, errors).
- **N-P2** `n_too_large` → `invalid_request`; the mapping test covers every code. **Done in step 3** (the mapping test now reads the Client API table from GATEWAY.md).
- **N-P3** the config-mismatch refusal raises `kaiak_control_outage` (or its own gauge —
  decide) and is named in the docs.
- **N-C3** dispatch cache keyed by model pointer too.

## Acceptance

Failing-first tests per item.

## Result

Each item had a test that failed on the old code first (behavior failures, after
no-op stubs of the new API).

- **E4 half-open decided at the first event** (`routing/circuit.go`, `routing.go`,
  `server/upstream.go`): `Slot.ResponseStarted()`, called by the relay at the first
  event (stream block, or non-stream body bytes) under a status below 400, ends the
  trial as a success — circuit closed, deployment eligible — while the request runs
  on; its final outcome then reports as an ordinary one (the trial number no longer
  matches). Tests: `TestHalfOpenTrialIsDecidedWhenItsResponseStarts` (failed: "still
  open after the trial's response started"), `TestResponseStartedOutsideATrialChangesNothing`
  (guard), server `TestLongStreamingTrialDoesNotBlockItsDeployment` (paced stream as the
  trial; a second request is served mid-stream — failed: "circuit still open after the
  trial's first event").
- **N-C2 hung non-stream backends**: new `routing.ResponseTimeout` outcome for a
  response timeout before the first bytes (after them: still neutral). A failure for
  the half-open trial; otherwise counted in a run (reset by a success only) and from
  the **3rd in a row** each counts as a failure toward the threshold
  (`responseTimeoutsAsFailure`). The prober keeps running while any circuit of the
  backend is open or half-open; a probe success leaves half-open circuits half-open, a
  failure re-opens them (log `circuit opened` with the probe trigger, transition
  `open`); the prober stops when a trial closes the last one. `probe succeeded` logs at
  debug when it half-opened nothing. Tests: `TestResponseTimeoutsInARowCountAsFailures`
  (failed), `TestHalfOpenTrialResponseTimeoutReopens`, `TestProberKeepsProbingHalfOpenCircuits`
  (failed: "no probe"), server `TestResponseTimeoutsOfAHungBackendOpenItsCircuit`
  (failed: "not open after three response timeouts"). `TestProberLifecycle` and
  `TestProbeKeepsUnlistedDeploymentsOpen` updated for the new prober life (they asserted
  the prober stopped at half-open — the behavior this item changes; now they assert it
  stops when the trial closes the circuit).
- **E8 stall timer** (`provider/openai.go`): the stall timer runs for what is left of
  the stall timeout since the last data event (silence summed across comment blocks,
  still paused while writing to the client); comments are still relayed. fakebackend
  gained `Reply.PingEvery`. Test `TestKeepAliveCommentsDoNotHoldOffTheStallTimer`
  (`: ping` every 40 ms, stall 150 ms; failed: "stream still open after 2 s of pings").
- **E5 series at 0** (`metrics/registry.go` `HistogramVec.Prepare`, `ops.go`,
  `cmd/kaiak/main.go`): at startup config loads for all 5 triggers × 2 results (errors,
  delivery counters and clamped were already at 0); on every applied config (in
  `Ops.ConfigLoaded`, and in `NewOps` for a holder already filled) per model queue
  rejections × reason, queue-wait and attempts histograms; per deployment attempts × 12
  outcomes; per model × deployment backend retries × 6 reasons, TTFT and decode-rate
  histograms; per backend attempt-duration histogram; `Circuits.PrepareSeries` (called
  by the applier) transitions × 3 and probes × 2. `CountRetry` now panics on an unknown
  reason, like the other fixed label sets. Test e2e `TestSeriesStartAtZero` (fresh
  gateway: every series at 0; a 500 on a then retry moves attempts/retries 0 → 1; a
  rejected SIGHUP moves config loads 0 → 1; a reload adding backend c + model chat-c
  creates their series at 0) — failed with every series absent.
- **N-P3** (`limits/shared.go` `ConfigMismatch()`, `metrics/control.go`, `main.go`):
  gauge `kaiak_control_config_mismatch`, 1 from the start of the mismatch (before the
  grace). Tests: `TestTotalsOfAnotherConfigKeepTheSpentBudget` extended (failed: "no
  mismatch reported"), `TestControlStateMetrics` extended (failed: gauge missing).
- **N-C3** (`routing.go` dispatch): the per-round first-attempt cache is reset when the
  waiter's model pointer differs from the cached one. Test
  `TestFreedSlotReachesTheNewSnapshotsWaiter` (the reviewer's reproduction; failed:
  "queued request never got an answer").
- **Docs**: GATEWAY.md — health summary, outcome-class table (response timeout split),
  the hung-backend exception, half-open (E4, settled), probes while half-open, log
  lines, `response_timeout_ms` / `stall_timeout_ms` (E8), metric list
  (`kaiak_control_config_mismatch`), usage-delivery alert text, Client API
  `budget_unavailable` row, totals-follow-their-config (N-P3), upstream-attempts
  outcomes, "Series at 0" (E5, with exceptions). CONTROL-PROTOCOL.md outage section
  (mismatch: same refusal, own gauge). DEPLOYMENT.md timeout table rows, alert rows
  "Totals for another config" (now the gauge), "Budget refusals", "Circuit flapping".
  ARCHITECTURE.md routing paragraph.
- **Suite**: new tests `-race -count=5` green. `GOFLAGS=-count=1 scripts/check-all.sh`
  → `all checks passed` (gateway gofmt/vet/staticcheck/race tests incl. e2e, live-kit
  self-test 13/13/13/16, control 445 tests + lint, cross-half e2e).

## Decisions made

- The trial's "first event" is any block (as for the first-event timeout), or the first
  non-stream body bytes; only under a status below 400 (the success class).
  **Revised in step 6** (main session): the trial is decided at its first **data**
  event — a comment block (`: ping`) no longer closes the circuit, consistent with E8.
- Run length N = 3, a constant (not config). The run is broken only by a success;
  neutral outcomes and failures in between do not reset it. From the 3rd each response
  timeout adds one failure toward `failure_threshold` (it does not open outright).
- Only a response timeout **before** the first bytes takes part; one after the first
  bytes stays neutral (the backend was sending).
- "Stuck half-open" resolution: the prober keeps probing half-open circuits; success
  keeps them half-open (still reported open — no trial, no close), failure re-opens
  them, including one whose trial is running (that trial's later outcome then counts
  against an open circuit: nothing).
- N-P3: a separate gauge rather than folding into `kaiak_control_outage` (the control
  plane is reachable; the remedy is the config). It reads 1 from the moment the mismatch
  starts, not only past the grace; alert on it held 5 min. Refusal code unchanged.
- E5 exceptions: usage metrics (owner/key labels from traffic — cardinality) and
  `kaiak_request_duration_seconds` (status class from the answer) stay on first use;
  removed deployments' series stay until restart. All 5 config-load triggers are
  created in both modes. No body/header refusal family exists (errors classes cover
  bodies; 431 comes from net/http, uncounted).

## Decisions for the user to confirm

1. Response-timeout run length 3, fixed, counted toward the threshold (a default
   threshold of 5 opens at the 7th response timeout in a row).
2. A failed probe re-opens half-open circuits, even one with a trial under way.
3. A half-open circuit with no traffic stays half-open (reported open,
   `kaiak_circuit_open == 1`) indefinitely while its models list answers — the
   "Circuit open for 5m" alert keeps firing for an idle recovered deployment.
   **Resolved in step 6**: half-open is reported `half_open` in status and
   `kaiak_circuit_half_open` in metrics; `kaiak_circuit_open` reads 0 for it.
4. `kaiak_control_config_mismatch` is 1 from the start (lead time), not only past the
   grace.
5. `kaiak_request_duration_seconds` is not pre-created (status class unknown).
