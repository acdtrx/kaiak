# Step 11 — review and green

**Status:** not started

## Intent

Review the whole branch before the live run, fix what the user rules in, and leave
the suite green three times in a row.

## How

- **Two agent reviews of the worktree:** one on correctness, privacy and memory (who
  is captured, nothing sensitive reaching logs or metrics, the budget given back on
  every path, the tee, the finisher, the exit), one on spec conformance and tests
  (the conventions at the pinned commit, the OTLP JSON rules, the message mapping
  per API).
- **An independent Codex review** of a plain copy (`/tmp/kaiak`, no git, no plan or
  review folders), with the diff since `v0.15.1` as `CHANGES.diff`. The user runs it.
- **Triage:** every finding checked against the code and reproduced; fixed with a test
  when it affects legitimate use or is high-impact; design-level findings put to the
  user; the rest recorded here.

## Acceptance criteria

- A table of findings: source, verdict, outcome.
- `scripts/check-all.sh` green **3× in a row** on the final code, recorded.
- The `/tmp` copies deleted, with no Codex processes left in them.
