# Step 3 — exporter

**Status:** done (2026-10-05)

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

**What changed**

- New package `gateway/internal/otlplog` (the plan's name kept: `otlp` + `log`, as
  `logattr` is `log` + `attr`). It imports only the standard library and `clip`; not
  `metrics`. Not wired yet (step 4).
  - `settings.go` — `ReadSettings(lookupEnv) (*Settings, error)`: nil when export is
    off; every `OTEL_*` variable of the spec with its fallback, default and refusal;
    `(*Settings).EndpointHost()` (`host:port`, the port filled in from the scheme —
    for `kaiak.log_export.endpoint`); `(*Settings).String()` prints the endpoint
    redacted and the header count, never a header.
  - `handler.go` — the `slog.Handler`: `Enabled` is the next handler's; `Handle`
    queues a copy taken then (attributes resolved, string arrays copied) and hands
    the record to the next handler; `WithAttrs`/`WithGroup` go to both, groups
    flattened with `.` (empty group left out, keyless group inlined, empty attribute
    ignored, as slog's handlers do). Value mapping per the spec, plus: a `uint64`
    above `int64` → its decimal text; a duration → `intValue` nanoseconds; a time →
    RFC 3339 with nanoseconds; an error → its message (its JSON when it marshals
    itself); anything else → the JSON handler's text.
  - `encode.go` — `ExportLogsServiceRequest` as JSON: lowerCamelCase keys,
    `timeUnixNano`/`observedTimeUnixNano`/`intValue` as decimal strings,
    `severityNumber` an integer (`level + 9`, clamped to 1–24: DEBUG 5, INFO 9,
    WARN 13, ERROR 17), `severityText` the slog level name, `body` a
    `stringValue`, NaN/±Inf doubles as `"NaN"`/`"Infinity"`/`"-Infinity"` (the
    protobuf JSON mapping). Resource: `service.name`, `service.version`,
    `service.instance.id`, then the other `OTEL_RESOURCE_ATTRIBUTES`; one scope
    `kaiak`, no version, no `schemaUrl`.
  - `exporter.go` — `New(*Settings, Resource{ServiceVersion, InstanceID},
    report *slog.Logger) *Exporter`; `(*Exporter).Handler(next) slog.Handler`;
    `Flush(ctx) error`; `Close()`; `Counts() Counts{Exported, Failed, Dropped}`.
    Queue 10 000, drop-newest; batches of 512 at once, a partial batch at the 1 s
    tick or a flush; one sender goroutine (one export in flight), owned by `Close`.
    Each `POST` sends `Content-Type: application/json`, `User-Agent:
    kaiak/<version>` and the configured headers. Retries on network errors and
    429/502/503/504: backoff 0.5 s doubling to 5 s, jitter over the upper half of
    each step, or the `Retry-After` (seconds or HTTP date), until the batch's
    timeout — a wait not shorter than the time left fails the batch at once. Any
    other status, or an answer above 4 MiB, fails it at once. A partial success
    counts its rejections as failed. `log export failing` (warn) goes to `report`
    at the first failure or drop, then at most once a minute, with the failed and
    dropped counts since the previous line, `http.response.status_code` when the
    collector answered and `exception.message` (the collector's Status message when
    it sent one, clipped).
- Tests (`settings_test.go`, `handler_test.go`, `exporter_test.go`, fixture
  `testdata/batch.json`), against `httptest` collectors, with a manual tick, retry
  waits that record their length and end at once, and a fake clock for the report
  limit. No sleeps for synchronization: `Flush`, an unbuffered tick and a
  batch-done hook are the sync points.
  - Settings: on/off for every combination (empty counts as unset; `none` and
    `OTEL_SDK_DISABLED=true` win over endpoints, even malformed ones; `otlp` with no
    endpoint → `http://localhost:4318/v1/logs`); the `LOGS` variable winning for
    endpoint, headers, timeout and protocol; `v1/logs` joined to the general
    endpoint's path (with and without a trailing slash, under a base path); service
    name precedence; resource decoding and the gateway's keys left out; 24
    malformed values, each error naming its variable and never holding a secret;
    the settings printed by `fmt`, the text and the JSON handler hold no secret.
  - Handler: the next handler's output is byte-identical with or without the
    exporter (including `WithAttrs`/`WithGroup` and the level threshold); 19
    attribute kinds; flattened groups (`WithAttrs` before and after `WithGroup`,
    nested, empty, keyless, empty attribute); copy at `Handle` (a mutated slice);
    severity numbers.
  - Encoding: the batch equals the hand-written fixture (decoded with
    `UseNumber`, so `"200"` and `200` differ — checked by mutating the fixture);
    NaN in the fixture, ±Inf and large doubles in a unit test. The fixture's times
    were computed with `date`, not Go (`2026-10-05T12:00:00Z` = 1791201600).
  - Delivery: the request's headers, resource and scope; batch by size (512 at
    once, the 513th waits for the tick) and by interval; retry on 429, 502, 503,
    504 (two retries, backoff within range) and on a dropped connection; backoff
    doubling to 5 s; `Retry-After` in seconds, `0`, an HTTP date, a past date and
    an unreadable one (backoff); `Retry-After` beyond the time left and backoff
    beyond the timeout fail at once; no retry on 400, 401, 404, 413, 500, with the
    report's status and message; partial success (string and number forms, more
    rejected than sent, warning only, empty and non-JSON bodies); an answer above
    4 MiB fails unretried (200 and 503); drop-newest when full, counted and
    reported; a stalled collector never blocks logging (1 000 lines, `-race`);
    `Flush` delivers 600 queued records in order; `Flush` ends at its deadline and
    `Close` counts the cut export as failed and the queue as dropped; after
    `Close` the next handler still writes and the record is dropped; report rate
    limit (first at once, none within the minute, then one with the totals since).
  - 30 runs in a row under `-race`: green.

