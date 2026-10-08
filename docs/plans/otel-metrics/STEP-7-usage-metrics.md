# Step 7 — usage metrics

**Status:** not started

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

