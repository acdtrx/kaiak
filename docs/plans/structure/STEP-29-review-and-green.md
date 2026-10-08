# Step 29 — independent review and green

**Status:** not started

## Intent

Check the whole branch with fresh eyes before it merges, fix what that finds, and record
which review findings each step resolved.

## Scope

- **Docs sweep:** `docs/ARCHITECTURE.md` and `docs/architecture/*.html` match the package
  graph (the `control → limits` edge, the model check in `provider`, `fixturetest`,
  `fakeotlp`, the `kaiak-control` `listeners` and `test-support` subsystems); every spec
  section an earlier step touched reads true; no history-narrating comment was left in
  touched code. Known: `docs/architecture/control-plane.html` still lists `totals` in the
  core's box (removed in step 8).
- **Upgrade notes:** `docs/DEPLOYMENT.md` → Upgrades gets one bullet for this plan's
  host-visible changes: the `kaiak-control` API changes of decision 11 (`totals`,
  `countedThrough`, store window starts, `BatchCursors.latest`, the sweep methods,
  `configHash`, `libraryName`) and the status state `starting` no longer existing.
- **Independent review:** the user's usual flow — a plain copy of the branch at
  `/tmp/kaiak` (no `docs/plans`, no `docs/reviews`), a Codex review against the plan's
  goal and constraints (behaviour preserved except decision 10; no metric, protocol,
  client API or config change beyond it), its report merged and each finding
  re-checked against code; its reproductions ported as tests. Delete the `/tmp/kaiak*`
  copies afterwards and check no Codex daemon keeps `/tmp/kaiak` as its cwd.
- **Fixes** from that review, each with a test where it is a behaviour.
- **Outcome table:** a section at the end of
  `docs/reviews/2026-10-07-structure/STRUCTURE.md` listing every in-scope finding with
  the step (and commit) that resolved it, and every out-of-scope one with where it went.
- **Removal checklists** of steps 1–28 re-run on the final tree.

## Acceptance criteria

- `scripts/check-all.sh` green **three times in a row**. **Phase 6 and the plan end
  here.**
- OVERVIEW's Verification status filled in.
