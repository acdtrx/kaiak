# Step 15 — the control ↔ limits seam

**Status:** not started

## Intent

The control client hands the limiter what it needs in the limiter's own types, so there
is no adapter in `main` and no copy of one in the tests. Today `main` assembles three
client getters into `limits.Contact` and converts totals field by field, and the server
tests carry a copy that has drifted: `testLimitsTotals` never sets `Complete`, and the
test contact drops `UsageUncountedSince`, so the controlled-gateway tests never run the
complete-totals branch.

## Findings

- T5 item 1, control-main F2, test-scaffolding F1(a):
  - `control` imports `limits` for its input types (decision 12);
  - `Client.LimitsContact() limits.Contact` replaces `Contact` + `UsageWaitingSince` +
    `UsageUncountedSince` for the limiter (`Contact()` stays for metrics);
  - `Options.OnTotals func(limits.Totals, counted uint64)`;
  - `control.TotalsWindow` / `limits.PushedWindow` become one type (the pushed window
    carries the JSON tags, or the one conversion lives in `control.takeTotals`);
    `control.TotalsUpdate` goes if it is then `limits.Totals` plus `Counted`;
  - `limitsTotals`, `controlContact`, `testLimitsTotals` and the test's contact closure
    go; `TestLimitsTotalsCarryWindowsByGroupAndType` moves to `control`;
  - `newControlledGateway` wires the real client → limiter path.
- `docs/ARCHITECTURE.md`: the package diagram gains the `control → limits` edge.

## Files likely touched

- `gateway/internal/control/{client,usage,messages,stream,schema}.go` and tests.
- `gateway/internal/limits/{limits,shared}.go`.
- `gateway/cmd/kaiak/main.go`, `main_test.go`.
- `gateway/internal/server/usage_path_test.go`.
- `docs/ARCHITECTURE.md` (and `docs/architecture/gateway.html` if it draws the edge).

## Decisions made during planning

- `limits` does not import `control`; the dependency runs one way, as
  `accounting.UsageRecord` already does.
- If wiring the real `Complete` flag makes a controlled-gateway test fail, that test
  relied on the drift: report it with the reason, fix the test's expectation only if
  the binary's behaviour is the correct one (it is the spec's).

## Removal checklist (clean at phase end)

- `git grep -nE 'func limitsTotals|func controlContact|testLimitsTotals|UsageWaitingSince|UsageUncountedSince' gateway/`
  → none outside `control`'s own unexported use.
- `git grep -n 'type TotalsWindow' gateway/internal/control` → none (if merged).

## Acceptance criteria

- The controlled-gateway tests run with complete totals as the binary does (a test shows
  a complete push resets a pushed window).
- `scripts/check-gateway.sh` green, or reds named with the step that clears them.
