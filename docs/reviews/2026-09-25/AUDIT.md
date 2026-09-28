# Follow-up audit — 2026-09-25 (branch `audit-fixes` at `6d9eebe`)

Re-audit after the audit-fixes plan (`docs/plans/audit-fixes/`), against the merged
2026-09-24 audit (`../2026-09-24/AUDIT.md`). Five fresh read-only reviews — security,
money, concurrency/lifecycle, the contract between the halves, operability — each told
to (1) verify the previous findings, (2) hunt regressions introduced by the fixes,
(3) find anything still missed. Key new findings re-checked against the code by the main
session (marked *re-checked*). The suite was green throughout (`check-all.sh` 3× at the
end of the plan; the reviewers' own race and stress runs green except N-C4 below).
**No fixes applied in this round** — the user decides.

## 1. Previous findings — status

All 12 high and 16 medium findings of the 2026-09-24 audit are **fixed**, except:

| ID | Status | Note |
|---|---|---|
| H10 `n` / multiplicity | Partly | reservation × sequences and `max_n` work; the window arithmetic can still overflow — see **N-M1** |
| M10 config epoch | Partly | snapshot, stream, LKG and totals carry it; **status does not** — see N-P1 |
| L8 spool order across old epochs | Not fixed | never scheduled in a step — see N-M3 |
| L13 shares vs Kubernetes Services | Backlog (by design) | "Demand-weighted shares" |
| L10 pruned maps | Mostly | a removed backend's pool can be re-created until the next apply (hygiene) |

Evidence per finding is in the reviewers' tables (kept in this session's reports; the
per-step results in `docs/plans/audit-fixes/STEP-*.md` name each regression test).

## 2. New findings

Severity as before: **high** — money or availability wrong in normal operation at the
target scale or cheaply exploitable by any key holder; **medium** — wrong under realistic
stress, misuse or operator error; **low** — hygiene, edge cases, docs. Prefix = review:
M money, S security, C concurrency, P protocol/contract, O operability.

### High

- **N-M1 A huge `max_tokens` disables every token limit while the request runs**
  (*re-checked*, reproduced by the reviewer). `limits/window.go:255` computes
  `used+need <= w.limit` without overflow protection; `server/limits.go:27` saturates the
  reservation at `MaxInt64` and `params.go:64` accepts any non-negative output limit when
  the model has no `output_limit`. `max_tokens: 9223372036854775807` wraps the check
  negative → admitted; the counter goes negative → every later request on every token
  window of those scopes (global too) is admitted until it ends. Any key holder can
  repeat it. Not a regression (the old code overflowed too), but H10's fix left it open.
  Fix: overflow-safe `admits`/`waitFor`/`add` (`need > limit - used`), cap the
  reservation at 2^53−1, clamp client output limits (≤ 2^53−1, ≤ context length).
- **N-S1 Any valid key can freeze the gateway's body budget at no cost** (*re-checked*,
  reproduced by the reviewer). `server/bodies.go:70-74` takes the budget for the whole
  declared `Content-Length` and allocates the buffer before a byte arrives; limits run
  after inbound, so these requests never count against the key's rate limit. 128 idle
  connections declaring 4 MiB each hold the default 512 MiB for the 60 s body-read
  deadline → every other client gets `503 server_busy`; repeatable every minute. A
  regression in cost introduced by M2. Fix: take budget and grow the buffer as bytes
  arrive even with a known length; optionally a per-key/connection share; optionally
  check `requests_per_minute` before reading.
### Medium

- **N-O2 Images declare non-numeric users** (*re-checked*): `USER nonroot` / `USER node`
  fail `runAsNonRoot: true` (restricted Pod Security Standard) unless `runAsUser` is set.
  Fix: `USER 65532:65532` / `USER 1000:1000`; DEPLOYMENT.md gets a full `securityContext`
  (runAsUser/Group, fsGroup, no privilege escalation, drop ALL, seccomp RuntimeDefault,
  `readOnlyRootFilesystem: true` — the gateway writes only `/data`).
- **N-O3 Hidden-reasoning Azure models vs the timeouts.** Azure/OpenAI reasoning
  deployments stream nothing during hidden reasoning: an early `prompt_filter_results`
  chunk counts as the first event and a >120 s silence then ends as `broke_off`
  (circuit failure); without that chunk the 60 s first-event timeout fires and the
  request is retried up to 3× — each abandoned run billed by Azure. DEPLOYMENT.md says
  "thinking models stream their reasoning" (true for vLLM only). Fix (docs, per-backend
  timeouts exist): a separate backend entry for reasoning deployments with ~600 s
  first-event and stall timeouts; verify on the real resource.
- **N-C1 / N-S3 A half-open trial blocks its deployment for the whole trial request.**
  The verdict is reported at the end of the relay; a long stream (or a slow reader) as
  the trial keeps a recovered single-deployment model answering `no_healthy_deployment`
  for minutes. Fix: decide the trial at the first event (a 2xx whose first event arrived
  closes the circuit; later break-offs count as ordinary failures).
- **N-C2 Non-stream traffic cannot detect a hung backend.** Response timeouts are
  neutral (D2) and a neutral trial just hands on; an engine hung behind a live `/models`
  gets 30-min non-stream waits forever and a circuit stuck half-open. Fix: shorter
  response timeouts for embeddings, count a response timeout as a failure when it was the
  trial or several arrive in a row, keep probing while half-open.
