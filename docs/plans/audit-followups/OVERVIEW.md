# Plan: audit follow-ups (after the 2026-09-25 follow-up audit)

## Goal

Close the follow-up audit's gateway findings (`docs/reviews/2026-09-25/AUDIT.md`) and
make the gateway stateless for the user's deployment (decisions E1–E12 in
`docs/reviews/2026-09-25/IMPLEMENTATION.md`), on the same branch before merging.

## Scope

- Stateless control-plane mode, boot order with the seed, drain flush reserve (E1–E3).
- The two high findings (E9, E10) and the medium ones (N-O2, N-O3, N-C1, N-C2, N-O4,
  N-P2, N-P3), E4, E5, E8, E12.
- The low findings that are cheap and clear (listed in the steps); the rest → backlog.
- e2e, the live kit (embeddings server), images rebuilt and pushed.

## Out of scope

The control plane's durability (E11); demand-weighted shares and key suspension
(backlog).

## Phases and steps

- **Phase 1 — stateless gateway** (steps 1–2).
  1. `STEP-1-stateless-boot.md` — no disk in control-plane mode (opt-in persistence),
     boot order control plane → seed → exit, seed guard, drain flush reserve.
  2. `STEP-2-deployment-docs.md` — guide rewritten: Deployment, read-only root, no
     volumes, numeric users + securityContext, Azure reasoning backends, alerts.
- **Phase 2 — high findings** (steps 3–4).
  3. `STEP-3-output-limits.md` — E9 + overflow-safe limits (N-M1), `best_of` on chat.
  4. `STEP-4-per-key-limit.md` — E10: per-key concurrency + body memory as bytes arrive,
     `MaxHeaderBytes`.
- **Phase 3 — medium and low** (steps 5–6).
  5. `STEP-5-circuits-and-metrics.md` — E4, N-C2, E8, E5, N-P2, N-P3, N-C3.
  6. `STEP-6-small-fixes.md` — E12 naming, status epoch, fakecontrol flake, api_key_env
     guard, the cheap lows.
- **Phase 4 — proof** (step 7).
  7. `STEP-7-e2e-live-images.md` — e2e, live kit incl. embeddings, images.

## End-to-end verification

Status (2026-09-25): check-all 3× green; live GPU-host run green (two vLLM chat copies with
failover, vLLM and llama-server embeddings); images pushed from the final commit.


`scripts/check-all.sh` 3× green; a gateway image running read-only with no volumes
against the sample; the live kit on the GPU host (two vLLM copies + the embeddings server);
images pushed.
