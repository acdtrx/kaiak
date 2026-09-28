# Step 5 — docs, alerts, proof

**Status:** done in the worktree (2026-09-25): docs, alerts, e2e, image build + smoke on `dev`. Pending for the main session: the live kit on the GPU host and the image push. D5 (step 2) still pending the user.

- D9: README sample walkthrough (`-models demo`, or the fake backend lists any model it
  is asked for); starter alerts: `for:` on usage-not-acknowledged, aggregation across
  replicas (`max by`/`sum by`), queue rejections as a rate + ticket severity, severity per
  alert; stream memory (~70 KiB live per stream) in the sizing formula; how token limits
  reserve the full output limit while a request runs and how to size per-minute limits;
  vLLM `--enable-prompt-tokens-details` for cached tokens.
- e2e where process-level proof is missing; `scripts/check-all.sh` 3× green.
- Live kit on the GPU host (main session) and images pushed (main session).

## Result

- **README walkthrough** — the failure proven first: with the fake backend listing only
  `fake` (its default), stopping and restarting it opened the `demo` circuit and every
  probe after the restart logged `circuit kept open: the backend does not list the
  deployment's model` — `503` forever (`backend back: 503 503 503 503 503 503 503 503`).
  Fix: the walkthrough runs the fake backend with `-models demo`, and says why (the
  gateway checks the models list at config apply and in the probe). Re-run with the
  sample, a control-plane-mode gateway and the fake backend (binaries built from the
  worktree): `before restart: 200`, `backend down: 502 ×5 503 ×2`, `backend back: 503
  ×6 200 ×4` — `probe succeeded` → `circuit half-open` → `circuit closed` 10 s after
  the circuit opened; the startup warning `the backend does not list the deployment's
  model` gone too. README step 4 now says a gateway started with the sample down
  waits up to 60 s for it (step 3's boot wait) before exiting.
- **Starter alerts** (`docs/DEPLOYMENT.md` → Observability): a Severity (page/ticket)
  and `for` column on every row; every expression aggregates across replicas —
  `count(…) > 0` for fleet-wide states (control plane, outage, usage acks, config
  mismatch), `max by (backend, deployment_model)` for circuits and cooldowns, `sum by
  (…)` for rates and counters.
  - Fixed, fired on a healthy system: **Usage not acknowledged** — `spool > 0 and
    time() - last_ack > 300` fired for the seconds between a batch sealing and its ack
    on any pod idle for 5 minutes. Now one expression, `count(kaiak_usage_spool_batches
    > 0 unless time() - kaiak_usage_last_ack_timestamp_seconds <= 300) > 0`, `for: 2m`
    (also covers "never acked", which needed a second expression). **Backend failure
    rate** — one failure out of two attempts on a quiet backend was 50%: a volume
    clause (`> 0.1` attempts/s, about 30 in 5 minutes) and `for: 5m`. **Queue
    rejections** — `increase(…) > 0` over 10 m fired on any burst: now rejections ÷
    requests (`kaiak_request_duration_seconds_count`) `> 1%`, `for: 10m`, ticket.
  - New: **Deployment often cooling down** (`avg_over_time(kaiak_deployment_cooling_down[30m])
    > 0.25`, ticket — Azure quota too small); **Global limit refusals**
    (`kaiak_limit_rejections_total{scope_kind="global"}`, ticket — team/workload/user
    refusals are their owners' business, the log line names the limit).
  - Checked two ways with a scratch script: every `kaiak_*` name in `DEPLOYMENT.md` and
    `README.md` is registered in `gateway/internal/metrics`; every alert's metric,
    label key, label value (`=`, each `=~` alternative) and `by (…)` label exists in a
    live `/metrics` scrape of a control-plane-mode gateway (sample + fake backend, one
    request, one ack); the 38 registered metrics and the 38 of `GATEWAY.md`'s metrics
    table are the same set. The script caught deliberate typos (a metric, a label
    value, a `by` label) when tried. `promtool check rules` (prom/prometheus on `dev`,
    removed after): 21 rules OK; `promtool test rules` with 9 scenarios: the idle pod
    does not fire usage-not-acked, stopped acks fire at 300 s + 2 m, a never-acked
    pod fires, one open circuit on 2 of 3 replicas is one alert, one failure on a
    quiet backend does not fire, a 10% failing backend on two replicas is one alert, a
    sustained 5% queue rejection fires for its model only, a 40%-cooling deployment
    fires — all pass; the old usage rule fires on the idle pod (proof of the misfire).
- **Sizing and behavior** (`DEPLOYMENT.md`): Resources — memory ≈ 2 × body budget +
  open streams × ~70 KiB (~130 KiB RSS without `GOMEMLIMIT`) + …; the default 1.5 GiB
  covers about 5 000 streams, +70 KiB per stream above. Config for many hosts — new
  bullet on token limits: the reservation (input estimate with 1000 per media item +
  effective output limit × sequences) stands while the request runs; sizing rule
  (peak concurrent × (input + output limit), per gateway share; the full limit must
  exceed ceiling × n + longest prompt; keep the default output modest); the refusal's
  log fields and how `requested` vs `limit_configured` tells "too large" from
  "window full". New bullet: vLLM `--enable-prompt-tokens-details` for cached tokens.
  Logs bullet names the owner, `ttft_ms`, limit, `upstream_error_*` and `tried`
  fields. The retry model (failover only), the 429 cooldown and the 60 s boot wait +
  startup probe (steps 2–3) were already in the page and are consistent with
  `GATEWAY.md` (checked: probe table row, Probes, Boot, env table, Azure quota,
  reliability defaults).
- **e2e** — `gateway/e2e/media_test.go`, `TestInlineImageKeepsItsDefaultOutput`: a
  1.4 MB base64 screenshot through the binary on a 32k-context model under a 100k
  tokens/min workload limit → 200 with `max_completion_tokens` 4096 sent to the
  backend; a ~100k-token text prompt → `429 rate_limit_exceeded` whose log line has
  `limit_scope=workload`, `limit_id=w`, `limit_type=tokens_per_minute`,
  `limit_configured=100000`, `requested > 100000`, `team`, `workload`, and
  `kaiak_limit_rejections_total{scope_kind="workload",type="tokens_per_minute"} 1`.
  Red on the pre-plan `main` (scratch worktree): `429 … needs 358700 tokens … the
  workload limit is 100000`. Green `-count=5` (8.7 s). The 429 cooldown's e2e from
  step 2 (`TestThrottledDeploymentFailsOverAndCoolsDown`) and step 3's boot/readiness
  e2e already cover their process-level behavior; nothing else added.
- **Images** — `scripts/build-images.sh` (no push) on `dev`: `kaiak:0.5.1-b3fcffa`
  18.9 MB, `kaiak-sample:0.5.1-b3fcffa` 373 MB; `scripts/smoke-images.sh` → `smoke
  passed` (file mode and control-plane mode, both read-only with no volume, chat 200,
  `kaiak_build_info{version="0.5.1-b3fcffa"}`, `usage flushed` on stop). Images, smoke
  containers and the promtool image removed from `dev`.
- **Suite** — `GOFLAGS=-count=1 scripts/check-all.sh` three times in a row:

  Run 1:

  ```text
  ok  	kaiak/cmd/kaiak	2.349s
  ok  	kaiak/e2e	88.236s
  ok  	kaiak/internal/routing	5.951s
  ok  	kaiak/internal/server	13.422s
  ℹ tests 472
  boundaries ok
  ==> cross-half e2e (sample control plane + two gateways)
  ok  	kaiak/e2e	49.398s
  all checks passed
  ```

  Run 2:

  ```text
  ok  	kaiak/cmd/kaiak	2.216s
  ok  	kaiak/e2e	86.878s
  ok  	kaiak/internal/routing	3.607s
  ok  	kaiak/internal/server	11.625s
  ℹ tests 472
  boundaries ok
  ==> cross-half e2e (sample control plane + two gateways)
  ok  	kaiak/e2e	43.847s
  all checks passed
  ```

  Run 3:

  ```text
  ok  	kaiak/cmd/kaiak	2.180s
  ok  	kaiak/e2e	87.672s
  ok  	kaiak/internal/routing	2.796s
  ok  	kaiak/internal/server	12.003s
  ℹ tests 472
  boundaries ok
  ==> cross-half e2e (sample control plane + two gateways)
  ok  	kaiak/e2e	38.847s
  all checks passed
  ```

## Decisions made during the step

- **`-models demo` in the README, not a fake backend that lists any model.** The
  fake backend cannot know the names a config uses after its own restart (a models
  list has no request to echo), and a list that grew from requests would hide the
  wrong-model check the walkthrough can show. The smoke script's fake needs nothing
  (it is never restarted).
- **Severity rule**: page when clients are refused now or will be at the outage
  grace (control plane lead time, outage, usage not acked, config mismatch, budget
  refusals, no healthy deployment, crash loop); ticket for the rest, including one
  open circuit (the model's other deployments carry it; none left pages as no
  healthy deployment).
- **Fleet-wide alerts use `count(…) > 0`** (one alert, value = pods affected) rather
  than per-pod alerts; per-pod ones stay per pod (usage near the memory bound,
  connections refused).
- **Cooling-down alert** from the gauge (`avg_over_time` over 30 m > 25%), not from
  `rate_limited` attempts: it measures lost capacity, which is what a quota raise fixes.
- `budget_unavailable` stays out of the limit-rejection alert (step 4's decision:
  it has its own row).

## Decisions for the user to confirm

1. `-models demo` in the walkthrough (above) rather than changing the fake backend.
2. The severity split and thresholds: usage-not-acked `for: 2m` after 300 s (fires
   ~7 min into an ack outage, 8 min before the grace); failure rate 5% with a
   0.1 attempts/s floor, `for: 5m`; queue rejections 1% of requests for 10 m; cooling
   down 25% of 30 m; global limit refusals any in 15 m.
3. Memory: "1.5 GiB ≈ 5 000 streams, +70 KiB per stream above" as the stated rule
   (from the performance review's measurement, not re-measured here).

## Pending (main session)

- Live kit on the GPU host, incl. an image request if a vision model is available (else a
  synthetic base64 image through the fake backend — `TestInlineImageKeepsItsDefaultOutput`
  covers the latter); vLLM started with `--enable-prompt-tokens-details` to see
  `tokens_cached`.
- `scripts/build-images.sh --push` after the merge.
- D5 (per-key concurrency default) — the user's call; untouched.

## Live GPU-host run (main session, 2026-09-25)

Two vLLM copies of Qwen3.5-2B (`qwen3.5-2b`, launcher entry `qwen35-2b-a/-b`) with
`-max-in-flight 4`:

- + vLLM embeddings (launcher entry `qwen3-embed`) with `-check-failover`: **16 passed, 0 failed,
  0 skipped** — failover under failover-only retries: 5 served while copy B was down
  (2 failed over), circuit opened after 8.4 s, half-opened by a probe after the restart,
  closed by its trial.
- + llama-server embeddings (`embed-host:11435`, path-style model):
  **15 passed, 0 failed, 1 skipped** (failover not requested).

No vision model on the GPU host: the inline-image path is proven by
`e2e/TestInlineImageKeepsItsDefaultOutput`. The GPU host was restored to its regular model.
