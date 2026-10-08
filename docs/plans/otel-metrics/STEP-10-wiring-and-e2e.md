# Step 10 — wiring and e2e

**Status:** done (2026-10-08)

## Intent

The gateway exports its metrics when configured, from the first collect to the
final export at exit, and an e2e test proves the push and the scrape agree.

## Files likely touched

- `gateway/cmd/kaiak/main.go` — start the exporter after the registry's families
  exist; the exit order (decision 21): usage flush, final status, final metric
  export, `kaiak stopped`, log flush; bounded like the log flush (the drain's
  deadline, or 1 s from its start, whichever is later; a second signal cuts it); a
  start that fails once the exporter runs exports what it has.
- `gateway/e2e/` — a fake collector for metrics (beside the logs one in
  `fakeotlp`): a gateway in file mode and in control-plane mode serves traffic; the
  push and a scrape taken after `ForceFlush` agree family by family (names through
  the translation, units, attributes, values); nothing is sent with no endpoint or
  with `OTEL_METRICS_EXPORTER=none`; the final export arrives at exit; one endpoint
  variable feeds both signals; the resource is the logs'.

## Decisions made during planning

- e2e compares a push and a scrape at a quiet moment (no traffic in flight) so
  values match exactly.

## Acceptance criteria

- The tests above pass under `-race`, uncached.
- Phase 3 end: `scripts/check-all.sh` green. Suite recorded.

## Result

### What changed

- `gateway/cmd/kaiak/main.go`:
  - The metric exporter starts when metric export is on, after every family is
    registered and before the config load or the boot — so a start that fails once
    it runs (a rejected config, a boot with no config) ends with the final export.
    Its own count (`metrics.RegisterMetricExport`) is registered right after `New`,
    an interval before the first collect. Its problem reports go to the process
    logger (the log export, when on, carries them), as the spec says.
  - The usage metrics moved into `newGraph` (`g.usage`): they are registered before
    the exporter starts instead of after the mode's setup.
  - Exit order, in run's deferred end: the final metric export, then `kaiak stopped`
    (moved from the body into the defer, with its reason — the drain's or the boot's
    stop signal) or `kaiak stopped with an error`, then the log flush. On the normal
    path the background work, the usage flush, the final status and the admin
    listener are all done by then.
  - One bound for both exporters at exit: `exitBound(hurry, by)` — ends at the
    drain's deadline or 1 s from its own start, whichever is later; once hurry ends
    (a second signal, before or during), the 1 s floor alone. `finishExport` (one
    function over an `exporter` interface: `ForceFlush` within the bound, then
    `Shutdown`) replaces `finishLogExport`; `logExportFlushFloor` became
    `exitExportFloor`. The log flush's behavior is unchanged (its two `ForceFlush`
    calls were the same bound in two steps; the metric exporter cuts its export at
    its context's end, so it needs the bound as one context).
- `gateway/cmd/kaiak/controlplane.go`: `startControlPlane` split into
  `newControlPlane` (limiter, client, their metric families) and `controlPlane.boot`
  (boot and the first-totals wait), so the exporter starts between the two.
- `gateway/internal/telemetry/metric`: `PrometheusName` and `PrometheusLabel`
  exported, so the e2e test translates the push with the registry's own translation.

### Tests

- `cmd/kaiak/metricexport_test.go`: `TestFinalMetricExportBeforeTheLastLine` (drained
  and start-failed: the one export is the final one, it arrives before `kaiak
  stopped` / `kaiak stopped with an error` is written, and carries
  `kaiak.config.loads{startup, applied|rejected} = 1`);
  `TestSecondSignalCutsTheFinalMetricExport` (mirror of the log one: a stalled
  final export, a second signal, run returns within the 1 s floor; the cut export is
  reported `metric export failing` … `export cut short` before `kaiak stopped`).
- `e2e/metricexport_test.go`:
  - `TestMetricExport` — file mode, one `OTEL_EXPORTER_OTLP_ENDPOINT` (with a path and
    a header) feeding both signals, 200 ms interval; a success, a 401, a limit
    refusal, an upstream 500. Quiet moment: scrape, the next export collected after
    it, scrape again — the export and the first scrape agree family by family (HELP,
    TYPE, every series by translated name and labels, values exactly), and the two
    scrapes agree, but for the named movers: the metric exporter's own count (each
    export counts its points after its own collect) and the log export's two counts
    (they send on their own schedule). Cumulative temporality. The final export:
    collected after SIGTERM and before the `kaiak stopped` line's time, equal to the
    last scrape (same movers), sent before the log flush that carries `kaiak
    stopped`; one resource (with `OTEL_RESOURCE_ATTRIBUTES`) on every log and metric
    export, scope `kaiak`; paths `/base/v1/logs`, `/base/v1/metrics`.
  - `TestMetricExportControlPlane` — fakecontrol, metrics endpoint only; after the
    usage is acked and the queue empty, the same quiet comparison (only the
    exporter's own count moves) and the control and usage-delivery families present.
  - `TestMetricExportOff` — no endpoint; `OTEL_METRICS_EXPORTER=none` beside a shared
    endpoint: no metric export even at exit, no exporter series, no endpoint on
    `kaiak starting`.
  - `TestMetricExportDelta` — delta preference: counters and histograms
    `aggregationTemporality` 1, up-down counters 2; every series of
    an export is in the next; an export with no traffic since the last carries the
    usage-record series at 0; summed over every export (the final one included)
    each counter and histogram series equals the scrape before SIGTERM (exporter's
    own count excluded, same reason), each up-down counter and gauge's last value
    equals it.
- `e2e/logexport_test.go`: with a shared endpoint the final metric export reaches the
  same collector, so the helpers read log exports only (`logExports`), TestLogExport
  accepts `/base/v1/metrics` for metric exports, TestLogExportOff counts log exports;
  the stalled-collector test now expects about 2 s (1 s per signal) and also checks
  the `metric export failing` report (`cut short`).
- Mutations tried and caught (then reverted): final metric export after the last
  line (cmd test and e2e both fail); the bound ignoring hurry (both second-signal
  tests fail).

### Suite

- `go test -race -count=4 -run 'TestMetricExport|TestLogExport' ./e2e/` ok (101.6 s);
  `go test -race -count=5 -run 'Metric|Final|Second' ./cmd/kaiak/` ok.
- Phase 3 end — `scripts/check-all.sh` twice, both exit 0:
  - run 1: gofmt, vet, staticcheck, telemetry boundary; race tests all ok
    (`cmd/kaiak` 5.3 s, `kaiak/e2e` 119.7 s, `telemetry/*` ok); live-test kit;
    control `npm test` 629 tests, 628 pass, 1 skipped, 0 fail; lint `boundaries ok`;
    cross-half e2e `ok kaiak/e2e 66.011s`; "all checks passed" (3:26).
  - run 2: the same, `cmd/kaiak` 5.5 s, `kaiak/e2e` 118.6 s, npm 629/628/1/0,
    cross-half `ok kaiak/e2e 65.411s`, "all checks passed" (3:21).
