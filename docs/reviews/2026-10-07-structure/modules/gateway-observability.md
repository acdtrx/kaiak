# gateway-observability — structure review

Modules: `gateway/internal/accounting`, `gateway/internal/metrics`,
`gateway/internal/otlplog`, `gateway/internal/logattr`. Hot files: `metrics/ops.go` (33
commits), `accounting/meter.go` (13), plus `server/metrics.go` (23, the main consumer,
read for context).

## Module summaries

### accounting
- Responsibilities:
  - Settles a finished attempt into one `UsageRecord` (protocol-shaped, JSON tags):
    units, cost, flags, clamp to 2^53−1, record ID, then hands it to an optional
    `Batcher` (the control client, returns a usage generation) and a `Sink` (usage
    metrics) — `accounting.go` `Recorder.Settle`.
  - Pricing: `PriceAt` (dated entries), `tierFor` (tier by input size), `Cost` (sum of
    priced units, cache-unit fallback to `tokens_in`, one rounding per record).
  - The `Meter`: per-attempt state (sent / answered / refused / status / stream) plus
    one `usageReader` per client API format (`openai_usage.go`, `messages_usage.go`,
    `responses_usage.go`) that reads usage reports and counts generated content.
  - `memberScanner` (`scan.go`): a streaming top-level-member capture of non-stream
    response bodies.
  - Input estimate (`estimate.go`): a one-pass JSON token-stream scan with
    per-format "role" tables (Messages blocks/sources, Responses items/parts, OpenAI
    media heuristics) — used for limits and for estimated records.
- Exported surface used outside (non-test):
  - `UsageRecord` — control (queue, messages, usage, decode), limits, server, metrics.
  - `Recorder`, `NewRecorder`, `RecorderOptions`, `Request`, `Batcher` — cmd/kaiak, server, control.
  - `Meter`, `NewMeter` — server/upstream.go.
  - `EstimateInput`, `InputEstimate` — server inbound*.go, server/pipeline.go.
  - `PriceAt`, `MaxAmount` — server/limits.go.
  - `Units` — server/api.go (request log line).
  - Exported but used only inside the package and in tests: `Cost`, `Sink` (one
    production implementer), `Fanout` (tests only), `EstimateTokens`, `InlineMediaTokens`.
- Dependencies: config, provider.
- Domain concepts encoded: **usage units** and their groupings (`meter.go:15`
  `tokenUnits`, `accounting.go:215` input units, `accounting.go:242-248` priced units
  and fallback); **price units / tiers** (`accounting.go:186-264`); **API format**
  (`meter.go:97` switch on `provider.Format`, three readers, `estimate.go:203-241`
  role tables); **usage flags** estimated/partial (`meter.go:171`); **protocol integer
  bound** (`accounting.go:158`).

### metrics
- Responsibilities:
  - `registry.go`/`text.go`: a self-made Prometheus registry and text exposition
    (counters, scaled counters, gauges, histograms, scrape-time gauge/counter funcs).
  - `ops.go`: the ops metric set (18 families + 6 scrape-time gauges), their closed
    label vocabularies, series-at-0 preparation per config, the error-class and
    attempt-outcome enums, the circuit counters (`Circuits`, a `routing.Observer`),
    **and** the build version (`version`, `Version`, `buildVersion`, `kaiak_build_info`).
  - `usage.go`: `UsageSink`, the accounting sink → usage counters.
  - `delivery.go`: `UsageDelivery`, implements `control.UsageObserver`.
  - `control.go`: control-plane gauges read from a `ControlState` interface.
  - `logexport.go`: the log-export counter, read from a callback.
- Exported surface used outside: `Registry`/`NewRegistry`/`Handler` (cmd/kaiak,
  server/admin.go); `Ops` + methods, `ErrorClass*`, `AttemptOutcome*`, `QueueFull`,
  `QueueTimeout` (server/metrics.go, server/upstream.go, server/pipeline.go,
  server/api.go); `NewCircuits`, `NewUsageSink`, `NewUsageDelivery`,
  `RegisterControlState`, `RegisterLogExport`, `LogExportCounts`, `Version`
  (cmd/kaiak).
