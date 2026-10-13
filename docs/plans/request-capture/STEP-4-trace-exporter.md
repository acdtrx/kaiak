# Step 4 — the trace exporter

**Status:** done

## Intent

Add the traces signal to the OTLP connection and a span exporter beside the log and
metric exporters, in the telemetry tree: a span model, its OTLP/HTTP JSON encoding,
and delivery with the existing rules. Nothing produces spans yet.

## Files likely touched

- `gateway/internal/telemetry/otlp/signal.go` — `Traces`: `OTEL_TRACES_EXPORTER`,
  `OTEL_EXPORTER_OTLP_TRACES_{ENDPOINT,HEADERS,TIMEOUT,PROTOCOL}`, path `v1/traces`,
  response `ExportTraceServiceResponse`, rejected `rejectedSpans`.
- `gateway/internal/telemetry/otlp/settings.go` — the explicit switch for traces
  (OVERVIEW decision 25): on with the traces endpoint, or with
  `OTEL_TRACES_EXPORTER=otlp` (the general endpoint plus `/v1/traces`, else the
  localhost default); off otherwise, the general endpoint alone included. As a
  property of the signal, not a special case in the reader.
- `gateway/internal/telemetry/otlp/client.go` — `readExportResponse` reads
  `rejectedSpans`.
- `gateway/internal/telemetry/otlptrace/` (new):
  - `span.go` — the model: trace ID (16 bytes), span ID (8), name, kind, start and end
    times, attributes (string, int, double, bool, string array — what the span
    needs), status (code, message);
  - `encode.go` — one span to its OTLP JSON (hex IDs, integer kind and status,
    decimal nanosecond strings, `intValue` as a decimal string); the envelope
    (`resourceSpans` → `scopeSpans` → `spans`) around already-encoded spans; the
    resource and scope as the log exporter writes them;
  - `exporter.go` — `Enqueue(encoded []byte, release func())`, never blocking; a
    sender batching encoded spans up to the export limit (16 MiB with the envelope,
    decision 23) or a tick; `release` called exactly once per span — exported,
    failed, or dropped at shutdown; counts (handed, exported, failed by
    `error.type`, dropped at shutdown); `ForceFlush`, `Shutdown`; the failure
    report line in the log exporter's style (`trace export failing`). No queue
    capacity of its own (decision 29: the caller's budget bounds it);
  - `MaxSpanBytes` exported, so the caller can drop a span that can never be sent.
- `gateway/internal/telemetry/fakeotlp/fakeotlp.go` — `resourceSpans` decoded (strict,
  as the others), the request path kept on each received export, helpers for spans
  and their attributes.

## Decisions made during planning

- **Encoded on the caller's side:** the caller (the capture finisher) encodes the span
  to learn its size for the budget, then hands over the bytes; the exporter
  concatenates them into an envelope. No second encoding, no span structs held in
  the queue.
- **No sampling, no span limits, no propagation:** nothing in this plan needs them.
- **Shared code with `otlplog` only where the purpose is the same** (CODING-RULES §2):
  the sender loop and the failure report may share a core if they read the same; the
  queue bounds differ by design.

## Acceptance criteria

- Unit tests: the encoding against the OTLP JSON rules (hex IDs, enums, nanoseconds,
  every attribute type, status); a batch never over the limit; a span alone over the
  limit refused by the caller's check; `release` called once on every path (exported,
  rejected in a partial success, failed after retries, shut down); retries and the
  delivery rules through `fakeotlp` as the log exporter's tests do.
- Settings tests: the explicit switch (general endpoint alone → off; traces endpoint →
  on; `OTEL_TRACES_EXPORTER=otlp` → on; `none` → off); malformed values fail.
- The telemetry tree's import check passes (no kaiak imports but `netfail`).
- `scripts/check-gateway.sh` green.

## Result

### What changed

