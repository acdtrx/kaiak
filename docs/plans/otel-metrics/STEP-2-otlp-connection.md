# Step 2 — the OTLP connection

**Status:** done (2026-10-08)

## Intent

One OTLP/HTTP connection for every signal, holding the delivery rules the logs
settled, and the log exporter moved onto it inside the telemetry tree. No behaviour
change.

## Files likely touched

- New `gateway/internal/telemetry/otlp/`:
  - settings: the `OTEL_*` reading of `otlplog/settings.go`, made per signal — a
    signal's endpoint (`OTEL_EXPORTER_OTLP_<SIGNAL>_ENDPOINT` as is, else the shared
    one plus `/v1/<signal>`), headers, timeout, protocol (`http/json` only), the
    signal's `OTEL_<SIGNAL>_EXPORTER`, `OTEL_SDK_DISABLED`; `OTEL_SERVICE_NAME` and
    `OTEL_RESOURCE_ATTRIBUTES` once for all;
  - the resource and its JSON encoding;
  - the client (no redirects) and one export: `POST` a body, read the answer by the
    rules (empty or the signal's `Export…ServiceResponse`, the partial-success count
    under the signal's field name — `rejectedLogRecords`, `rejectedDataPoints`,
    `rejectedSpans`), retry with backoff and `Retry-After` within the timeout, and
    report the outcome in the gateway's own words (status, rejected count, failure
    class) — never the collector's text;
  - the shared JSON encoding helpers (`anyValue`, 64-bit integers as strings,
    doubles, `keyValue`).
- `gateway/internal/otlplog/` → `gateway/internal/telemetry/otlplog/`: the handler,
  queue, batching and problem reports stay; settings, client, `post`,
  `readExportResponse`, `retryAfter`, `backoff` and the encoding helpers move to
  `otlp`. `Flush`/`Close` become `ForceFlush(ctx)`/`Shutdown(ctx)` (decision 12),
  same semantics.
- `gateway/cmd/kaiak/main.go` — wiring of the moved package and the per-signal
  settings (logs only so far).
- `scripts/check-gateway.sh` — the boundary check: no package under
  `internal/telemetry/` imports `kaiak/…` other than `kaiak/internal/telemetry/…`
  and `kaiak/internal/netfail` (`go list -deps`).
- Tests move with their code: `otlplog/{settings,exporter,handler}_test.go` split
  between `otlp` and `otlplog`; what they assert is unchanged.

## Decisions made during planning

- `otlp` knows nothing of batching or queues: each signal's exporter batches its own
  way (logs queue records; metrics collect on an interval) and hands `otlp` one body.
- The `log export failing` report stays in `otlplog`: its attributes are the logs'.
  `otlp` returns what the report needs.
- An unknown signal name is a programming error (panic), not a setting.

## Acceptance criteria

- Every test of `otlplog` before the step has a counterpart after it, asserting the
  same (count tests before and after; a dropped one named with its reason).
- The boundary check fails on a planted `kaiak/internal/config` import in
  `telemetry/` (tried once, recorded, reverted).
- `go test ./internal/telemetry/...` passes on its own.
- `scripts/check-all.sh` green. Suite recorded.

## Result

### What changed

- New `gateway/internal/telemetry/otlp/`:
  - `signal.go` — `Signal` (`Logs`, `Metrics`) and its description: the signal's
    own variables, the endpoint path, the export response's name, the
    partial-success member and what it counts (`records`, `data points`). An
    unknown signal panics.
  - `settings.go` — `ReadSettings(sig, lookupEnv)`, the old reading made per
    signal (the signal's variable, else the general one); the resource's variables
    read by one `readResource` for every signal, only when that signal is on (as
    before: a malformed `OTEL_RESOURCE_ATTRIBUTES` with export off is not an
    error). `EndpointHost`, `String` (now `otlp.Settings{signal: …}`; still no
    headers).
  - `encode.go` — the shared messages (`Resource`, `InstrumentationScope`,
    `ScopeName`, `KeyValue`, `AnyValue`, `ArrayValue`, `Double`), value
    constructors (`StringValue`, `IntValue`, `DoubleValue`, `BoolValue`,
    `StringsValue`), `UnixNano`; `Service` (version, instance ID) and the
    resource built from settings + service.
  - `client.go` — `NewClient(s, svc)` (no redirects, `User-Agent: kaiak/<v>`),
    `Resource()`, `CloseIdleConnections()`, `Export(ctx, body) Outcome`: one
    export with retries, backoff and `Retry-After` within the settings' timeout
    (or ctx); `Outcome{Status, Err, Rejected, RejectErr}` in the gateway's words;
    `readExportResponse` reads the signal's member (`rejectedLogRecords`,
    `rejectedDataPoints`) and ignores the other's.
- `gateway/internal/otlplog/` → `gateway/internal/telemetry/otlplog/`: queue,
  batching, counts, problem reports and the handler stay; `export` hands the body
  to `otlp.Client.Export` and counts the outcome (the clamp of rejected to the
  batch size stays here). `Flush`/`Close` → `ForceFlush(ctx) error` /
  `Shutdown(ctx)`, same semantics; `Resource` → `otlp.Service`; `errClosed` →
  `errShutdown`.
