# Step 7 — review and green

**Status:** not started

## Intent

An independent review checks the plan's code, and the branch is green and ready to
merge except for the live run (step 8).

## Files likely touched

- Fixes from the review, each with its test.
- `docs/BACKLOG.md`: entries the review or the plan left open, each with its revisit
  trigger.
- `docs/plans/rerank/OVERVIEW.md`: the review outcome and the verification status.

## Decisions made during planning

- **The review runs as before:** a plain copy reviewed in Codex, plus agent reviews
  of the worktree, over the branch diff against `v0.12.1`. Findings are merged and
  re-checked against the code.
- **Findings are triaged by frequency** (kaiak working style). Rare edge findings stay
  recorded in the review rather than fixed.

## Acceptance criteria

- Every review finding is fixed or answered.
- `scripts/check-all.sh` green 3× in a row. Suite recorded.

## Result

(filled in when the step is done)
