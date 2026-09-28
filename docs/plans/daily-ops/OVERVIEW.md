# Plan: daily-operations fixes

## Goal

Fix what affects daily operation under legitimate use, per the merged daily-operations
review (`docs/reviews/2026-09-25/AUDIT-daily-ops.md`, handling D1–D9 in
`IMPLEMENTATION.md`).

## Phases and steps

- **Phase 1 — requests** (steps 1–2).
  1. `STEP-1-estimates.md` — D1 inline media flat estimate, D2 per-prompt default output.
  2. `STEP-2-routing.md` — D3 429 cooldown, D4 failover-only retries, D5 per-key limit
     off by default (pending user confirmation).
- **Phase 2 — lifecycle and visibility** (steps 3–4).
  3. `STEP-3-startup.md` — D7 boot warm-up, D8 readiness waits for first totals.
  4. `STEP-4-visibility.md` — D6 log fields, TTFT, attempt metrics at release.
- **Phase 3 — docs and proof** (step 5).
  5. `STEP-5-docs-proof.md` — D9 docs and alerts; e2e; live kit on the GPU host; images.

## Verification

Each finding's scenario as a failing-first test; `scripts/check-all.sh` 3× green; live
kit on the GPU host incl. an image request if a vision model is available (else a synthetic
base64 image through the fake backend); images pushed.

**Verification status (2026-09-25):**

- [x] Each finding's scenario as a failing-first test — steps 1–4 (D1–D4, D6–D8) and
  step 5's e2e (`TestInlineImageKeepsItsDefaultOutput`, red on the pre-plan `main`).
- [x] `scripts/check-all.sh` 3× green — step 5 (outputs recorded there).
- [x] Synthetic base64 image through the binary and the fake backend — step 5's e2e.
- [x] Images built and smoke-tested on `dev` (not pushed) — step 5.
- [ ] Live kit on the GPU host, incl. an image request if a vision model is available —
  main session.
- [ ] Images pushed — main session.
- [ ] D5 — pending the user.