- Dependencies: accounting, config, routing.
- Domain concepts encoded: **error classes** (`ops.go:15-41`, mapping from codes lives
  in `server/metrics.go:136`); **attempt outcomes** (`ops.go:46-70`); **retry reasons**
  (`ops.go:100`, a copy); **limit types / scope kinds** (`ops.go:105-109`, copies);
  **config triggers/results** (`ops.go:114-115`, copies); **batch results / drop
  reasons** (`delivery.go:10-15`, copies); **usage key labels** and the
  `key_id_label`/`group_label` switches (`usage.go:16,61-74`, `ops.go:405-418`);
  **usage units** as a label (`usage.go:82-87`).

### otlplog
- Responsibilities: read the `OTEL_*` environment (`settings.go`); an `slog.Handler`
  that tees every record to the stderr handler and a copied, flattened, OTLP-typed
  version to a queue (`handler.go`); a background batch sender with OTLP retry rules,
  partial-success handling and rate-limited self-reporting (`exporter.go`); the
  OTLP/HTTP JSON encoding (`encode.go`).
- Exported surface used outside: `ReadSettings`, `Settings.EndpointHost`, `New`,
  `Resource`, `Exporter.Handler/Counts/Flush/Close`, `Counts` — cmd/kaiak only.
- Dependencies: netfail only.
- Domain concepts: OTLP record mapping, severity mapping, resource attributes. No
  overlap with the slog setup: `cmd/kaiak/main.go:59-61` builds the plain JSON/text
  handler and `main.go:344` wraps it; nothing else configures logging.
- Considered and kept: the intermediate `value` type (`handler.go:84`) between
  `slog.Value` and the wire `anyValue` looks like a conversion layer, but it lets
  capture on the request goroutine copy compactly and defer string formatting and
  pointer allocation to the sender goroutine. **Clean; leave as is.**

### logattr
- Two helpers (`Seconds`, `SecondsMicro`) fixing the log vocabulary's duration unit;
  used by cmd/kaiak, config, limits, routing, server, control. **Clean.**

## Findings

### F1 — Usage-unit groupings are restated inline at every use; the 5-unit record is built positionally
- **Kind** — cross-module.
- **Where** — `accounting/meter.go:15` `tokenUnits(in, cached, cacheWrite, out, reasoning int64)`
  (7 call sites); `accounting/accounting.go:215` `inputSize` list
  {in, cached, cache_write}; `accounting/accounting.go:242-248` `Cost` hard-codes the
  four priced units and `cacheInputPrice` the fallback; `limits/limits.go:635`
  `amountOf` list {in, cache_write, out}; `server/api.go:244-250` re-derives "all
  input" = in + cached + cache_write; `config/schema.go:109-111` `priceUnits`;
  `control/schema.go:33-35` `tokenUnits`. Mirrored on the control side in
  `control/kaiak-control/src/usage/aggregate.ts:49-50` (the counted-tokens sum).
- **Now** — four distinct unit sets (all token units, priced units, input units,
  "load" units counted by token limits) are each written as a literal list or an
  arithmetic expression where they are used: 7 places in the gateway, none named.
  `Cost` is a hand-written sum, not a loop over the priced set. The record's units are
  built through a positional five-`int64` constructor where swapping `cached` and
  `cacheWrite` compiles silently.
- **How it got here** — `tokens_cached` came first (2026-09-24); `tokens_cache_write`
  (b54af0e, "meter, cost, limits, metrics, versions") was threaded by hand into each
  place, growing `tokenUnits` from four to five positional parameters and each list by
  one entry. The spec's own note (CONTROL-PROTOCOL.md:639-641) defers per-lifetime
  cache-write units "until Bedrock is built" — the same lockstep edit is expected again.
- **Proposed shape** — in `config` next to `Unit`: `TokenUnits` (record order),
  `PricedUnits`, `InputUnits` (sum = the backend's prompt; picks the tier and is
  `gen_ai.usage.input_tokens`), `CountedUnits` (what token limits and totals count),
  and the fallback as data (`PriceFallback = map[Unit]Unit{Cached: In, CacheWrite: In}`).
  `Cost` becomes `for _, u := range PricedUnits { nano += units[u] * priceOf(tier, u) }`
  (same summation order, so identical rounding); `inputSize`, `amountOf`, the log line,
  `config/schema.go` and `control/schema.go` read the named sets. Readers build
  `Units{config.UnitTokensIn: …}` literals (or a named-field helper) instead of the
  positional constructor.