- **N-O4 "Config rejected" and first-burst alerts miss** (*re-checked*): counters created
  on first use (`kaiak_config_loads_total`, `kaiak_upstream_attempts_total`, queue
  rejections, circuit transitions) start at their first value, so `increase(...) > 0`
  misses the first rejection or burst — exactly the H3 case. Fix: pre-create the series
  at 0 (per trigger × result; per configured deployment × outcome), or reword alerts.
- **N-P2 `400 n_too_large` counted as `class="internal"`** (*re-checked*): missing from
  the error-class mapping (`server/metrics.go`); the mapping test skips ~9 codes. Fix +
  make the test cover every code.
- **N-P3 The config-mismatch refusal is invisible in `kaiak_control_outage`** and in the
  docs describing `budget_unavailable`. Fix: include it in the gauge (or a new one) and in
  the Client API row and CONTROL-PROTOCOL outage text; alert thresholds (N-O5) too.

### Low

- **N-C4 The rare gateway flake, found**: `TestResyncAppliesALowerVersion` fails
  40/40 with `-cpu=1`, 31/40 with `-cpu=2` — `fakecontrol.Restart` leaves the old stream
  registered, the next `Publish` queues the new-epoch config on it. Harness bug (the
  gateway is correct); matters for CI with few CPUs. Fix in `fakecontrol`.
- N-C3 dispatch cache keyed per queue strands free slots across config snapshots after a
  reload adds capacity (reproduced); key by model pointer too.
- N-C5 the body budget couples models (a saturated model's queued bodies starve others);
  document or share per model.
- N-C6 the live-gateway count never falls during an outage (scaled-down gateways keep
  small shares); after a control-plane restart the first totals briefly over-share.
- N-M2 a totals push naming a future window (control-plane clock step) stops hour/month
  enforcement until the gateway clock catches up (D4 robustness); accept earlier windows
  that match the gateway's own clock, warn on future windows.
- N-M3 (= L8) old-epoch spool order is random; a twice-lost index can double-count.
- N-S2 `api_key_env` may name `KAIAK_CONTROL_TOKEN`/`KAIAK_METRICS_TOKEN`: a config author
  can make the background model check send the gateway's own token to any URL. Refuse
  `KAIAK_`-prefixed names (both halves), optionally require https for Azure.
- N-S4 `best_of` on chat not owned (vLLM versions accept it). N-S5 no `MaxHeaderBytes`
  (1 MiB default × unauthenticated connections). N-S6 the sample's `/events` has no
  connection cap (same port as the gateway streams).
- N-S7 no whole-response bound: a slow reader holds a slot for response size ÷ read rate
  (by design; document).
- N-P1 status lacks `applied_config_epoch` (the page can show a stale-store v3 as current).
- N-P4 the seed's credentials are checked only when applied (docs promise at startup).
- N-P5 status `max_in_flight` saturates at 2^31−1. N-P6 stale schema descriptions (circuit
  "first success closes", `failure_threshold` timeouts, `control_outage_grace_ms` vs D6/
  M16/H3, cap split). N-P7 GATEWAY.md:54 still says 5xx text is logged. N-P8 retry budget
  counts approved-but-unsent retries. N-P9 fixture gaps (first-event/response timeout
  invalids, `user_label` type). N-P10 a 4xx on opening the stream never falls back to a
  snapshot (corrupt LKG epoch). N-P11 status says `starting` after a seed boot.
  N-P12 doc drift (ARCHITECTURE diagram edge, "planned layout" heading, e2e list,
  `INIT_CWD`/`LIVE_*` in DEPLOYMENT, build script "MB compressed", comments).
- N-O5 alert thresholds equal the outage grace (no lead time); N-O6 4 MiB body cap vs
  Azure vision / long context (document raising it with the ingress and budget);
  N-O7 `kaiak_build_info{version="(devel)"}` — set it with `-ldflags -X`, add an applied
  config version/epoch metric; N-O8 guide gaps: PVC sizing by outage × rate, private CAs
  (`SSL_CERT_FILE`), sample probes/resources (no health endpoint), PodDisruptionBudget,
  topology spread, StatefulSet `serviceName`, the stuck-rollout quirk, LB idle timeout vs
  120 s, UTC month boundary; nice-to-haves (GOMEMLIMIT via downward API, alert
  aggregation, a config generator).

## 3. Decisions worth revisiting (reviewers' view)

- Step 8: pre-create attempt and config-load series (N-O4).
- Step 1: numeric `USER` (N-O2); version via `-X` (N-O7).
- Step 7 #6: also use the seed when the control plane answers `503 config-unavailable`
  (a restarted sample with an invalid file), still never on a rejected token/snapshot.
- Step 3: surface the mismatch in the outage gauge or docs (N-P3).
- Step 10/11 open question (drain reserve for the usage flush): with a PVC per pod the
  spool survives — no reserve needed; keep today's behavior.
- Kept as decided: retry-budget numbers, M16 raising the outage gauge, 512 MiB budget
  refusing at once.

## Out of scope

- A durable control plane (the sample's in-memory store) — the user ruled this audit is
  for the gateway only (2026-09-25); the real control plane is a separate project.

## 4. Not verified here

The Azure live check (no access). The GPU host live re-run was done on 2026-09-25 against
two vLLM copies of Qwen3.5-2B: 14 passed, 0 failed (embeddings skipped), including
failover with the half-open recovery (circuit opened after 7.4 s, half-opened by a
probe after the restart, closed by its trial). Post-fix images were rebuilt, smoke-tested and pushed:
`registry.example.com/kaiak/kaiak:0.3.1-6d9eebe` and `…/kaiak-sample:0.3.1-6d9eebe`
(also `latest`).
