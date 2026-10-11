# Step 10 — review and green

**Status:** not started

## Intent

Review the whole branch before the live run, fix what the user rules in, and leave the
suite green three times in a row.

## How

- **Two agent reviews of the worktree:** one on correctness and security (the
  multipart reader and the rebuild, the audio readers, the budgets and deadlines, the
  routing and circuit changes), one on spec conformance and tests.
- **An independent Codex review** of a plain copy (`/tmp/kaiak`, no git, no plan or
  review folders), with the diff since `v0.14.1` as `CHANGES.diff`. The user runs it;
  its report is `AUDIT-B.md`.
- **Triage:**
  - every finding checked against the code, its reproduction run;
  - fixed with a test when it affects legitimate daily use or is high-impact;
  - design-level findings put to the user;
  - the rest recorded here.
- Decisions 22–30 (made while planning) confirmed with the user, or changed.

## Acceptance criteria

- A table of findings: source, verdict, outcome.
- `scripts/check-all.sh` green **3× in a row** on the final code, recorded.
- The `/tmp` copies deleted, with no Codex processes left in them.
