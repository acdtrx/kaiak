# Step 4 — the trace exporter

**Status:** not started

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
