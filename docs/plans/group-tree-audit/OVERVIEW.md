# Plan: group-tree audit fixes

## Goal

Fix what the 0.7.0 group-tree review found (`docs/reviews/2026-09-27/AUDIT.md`): the
scale problems that can block or crash gateways and the control plane, the test gaps
the mutation review proved, and the semantics the docs leave unclear.

## Scope

- **R1** carry-over on reload made linear in both halves (gateway limiter, kit
  `carryOvers`).
- **R2** a bound on total effective limits, a config rule in both halves.
- **R3** kit tree validation made linear (memoized like the gateway's `tree()`).
- **R4** gateway `mergeLimits` made linear (identities precomputed, map lookup).
- **T1–T7** the missing tests.
- **S1–S6** spec, `GUIDE.md` and `DEPLOYMENT.md` wording; **S4** the usage metric
  label `group` → `key_group`.
- R6's stale comment; keygen checks the group ID's shape before minting.
- Correct the group-tree plan's verification box (it claimed T1 was covered).

## Out of scope

R5 (totals size ceiling), R6's real fix (sealing batches by encoded size), R7 (sample
page render time): backlog entries with triggers (step 4). The "recorded only" list in
the audit.

## Decisions (settled 2026-09-27 with the user)

1. **S1 — a re-created group ID resumes its window's spend.** Windows and counters are
   keyed by group ID; a group deleted and re-created with the same ID within the same
   hour or month gets that window's spend back, under whatever parent it now has. It is
   a new group in the tree, but the ID's budget in the current window carries on. The
   spec and GUIDE say so (GUIDE currently says the opposite). No code change.
2. **S4 — usage metric label `group` becomes `key_group`**: the key's group, not the
   Prometheus target label `group` that scrape configs commonly set. `root_group` stays.
   `group_label` keeps its name and meaning (it switches `key_group`).
3. **R2 — one rule bounds the total effective limits**: the sum over global and every
   group of their effective limits (own + inherited `child_defaults`) must not exceed
   **50 000** — about 70 MB of counters per gateway at the measured ~1.4 KB each.
   New semantic code `effective-limits-exceeded` in both halves, reported once at the
   document root. The value is a constant in both halves and the spec.
4. **S2 — `child_defaults` is a default, not a ceiling** (unchanged behaviour): the
   spec, GUIDE and DEPLOYMENT say that a hard restriction for a subtree goes on the
   parent's own `allowed_models` / `limits`.
5. **S3 — each side carries against the config it last held**; totals computed under
   the new config settle any difference (spec, Budgets → Model-set edits).

## Phases and steps

- **Phase 1 — fixes and tests** (steps 1–3). Green at the end.
  1. `STEP-1-contract.md` — spec wording (S1–S3, S5, S6, the rule, `key_group`),
     fixtures for the rule and T7's missing cases.
  2. `STEP-2-gateway.md` — R1, R2, R4, S4 in the gateway; tests T1, T3–T6.
  3. `STEP-3-kit.md` — R1, R2, R3 in the kit; T2; keygen group check; R6 comment.
- **Phase 2 — docs and proof** (step 4).
  4. `STEP-4-docs.md` — GUIDE, DEPLOYMENT (label rename, alerts), architecture pages,
     backlog entries (R5, R6, R7), the group-tree plan's verification box;
     check-all 3×.

Steps 2 and 3 are independent (Go vs TypeScript) and may run in parallel after step 1.
Expected reds: after step 1 both halves fail the new fixtures (steps 2 and 3 clear
them).

## Verification

- Each T-gap: its mutation from the audit now fails a test (re-run the mutation,
  record the failing test, revert).
- R1, R3, R4: the audit's scenarios re-measured (throwaway benchmarks, numbers in the
  step result): linear growth, e.g. carry-over for 40k children well under 100 ms.
- R2: a config one limit over the bound refused by both halves with the same code;
  at the bound accepted.
- `scripts/check-all.sh` green 3×.

**Verification status:** done (2026-09-27).

- [x] T1–T7: each mutation fails a test — T1, T3–T6 in `STEP-2-gateway.md`, T2 in
  `STEP-3-kit.md`; T7 fixtures in `STEP-1-contract.md`.
- [x] R1, R3, R4 linear, re-measured — R1 gateway 40k children 35.4 s → 45–65 ms,
  R4 6.1 s → ~1.5 ms (`STEP-2-gateway.md`); R1 kit 20k 24.8 s → 19 ms, R3 20k chain
  11.4 s → 62 ms, cycle 20.6 s → 40 ms (`STEP-3-kit.md`).
- [x] R2: the at-bound fixture accepted and the one-over fixture refused with
  `effective-limits-exceeded` by both halves (fixtures `STEP-1-contract.md`; tests
  `STEP-2-gateway.md`, `STEP-3-kit.md`).
- [x] `scripts/check-all.sh` green 3× in a row (`STEP-4-docs.md`).

## Git

- Anchor tag `0.7.1` on `main` before step 1.
- Branch `group-tree-audit`, worktree `.claude/worktrees/group-tree-audit`; commit per
  step; rebase onto `main` and fast-forward merge after Phase 2.