- **Payoff** — adding a token unit: ~9 gateway places (const, two schema lists,
  `tokenUnits` signature + 7 call sites, `inputSize`, `Cost`, `amountOf`, log line)
  → const + set membership + the readers that report it. One concept ("which units
  count as input / are priced / load the backend") gets one definition each.
- **Cost / risk** — small: ~40 lines touched across config, accounting, limits,
  server, control; no protocol or spec change (the spec already defines these sets in
  prose). Tests unaffected except constructor call sites in accounting tests.
- **Confidence** — high on the duplication; medium on naming `CountedUnits` in `config`
  rather than `limits` (it is also the control plane's totals unit — check
  `control/messages.go:60`).

### F2 — Retry reasons are a second classification of the attempt outcome, listed in three places
- **Kind** — cross-module (metrics + server).
- **Where** — `metrics/ops.go:100` `retryReasons` (string list) and the help text at
  `ops.go:174`; `server/upstream.go:22-32` `retry*` consts; `server/upstream.go:259-296`
  `retryReason`; `server/upstream.go:491-547` `classifyAttempt`; `metrics/ops.go:46-66`
  `AttemptOutcome`.
- **Now** — every retry reason string is also an `AttemptOutcome` value
  (`unavailable, timeout, server_error, rate_limited, auth_failed, model_missing,
  path_missing, endpoint_missing`). `retryReason` and `classifyAttempt` each switch over
  the same provider error codes, busy statuses and error-event kinds; reading both,
  `retryReason(rq)` equals `classifyAttempt(rq)`'s outcome whenever that outcome is in
  the retryable set and "" otherwise (at decision time nothing was relayed, so
  `relayEnd` is empty and the two switches walk identical branches; all eight
  `provider.Code` values agree). `CountRetry` takes an untyped string and panics on
  values outside `metrics.retryReasons`.
- **How it got here** — retries (8bd91c9) predate the per-deployment attempt outcomes
  (cb18bd2, M4); each later failure kind (auth_failed, model_missing, path_missing,
  endpoint_missing, error events) was added to both switches and both lists.
- **Proposed shape** — `metrics` declares `RetryableOutcomes` (subset of
  `AttemptOutcome`) next to the enum; server computes the outcome once
  (`classifyAttempt` is pure over `rq`) and retries when it is retryable;
  `attempt.retryReason` becomes an `AttemptOutcome`; `CountRetry(model, backend,
  AttemptOutcome)`; `prepareSeries` iterates `RetryableOutcomes`. `retryReason`, the
  server `retry*` consts and `metrics.retryReasons` go.
- **Payoff** — ~50 lines and one vocabulary removed; adding a failure kind touches one
  switch instead of two switches plus two lists; the "may this be retried" rule sits
  beside the outcome it classifies.
- **Cost / risk** — small/medium, mostly in `server/upstream.go` (another reviewer's
  module; coordinate). `kaiak_retries_total` label values unchanged. Tests:
  `server/retry_test.go`, `endpoints_test.go` keep passing if the equivalence holds.
- **Confidence** — medium-high; a table test asserting `retryReason(rq) ==
  retryable(classifyAttempt(rq))` over the existing `endpoints_test.go` cases would
  make it high.

### F3 — Closed label vocabularies: two conventions, most values copied into metrics by hand
- **Kind** — cross-module.
- **Where** — `metrics/ops.go:90-116` (`queueReasons`, `retryReasons`,
  `limitScopeKinds`, `limitTypes`, `configTriggers`, `configResults`);
  `metrics/delivery.go:10-15` (`batchResults`, `dropReasons`). Owners:
  `control/usage.go:39-41` `Batch*`, `control/usage.go:64-68` `Dropped*`,
  `control/client.go:32-33` `Trigger*` plus string literals `"startup"`/`"sighup"` at
  `cmd/kaiak/main.go:393,705`; `config/snapshot.go:72-75` `Limit*` (and a third copy in
  `config/schema.go:103-105`); `limits/limits.go:32-33` `Scope*`.
- **Now** — `ErrorClass`, `AttemptOutcome`, `QueueFull/QueueTimeout` are owned by
  `metrics` and imported by producers; the other seven vocabularies are owned by their
  producer and re-typed in `metrics` as string literals, each guarded by a
  `slices.Contains … panic` at the counting method (6 such guards) so a drift shows up
  only at run time. The HELP strings enumerate the values a third time.
- **How it got here** — each metric arrived with its feature; the "series at 0"
  requirement (E5, 4944025) needed full value lists, which were written locally in
  `ops.go` rather than exported by the owners.
- **Proposed shape** — one rule: the producer owns the typed vocabulary and exports its
  full list (`control.BatchResults`, `control.DropReasons`, `config.LimitTypes` — also
  used by `config/schema.go` —, `limits.Scopes`, `config.LoadTriggers` including
  startup/sighup); `metrics` pre-creates from those lists and takes typed parameters.
  No import cycle: `metrics` → control/limits is free (neither imports `metrics`).
  Alternatively, move all to `metrics` as `ErrorClass` is — either way one convention.
- **Payoff** — adding a value (a drop reason, a trigger, a limit type): 2–3 places →
  1; the run-time panics become compile-time types; `config` loses its duplicate
  `limitTypes` list.
- **Cost / risk** — small (~40 lines moved). No metric names or label values change.
- **Confidence** — medium: the direction (producer-owned vs metrics-owned) is a taste
  call; the duplication itself is certain.

### F4 — The key-labelling rule for per-key metrics is written twice
- **Kind** — cross-function.
- **Where** — `metrics/usage.go:61-79` `UsageSink.Record`; `metrics/ops.go:405-418`
  `Ops.CountRequestError`; label lists `usage.go:16` vs the literal at `ops.go:164`.
- **Now** — both derive `(key_group, root_group, key_id)` from the group path and
  blank them by `snap.KeyIDLabel` / `snap.GroupLabel`; one takes `*config.Group`, the
  other `[]string`. `kaiak_request_errors_total`'s contract is "labelled as the usage
  metrics are" (GATEWAY.md:2378) — held only by copy.
- **How it got here** — 8a67686 added the request-error counter by copying the usage
  sink's label logic.
- **Proposed shape** — one `keyLabels(holder, path []string, keyID) []string` and one
  `keyLabelNames` prefix used by both families.
- **Payoff** — ~12 lines; the shared contract holds by construction; a third per-key
  family (or a new switch) touches one place.
- **Cost / risk** — trivial; no output change.
- **Confidence** — high.

### F5 — Per-config series and scrape-time zero-fill repeated across ops.go; circuits prepared by a separate entry point
- **Kind** — cross-function.
- **Where** — `metrics/ops.go:255-275` `circuitGauge` and `ops.go:282-300` the cooling
  gauge (identical "every deployment of the snapshot at 0, then mark active" bodies);
  `ops.go:218-232` and `240-254` (identical zero-fill of router counts over config
  IDs); `ops.go:329-339` `Circuits.PrepareSeries` called by `cmd/kaiak/main.go:378`,
  while `Ops.ConfigLoaded` (`ops.go:482-496`) prepares its own series.
- **Now** — four near-identical gauge closures; two "prepare per applied config" paths
  wired differently (one inside `Ops.ConfigLoaded`, one by `main`).
- **How it got here** — half-open (c43b2df), one-sample-per-deployment (021fb54) and
  the cooldown gauge (2710969) were added one at a time; `Circuits` is split from
  `Ops` because the router needs its observer before `Ops` (which needs the router)
  exists.
- **Proposed shape** — a `deploymentGauge(holder, active func() map[routing.DeploymentID]bool)`
  helper used for open / half-open / cooling; a `zeroFilled(counts, keys)` helper for
  in-flight and queued; `NewOps` takes the `*Circuits` and `ConfigLoaded` prepares both,
  so `main` drops its separate call.
- **Payoff** — ~30 lines out of the hottest file; one place decides which series exist
  after a config apply.
- **Cost / risk** — small; outputs identical; `metrics_test.go` unaffected.
- **Confidence** — high.

### F6 — Sink / Fanout / OutOfRange: a fan-out with one production sink, and docs that still describe two
- **Kind** — cross-module.
- **Where** — `accounting/accounting.go:58-95` (`Sink`, `Fanout`, `Batcher`,
  `RecorderOptions.OutOfRange`); `cmd/kaiak/main.go:452-458`; tests
  `server/usage_path_test.go:97`, `server/server_test.go:213`; docs
  `docs/specs/GATEWAY.md:1780-1786` and `:2064`, `docs/ARCHITECTURE.md:146,164`.
- **Now** — production wires exactly one sink (usage metrics). `Fanout` is used only
  by tests. The clamp callback `OutOfRange` points at the same object
  (`usageMetrics.RecordClamped`). The spec still says "usage metrics and the
  control-plane sender each register one [sink]" and "the client is a sink on
  accounting's fan-out", but the client became a `Batcher` (private 1f9653e) because
  it must return the generation before the sink sees the record.
- **How it got here** — settled 2026-09-24 for two sinks; the control sender moved to
  the `Batcher` slot and the fan-out stayed.
- **Proposed shape** — `RecorderOptions{Batcher, Metrics}` with
  `Metrics interface{ Record(UsageRecord); RecordClamped() }` (or keep `Sink` and add
  `Clamped` to it); `Fanout` moves into a test helper; the spec's Sinks decision is
  restated as "batcher, then usage metrics".
- **Payoff** — one interface and one callback instead of two interfaces, a callback and
  a test-only type; docs match the code.
- **Cost / risk** — small; spec edit of a settled decision (dated). Test wiring in two
  server test files.
- **Confidence** — medium: a future second sink (e.g. a usage log) would bring
  `Fanout` back, but none is planned.

### F7 — OpenAI and Responses usage readers duplicate the "cache inside input" decomposition
- **Kind** — cross-function.
- **Where** — `accounting/openai_usage.go:98-120` `openAIUsage.parse` vs
  `accounting/responses_usage.go:38-56` `responsesUsage.parse`; identical
  `latest`/`bodyUsage`/`reported` at `openai_usage.go:45-49,68` and
  `responses_usage.go:110-114,157`.
- **Now** — two copies of: reject non-object, require one of two counts, clamp cached ≤
  input, written ≤ rest, reasoning ≤ output, then `tokenUnits(input-cached-written, …)`.
  They differ only in JSON field names (`prompt_tokens`/`input_tokens`,
  `…_details`) and the embeddings special case.
- **How it got here** — the Responses reader (7d23023) was written by copying the
  OpenAI one.
- **Proposed shape** — one `inclusiveUnits(input, output *int64, cached, written,
  reasoning int64) (Units, bool)` holding the clamping rule (the rule the spec states
  once); a small embedded `latestReport` struct for the shared state/methods.
- **Payoff** — ~25 lines; the clamping rule has one home (a change to it — e.g. a new
  input detail — is made once).
- **Cost / risk** — trivial; `meter_test.go`/`responses_usage_test.go` cover it.
- **Confidence** — high.

### F8 — The build version lives in the ops metrics file
- **Kind** — cross-module.
- **Where** — `metrics/ops.go:350-378` (`version`, `Version`, `buildVersion`,
  `registerBuildInfo`, called from `NewOps` at `ops.go:301`); `gateway/Dockerfile:27`
  (`-X kaiak/internal/metrics.version`); `cmd/kaiak/main.go:342` (OTLP
  `service.version` and the export User-Agent read `metrics.Version()`).
- **Now** — a process-wide fact used by log export and metrics is owned by `metrics`
  and registered as a side effect of `NewOps`.
- **How it got here** — 23c59b6 linked the version for `kaiak_build_info`; OTLP export
  (699637f) later reused it from there.
- **Proposed shape** — `main` owns `version` (`-X main.version`) and the fallback
  logic; `metrics.RegisterBuildInfo(reg, version)` and `otlplog.Resource` receive it.
- **Payoff** — ~30 lines out of `ops.go`; `NewOps` stops registering an unrelated
  family; the version has one obvious home.
- **Cost / risk** — small; Dockerfile ldflags path change; `buildVersion` test moves.
- **Confidence** — medium (low payoff, but no downside).

### F9 — Per-format stream reading split between provider and accounting
- **Kind** — cross-module.
- **Where** — `accounting/meter.go:95-106` (switch on `provider.Format`) and the three
  readers' `streamEvent`; `provider/stream_end.go:28-37` (`newStreamEnd`, another
  switch on `Format`) with `openAIStreamEnd`/`messagesStreamEnd`/`responsesStreamEnd`;
  `provider/body.go:157-172` `isUsageOnlyChunk`.
