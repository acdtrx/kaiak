# Step 2 — test fakes

**Status:** not started

## Intent

The gateway's test fakes say what they mean in one shape each: fakebackend's faults, its
recorded captures, fakecontrol's surface, and the fake OTLP collector.

## Findings

- test-scaffolding F8: fakebackend `Reply` — 22 fields, three mutually exclusive
  before-body bools, four mid-stream faults with inconsistent zero meanings, the error
  event handled outside `interrupt`. Proposed: `Before BeforeFault` and
  `Fault *StreamFault{At, Kind, Code, PingEvery}`; `interrupt(i)` handles every kind.
- test-scaffolding F9: fakebackend embeds `captures/` and exports
  `Captured(server, name)`; `provider` and `accounting` tests use it.
- test-scaffolding F10: fakecontrol `Totals` (no caller) deleted; `Hash`,
  `ConfigEvent`, `Counted` unexported.
- test-scaffolding F7: `internal/fakeotlp` beside the other fakes, with its own decode
  types (e2e's), used by `otlplog`, `cmd/kaiak` and `e2e` tests.

## Files likely touched

- `gateway/internal/fakebackend/{fakebackend,messages,responses}.go`, `captures/`,
  `fakebackend/cmd/fakebackend` (if it builds a `Reply`), ~60 test call sites.
- `gateway/internal/provider/messages_test.go`,
  `gateway/internal/accounting/messages_usage_test.go`.
- `gateway/internal/fakecontrol/fakecontrol.go`.
- New `gateway/internal/fakeotlp/`; `otlplog/exporter_test.go`,
  `cmd/kaiak/logexport_test.go`, `e2e/logexport_test.go`.

## Decisions made during planning

- `fakeotlp` decodes with its own types, not `otlplog`'s encoder types: an independent
  decode is what makes it a check of the encoder.
- fakecontrol's `protocolVersion = "5"` stays duplicated: fakecontrol cannot import
  `control`, and a mismatch fails every control test at once.

## Removal checklist (clean at phase end)

- `git grep -nE 'StallBeforeFirstByte|CutBeforeBody|StallBeforeBody|HangAfter|CutAfter|EndAfter|ErrorEventAfter' gateway/`
  → none.
- `git grep -n 'func (s \*Server) Totals' gateway/internal/fakecontrol` → none.
- `git grep -n '"fakebackend", "captures"' gateway/` → none.

## Acceptance criteria

- `Reply` has one before-body fault and one stream fault; one zero rule.
- One fake OTLP collector; the three tests use it with no lost assertion.
- Test counts in the touched packages unchanged (record before/after).
- `scripts/check-gateway.sh` green (the live-kit self-test included: it runs
  the fakebackend command).
