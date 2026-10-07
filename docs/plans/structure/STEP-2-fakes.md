# Step 2 — test fakes

**Status:** done (2026-10-08)

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

## Result

**What changed**

- `gateway/internal/fakebackend/fakebackend.go` — `Reply` loses its ten fault fields
  (`StallBeforeFirstByte`, `CutBeforeBody`, `StallBeforeBody`, `HangAfter`,
  `PingEvery`, `CutAfter`, `EndAfter`, `ErrorEvent`, `ErrorEventAfter`,
  `ErrorEventCode`) for two: `Before BeforeFault` (`StallFirstByte`, `CutBody`,
  `StallBody`; zero = none) and `Fault *StreamFault{At, Kind, Code, PingEvery}` with
  `Kind` one of `Hang`, `Cut`, `End`, `ErrorEvent`. One zero rule: nil `Fault` is no
  fault; `At` is the number of text events sent before it, so `At: 0` comes before
  the stream's first event (before a Messages/Responses stream's opening events
  too). `interrupt(i)` handles every kind; the error event's payload comes from the
  API's writer (`startStream` takes `messagesErrorEvent` / `responsesError`, nil for
  OpenAI streams). The Messages and Responses writers call `interrupt(0)` before
  their opening events and `interrupt(i)` before each text event; their own
  error-event branches are gone.
- 71 call-site lines in 21 test files (`e2e` 4, `provider` 1, `server` 16) moved to
  the new shape, five of them reading a reply's fault (`drain_test.go` ×2,
  `queue_test.go`, `upstream_test.go`: `Before == fakebackend.StallFirstByte`;
  `responses_test.go`: `Fault.Kind == fakebackend.ErrorEvent`).
- `gateway/internal/fakebackend/captures.go` (new) — `//go:embed captures` and
  `Captured(server, name) []byte` (panics on a missing recording).
  `provider/messages_test.go` loses `captured`; it and `provider/responses_test.go`
  call `fakebackend.Captured`; `accounting/messages_usage_test.go`'s
  `recordedStreamMeter` reads through it.
- `gateway/internal/fakecontrol/fakecontrol.go` — `Totals` deleted; `Hash` →
  `configHash`, `ConfigEvent` → `configEvent`; `Counted` folded into its only
  caller `CountedRecords` (an unexported method would clash with the `counted`
  field). No caller outside the package (grep over `gateway/` and `scripts/`).
- `gateway/internal/fakeotlp/` (new) — `New(t, respond)`, `AnswerStatus(status)`,
  `Collector.{URL, Next, None, Requests, Received, Bodies}`, `Received.{Header,
  Export, Status, Records(), Messages()}`; decode types are e2e's `otlpExport` shape
  (`Export`, `ResourceLogs`, `ScopeLogs`, `Record`, `KeyValue`, `Value`).
- `otlplog/exporter_test.go` — `received`/`collector`/`newCollector`/`next`/`none`
  gone; tests use `fakeotlp`. `TestExportRequestShape` asserts the same resource
  attributes, scope name, messages and headers through the fake's types.
- `cmd/kaiak/logexport_test.go` — `fakeCollector` gone; `TestSecondSignalCutsTheFinalLogFlush`'s
  inline collector is a `fakeotlp` one too.
- `e2e/logexport_test.go` — `otlpExport` types and `otlpCollector` gone;
  `plain`/`line` methods became `plainValue`/`recordLine` functions on the fake's
  types; `records`/`count` became `otlpRecords`/`acceptedRecords` over
  `Received()` (accepted = answered 200, refused = answered anything else);
  `TestLogExportRedirectIsNotFollowed`'s inline collector is a `fakeotlp` one.
- `docs/ARCHITECTURE.md` — `fakeotlp` added beside `fakebackend` and `fakecontrol`.

**Decisions made during the step**