- **Now** — each successful stream event is JSON-decoded once by the provider's
  end reader (completion, error events, nested model), possibly again by
  `isUsageOnlyChunk`, and again by the meter's reader (usage, content bytes). Two
  parallel per-format type families, chosen by two switches, encode one format's
  stream grammar (e.g. `response.completed`/`response.incomplete` appears in both
  `provider/stream_end.go:169,189` and `accounting/responses_usage.go:94`).
- **How it got here** — accounting's reader was OpenAI-only; Messages (91e2c1e) and
  Responses (7d23023) passthrough each added a reader in both packages.
- **Proposed shape** — one per-format event reader (in provider, or a shared
  format package) that decodes each payload once and yields usage, content bytes,
  error event and completeness; the meter consumes its results instead of re-parsing.
- **Payoff** — adding a client format: two reader families → one; one decode per event
  instead of two or three.
- **Cost / risk** — high: crosses the provider/accounting boundary, touches the hot
  relay path and many tests; the CPU gain is unmeasured.
- **Confidence** — low-medium; a profile showing decode cost per event under load, or
  a fourth client format on the roadmap, would justify it. Otherwise leave as is.

### F10 — `metrics.LogExportCounts` mirrors `otlplog.Counts`
- **Kind** — cross-module (tiny).
- **Where** — `metrics/logexport.go:6-8`, `otlplog/exporter.go:53-55`,
  `cmd/kaiak/main.go:363-366`.