**Decisions made during the step**

- Package API for step 4: `ReadSettings` → `New(settings, Resource{…}, report)` →
  `Handler(stderrHandler)`; `Flush(ctx)` then `Close()` at exit; `Counts()` for the
  metric. `New` starts the sender, `Close` stops and waits for it (no `Run(ctx)`:
  the exporter must outlive `run`'s context to flush after `kaiak stopped`).
  `report` must be a logger on the stderr handler alone.
- Only the on/off switches (`OTEL_SDK_DISABLED`, `OTEL_LOGS_EXPORTER`) are checked
  when export is off; every other variable is read only when it is on — "turn it
  off whatever else is set", and nothing is attempted when nothing is set. An
  injected `OTEL_EXPORTER_OTLP_PROTOCOL=grpc` with no endpoint does not fail a start.
- `OTEL_LOGS_EXPORTER` and the protocol are matched in any case (the specification
  reads enums case-insensitively); a list (`otlp,console`) is refused.
- Headers and resource attributes: spaces around members, keys and values trimmed;
  empty members (a trailing comma) skipped; a later duplicate resource key wins.
  Header names must be HTTP tokens and decoded values free of control characters
  (else `net/http` would refuse every export); resource keys are percent-decoded
  too (the specification has `,` and `=` encoded in keys as well). Errors name the
  entry by position, never its text; endpoint errors never echo the URL (it may
  carry credentials).
- The endpoint's query is kept (the specification lets an exporter ignore it, and
  keeping it is "as is"). Configured headers go first; `Content-Type` and
  `User-Agent` are the exporter's.
- Network errors lose the `url.Error` wrapper, so a report never repeats the
  endpoint (query included). An export cut by the batch timeout or by `Close`
  says `export cut short`.
- A 2xx whose body is empty, not JSON or cut short counts as delivered: the status
  says the batch was accepted. A non-2xx's Status `message` is added to the error
  when the body is JSON.
- `Retry-After` is not capped at 5 s (the spec: honoured until the timeout); a value
  in seconds beyond a day is treated as unreadable (overflow guard), which then
  backs off.
- After each report the last status and error reset, so a later drops-only line
  carries neither. `Close` reports too, within the same once-a-minute limit — so
  drops counted at exit right after a reported failure show only in the counts
  (the spec's limit, kept).
- An export in flight when `Close` runs counts as failed (it was sent), the queue
  left behind as dropped (never sent).
- `docs/ARCHITECTURE.md` and `docs/architecture/gateway.html` are left to step 4:
  the shape changes when the package is wired.

**Suite** — `scripts/check-all.sh`, green:

```
==> gofmt / go vet / staticcheck 2026.2.1 (gateway)
==> go test -race (gateway): ok cmd/kaiak, e2e, accounting, auth, clip, config,
    control, limits, logattr, metrics, otlplog (1.516s), provider, routing,
    schemacheck, server, sse, state
==> gofmt / go vet / staticcheck (live-test kit); self-test passed for vllm,
    llama-server, openai, azure-openai, vllm with two backends
==> npm test (control): tests 574, pass 574, fail 0
==> npm run lint (control): boundaries ok
==> cross-half e2e: ok kaiak/e2e 69.266s
all checks passed
```
