# Plan: audit fixes (after the 2026-09-24/25 audit)

## Goal

Make kaiak safe to run for the first real deployment — ~20 vLLM hosts plus Azure
OpenAI, many keys and budgets, several gateway replicas, Kubernetes — by fixing the
merged audit's findings (`docs/reviews/2026-09-24/AUDIT.md`) along the fix sketches and
the user's decisions D1–D8 in `IMPLEMENTATION.md`.

## Scope

Every high and medium finding, the low ones listed in the steps, and the container
image (D8). Item IDs below refer to the merged `AUDIT.md`.

## Out of scope

- A persistent control-plane store (the real control plane's job, D7) — this plan
  makes the store contract able to guarantee exactly-once and enforces one process.
- Demand-weighted shares (backlog, extended to caps), half-open circuits beyond the
  simple version in step 6 if it grows, L13.

## Constraints

- Settled decisions D1–D8 (dated in `IMPLEMENTATION.md`) are recorded in the owning
  spec in the same change that implements them.
- Protocol changes land on both halves at once; gateway standard library only; no new
  runtime dependencies (the Dockerfile uses base images only).
- Every behavior change gets a test that fails without it; `scripts/check-all.sh` green
  at every phase end; no leftover processes.

## Risks

- **Money path rework** (H3, H4, D1, D4, D5, M5): the fixes touch the same code as the
  bugs; each needs a regression test reproducing the audit's scenario first.
- **Timeouts** (D2) change config fields and retry/circuit semantics at once — easy to
  regress P3 behavior; the P3 e2e must stay green.
- **Scope size** — 11 steps; phases are real stopping points.

## Phases and steps

- **Phase 0 — delivery first** (step 1): the container image, so the user can test it on
  their network while the rest proceeds.
  1. `STEP-1-dockerfile.md`
- **Phase 1 — money correctness** (steps 2–4). Complete 2026-09-25, suite green.
  2. `STEP-2-contract-fixes.md` — both halves: config epoch, userinfo, integer bounds,
     `defaults` numbers, store de-dup + single process, control-plane hygiene.
  3. `STEP-3-usage-path.md` — record/seal ordering, clamping, billing what was sent,
     totals gated by config version.
  4. `STEP-4-limit-semantics.md` — windows during outages, model-list carry-over, free
     models, restored totals, impossible shares, ack-age contact, `n`.
- **Phase 2 — timeouts and routing** (steps 5–6). Complete 2026-09-25, suite green.
  5. `STEP-5-timeouts.md` — three upstream timeouts, client-side deadlines, stream
     establishment, terminal state, example config.
  6. `STEP-6-routing.md` — wrong-model detection, cap split, dispatch cost, retry
     hygiene, small routing items.
- **Phase 3 — resources, metrics, sample** (steps 7–9). Complete 2026-09-25, suite green.
  7. `STEP-7-memory-and-state.md`
  8. `STEP-8-metrics-and-logs.md`
  9. `STEP-9-sample-and-package.md`
- **Phase 4 — operations and proof** (steps 10–11). Automated verification complete
  2026-09-25 (`check-all.sh` green 3×); the live kit re-run is the user's.
  10. `STEP-10-deployment-guide.md`
  11. `STEP-11-e2e-and-live.md`

## End-to-end verification

1. `scripts/check-all.sh` green; each audit scenario that was reproduced ([B]'s probes,
   [A]'s scenarios) has a test that now passes for the right reason.
2. The image builds and runs locally against the fake backend; the user runs it on their
   network.
3. Live kit re-run against the two vLLM copies on the GPU host (with failover) — deferred
   to the user (command in `STEP-11-e2e-and-live.md`).

Status: 1 done (2026-09-25, step 11); 2 — images built, smoke-tested and pushed (step 1), the
network test is the user's; 3 pending.
