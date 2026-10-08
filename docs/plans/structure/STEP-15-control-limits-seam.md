# Step 15 — the control ↔ limits seam

**Status:** done (2026-10-08)

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

## Result

**What changed**

- `control` imports `limits` for the limiter's input types (decision 12); `limits`
  imports nothing of `control` (`go list`: `limits` → `accounting config logattr`).
- One window type: `limits.PushedWindow` carries the totals window's JSON tags
  (`group,omitempty`, `type`, `window_start`, `used,string`) and `control.Totals.Windows`
  is `[]limits.PushedWindow`, so the message decodes straight into it.
  `control.TotalsWindow` is gone; its field docs (window start; the `used` string and
  why) moved onto `PushedWindow`.
- `Options.OnTotals func(totals limits.Totals, counted uint64)`; `control.TotalsUpdate`
  is gone (its field docs moved onto `OnTotals`). `takeTotals` builds the
  `limits.Totals` (`LiveGateways`, `Complete`, `Windows` as decoded) and hands it
  with the counted generation.
- `Client.LimitsContact() limits.Contact` replaces the exported `UsageWaitingSince`
  and `UsageUncountedSince`: both clocks come from one unexported
  `usageSender.waiting()` under one lock (they were two locks). `Contact()` stays:
  `metrics` reads it through `controlState` in `main`.
- `cmd/kaiak/main.go`: `controlContact` and `limitsTotals` are gone. The limiter's
  contact is `func() limits.Contact { return client.LimitsContact() }` (a closure:
  `client` is set after the limiter is built), and `OnTotals` hands the totals and
  the generation to `limiter.TakeTotals` and the live count to the router.
- Server harness (`usage_path_test.go`): `testLimitsTotals` and the hand-built contact
  closure are gone; `newControlledGateway` wires `client.LimitsContact` and `OnTotals`
  straight to the limiter, as `main` does. Its observation channel carries only the
  counted generation (`nextCounted` returned the update, but no caller used it).
- `TestLimitsTotalsCarryWindowsByGroupAndType` moved from `main_test` to
  `control/client_test.go`, through the real stream: the stream's first totals reach
  `OnTotals` complete, with live 3 and two windows (a group's month window and a
  global hour window) equal by group, type, start and amount; the second are not
  complete.
- New `TestCompleteTotalsResetAPushedWindowTheyDoNotList` (server): a pushed 600-token
  hour window, then the control plane goes down, the stream closes, the window is
  dropped, and the reconnect's complete totals take the base back to 0. Checked
  against the drift: with `Complete` forced false in `takeTotals` it fails (the base
  stays 600); restored.
- `e2e/sample_test.go` (crosshalf): reads `limits.PushedWindow` / `Start`.
- `docs/ARCHITECTURE.md`: `limits` imports nothing of `control` (its `Totals` and
  `Contact` are what the client hands it; the windows decode into `PushedWindow`);
  `control` imports `limits` for those types, as it imports `accounting` for the
  usage record, and `cmd/kaiak` hands them over with no conversion.
  `docs/architecture/gateway.html`: the caption no longer says `cmd/kaiak` converts
  the totals; it says `control` imports `limits`, never the reverse. The SVG already
  draws `control → limits` ("totals, live count"); there is no package-import
  diagram for the gateway in `ARCHITECTURE.md`, so the edge is stated in the text.

**Decisions made during the step**

- **Tags on `PushedWindow` over a conversion in `takeTotals`.** The window the
  limiter keeps is the control plane's window by contract, field for field; two
  four-field mirrors that must change together are the class of drift this step
  removes. The tags are declarative only: `limits` gains no import and no decoding
  code, and validation stays in `control` (walker, rules, strict decode). The
  precedents are `accounting.UsageRecord` (the wire record in its own package) and
  decision 14 (config types carry the `/v1/models` tags). The cost: `limits` names
  the wire field names and the `,string` encoding of `used`, which its doc comment
  states. A conversion in `control` would leak nothing, but keep the mirror type.
- **`control.Totals` stays** as the message type: `counted_through` is the client's
  concern, and `Complete` is not on the wire.
- **Control tests keep their assertions on the new shape.** A test-local
  `totalsUpdate{totals, counted}` replaces `TotalsUpdate` in the harness. The two
  assertions on the pushed message's `CountedThrough` (one entry, sequence 1) now
  read the client's `lastCounted` (set from that message's `counted_through` before
  `OnTotals` runs) and require one entry, 1, for the client's own epoch — the same
  fact, plus the epoch. The four `UsageWaitingSince()`/`UsageUncountedSince()`
  reads in `totals_test.go` read `LimitsContact()`'s fields.

**Report vs code** (034329e line numbers; code at 4385408)

- control-main F2 / T5 item 1: as reported. `controlContact` had moved to
  `main.go:723`, `limitsTotals` to `:790`.
- The bug noted in passing (test drift): confirmed — `testLimitsTotals` never set
  `Complete`, and the test contact dropped `UsageUncountedSince`. With the real
  wiring, **no controlled-gateway test changed outcome**: the three existing ones
  (`TestRecordFillingABatchIsCountedOnce`,
  `TestBackendReportingTooManyTokensDoesNotLoseItsBatch`,
  `TestUsageAcksFailingPastTheGraceRefusePricedBudgets`) pass unchanged, also
  `-race -count=10`. None relied on the drift: their first totals list no windows
  before any usage, and every counted batch is pushed at once (no uncounted wait
  reaches the grace).
- test-scaffolding F1(a) and the `TotalsWindow`/`PushedWindow` hint: as reported.

**Tests** (`go test -count=1 -v`, `--- PASS`; before → after, no skips, no fails)

- `internal/control` 239 → 240 (+1: the moved test); `internal/limits` 85 → 85;
  `internal/server` 456 → 457 (+1: the complete-totals reset); `cmd/kaiak` 33 → 32
  (−1: the moved test).

**Removal checklist** (`git grep`)

- `func limitsTotals|func controlContact|testLimitsTotals` in `gateway/` → none.
- `UsageWaitingSince|UsageUncountedSince` in `gateway/` → only the fields of
  `limits.Contact` (`limits.go:177-183`, read in `shared.go:221-224` and
  `shared_test.go:27`), `LimitsContact` filling them (`control/client.go:261`) and
  the control tests reading them (`totals_test.go`). The getters are gone; the
  fields are the destination type the checklist keeps, not removed names.
- `type TotalsWindow` in `gateway/internal/control` → none; `TotalsUpdate` in
  `gateway/` → none.


**Suite**: `scripts/check-gateway.sh` passed (exit 0) on the final code. Because
`e2e/sample_test.go` changed, the cross-half e2e ran on its own as well
(`go test -race -tags crosshalf -run '^TestAcrossHalves' -count=1 ./e2e`): passed.
`npm test`/lint (control) were not run: no `control/` file changed.

```
==> gofmt / go vet / staticcheck 2026.2.1 (gateway)
==> go test -race -count=1 (gateway): ok cmd/kaiak, e2e (108s), accounting, auth, clip,
    config, control, limits, logattr, metrics, netfail, otlplog, provider, routing,
    schemacheck, server, sse
==> live-test kit: gofmt / vet / staticcheck; self-test passed for vllm, llama-server,
    openai, azure-openai, anthropic, azure-anthropic, vllm with two backends
gateway checks passed
crosshalf: ok  kaiak/e2e  65.581s
```
