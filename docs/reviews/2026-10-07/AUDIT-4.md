# Pre-merge review, round 4 — 2026-10-07 (`control-replicas` at `b5e007c`)

## Scope

What phase 6 (steps 15–17) added:

- the stale-totals outage signal;
- a malformed totals event ending the stream;
- the spool kept until a covering `totals.json` save, with the crash rebuild;
- counters retained by scope;
- the counter bound.

## Reviewers

- **[G]**, the gateway: a read-only worktree at `b5e007c`. The full report is in the
  session scratchpad (`r4-G.md`).
- **[D]**, Codex on a plain copy: `AUDIT-4-independent.md`. The main session re-ran its
  four reproduction tests (`audit-d4/` in the scratchpad), and every one fails as
  claimed.

## Verdict

- **Sound on the main path:**
  - each crash point between ack, covering push, save and delete rebuilds exactly;
  - the stale-totals clock;
  - malformed totals;
  - the changes-only `TakeTotals`;
  - locks and shutdown.
- **Every finding but one is in the data-directory path.** Rounds 2, 3 and 4 each
  found a High in the on-disk spool and restart code. Decision 35 (2026-10-07) removes
  the data directory, and those findings dissolve with it.
- **D-H3 needs a fix either way.**

## Findings

| ID | Severity, frequency | Finding | Outcome |
|---|---|---|---|
| D-H1, [G] G4-L2 | High, rare | An ack for another instance's batch deletes the only restart copy of its spend before any save holds it | dissolved: decision 35 |
| D-H2, [G] G4-L1 | High, rare | A lost spool index reorders restored epochs; covering one counted batch retires an unsent one | dissolved: decision 35 |
| D-H3 | High, occasional | A retained counter is pruned while a running request still holds it: zero-amount USD reservations, and token reservations across the hour boundary. The request settles into a detached counter, so a recreated group loses that spend (file mode and control-plane mode) | decision 36 (step 19) |
| [G] G4-M1 | Medium, rare | A restored acknowledged batch whose epoch has no cursor any more (dropped after 7 days, or a restored store) is never covered: it is waited on forever, double-counted, and restored on every restart | dissolved: decision 35 |
| D-L1, [G] G4-L3 | Low | Wording: an ack "lets the batch leave the spool"; the 10 000 bound "in every mode"; a wrong warning at `usage.go:367` | dissolved: decision 35 (the text goes with the data directory) |
| [G] G4-L3 | Low | Retained counters are outside the `counters-exceeded` bound | decision 36: bounded by their window, accepted |

## Outcome

Filled in by step 20.