- **Now** — two identical three-field structs and a field-by-field copy in `main`.
- **Proposed shape** — `RegisterLogExport(reg, func() (exported, failed, dropped uint64))`
  with `Exporter.Counts` returning the three values, or `metrics` accepting any type
  with that method; one struct gone.
- **Payoff** — ~8 lines, one mirror type. **Cost** — trivial. **Confidence** — high
  that it is a mirror; low payoff — fine to leave.

### Modules judged clean
- `otlplog`: well-bounded, single job, no overlap with the slog setup. Leave as is.
- `logattr`: leave as is.
- `accounting/scan.go`, `accounting/estimate.go`: complex but single-purpose; the
  per-format role tables are the natural shape for one-pass scanning. Leave as is.
- `metrics/registry.go`, `text.go`, `control.go`, `delivery.go` (apart from F3):
  leave as is.

## Cross-module hints
- Which provider error codes make the meter "refused" is decided in server:
  `server/upstream.go:223-240` lists five `provider.Code`s inline; the same codes are
  switched on again in `retryReason` and `classifyAttempt` (F2). A `Code` method
  (`AnsweredWithoutProcessing()`) in provider would hold the rule once.
- `server/metrics.go`: `errorClass` and `errorCode` overlap (both special-case
  relayed status ≥ 400 via `relayedStatusClass`); `errorCodeClass` maps the gateway's
  error codes — a third list of codes beside the error constructors in server.
