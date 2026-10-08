# Audit B — OTLP metrics branch

Reviewed the final tree against `BRANCH.diff` (v0.11.6), `docs/specs/GATEWAY.md`, and `docs/TECH-STACK.md`. **Two new correctness findings**, both medium severity. An inherited histogram limitation is recorded separately. Production code is unchanged; reproduction tests are in two `*_codex_test.go` files.

## 1. Medium / P2 — Collection and encoding escape the export and shutdown time bounds

**Location:** `gateway/internal/telemetry/otlpmetric/exporter.go:222` and `:234`; shutdown waits at `:177`.

`export` collects the entire registry, computes deltas, and encodes every split request **before** starting `e.exportTimeout`. Neither collection nor encoding observes cancellation. The shorter of the two configured timeouts is calculated correctly, but bounds only the subsequent HTTP work.

**Concrete scenario:** a gateway with 100,000 accumulated usage series exports with `OTEL_METRIC_EXPORT_TIMEOUT=10`. On this tree, the reproduction starts its first HTTP request **1.647 seconds** after starting the export, despite the 10 ms whole-export bound. The collector is a local server; there is no deliberately slow callback or transport. This matters at the documented target scale, where the spec explicitly anticipates exports measuring tens of megabytes.

The same mechanism affects exit: `ForceFlush` can return when its context expires, but `finishExport` then calls `Shutdown`, which waits for the uncancellable collection/encoding to finish. A second signal cancels the context but cannot stop that work. Thus the documented drain deadline / one-second floor is not an upper bound when the exporter is collecting or encoding. The reproduction measures the export-timeout failure; this shutdown consequence follows from the same synchronous path.

**Reproduction:** `gateway/internal/telemetry/otlpmetric/export_codex_test.go`, `TestCodexExportTimeoutIncludesEncoding`.

```sh
cd /tmp/kaiak/gateway
GOCACHE=/tmp/kaiak-audit-go-cache go test -race -count=1 -run TestCodexExportTimeoutIncludesEncoding ./internal/telemetry/otlpmetric/
```

Observed:

```text
--- FAIL: TestCodexExportTimeoutIncludesEncoding (2.40s)
    export_codex_test.go:52: first HTTP request started after 1.6470995s, despite a 10ms whole-export timeout
FAIL
```

**Fix direction:** start the deadline before collection and make the potentially large collect/transform/encode work cancellation-aware. Merely moving `WithTimeout` earlier prevents late HTTP sends but does not bound shutdown's wait. Current timeout and second-signal tests stall HTTP delivery with small registries; they miss this CPU-work phase.

## 2. Medium / P2 — A series born during collection has an inverted OTLP time interval

**Location:** `gateway/internal/telemetry/metric/collect.go:163` and `:184`; series creation at `gateway/internal/telemetry/metric/registry.go:296`.

`Collect` captures its timestamp before callbacks and before copying each family's series map. A request can create a series after that timestamp but before its family is copied. The snapshot includes that point with its later creation time as `StartTime`. The OTLP encoder writes these timestamps unchanged, producing `startTimeUnixNano > timeUnixNano`.

**Concrete scenario:** while the exporter reads routing gauges, the first request for a key/model settles and creates its usage series. That first point describes a measurement interval ending before it began. This affects cumulative export and the first delta/lowmemory point; the delta reader also saves the earlier collection time as the next interval's start. Downstream handling of the invalid interval depends on the receiver, but the invalid emitted interval itself is reproduced without timing luck.

**Reproduction:** `gateway/internal/telemetry/metric/collect_codex_test.go`, `TestCodexSeriesCreatedDuringCollectHasValidTimeRange`. A callback barrier forces a concurrent first recording between collection's timestamp and its series-map copy.

```sh
cd /tmp/kaiak/gateway
GOCACHE=/tmp/kaiak-audit-go-cache go test -race -count=1 -run TestCodexSeriesCreatedDuringCollectHasValidTimeRange ./internal/telemetry/metric/
```

Observed:

```text
--- FAIL: TestCodexSeriesCreatedDuringCollectHasValidTimeRange (0.00s)
    collect_codex_test.go:29: test.requests starts 8.292µs after its exported end time
FAIL
```

**Fix direction:** assign an end timestamp that cannot precede an included point's creation, or consistently exclude points born after the collection cutoff. Existing start-time tests create new series between complete exports, so they do not exercise this interleaving.

## Inherited limitation — Low / P3, not a new branch regression

**Location:** `gateway/internal/telemetry/metric/registry.go:268` and `gateway/internal/telemetry/metric/collect.go:224`.

A histogram observation updates its bucket and sum separately; collection reads them separately. Under concurrent recording, a point can contain a count and sum from different observation sets. `TestCodexHistogramCollectIsCoherentDuringRecording` in `collect_codex_test.go` records only values of 1 and failed with **count=11, sum=10** under `-race`. There is no Go data race; this is a logical snapshot inconsistency. It can also put a bucket increment and its sum into different delta intervals.

