# Step 2 — routing (D3, D4, D5)

**Status:** D3 and D4 done (2026-09-25); D5 pending the user's confirmation — not started

- **D3** a deployment that answered 429 cools down for `min(Retry-After|Retry-After-Ms,
  60 s)`, default 5 s without a header: not eligible for new first attempts while another
  deployment of the model is eligible; with none, still used (the client gets the 429
  honestly). Circuit neutrality kept. Tests: [A]'s experiment and [B]'s probe (a
  throttled deployment no longer takes most first attempts; spill-over succeeds).
- **D4** failover-only retries: never retry on the same deployment; a retry must pick a
  different deployment of the model (within `max_attempts`); a single-deployment model
  returns the error to the client. Remove the same-deployment pause. The retry budget
  still bounds failovers. Update specs, tests (`TestSingleDeploymentIsRetriedOnItself`
  becomes "not retried").
- **D5** (pending user confirmation) `max_concurrent_requests_per_key` off by default
  (unset/0 = no limit), both halves; docs explain the protections that remain.

## Result (D3, D4)

- **D3 429 cooldown** — `routing`: per-deployment cooldowns (`Slot.Throttled(d)`,
  `Router.CoolingDown()`), kept like circuits (dropped with the deployment on reload,
  ignored for deployments the applied config lacks). A cooling deployment is usable
  only when no eligible, non-refused deployment of the model is warm — the same
  "prefer, else fall back" slot the untried preference held, so the dispatcher and
  queued requests respect it with no extra path. One `time.AfterFunc` per cooldown
  ends it and dispatches (a queued request gets the deployment back at once); a
  starting cooldown dispatches too (the last warm one cooling makes the cooling ones
  usable). `server`: every attempt answered `429` calls `Throttled` with
  `throttleCooldown(headers)` — `Retry-After-Ms` (float ms) when readable, else
  `Retry-After` (seconds or HTTP date), capped at 60 s; 5 s without either; 0 starts
  none. Circuit neutrality unchanged. Metric `kaiak_deployment_cooling_down{backend,
  deployment_model}` (0/1, one sample per configured deployment, as the circuit gauges).
- **D4 failover-only retries** — `routing.Avoid` keeps one list, `Refused`: every tried
  deployment (plus all of a backend's after a `401`/`403`); the untried/tried
  preference is gone. `server/upstream.go`: `avoidAfter` refuses each attempt's
  deployment; the same-deployment pause (L1) and its test removed. A single-deployment
  model answers its first attempt's error (`retry_refused: "no_deployment_left"`). The
  retry budget is unchanged and still bounds failovers.
- **Failing first** (red before, green after):
  - `TestThrottledDeploymentCoolsDown` — the reviewers' scenario (A: `429 Retry-After:
    60` at once; B: one held slot; 100 sequential requests): **before** throttled
    backend calls = 100, healthy = 24, served = 24 (the probe's 24/76 exactly);
    **after** throttled = 1, healthy = 100, served = 100.
  - `TestSingleDeploymentIsNotRetried` (was `…IsRetriedOnItself`),
    `TestAllAttemptsFailingAnswerTheLastError` (now on two deployments),
    `TestNoRetryWhenTheOtherDeploymentIsOpen` (was `TestRetryReusesATriedDeployment…`),
    `TestClientRepeatsCountOneFailureEach` (threshold 3, default 3 attempts: the circuit
    opens at the third call, not the first) — all red before.
  - New (green after): `TestThrottledSingleDeploymentIsStillUsed`,
    `TestAllDeploymentsThrottledAreStillUsed`, `TestCooldownEnds` (Retry-After-Ms 50),
    `TestThrottleCooldown` (header table); routing `cooldown_test.go` — skipped while
    another is eligible, used when all cool / the other is open / single, a queued
    request gets the deployment when its cooldown ends, waiters get a cooling
    deployment when the last warm one cools, cooldowns follow the config; routing
    `retry_test.go` rewritten for `Refused` only; e2e
    `TestThrottledDeploymentFailsOverAndCoolsDown` (was `TestBusyDeploymentIsNotRetried`).
- **Tests updated to the new contract** (they relied on same-deployment retries):
  `TestMaxAttempts` (model "down" given five deployments), `TestUpstreamAttemptMetrics`
  (the retried request on `pair`), `TestRetryBudgetStopsRetries` (on the two-deployment
  model), `TestUpstreamFailuresBeforeTheFirstByte` (one attempt),
  `TestSlotIsHandedOnEveryRelayEnd/backend_error` (the holder answers 504, its slot
  goes to the waiter).
- **Spec**: `docs/specs/GATEWAY.md` — Retries (failover only, settled, with the reason
  and the rejected pause), retry table rows, Choice across attempts, the pause entry
  removed, new "429 cooldown" entry, the backend-error and wrong-model passages, the
  metrics table. `docs/DEPLOYMENT.md` — many deployments, reliability defaults, Azure
  quota and reasoning-timeout notes.
- **Suite**: `GOFLAGS=-count=1 scripts/check-all.sh` — green (phase 1 ends here):

  ```text
  ok  	kaiak/internal/routing	4.840s
  ok  	kaiak/internal/server	13.421s
  ok  	kaiak/e2e	73.180s
  ...
  boundaries ok
  ==> cross-half e2e (sample control plane + two gateways)
  ok  	kaiak/e2e	53.455s
  all checks passed
  ```

## Decisions made during the step

- **Cooldown applies to every attempt kind**, retries included: a retry whose only
  untried deployment is cooling still uses it (nothing else is left), consistent with
  "used when no other is eligible".
- **"Eligible" means circuit-eligible, not "has a free slot"**: with the warm deployment
  full, a request queues for it rather than take the cooling one (a near-certain
  `429`). With a queue size of 0 that request gets `queue_full` instead.
- **A later `429` replaces the cooldown's end** (the backend's latest word), rather than
  keeping the longer one.
- **`Retry-After-Ms` wins over `Retry-After`** when both are present (finer; Azure sends
  both). `Retry-After: 0` or a date in the past starts no cooldown.
- **Scope: per gateway, per deployment** (backend ID + backend model), not per Azure
  resource: two deployments on one resource cool down separately.
- **Metric, not status**: `kaiak_deployment_cooling_down` gauge; status unchanged (no
  protocol change).

## Decisions for the user to confirm

- Default 5 s without a header, 60 s ceiling (as briefed) — kept as code constants, not
  config.
- A request queues for a full warm deployment rather than use a free cooling one
  (above) — the alternative is "cooling only loses ties/free-slot contests", which
  sends traffic into near-certain `429`s under load.
- Per-deployment scope rather than per backend/resource (Azure quota is per
  deployment in a resource, so this matches Azure; a shared-quota backend would cool
  each deployment on its own first `429`).

## D5 — pending

`max_concurrent_requests_per_key` off by default awaits the user's confirmation; nothing
of it is implemented in this run.

## D5 — deferred (user, 2026-09-25)

Left as is: `max_concurrent_requests_per_key` keeps its default of 16. The user will
revisit after real-world testing; the proper fix (fair dispatch across owners) is in
`docs/BACKLOG.md`. All other "Decisions for the user to confirm" in this plan were
accepted as implemented.
