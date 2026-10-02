# Step 3 — gateway

**Status:** not started

## Intent

The gateway meters written tokens from the backend's usage report, prices them,
counts them toward tiers and token limits, and reports them in records, metrics and
logs.

## Files likely touched

- `gateway/internal/config/snapshot.go`: `UnitTokensCacheWrite`; the price-unit set.
- `gateway/internal/config/schema.go`: `FormatVersion` 4; the unit accepted in a
  tier's `usd_per_million`.
- `gateway/internal/accounting/meter.go`: `usageReport` reads
  `prompt_tokens_details.cache_write_tokens`; `parseUsage` maps and clamps it
  (cached ≤ prompt, then written ≤ prompt − cached; `tokens_in` is the rest);
  `tokenUnits` carries five units.
- `gateway/internal/accounting/accounting.go`: `inputSize` adds the unit; `Cost`
  prices it, unpriced → the tier's `tokens_in` (one helper for both fallbacks if
  they share the rule's purpose — CODING-RULES §2).
- `gateway/internal/limits/limits.go`: the actual-tokens sum adds the unit.
- `gateway/internal/metrics`: the usage token counter emits the fifth unit.
- The request log line (wherever it writes `tokens_cached`).
- `gateway/internal/fakebackend/fakebackend.go`: reports `cache_write_tokens` when the
  test sets it, in both body and stream usage.
- Versions: `control.ProtocolVersion` 4 (and `fakecontrol`'s header);
  `lastKnownGoodFormat` 5; `spoolFormat` 3.
- `scripts/live/config.go` (format 4), `scripts/live/checks.go` (print the unit in
  the usage check; count it in the "no input" check).
- Tests that build configs or records (moved to format 4 and five units).

## Decisions made during planning

- The meter stays provider-blind: every backend type reads the same OpenAI usage
  object; a backend that omits the field reports 0.

## Acceptance criteria

- The gateway passes every shared fixture with the codes `cases.json` names.
- Meter tests: writes present and absent; stream (usage chunk) and non-stream; cached
  + written above prompt clamped in order; embeddings ignore it.
- Cost tests: writes at their price; unpriced → the tier's `tokens_in`; written
  tokens push the input size over a tier threshold.
- Limits test: written tokens count toward a token limit's actual usage.
- Metrics test: `kaiak_usage_tokens_total{unit="tokens_cache_write"}`.
- A spool or last-known-good file of the old format is discarded with its log line.
- `scripts/check-gateway.sh` green (incl. the live kit's self-test). Suite recorded;
  expected red: cross-half e2e if its configs still need moving (step 4).

## Result

_Not started._
