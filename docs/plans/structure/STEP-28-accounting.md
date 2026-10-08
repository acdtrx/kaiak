# Step 28 — accounting tidy

**Status:** done (2026-10-08)

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

## Result

**What changed**

- F7: `accounting/meter.go` gains `inclusiveUnits(input, output *int64, cached,
  written, reasoning int64) (Units, bool)` — the "cache inside input" rule (neither
  count → no report; cached ≤ input, then written ≤ the rest, reasoning ≤ output;
  `tokens_in` = input − cached − written), once — and `latestReport` (`latest`, `keep`,
  `reported`). `openAIUsage` and `responsesUsage` embed `latestReport`; their `parse`
  keeps only the non-object check, the decode and their field names, and hands the five
  amounts to `inclusiveUnits`. OpenAI keeps the embeddings case (prompt only), with its
  own neither-count check since it returns before `inclusiveUnits`. `messagesUsage` is
  untouched (cache outside input, per-field merge: it does not fit `latestReport`).
- F10: `metrics.LogExportCounts` and `otlplog.Counts` are gone.
  `Exporter.Counts()` returns `(exported, failed, dropped uint64)`;
  `RegisterLogExport(reg, counts func() (exported, failed, dropped uint64))`; `newGraph`
  in `cmd/kaiak/main.go` passes `logExport.Counts` directly — the copy is gone.
- Hint: `request.totalUsage() (accounting.Units, int64)` in `server/attempts.go` (next
  to `settle`, which appends the records) sums units and cost over `rq.records`;
  `logRequest` calls it. Same plain `+=` sum as before; the log line is unchanged.
  Limits' own sum is left as it is.

**Decisions made during the step**

- `inclusiveUnits` and `latestReport` live in `meter.go`, beside `withEveryTokenUnit` and
  `nonNegative`: they are shared by two readers and belong to neither.
- `bodyUsage` stays a one-line method on each reader (`u.keep(u.parse(raw))`): giving
  `latestReport` the per-format `parse` would need a func field set at construction,
  which is more code than it saves.
- F10 took the review's first shape (`Exporter.Counts` returning the three values)
  rather than keeping `otlplog.Counts`, since only a three-value method lets `main` pass
  `logExport.Counts` without copying fields. So `otlplog.Counts` went too.
  `otlplog/exporter_test.go` gets a test-local `counts` struct, which `wantCounts` fills
  from the three values (the 23 `Counts{…}` call sites become `counts{…}`). The
  whole-struct comparison and every expected value are unchanged.
  `TestLogExportMetrics` now feeds three local variables instead of a
  `LogExportCounts`; its expected output is unchanged.
- The spec reference (CONTROL-PROTOCOL.md, Units and price units) moved to
  `inclusiveUnits`' comment; the readers' `parse` comments point to it.

**Report vs code**

- F7's sites matched the review once step 12 was accounted for: both readers already
  built `Units` through `withEveryTokenUnit`, so the shared body moved as a whole.
- F10: the copy is in `cmd/kaiak/main.go` `newGraph` (step 19), not where the review's
  line numbers put it.
- Hint: the sum was in `server/requestlog.go` (steps 22/23), not `api.go`.

**Test counts** (top-level tests from `go test -list`; in brackets, passes including
subtests, `go test -v`), before → after

- `internal/accounting`: 36 (122) → 36 (122).
- `internal/metrics`: 12 (12) → 12 (12).
- `internal/otlplog`: 41 (161) → 41 (161).
- `internal/server`: 189 (462) → 189 (462).
- `cmd/kaiak`: 28 (32) → 28 (32).
- `meter_test.go` and `responses_usage_test.go` are untouched and pass. The two
  log-export count tests changed only how their values are fed in (see above). No
  assertion changed.

**Removal checklist**

- `git grep -n 'LogExportCounts' gateway/` → none.
- Also clean: `otlplog\.Counts|[^a-z]Counts\{` and `u\.latest` in `gateway/`.

**Suite**: `scripts/check-gateway.sh` passed (exit 0). `scripts/check-all.sh` was not
run: step 24 was in progress under `control/` in the same worktree.

```
==> gofmt / go vet / staticcheck (gateway)
==> go test -race -count=1 (gateway): ok cmd/kaiak, e2e (106s), accounting, auth, clip,
    config, control, limits, logattr, metrics, netfail, otlplog, provider, routing,
    schemacheck, server, sse
==> live-test kit: self-test passed for vllm, llama-server, openai, azure-openai,
    anthropic, azure-anthropic, vllm with two backends
gateway checks passed
```