```sh
cd /tmp/kaiak/gateway
GOCACHE=/tmp/kaiak-audit-go-cache go test -race -count=1 -run TestCodexHistogramCollectIsCoherentDuringRecording ./internal/telemetry/metric/
```

Observed:

```text
--- FAIL: TestCodexHistogramCollectIsCoherentDuringRecording (0.00s)
    collect_codex_test.go:59: all observations are 1, but exported count=11 sum=10
FAIL
```

The deleted `internal/metrics/registry.go` and `text.go` in `BRANCH.diff` use the same independent bucket/sum operations. This is therefore an inherited approximation, not evidence that the move introduced a new concurrency regression. The existing concurrent-recording test verifies final totals after writers stop, not coherence during recording.

## Checked without finding a new defect

- Temporality selection: cumulative up-down counters in all modes; lowmemory leaves observable counters cumulative. Delta state is exporter-owned, advances after failed exports as specified, retains zero series, and subtracts scaled counters in integer subunits. Prometheus collection does not consume it.
- Histogram wire shape: explicit bounds, per-bucket OTLP counts, an extra infinity bucket, decimal-string counts, and Prometheus cumulative buckets. Count is calculated from the same bucket reads; the separate sum caveat is above.
- Size splitting: ordinary points and whole/split families preserve their points and stay within the configured request limit in `TestSizeSplit`; per-request failure accounting is covered. The encoder explicitly permits an individual oversized point to exceed the limit, but I did not establish a production-reachable gateway series that large and have not promoted that generic-library edge case to a finding.
- Naming, kinds, attributes and zero series: the metric-list comparison and metrics/routing tests pass, including typed HTTP status attributes, fixed routes/methods, GenAI input including both cache components, circuit states, backend shares, and retired backends/models with active work.
- Delivery/security: shared transport tests cover redirects, retry statuses/backoff, partial success, unreadable/oversized responses, and sanitized transport/collector errors. Header parsing errors omit header contents. No new remote-text or header-value leak was found in the reviewed paths.
- Shutdown ordering: usage/control teardown and listener shutdown precede metrics; metrics precede the stopped line and final log flush. Existing final-export and second-signal tests pass for stalled HTTP delivery. Collection/encoding is the missing bound identified above.
- Request recording does no export I/O. Existing-series recording uses a map read lock and atomics; collection releases map locks before sorting, encoding or network work. Routing status and gauges use the shared `Serving` picture.

## Verification

Before adding reproductions, the following race-enabled package tests passed:

```text
go test -race ./internal/telemetry/... ./internal/metrics ./internal/routing ./internal/server ./cmd/kaiak

ok  kaiak/internal/telemetry/metric
ok  kaiak/internal/telemetry/otlp
ok  kaiak/internal/telemetry/otlplog
ok  kaiak/internal/telemetry/otlpmetric
ok  kaiak/internal/metrics
ok  kaiak/internal/routing
ok  kaiak/internal/server
ok  kaiak/cmd/kaiak
```

The complete existing gateway test suite also passed, uncached and with the race detector. Only the three newly added, intentionally failing audit tests were excluded from this baseline run:

```sh
GOCACHE=/tmp/kaiak-audit-go-cache go test -race -count=1 -skip '^TestCodex' ./...
```

Output excerpts (exit status 0; every package passed):

```text
ok  kaiak/cmd/kaiak                         5.088s
ok  kaiak/e2e                            121.151s
ok  kaiak/internal/metrics                 4.243s
ok  kaiak/internal/routing                 4.170s
ok  kaiak/internal/server                 12.516s
ok  kaiak/internal/telemetry/metric         5.194s
ok  kaiak/internal/telemetry/otlp           4.849s
ok  kaiak/internal/telemetry/otlplog         4.215s
ok  kaiak/internal/telemetry/otlpmetric      4.344s
```

The transitive telemetry import-boundary check passed, including tests. Both added test files are gofmt-clean. This was a gateway review; `scripts/check-all.sh` (control tests/lint and cross-half e2e) was not run.

The added tests intentionally fail on the reviewed tree. The initial sandbox attempt could not use the default Go cache or bind test-server ports; successful runs used the temporary `GOCACHE` shown above and permission to bind loopback test servers. No dependencies or production files were changed.

## Outcome

- **1. Collection and encoding escape the export bound** — fixed (commit to
  follow): the export's timeout starts before the collect, and encoding checks
  its context every 1,024 points; a cut export counts its points as `timeout`.
  Collect and the temporality transform stay uncut (tens of ms at 100,000 series).
  Tests `TestExportTimeoutCoversEncoding`, `TestExitCutsAnExportWhileItEncodes`.
- **2. A series born during collection has an inverted interval** — fixed (commit
  to follow): the snapshot's time is taken once every series is read. Test
  `TestSeriesCreatedDuringACollectStartsBeforeItsTime`.
- **Inherited histogram count/sum coherence** — accepted: metrics are approximate
  by design (docs/kaiak.md, principle 7); coherence would cost a lock or a
  hot/cold swap on every observation.
