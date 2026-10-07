# Step 17 — one stream per boot

**Status:** not started

## Intent

Boot keeps the stream it opened through the first totals and hands it to `Run`. Today
every boot opens two streams (the first closed once the config arrives, the second
re-sends the same config, skipped by hash), the stream reader has a `firstOnly` mode,
and the first-totals wait is split over `main.waitFirstTotals`, a second copy of the
boot deadline and `limits.FirstTotals`.

## Findings

- T5 item 3, control-main F4:
  - Boot reads to the first config and applies it; for a control-plane config it keeps
    reading until the first totals have gone through `OnTotals`, or until the boot
    deadline; it logs the totals-wait lines;
  - it hands the open stream (response, `sse.Reader`, start time, first-totals flag) to
    `Run`, whose config follower resumes it before reconnecting;
  - `followStream`'s `firstOnly` parameter and `streamResult.first` go;
    `waitFirstTotals` and `main`'s `bootDeadline` go; `limits.firstTotals` /
    `FirstTotals()` go.

## Files likely touched

- `gateway/internal/control/{client,stream}.go`, `client_test.go`, `seed_test.go`.
- `gateway/internal/limits/{limits,shared}.go`.
- `gateway/cmd/kaiak/main.go`, `main_test.go`.
- `docs/specs/GATEWAY.md` (Control-plane mode → Boot / Readiness text and the boot
  diagram, where they mention the second stream).

## Decisions made during planning

- **Spike first** (decision 13): resume an already-open stream in the config follower.
  If the hand-off needs more than a small struct and one resume path, stop and report
  with what was found; the plan then keeps today's shape and records why.
- Behaviour to keep: no config published (stream open, nothing sent) ends at the boot
  deadline with `errNoConfigYet`; a stop signal during boot still cancels the totals
  wait; the seed fallback and its exits are unchanged.

## Removal checklist (clean at phase end)

- `git grep -nE 'firstOnly|streamResult\)?\.first\b|waitFirstTotals|bootDeadline|FirstTotals\(' gateway/` → none.

## Acceptance criteria

- A test counts one opened stream for a boot that gets config then totals (the existing
  `bootStreams` counters change from 2 to 1).
- Boot, seed, readiness and stateless tests pass; e2e startup tests pass.
- `scripts/check-gateway.sh` green, or reds named with the step that clears them.
