# Step 3 — exporter

**Status:** not started

## Intent

A self-contained exporter package, on top of the vocabulary step 2 settled: read its
settings from the environment, encode `slog` records as OTLP/HTTP JSON, queue and
send them in the background without ever blocking the caller, and flush on demand.

## Files likely touched

- `gateway/internal/otlplog/` (new package; name settled in the step if a better one
  fits the code's naming):
  - settings from a `lookupEnv` function (the pattern `cmd/kaiak` uses), with the
    fallbacks, defaults and refusals the spec names;
  - an `slog.Handler` that passes each record to the next handler (stderr) and
    enqueues a copy — `WithAttrs` / `WithGroup` carried to both;
  - the JSON encoding of a batch (`resourceLogs` → `scopeLogs` → `logRecords`),
    64-bit integers as strings as OTLP JSON requires;
  - the sender: one export in flight, batches by size and interval, retries with
    backoff and `Retry-After`, drop-newest on a full queue, counters for exported,
    dropped and failed records, `Flush(ctx)` and `Close`.
- Tests in the package with an `httptest` collector.

## Decisions made during planning

- The handler copies what it needs from the record at `Handle` time (attributes are
  resolved then), so the caller's record is never shared with the sender.
- The package does not import `metrics`: it exposes counts the wiring step reads
  (`internal/` packages stay acyclic).

## Acceptance criteria

- Tests: every variable and fallback; malformed values and the protocol refusal;
  every attribute kind, nested groups, `WithAttrs`/`WithGroup`; the encoded batch
  matches a hand-checked fixture; batch by size and by interval; retry on each
  retryable status and on a network error, honouring `Retry-After`; no retry on
  `400`; drop-newest when full, counted; a stalled collector never blocks `Handle`
  (`-race`); `Flush` delivers what is queued within its deadline.
- `scripts/check-gateway.sh` green. Suite recorded.

## Result

_Not started._
