# Step 5 — verify

**Status:** not started

## Intent

Prove the phase: every review repro passes, e2e covers the two daily-impact fixes, the
suite is green three times.

## Files likely touched

- `gateway/e2e`: the exporter against a collector that redirects (nothing delivered
  elsewhere, counted failed); a token refusal blocked only by in-flight reservations
  answering the short `Retry-After`, then admitted.
- `docs/plans/audit-2026-10-05/OVERVIEW.md` verification status;
  `docs/reviews/2026-10-05/AUDIT.md` gets an implementation note per finding (fixed in
  which commit).

## Acceptance criteria

- `scripts/check-all.sh` ×3, Go test cache cleared before each, all green; recorded.

## Result

_Not started._