- `internal/telemetry/otlp`:
  - `signal.go`: `Traces` — its variables, `v1/traces`, `ExportTraceServiceResponse`,
    `rejectedSpans`, and `explicitSwitch`, the signal's property the reader consults.
  - `settings.go`: `readEndpoint` decides on or off: the signal's own endpoint, else —
    for a signal with the explicit switch only with `OTEL_<SIGNAL>_EXPORTER=otlp` —
    the general endpoint with the signal's path, else with `otlp` the localhost
    default. Without the switch the general variable is not read for traces (as
    `none` already skips a malformed endpoint).
  - `client.go`: `readExportResponse` reads `rejectedSpans`; the package doc names the
    trace export and the shared pieces below.
  - `sender.go` (new): `Sender` — the background sender the queued exporters share:
    `StartSender(tick, send)`, `Wake`, `ForceFlush`, `Stop`, `ErrShutdown`; one
    export in flight, full batches on a wake, everything at each 1 s tick and flush.
  - `problems.go` (new): `Problems` — failed items by `error.type` and the `… export
    failing` line (first failure or drop, then at most once a minute, `ReportNow`
    never held back), named by a `ProblemLine`.
- `internal/telemetry/otlplog`: moved onto `otlp.Sender` and `otlp.Problems` with no
  change of behavior; its queue (a count with a capacity, dropping the newest) stays
  its own. Its tests pass unchanged but for `otlp.ErrShutdown`.
- `internal/telemetry/otlptrace` (new):
  - `span.go`: `Span` (trace and span IDs, name, kind, start and end, attributes,
    status), `NewTraceID`/`NewSpanID` (crypto/rand), `KindClient`, `StatusUnset`/
    `StatusError`, `Attribute` (OTLP's `KeyValue`) with `String`, `Int`, `Float64`,
    `Bool`, `Strings`.
  - `encode.go`: `Encode(Span)` — lowercase hex IDs, integer kind and status code,
    decimal-string nanoseconds and `intValue`s, the status only when set, no HTML
    escaping, in a slice of its own size; the envelope (`resourceSpans` → the client's
    resource → `scopeSpans` → scope `kaiak` → `spans`) built once per exporter.
  - `exporter.go`: `New(settings, service, report)`; `Enqueue(encoded, release)` never
    waits and has no capacity of its own; a request is taken when the queue reaches
    the export limit (16 MiB with the envelope) or at the tick, as many of the oldest
    spans as fit (at least one); `release` runs once per span after its export ends
    (counted first), or at shutdown, or at once for a span queued after shutdown;
    the body takes the spans' bytes and the queue lets go of them;
    `MaxSpanBytes()` = the limit less this exporter's envelope; `Counts` (`Handed`,
    `Shutdown`, `Exported`, `Failed` by `error.type`); `ForceFlush`, `Shutdown`;
    `trace export failing` with `kaiak.trace_export.failed`/`.dropped`.
- `internal/telemetry/fakeotlp`: `resourceSpans` decoded strictly (`Span` refuses
  `parentSpanId`, events, links, any unknown member); `Received.Path`;
  `Received.Spans`, `Collector.Spans`, `Span.Attrs(t)` (plain values, `intValue` read
  as `int64`, exactly one kind per value).
- `docs/ARCHITECTURE.md`: the `telemetry/otlp` bullet names the shared sender and
  problem report.
- No dependency added; nothing in `cmd/kaiak` or `internal/metrics` reads the new code
  (step 5).

### Decisions made while implementing

- **`MaxSpanBytes` is a method of the exporter**, not a package constant: the
  envelope carries `OTEL_RESOURCE_ATTRIBUTES`, so the room for a span is known only
  per exporter. Step 6 checks `len(encoded) > exp.MaxSpanBytes()`. A span over it,
  queued anyway, goes alone in a request over the limit (pinned by a test).
- **`release` may run within `Enqueue`** (a span queued after `Shutdown`): the caller
  must not hold anything `release` takes when it calls `Enqueue`.
