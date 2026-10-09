# Step 11 — review and green

**Status:** not started

## Intent

An independent review checks the plan's code, and the branch is green and ready to
merge except for the live run (step 12).

## Files likely touched

- Fixes from the review, each with its test.
- `docs/BACKLOG.md`: what the review leaves open, each with its revisit trigger.
- `docs/plans/media-generation/OVERVIEW.md`: the review outcome and the verification
  status.

## Decisions made during planning

- **The review runs as for the rerank plan:** two agent reviews of the worktree and a
  Codex review of a plain copy (`/tmp/kaiak`, run by the user), over the branch diff
  against `v0.13.1`. Findings are re-checked against the code, and reproductions are
  run.
- **Findings are triaged by frequency.**

## Acceptance criteria

- Every finding is fixed or answered.
- `scripts/check-all.sh` green 3× in a row.

## Result

(filled in when the step is done)