- `gateway/internal/fakeotlp/` → `gateway/internal/telemetry/fakeotlp/`
  (unchanged code); imports in `cmd/kaiak/logexport_test.go`,
  `e2e/logexport_test.go`.
- `gateway/cmd/kaiak/main.go`, `settings.go` — `otlp.ReadSettings(otlp.Logs, …)`,
  `otlplog.New(…, otlp.Service{…}, …)`, `finishLogExport` on
  `ForceFlush`/`Shutdown`.
- `scripts/check-gateway.sh` — the telemetry boundary step after the lint: every
  package of `./internal/telemetry/...` with `go list -test`, its full `.Deps`
  (transitive, test variants included); a `kaiak/…` dependency other than
  `kaiak/internal/telemetry/…` or `kaiak/internal/netfail` fails, naming the
  package and the dependency.
- Docs: `ARCHITECTURE.md` (`fakeotlp` entry moved under the telemetry tree),
  `docs/architecture/gateway.html` (the `otlplog` and `fakeotlp` rows' paths).

### Decisions settled while implementing

- **`fakeotlp` moves into the tree**: the tree's tests may use no kaiak fixture,
  and the boundary check covers test imports; the fake collector is OTLP test
  tooling that leaves with the tree. `cmd/kaiak` and e2e import it from there.
- **One settings read per signal**, the resource part shared code: main calls
  `ReadSettings` once per signal (logs only so far). Step 8 adds the metric-only
  variables (interval, export timeout, temporality) to the `Metrics` read; the
  `Metrics` signal's endpoint, headers, timeout, protocol and exporter variables
  are already read and tested.
- **`Shutdown(ctx)` does not consult ctx**: cutting the export in flight is what
  stops the sender, so its wait never depends on the collector; the time for
  sending what is queued is `ForceFlush`'s. No error return (it had none).
- **No failure class in `Outcome` yet**: the counts' `error.type` values (step 1's
  list) arrive with the counts' new family; `Outcome` is where they will be read
  from (status, cut short, transport class, malformed answer).
- `Scope` is a constant name (`otlp.ScopeName`), not a shared mutable value.

### Tests

- Before: `internal/otlplog` 41 top-level tests, 161 passes with subtests.
- After: `otlp` 29 (146 passes), `otlplog` 22 (57 passes): 51 (203). None
  dropped. Renamed: `TestAfterClose` → `TestAfterShutdown`,
  `TestFlushDeliversWhatIsQueued` → `TestForceFlushDeliversWhatIsQueued`,
  `TestFlushEndsWithItsDeadlineAndCloseDrops` →
  `TestForceFlushEndsWithItsDeadlineAndShutdownDrops`,
  `TestLongRetryAfterFailsTheBatch` → `TestLongRetryAfterFailsTheExport` (otlp).
- Moved to `otlp`, asserting the same on the outcome (requests made, waits,
  delivered or failed, status, error text, rejected count) where they asserted
  counts: retryable statuses, backoff, network error, `Retry-After` (honoured,
  beyond the time left, below the backoff, saturating), timeout, oversized answer,
  redirects (target not reached, login page), unreadable 2xx, answers that
  deliver, transport and collector text (`…IsNeverInTheOutcome`), the settings
  tests, `Double`. Kept in `otlplog`, unchanged in what they assert, the tests of
  counts and `log export failing` reports — `NoRetryOnOtherStatuses`,
  `PartialSuccessCountsRejectedAsFailed` (with the clamp),
  `PartialSuccessIsReported`, `CollectorTextIsNeverReported`,
  `TransportErrorTextIsNeverReported`, `ProblemReportsAreRateLimited` — with
  `otlp` counterparts for the outcome side. `TestBatchByInterval` also asserts no
  report for a delivered batch (the "no reports" the moved delivery tests held).
- New in `otlp`: `TestResource`, `TestPartialSuccessMemberIsTheSignals`,
  `TestSettingsPerSignal`, `TestUnknownSignalPanics`,
  `TestExportCutShortByItsContext`, `TestExportRequest`.

### Boundary check tried

`_ "kaiak/internal/config"` planted in `otlp/client.go`: `scripts/check-gateway.sh`
passed gofmt, vet and staticcheck, then failed at `==> telemetry boundary` with
`kaiak/internal/telemetry/otlp … depends on kaiak/internal/config` (and its
transitive `logattr`, `schemacheck`), exit 1. `_ "kaiak/internal/fakebackend"`
planted in `otlplog/handler_test.go` alone: failed with
`kaiak/internal/telemetry/otlplog.test depends on kaiak/internal/fakebackend`.
Both reverted; the step passes clean.

### Suite

- `go test ./internal/telemetry/...` alone from `gateway/`: `otlp` ok, `otlplog`
  ok (`fakeotlp` no test files).
- `scripts/check-all.sh`: exit 0 — gofmt, vet, staticcheck, telemetry boundary,
  race tests (`kaiak/e2e` 105.0 s, `telemetry/otlp`, `telemetry/otlplog` ok), the
  live-test kit's lint and self-test, control `npm test` (629 tests, 628 pass, 1
  skipped, 0 fail), lint (`boundaries ok`), cross-half e2e `ok kaiak/e2e
  65.345s`, "all checks passed".
- `git grep kaiak/internal/otlplog\|internal/fakeotlp` outside `docs/plans` and
  `docs/reviews`: nothing.