- **Shared with `otlplog`: the sender and the problem report**, the same purpose in
  both (and the spec's "as for logs"). The queues stay apart: a count with a
  capacity against bytes under a caller's budget with release callbacks.
  `otlpmetric`'s own report (no drops, periodic) is left as it is.
- **IDs live with the span model** (`NewTraceID`, `NewSpanID`), for step 6.

### Tests

- `otlptrace` (17 tests, 24 with subtests): `TestEncodeSpan` (the exact bytes: hex
  IDs, kind 3, nanoseconds, `intValue` 2^53+1 as a string, double, bool, string
  arrays including an empty one, `<`/`&`/`>` unescaped, `status {2, message}`; then a
  strict decode by `fakeotlp`), `TestEncodeSpanWithoutErrorHasNoStatus`,
  `TestEncodedSpanHoldsItsOwnSize`, `TestEnvelope`, `TestNewIDsAreRandom`,
  `TestExportRequestShape` (path `/v1/traces`, headers, resource, scope, decoded
  span), `TestFullRequestGoesAtOnce` (two spans filling the limit exactly go at once,
  the rest waits for the tick), `TestRequestsNeverPassTheLimit` (60 spans of random
  sizes: every request ≤ the limit, order kept), `TestMaxSpanBytes` (16 MiB less the
  envelope; a resource attribute shrinks it by its own JSON; a span of exactly
  `MaxSpanBytes` makes a request of exactly the limit, one byte more passes it),
  `TestReleasedOnceOnEveryOutcome` (exported, rejected in a partial success, failed
  after retries — two attempts then out of time —, a 2xx that cannot be read, a
  status not retried), `TestReleaseWaitsForTheExportToEnd`,
  `TestEnqueueNeverWaitsAndDropsNothing` (2 000 spans behind a stalled collector),
  `TestShutdownReleasesWhatIsLeft` (the cut export `timeout`, the queued `shutdown`,
  a late span released at once, two report lines), `TestRetriedUntilDelivered`,
  `TestRetryAfterBeyondTheTimeLeftFailsAtOnce`, `TestRedirectIsNotFollowed`,
  `TestCollectorTextIsNeverReported`.
- `otlp`: `TestTraceExportNeedsAnExplicitSwitch` (13 cases: the general endpoint
  alone → off, and not read; the traces endpoint → on as is; `otlp` → the general
  endpoint with `/v1/traces` or the localhost default; `none` and
  `OTEL_SDK_DISABLED` → off), `TestTraceSettings` (the signal's variables win, no
  interval; nine malformed values fail naming the variable, never echoing a secret,
  and leave logs untouched; a malformed general endpoint fails once switched on),
  `rejectedSpans` rows in `TestPartialSuccessMemberIsTheSignals`, `Sender` (4
  tests) and `Problems` (3 tests). `TestUnknownSignalPanics` uses `profiles` now
  that `traces` is known.
- Mutation check (reverted): letting `take` pass the limit by 200 bytes fails
  `TestRequestsNeverPassTheLimit` (`request 12 of 4250 bytes, over the limit of
  4096`).
- `go test -race -count=5` over `otlp`, `otlplog`, `otlptrace`: green.

### Suite

- `scripts/check-gateway.sh`: green. gofmt, vet, staticcheck and the telemetry
  boundary are clean (the tree still imports only the standard library and
  `netfail`); `go test -race -count=1` passes every package (916 tests, examples and
  fuzz targets listed — 890 at step 3, plus the 26 above). The live-test kit's lint
  is clean and its self-test passes for vllm, llama-server, openai, azure-openai,
  anthropic, azure-anthropic, and vllm with two backends.
- `scripts/check-all.sh`: green — phase 2 stays green mid-phase, as planned (nothing
  reads the new code before step 5). Control `npm test`: 679 tests, 678 pass, 0 fail,
  1 skipped (the store contract's reconnect test, as before). `npm run lint`: tsc
  clean, "boundaries ok". The cross-half e2e passes. Final lines:

  ```
  ==> npm run lint (control)
  > tsc -p . && node scripts/check-boundaries.ts
  boundaries ok
  ==> cross-half e2e (sample control plane + two gateways; two cores over one store)
  ok  	kaiak/e2e	80.410s
  all checks passed
  ```
