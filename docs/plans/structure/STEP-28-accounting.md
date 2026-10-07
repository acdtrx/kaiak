# Step 28 — accounting tidy

**Status:** not started

## Intent

The rule that splits an inclusive token count into its parts has one home, and the
log-export counts are one type.

## Findings

- observability F7 (independent F01): one
  `inclusiveUnits(input, output *int64, cached, written, reasoning int64) (Units, bool)`
  holding the clamping rule (cached ≤ input, written ≤ rest, reasoning ≤ output); a small
  embedded `latestReport` for the shared `latest`/`bodyUsage`/`reported` state; the
  OpenAI and Responses readers keep only their field names and the embeddings case.
- observability F10: `metrics.LogExportCounts` goes; `RegisterLogExport` reads
  `otlplog`'s counts through a function returning the three values; the copy in `main`
  goes.
- observability hint: `request.totalUsage()` for the log line's sum over the request's
  records (limits sums the same records its own way; leave that).

## Files likely touched

- `gateway/internal/accounting/{openai_usage,responses_usage,meter}.go` and tests.
- `gateway/internal/metrics/logexport.go`, `gateway/internal/otlplog/exporter.go`,
  `gateway/cmd/kaiak/main.go`.
- `gateway/internal/server/requestlog.go` (or `api.go`).

## Removal checklist (clean at phase end)

- `git grep -n 'LogExportCounts' gateway/` → none.

## Acceptance criteria

- `meter_test.go`, `responses_usage_test.go`, the log-export metric tests pass unchanged.
- `scripts/check-gateway.sh` green, or reds named with the step that clears them.
