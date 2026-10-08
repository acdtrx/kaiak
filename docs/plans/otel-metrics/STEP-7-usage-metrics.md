# Step 7 — usage metrics

**Status:** done (2026-10-08)

## Intent

The usage metrics take the target form: the five GenAI usage counters, records,
clamped records and cost under `kaiak.usage.*`, all with the usage labels — the key
labels, the public model, the operation, the provider, the status (decisions 5, 19,
22).

## Files likely touched

- `gateway/internal/accounting/` — `UsageRecord.Operation` (gateway-local,
  `json:"-"`), set at settlement from the request's endpoint; the protocol and its
  fixtures unchanged (a test asserts the field never reaches the wire).
- `gateway/internal/server/` — the endpoint's operation handed to settlement.
- `gateway/internal/metrics/usage.go` — the counters: input = in + cached +
  cache-write (`config.InputUnits` sum), its two cache parts, output (reasoning
  included), reasoning; `gen_ai.token.modality="unknown"`; `gen_ai.provider.name`
  from the deployment's backend type in the live config (the log line's mapping,
  shared rather than copied), absent for self-hosted types or a backend the config
  dropped; the key labels through the F4 helper, in their new names, also on
  `kaiak.request.errors`.
- Tests: `usage_path_test.go`, `metrics_test.go`, e2e usage scrapes,
  `scripts/live/checks.go`.

## Decisions made during planning

- A record of a token-counting endpoint never exists (no usage record), so the
  operation is always one of `chat`, `text_completion`, `embeddings`.
- The provider mapping lives once (provider or config), read by the log line and the
  usage metrics.

## Acceptance criteria

- Input tokens on the metric equal the log line's `gen_ai.usage.input_tokens` for the
  same request (a test over a cached, cache-writing, reasoning response).
- The key-ID and group switches still drop `kaiak.key.id` / `kaiak.key.group` from
  new series.
- Grep: no `kaiak_usage_tokens_total`, no `unit=` label, no `\bkey_group\b`,
  `\broot_group\b`, `\bkey_id\b` as metric labels.
- Phase 2 end: `scripts/check-all.sh` green. Suite recorded.

## Result

### What changed

- **`accounting`**: `UsageRecord.Operation` (`json:"-"`, beside `Generation`) and
  `Request.Operation`; `Settle` stamps it. `server/attempts.go` passes
  `rq.endpoint.operation` (`chat`, `text_completion`, `embeddings`; token-counting
  endpoints settle no record). The endpoint table's `operation` comment
  (`server/pipeline.go`) names both readers.
- **`metrics/usage.go`**: usage labels `kaiak.key.group`, `kaiak.key.root_group`,
  `kaiak.key.id`, `gen_ai.request.model`, `gen_ai.operation.name`,
  `gen_ai.provider.name`, `kaiak.usage.status` on `kaiak.usage.records` and
  `kaiak.usage.cost_usd` (scaled counter, unchanged); `kaiak.usage.clamped_records`
  unlabelled, at 0 from startup (unchanged). `kaiak.usage.tokens{unit}` replaced by the
  five `gen_ai.client.inference.usage.*` counters `{token}` with
  `gen_ai.token.modality="unknown"`: input = `Units.Sum(config.InputUnits)`,
  cache_read = `tokens_cached`, cache_write = `tokens_cache_write`, output =
  `tokens_out`, reasoning = `tokens_reasoning`. `keyLabels` takes the snapshot (one
  `holder.Current()` per record serves the switches and the provider); `ops.go`
  passes `o.holder.Current()`.
- **Provider mapping**: already in one place — `provider`'s `kinds` table
  (`backendKind.providerName`) behind `provider.ProviderName`; the log line
  (`requestlog.go`) already called it, so nothing moved. The usage metrics read it
  through `metrics.providerName(snap, backendID)`: the record's backend looked up in the
  live config, `""` when the config no longer has it or the type is self-hosted. Why
  provider, not config: the kinds table is the one place a backend type is named
  (endpoints served, module, provider name); a second table in config would split
  that. `metrics` → `provider` adds no cycle (`accounting` already imports
  `provider`).
- **Schema descriptions** (`protocol/schema/config.schema.json` and the
  `kaiak-control` copy, byte-identical): `group_label`'s description named the labels
  `key_group` / `root_group`; now `kaiak.key.group` / `kaiak.key.root_group`.
  Description text only — no protocol change; flag if the protocol files should
  have been left alone.
- **Docs**: `DEPLOYMENT.md` cardinality (the spec's formula, × operations ×
  providers × 7; 21,000 and 252 series), `architecture/gateway.html` usage labels.
  `LIVE-BACKENDS.md` names no usage metric: unchanged.
- **Tests**: `metrics/metrics_test.go` (the counters, provider present for a cloud
  type, absent for a self-hosted one and for a backend the live config dropped,
  operations, both switches); `metrics/spec_test.go` (`pendingFamilies` holds only
  `otel_sdk_exporter_metric_data_point_exported_total`; the full scrape's record
  carries an operation and a backend of type `openai`); `server/metrics_test.go`
  (new `TestUsageMetricsCountAsTheRequestLine`: one `on-azure` request with 40 cached,
  20 cache-written and 4 reasoning tokens — input on the metric equals the log line's
  `gen_ai.usage.input_tokens` (100), output its `gen_ai.usage.output_tokens`, provider
  `azure.ai.openai`; then `embeddings` and `text_completion` operations; the switch
  tests updated); `server/retry_test.go`, `e2e/{e2e,grouptree,reliability}_test.go`
  (the cache-write scrape now also checks all input, 221), `scripts/live/checks.go`.

### Wire safety

- `control/fixtures_test.go` `TestGatewayLocalRecordFieldsAreNeverSent`: every valid
  `usage-batch` fixture, decoded, its records given `Generation` and `Operation`,
  encodes back to the fixture unchanged (checked to fail with a `json:"operation"`
  tag).
- `accounting_test.go`: the settled record with `Operation: "chat"` reaches batcher
  and metrics with it, and its encoding is the exact protocol JSON as before.
- `usage_path_test.go`: records counted by the fake control plane pass the strict
  record checks (unknown members refused) — comment says so.
- `protocol/` schemas and fixtures: untouched but the one description above.

### Greps (`git grep -P … -- ':!docs/plans' ':!docs/reviews'`)

- `kaiak_usage_tokens_total`, `\bunit="`: only `GATEWAY.md`'s Was column.
- `(?<![.\w])(key_group|root_group)\b`: `GATEWAY.md`'s renames line and
  `telemetry/metric/prometheus_test.go` (a name-translation case, not a metric
  label). `\bkey_group\b` / `\broot_group\b` also match the dotted
  `kaiak.key.*` names — fine.
- `(?<![.\w])key_id="`: none. Remaining `key_id` hits are the usage record's protocol
  field and the config switch `key_id_label`.

### Suite

`scripts/check-all.sh`: exit 0 — gofmt, vet, staticcheck, telemetry boundary, race
tests (`kaiak/e2e` 112.6 s; `cmd/kaiak`, `accounting`, `control`, `metrics`,
`server`, `provider` and the rest ok), live-test kit lint and self-test (every kind
passed), control `npm test` (629 tests, 628 pass, 1 skipped, 0 fail), lint
(`boundaries ok`), cross-half e2e `ok kaiak/e2e 65.342s`, "all checks passed".
Phase 2 ends green.
