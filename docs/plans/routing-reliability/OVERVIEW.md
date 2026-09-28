# Plan: routing reliability (P3)

## Goal

A failing or overloaded backend stops hurting clients: requests retry on another copy
of the model before anything reached the client, failing deployments are taken out of
rotation and probed back in, and each backend process gets no more concurrent requests
than it handles well — with the waiting done, visibly and bounded, in the gateway.

## Scope

- Config: per-backend `max_in_flight`; queue size and timeout (global default,
  per-model override — one queue per model);
  retries (global max attempts, per-model override); circuit breaker settings
  (failure threshold, probe interval).
- Gateway: concurrency cap + gateway-held queue; circuit breaker per deployment with
  `GET …/models` probes; retry loop before the first byte; usage across attempts.
- Status to the control plane: backend health, circuit state, queue depth — the
  protocol change deferred from P2 — stored by `kaiak-control`, shown on the sample
  page.
- Metrics: circuit state, queue depth and waits, retries/attempts.

## Out of scope

- Fallback to another model or provider (settled 2026-09-24: none).
- Always-on active health checks; completion probes (`docs/BACKLOG.md`).
- Weighted load balancing.

## Constraints

- Decisions settled in `docs/specs/GATEWAY.md` → Routing and reliability (2026-09-24).
- One pipeline: retries, queueing and circuit checks live in routing/provider stages,
  not special cases elsewhere. Only provider code talks to backends (probes included).
- Gateway standard library only; protocol changes on both halves at once.

## Risks

- **Retry × accounting × limits**: one client request, several attempts — reservations,
  usage records and in-flight counts must stay exactly right on every path
  (success on retry, all attempts failing, client leaving mid-queue or mid-retry).
- **Queue × drain**: queued requests during drain must end cleanly (served or refused),
  never hang past the drain timeout.
- **Circuit flapping**: thresholds and probe timing must not open/close in a loop under
  intermittent failure; tests with scripted failure sequences.

## Phases and steps

- **Phase 1 — contract** (step 1): config fields and the status extension, both halves.
  1. `STEP-1-config-and-status-contract.md`
- **Phase 2 — gateway behavior** (steps 2–4). Ends with: the gateway caps, queues,
  breaks circuits, probes and retries, all tested.
  2. `STEP-2-concurrency-queue.md`
  3. `STEP-3-circuit-breaker.md`
  4. `STEP-4-retries.md`
- **Phase 3 — visibility and end to end** (steps 5–6).
  5. `STEP-5-status-page-metrics.md`
  6. `STEP-6-e2e.md`

## End-to-end verification

Status: items 1–3 automated and green (step 6). Item 4 done 2026-09-24 on the user's
A GPU host (GB10): two vLLM 0.30.0 processes serving `Qwen/Qwen3.5-2B` as `qwen3.5-2b`
on ports 8001/8002 (launcher entries `qwen35-2b-a`/`-b`), kit run with `-max-in-flight 4
-check-failover` — 14 passed, 0 failed (embeddings skipped): 3/3 spread, 10 requests
over 2×4 slots all 200 with 2 queued, exact backend token counts; copy B stopped →
requests kept succeeding (2 retried), its circuit opened; restarted → a probe closed
it and traffic returned.

1. `scripts/check-all.sh` green.
2. Gateway e2e with two fake backends serving one model: a backend that refuses
   connections → request succeeds on the other, circuit opens after the threshold,
   probes bring it back when it recovers; a backend answering 500 then OK; a 429 on
   one → served by the other; capped backend: extra requests queue then succeed,
   queue full → `429 queue_full`, queue wait past timeout → `429 queue_timeout`;
   first-byte timeout on one → estimated partial record for that attempt, answer from
   the other; drain with a queue.
3. Cross-half: the sample page shows circuit state and queue depth reported by the
   gateways. (Automated as: the sample accepts a status report carrying an open
   circuit and a queued model; the page's rendering of it is the sample's unit
   tests.)
4. Manual, with the user when a vLLM setup is available: the live-test kit against two
   vLLM processes serving one model (`-base-url-2 … -max-in-flight N
   -check-failover`). Pending.