- `fakeotlp.New` takes a full handler, `Respond func(w, r, n)`, not
  `answer func(n, r) int`: otlplog's tests hijack the connection, redirect, set
  `Retry-After` and write arbitrary bodies. `AnswerStatus` adapts the status-only
  form e2e uses (200 with `{}`, otherwise an OTLP Status body — e2e's answers). The
  fake records the status each export was answered (`Received.Status`) through a
  writer wrapper that passes `Hijack` through; e2e's accepted/refused split reads it.
- An export is recorded before `respond` runs (otlplog's stalled-collector tests
  need `Next` to return the held export). `Next` walks the recorded list rather than
  a bounded channel; its wait is 15 s (e2e's `waitLimit`; otlplog's was 10 s).
- An export that does not decode fails the test and is answered 400 (e2e's and
  cmd's behaviour; otlplog's fake answered as scripted after the error).
- `nil` respond answers 200 `{}`; cmd's fake answered 200 with no body — both
  deliver (`TestAnswersThatDeliver`).
- The two ad-hoc inline collectors (cmd `TestSecondSignalCutsTheFinalLogFlush`,
  e2e `TestLogExportRedirectIsNotFollowed`) also use `fakeotlp`, so there is one fake
  collector. The cmd test's "the export holding `kaiak stopped`" check reads the
  decoded messages (exact message) instead of a substring of the raw body.
- A `StreamFault` with an unset `Kind`, or `ErrorEvent` on an OpenAI stream (no error
  event there), panics in the handler: a misuse is loud, not a silent no-fault.
- Kinds and `BeforeFault` start at 1, so their zero values mean "none" / "unset".

**Fault translation** — each test injects the same fault at the same point:
`HangAfter: n` → `{At: n, Kind: Hang}` (n ≥ 1 everywhere, as 0 was "off");
`HangAfter: 2, PingEvery: 40ms` → `{At: 2, Kind: Hang, PingEvery: 40ms}`;
`CutAfter`/`EndAfter: n` → `Cut`/`End` at n; `ErrorEvent, ErrorEventAfter: 2` →
`{At: 2, Kind: ErrorEvent}` (after message_start/response.created and two deltas,
as before); `ErrorEvent` alone (`ErrorEventAfter: 0`, the error as the stream's first
event) → `{At: 0, Kind: ErrorEvent}`, which `interrupt(0)` sends before the opening
events, as before; `ErrorEventCode` → `Code`.

**Report vs code** (034329e line numbers): `Reply` had 23 fields, not 22 (15 now).
The capture readers include `provider/responses_test.go` (through `captured`), not
only `messages_test.go`. `Counted` had one caller (`CountedRecords`), so it was
folded in rather than unexported. Beyond the three named collectors there were two
inline ones (above). Otherwise as reported.

**Test counts** (before → after; `=== RUN` names diffed, identical)

- `cmd/kaiak`: 29 → 29 tests; 33 → 33 runs.
- `e2e`: 40 → 40 tests (42 → 42 with `crosshalf`); 120 → 120 runs.
- `internal/server`: 184 → 184; 432 → 432.
- `internal/provider`: 52 → 52; 192 → 192.
- `internal/accounting`: 36 → 36; 122 → 122.
- `internal/control`: 76 → 76; 228 → 228.
- `internal/otlplog`: 41 → 41; 161 → 161.

**Removal checklist** — all three greps return nothing; so do
`newCollector|newFakeCollector|newOTLPCollector|otlpExport|otlpCollector|fakeCollector`
over `gateway/`.

**Suite** — `scripts/check-gateway.sh`, green:

```
==> gofmt / go vet / staticcheck 2026.2.1 (gateway)
==> go test -race -count=1 (gateway): ok cmd/kaiak, e2e, accounting, auth, clip,
    config, control, limits, logattr, metrics, netfail, otlplog, provider, routing,
    schemacheck, server, sse
==> gofmt / go vet / staticcheck (live-test kit)
==> live-test kit self-test: passed for vllm, llama-server, openai, azure-openai,
    anthropic, azure-anthropic, vllm with two backends
gateway checks passed
```