- The "counted tokens" sum (in + cache_write + out) exists in
  `limits/limits.go:635` and `control/kaiak-control/src/usage/aggregate.ts:49-50`
  (protocol-level concept; F1 names it on the Go side).
- Saturating arithmetic is defined three times: `accounting.inputSize` (inline),
  `limits/window.go:191`, `server/inbound.go:306,314`.
- `UsageRecord` carries gateway-local state (`Generation`, `json:"-"`) beside protocol
  fields; consumed by limits, set by the batcher — fine today, but the record is the
  shared shape across accounting/control/limits/metrics/server (16 references).
- `server/api.go:230-253` sums units/cost over `rq.records` for the log line; limits
  sums the same records via `amountOf`; ops reads `rq.usage` (last record) for decode
  speed — three readers of the request's records with different aggregation; correct,
  but a `request.totalUsage()` would serve the log line and any future per-request
  consumer.

## Bugs noticed in passing
- Doc drift (not code): `docs/specs/GATEWAY.md:1780-1786` and `:2064`, and
  `docs/ARCHITECTURE.md:164`, describe the control client as a sink on accounting's
  fan-out; it is the `Batcher` (see F6).
- `accounting/scan.go:8-13` says the scanner keeps `"usage", "choices"` and reads
  "where the OpenAI format puts them"; it now also keeps Messages `content` and
  Responses `output` (comment only).
