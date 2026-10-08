# Step 12 — review and green

**Status:** not started

## Intent

The plan is checked by an independent review and closed out.

## Files likely touched

- Fixes from the review, each with its test.
- `docs/BACKLOG.md` — OpenTelemetry export → Metrics removed (built); Traces updated
  (the dependency ruling allows the trace SDK core; the shared connection exists); a
  new entry for Go runtime and process metrics with its revisit trigger.
- `docs/reviews/2026-10-07-structure/STRUCTURE.md` → Outcome: F3, F4, F5, F8, F10
  and T12 marked done with their steps.
- `docs/plans/otel-metrics/OVERVIEW.md` — verification status.

## Decisions made during planning

- The independent review runs as before (a plain copy, Codex), over the branch diff
  against the anchor tag.

## Acceptance criteria

- Every review finding fixed or answered in the review file.
- `scripts/check-all.sh` green 3× in a row. Suite recorded.

## Result

