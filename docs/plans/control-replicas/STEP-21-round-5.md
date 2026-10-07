# Step 21 — round-5 fixes

**Status:** not started

## Intent

Fix the three findings of the fifth, Codex-only review
(`docs/reviews/2026-10-07/AUDIT-5-independent.md`), then end the plan green. This
step is **phase 8** and ends green.

## Scope

- **E-M1:** dropped and removed usage payloads stay reachable.
  - Where: `gateway/internal/control/queue.go:66` and `usage.go:372`.
  - Advancing a slice head leaves the removed element's records in the backing
    array, so a batch dropped under the memory bound stays in memory.
  - Fix: zero each removed element before advancing, and release empty backing
    arrays. The sender keeps its own reference to the outstanding batch until its
    send finishes.
- **E-L1:** in file mode, a deleted group's retained counters are pruned only on a
  reload.
  - Fix: prune through ordinary limiter activity, without a config change. For
    example, prune a retained counter when its last reservation settles, and at
    window boundaries, keeping decision 36's conditions.
- **E-L2:** `docs/specs/CONTROL-PROTOCOL.md:513` still names the file-mode snapshot.
  Remove it, and grep for any other live mention.
- **Regression tests:** port Codex's two reproductions from the session scratchpad
  (`audit-e5/control_audit_e_test.go`, `limits_audit_e_test.go`), renamed to describe
  the behaviour. Each fails before its fix.
- **AUDIT-5 outcome:** add an Outcome section at the end of
  `AUDIT-5-independent.md`, marking each finding fixed with its commit.

## Acceptance criteria

- `scripts/check-all.sh` green **three times in a row**. **Phase 8 and the plan end
  here.**

## Result
