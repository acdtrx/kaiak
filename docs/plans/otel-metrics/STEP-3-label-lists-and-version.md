# Step 3 — label lists, key labels, build version

**Status:** done (2026-10-08)

## Intent

The structure findings that change no output: each closed label vocabulary owned by
the code that produces it (F3), one key-label helper (F4), the build version owned by
`main` (F8), the log-export count mirror gone (F10). Label values and names unchanged
here — step 6 renames.

## Files likely touched

- `gateway/internal/control/usage.go`, `client.go` — export `BatchResults`,
  `DropReasons` as typed lists; `Trigger*` with startup and sighup
  (`config.LoadTriggers`, or wherever the trigger type best lives — the step decides
  and says why).
- `gateway/internal/limits/limits.go` — `Scopes` (the scope kinds).
- `gateway/internal/config/` — the load results (applied, rejected), if the load
  owns them.
- `gateway/internal/metrics/{ops,delivery}.go` — pre-create from those lists; typed
  parameters; the six `slices.Contains … panic` guards and the HELP strings that
  enumerate values go (HELP says what the label means; the values live in the type).
- `gateway/internal/metrics/{usage,ops}.go` — one `keyLabels` used by the usage
  metrics and `kaiak_request_errors_total` (F4).
- `gateway/cmd/kaiak/main.go`, `gateway/Dockerfile` — `-X main.version`; `main`
  computes the version (stamped, else the module's, else `(devel)`) and passes it
  to `metrics.RegisterBuildInfo` and the OTLP resource; `buildVersion` and its test
  move to `main` (F8).
- `gateway/internal/metrics/logexport.go` — reads the exporter's counts directly; no
  mirror struct (F10). Step 6 replaces the family itself.
- `scripts/build-images.sh` if it names the ldflags path.

## Decisions made during planning

- Typed vocabularies: a named string type per list with its values as constants and
  an exported list; `metrics` takes the type, so a wrong value does not compile.
- `metrics` may import `control` and `limits` (neither imports `metrics`); `go vet`
  catches a cycle should one appear.

## Acceptance criteria

- `/metrics` output byte-identical before and after on a fixed scenario (a golden
  test over a registry fed the same calls, or the e2e scrape compared).
- `git grep -n "slices.Contains" gateway/internal/metrics` finds no guard.
- `git grep -n "internal/metrics.version"` finds nothing; the image build's
  `kaiak_build_info` still reports the `git describe` version (`smoke-images.sh`).
- `scripts/check-all.sh` green. Suite recorded.

## Result

### What changed

- `gateway/internal/config/loader.go` — `Trigger` (`TriggerStartup`, `TriggerSIGHUP`,
  `TriggerControl`, `TriggerSeed`) with `Triggers`; `LoadResult` (`LoadApplied`,
  `LoadRejected`) with `LoadResults` and `Load.Result()`. `Load.Trigger`,
  `Applier.Apply`, `Applier.Reject`, `FileLoader.Load` take `Trigger`; the log
  attribute stays a plain string (`kaiak.trigger=startup`).
- `gateway/internal/control/client.go` — `TriggerControl`/`TriggerSeed` removed;
  the client applies with `config.TriggerControl`/`config.TriggerSeed`.
- `gateway/internal/control/usage.go`, `queue.go` — `BatchResult` (`BatchAcked`,
  `BatchRejected`, `BatchFailed`) with `BatchResults`; `DropReason`
  (`DroppedInvalid`, `DroppedMemoryBound`) with `DropReasons`; `UsageObserver`
  takes them.
- `gateway/internal/limits/limits.go` — `Scopes` (`ScopeGlobal`, `ScopeGroup`).
- `gateway/internal/metrics/ops.go` — `limitScopeKinds`, `configTriggers`,
  `configResults` gone; series pre-created from `limits.Scopes`, `config.Triggers`
  × `config.LoadResults`; `CountLimitRejection(limits.Scope, config.LimitType)`;
  `ConfigLoaded` reads `Load.Result()`; queue reasons typed `QueueReason`;
  the four guards (error class, limit, retry, attempt outcome) removed; HELP of
  `kaiak_queue_rejections_total`, `kaiak_retries_total`,
  `kaiak_config_loads_total` say what the label means. Build version code gone;
  `NewOps` no longer registers `kaiak_build_info`.
- `gateway/internal/metrics/delivery.go` — `batchResults`/`dropReasons` and their
  two guards gone; pre-created from `control.BatchResults`/`control.DropReasons`;
  typed parameters; HELP of `kaiak_usage_batch_sends_total`,
  `kaiak_usage_dropped_records_total` without value lists.
- `gateway/internal/metrics/usage.go` — `keyLabelNames` and `keyLabels(holder,
  path, keyID)`, used by `UsageMetrics.Record` and `Ops.CountRequestError` (label
  names and values alike).
- `gateway/internal/metrics/buildinfo.go` (new) — `RegisterBuildInfo(reg, version)`.
- `gateway/cmd/kaiak/main.go` — `version` (`-X main.version`), `reportedVersion`,
  `buildVersion`; `run` computes it once and passes it to `otlp.Service` and
  `newGraph`, which calls `metrics.RegisterBuildInfo`. Loads use
  `config.TriggerStartup`/`TriggerSIGHUP`.
- `gateway/Dockerfile` — `-X main.version=${VERSION}`. `scripts/build-images.sh`
  names no ldflags path: unchanged.
- `gateway/internal/server/metrics.go` — passes `rej.Scope()` as is.
- Tests: `TestBuildVersion` moved to `cmd/kaiak/main_test.go`; new
  `TestBuildInfo` in metrics; the panic assertions of the removed guards gone
  (`unknown attempt outcome`, `not a retryable outcome`, `unknown error class`,
  `unknown batch result`); the server test gateway registers build info as main
  does; control and config tests on the typed values.
- Docs: `TECH-STACK.md` (Container images: the ldflags path; Version stamping:
  the variable lives in `cmd/kaiak`); `BACKLOG.md` — the F3, F4, F8 sub-items of
  OpenTelemetry export → Metrics removed (resolved here), its pointer now names F5.

### Decisions settled while implementing

- **Triggers live in `config`**: the trigger is a parameter of `config`'s apply
  path and a field of `config.Load`, which every source (file loader, control
  client, seed) goes through; `config` cannot import `control`, so the type and
  the full list sit with the apply path, the control client's two values included.
- **Load results live in `config`** beside `Load` (`Load.Result()`): the load
  decides applied or rejected.
- **Queue reasons, error classes and attempt outcomes stay in `metrics`**: their
  producer is `server`, which imports `metrics`, so a `server`-owned list that
  `metrics` pre-creates from would be an import cycle; `routing` only returns
  errors that `server` maps to codes. `QueueReason` is now a named type like the
  other two.
- **Lists are exported variables** (`limits.Scopes`, `control.BatchResults`,
  `control.DropReasons`, `config.Triggers`, `config.LoadResults`), as
  `metrics.RetryableOutcomes` is; `config.LimitTypes()` stays a function (built
  from its table).
- **`CountRetry`'s retryable-subset check went with the guards**: the type admits
  any `AttemptOutcome`; `server` chooses only retryable ones (its tests), and the
  metrics test still checks `RetryableOutcomes` ⊂ every outcome.
- **F10 was already done** before this step (the structure plan):
  `RegisterLogExport` takes `Exporter.Counts` as a function, no mirror struct.
  Nothing to change.
- `metrics` now imports `control` and `limits` outside tests; neither imports
  `metrics` (`go list -deps`), `go vet` clean.

### Byte-identity

A temporary test in `internal/metrics` built one registry as `main` does
(`NewCircuits` + router observer, `NewOps`, `PrepareSeries`, `NewUsageMetrics`,
`NewUsageDelivery`, `RegisterLogExport`) over a two-backend, two-model config, then
fed fixed calls into every touched family: config loads for all four triggers,
applied and rejected, with and without a document; limits sync; request durations,
first token, output rate; errors; limit rejections (global and group); queue wait
and both queue reasons; a retry; upstream attempts; circuit transition and probes;
request errors and usage records (complete, partial) under all four
`key_id_label`/`group_label` combinations, keyless included; clamped records;
batch sends of all three results, queue depth, both drop reasons. It wrote the
scrape (611 lines) before any change and again after (the only test edit: a
`RegisterBuildInfo(reg, "(devel)")` call, the value the test binary reported
before). `diff`: identical except the five `# HELP` lines rewritten on purpose
(config loads, queue rejections, retries, batch sends, dropped records); every
`# TYPE` and sample line identical. The test was removed (step 5 and 6 rewrite the
output; the per-family tests stay). A stamped build (`go build -ldflags "-X
main.version=9.9.9-stamp-check"`) run on the minimal fixture served
`kaiak_build_info{version="9.9.9-stamp-check",go_version="go1.27.1"} 1`.
`smoke-images.sh` was not run (no image build in this step).

### Greps

- `git grep -n "slices.Contains" gateway/internal/metrics`: `registry.go` (label
  validation) and `metrics_test.go` (the subset check) — no guard.
- `git grep -n "internal/metrics.version"`: only `docs/plans/` and `docs/reviews/`.
- `metrics.Version`, `registerBuildInfo`, `limitScopeKinds`, `configTriggers`,
  `configResults`, `batchResults`, `dropReasons`, `control.Trigger*`: only
  `docs/plans/` and `docs/reviews/`.

### Suite

`scripts/check-all.sh`: exit 0 — gofmt, vet, staticcheck, telemetry boundary, race
tests (`kaiak/e2e` 108.9 s, `metrics`, `control`, `config`, `server`, `cmd/kaiak`
ok), live-test kit, control `npm test` (629 tests, 628 pass, 1 skipped, 0 fail),
lint (`boundaries ok`), cross-half e2e `ok kaiak/e2e 65.632s`, "all checks passed".
