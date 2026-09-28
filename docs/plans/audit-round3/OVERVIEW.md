# Plan: third audit round (independent audit on 0.4.0)

## Goal

Fix the seven findings of the independent audit on `0.4.0` and its deployment notes
(`docs/reviews/2026-09-25/AUDIT-independent.md`), as agreed in
`docs/reviews/2026-09-25/IMPLEMENTATION.md` (F1–F7 + notes). The auditor's reproduction
tests become regression tests: each must fail on `0.4.0` and pass after its fix.

## Phases and steps

- **Phase 1 — input hardening** (step 1): F1 duplicate members in every document the
  gateway parses + `KAIAK_` at the credential boundary; F2 duplicate top-level request
  members → `400`; F4 sequence and embeddings-input caps.
- **Phase 2 — money and routing correctness** (step 2): F3 status kept on a cut-off
  error body; F5 retired control-plane processes; F6 billability from the request's
  snapshot; F7 window incarnations.
- **Phase 3 — deployment and proof** (step 3): usage memory bound sized by bytes,
  optional `KAIAK_MAX_CONNECTIONS`, docs; e2e; images rebuilt and pushed.

## Verification

The auditor's eight `TestAudit*` tests (kept as regression tests, renamed to the
project's style) fail on `0.4.0` and pass; `scripts/check-all.sh` 3× green; images
smoke-tested read-only and pushed.

**Status (2026-09-25):** phases 1–3 done. The regression tests failed on the unfixed
code and pass (STEP-1, STEP-2 Results); `GOFLAGS=-count=1 scripts/check-all.sh`
green three times in a row at the plan's end (STEP-3); images built and
smoke-tested read-only on `dev` (`smoke passed`). **Push pending — main session.**
