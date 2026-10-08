# Step 11 — one limit-type table in the gateway

**Status:** done (2026-10-08)

## Intent

A limit type's window, measure and whether the control plane counts it are stated once,
in `config`, and read everywhere else. Today they are restated in ~10 places across 6
packages, and `limits.shape` silently treats any type it does not name as a USD-per-month
budget.

## Findings

- T1, config F2, routing-limits F9, observability F3 (the limit-type list only):
  - `config`: `LimitType → {Window: minute|hour|month, Measure: requests|tokens|cost,
    Counted bool}` (Counted = the control plane counts it), with `t.Window()`,
    `t.Measure()`, `t.Counted()` and an ordered `LimitTypes()`;
  - derived from it: the schema walker's enum and its "must be an integer" list
    (`Measure != Cost`); the per-minute set in `semantic.go` and `countCounters`' literal
    `2`; `limits.shape` (no fallthrough: an unknown type is a programming error), the
    `countedTypes` set and the four `kind != SlidingMinute` checks (they read `Counted`);
    `control/schema.go`'s totals enum and its window-start switch; `metrics/ops.go`'s
    `limitTypes`; `server/limits.go`'s `tokenWindow` and the "per minute" / "per month"
    wording in `errLimited`.

## Files likely touched

- `gateway/internal/config/{snapshot,schema,semantic}.go`.
- `gateway/internal/limits/{limits,shared,window}.go`.
- `gateway/internal/control/schema.go`.
- `gateway/internal/metrics/ops.go`.
- `gateway/internal/server/limits.go`.

## Decisions made during planning

- `config` owns the table: `LimitType` is a config concept, and every reader already
  imports `config`.
- `limits.Kind` stays (window mechanics); it is computed from `Window`.
- `metrics` reads `config.LimitTypes()` for its series; label values unchanged
  (decision 2 assigns the rest of the label lists to the OTel metrics plan).

## Removal checklist (clean at phase end)

- `git grep -nE 'countLimitTypes|countedTypes|totalsLimitTypes' gateway/` → none.
- `git grep -nE 'tokens_per_minute|tokens_per_hour|requests_per_minute|usd_per_month' gateway/internal --  ':!*_test.go'`
  → only the constants and the table in `config`.

## Acceptance criteria

- One table; a test asserts every `LimitType` constant has a row and the schema's enum
  equals `LimitTypes()`.
- No behaviour change: limits, config fixture and server limit tests pass unchanged.
- `scripts/check-gateway.sh` green, or reds named with the step that clears them.

## Result

**What changed**

- `config` (`snapshot.go`, next to the `LimitType` constants): the one limit-type table.
  - `Window` (`WindowMinute`/`WindowHour`/`WindowMonth`, string values `minute`,
    `hour`, `month`: the words the client messages use) and `Measure`
    (`MeasureRequests`/`MeasureTokens`/`MeasureCost`).
  - `limitTypeTable []limitTypeRow{typ, window, measure, counted}`, in the schema's
    order. `LimitTypes()` lists it; `t.Window()`, `t.Measure()`, `t.Counted()` read a
    row. An unknown type panics (`config: unknown limit type "…"`): only valid types
    get past the schema.
- Derived from it, each hand list or switch removed:
  - `config/schema.go`: `limitTypes` and `countLimitTypes` are gone. The walker's enum
    is `limitTypeNames` (from `LimitTypes()`); "must be an integer" is
    `Measure() != MeasureCost`.
  - `config/semantic.go`: `countCounters`' per-minute set is `!Counted()`, and the
    literal `2` is `perScope`, the number of counted types.
  - `limits`: `shape` is gone. `newCounter` reads `k.typ.Measure()` and
    `kindOf(k.typ.Window())`; `kindOf` (`window.go`) maps a `config.Window` to its
    `Kind` and panics on an unknown one. `effectiveLimit` reads `Type.Measure()`.
    `prunePushedLocked` reads `kindOf(k.typ.Window())`.
  - `limits`: `countedTypes` is gone; `sync` takes every `Counted()` type for every
    scope. Every check that meant "counted" or "a local share" reads `Counted()`: the
    `l.minute` collection, the retained-count filter, `newCounter`'s `w.shared`,
    `applyLimit`'s share branch, and the full-limit exception in `admits` and
    `blockedByRunning`. `warnSmallSharesLocked`'s `!= LimitTokensPerMinute` is
    `Counted() || measure != MeasureTokens`. The `kind` checks left are in
    `window.go`: window mechanics (buckets vs fixed windows).
  - `limits.Measure` is gone; `limits` uses `config.Measure` (`counter.measure`,
    `Rejection.Measure`, `LogValue`, `amountOf`, `need`, headers). Its "cost counts
    nano-USD" note moved to the `counter` comment.
  - `control/schema.go`: `totalsLimitTypes` is gone. The totals window enum is
    `windowTypes` (the `Counted()` types); the window-start check switches on the type's
    `Window()` (hour → top of an hour, month → first of a month), after an enum guard;
    a counted window with no start shape panics.
  - `metrics/ops.go`: `limitTypes` is gone. The `kaiak_limit_rejections_total` series
    are pre-created from `config.LimitTypes()`, and `CountLimitRejection` checks
    against it.
  - `server/limits.go`: `tokenWindow` is gone. `errLimited` writes
    `r.Type.Window()` for "per minute" (requests), "per minute"/"per hour" (tokens) and
    "per month" (budget). The format strings are otherwise unchanged.
  - `control/messages.go`: `TotalsWindow.WindowStart`'s comment no longer names the
    two types.
- Tests:
  - `config/schema_test.go`: `TestEveryLimitTypeHasARow` (parses the package's non-test
    files with `go/parser`; every `LimitType` constant has a row, and no other row
    exists) and `TestLimitTypesAreTheSchemaEnum` (walker enum = `LimitTypes()` =
    `config.schema.json`'s `$defs.limit` enum; the schema's integer `if` enum = the
    non-cost types). Checked by mutation, then reverted: dropping the
    `usd_per_month` row failed both with messages naming it.
  - `control/fixtures_test.go`: `TestTotalsWindowTypesAreTheCountedLimitTypes`
    (walker enum = counted types = `totals.schema.json`'s window type enum).
  - Renames only, assertions unchanged: `MeasureX` → `config.MeasureX` in
    `limits_test.go` (4) and `shared_test.go` (6); `limits.MeasureX` →
    `config.MeasureX` in `server/limits_test.go` (2) and `server/metrics_test.go` (3).
  - `server/metrics_test.go`'s three `errLimited` answers now also set `Type`
    (`requests_per_minute`, `tokens_per_minute`, `usd_per_month`): `errLimited` reads
    the window from the type, and a rejection without one would panic. The test asserts
    only the class, which is unchanged.
- No spec change: no contract changed. The client messages are byte-identical.

**Decisions made during the step**

- **One `Counted` flag.** "Counted by every scope, limited or not" and "the control
  plane counts it" are the same set in the code (both hour + month) and in the spec
  (GATEWAY.md, Limits → Every scope is counted: per-minute windows are local shares
  counted only for their limits). One flag; the table comment states both meanings.
- **One `Measure` type, in `config`.** It is what a limit type counts, a config fact;
  `limits` adds only the nano-USD unit of a cost counter, kept in its own comment.
  `limits.Kind` stays (window mechanics) and is computed from `Window` by `kindOf`.
- **`Window` is a string type** whose values are the words of the client messages, so
  `errLimited` writes it directly.
- **`admits`/`blockedByRunning` read `Counted()`**, not the kind: their exception is
  for a per-minute *share*, which only a non-counted type has. The finding listed four
  sites; these two mean the same thing. No behaviour change (in file mode and for
  counted windows `w.limit` equals the full limit, so the clause was false anyway).
- **`Rejection.Measure` stays** (a copy the limiter sets from the counter, read by
  `errLimited` and the log line); removing it is not a vocabulary restatement.
- **The `counters-exceeded` message keeps "two for global and for every group"**: the
  same text is in `kaiak-control`'s `semantic.ts` and CONTROL-PROTOCOL.md; rewording
  it is a both-halves message change, out of this step.
- **`fakecontrol` keeps its `usd_per_month` literal** (`fakecontrol.go:317`, month vs
  hour window of a scripted totals window). The fake stands in for the control plane
  and imports no gateway package; reading the gateway's table there would make the
  fake agree with the code under test by construction. Flagged for review: it is the
  one non-test hit of checklist item 2.

**Report vs code** (034329e; code at af6b7b6)

- The four kind checks of routing-limits F9 were at `limits.go:291,296,346,380`
  (report: 298, 303, 350, 384). Two more checks with the share meaning were at
  `:400,425` (`admits`, `blockedByRunning`), not in the report.
- `shape` was at `limits.go:53-63`, `countedTypes` at `:95`. `control/schema.go`'s list
  at `:29` and switch at `:139-148`. `tokenWindow` at `server/limits.go:121-126`.
- `warnSmallSharesLocked` (`shared.go:121`) also named `LimitTokensPerMinute`; not in
  the report.
- `control/outbound_test.go` restates neither the totals enum nor the window starts.

**Metrics series** (`kaiak_limit_rejections_total`, pre-created, captured with a
throwaway test on `NewOps` before and after, then deleted): the 10 lines (HELP, TYPE,
8 series `scope_kind` × `type` at 0) are **byte-identical**.

**Tests** (`go test -count=1 -v`, `=== RUN` / `--- PASS`, before → after)

| Package | before | after |
|---|---|---|
| `internal/config` | 204 | 206 (+`TestEveryLimitTypeHasARow`, +`TestLimitTypesAreTheSchemaEnum`) |
| `internal/limits` | 80 | 80 |
| `internal/control` | 237 | 238 (+`TestTotalsWindowTypesAreTheCountedLimitTypes`) |
| `internal/metrics` | 12 | 12 |
| `internal/server` | 456 | 456 |

- The passing-test name lists differ only by the three added tests. None failed.

**Removal checklist** (step 11)

- `git grep -nE 'countLimitTypes|countedTypes|totalsLimitTypes' gateway/` → none (also
  `tokenWindow`, `shape(` → none).
- `git grep -nE 'tokens_per_minute|tokens_per_hour|requests_per_minute|usd_per_month' --
  gateway/internal ':!*_test.go'` → the four constants in
  `config/snapshot.go`, and `fakecontrol/fakecontrol.go:317` (see Decisions).

**Suite**: `scripts/check-gateway.sh` passed (exit 0) on the final code; only this step
file was edited after it started. The control half (`npm test`, cross-half e2e) was not
run: this step touches no `control/` code or shared fixture.

```
==> gofmt / go vet / staticcheck 2026.2.1 (gateway)
==> go test -race -count=1 (gateway): ok cmd/kaiak, e2e (106s), accounting, auth, clip,
    config, control, limits, logattr, metrics, netfail, otlplog, provider, routing,
    schemacheck, server, sse
==> live-test kit: gofmt / vet / staticcheck; self-test passed for vllm, llama-server,
    openai, azure-openai, anthropic, azure-anthropic, vllm with two backends
gateway checks passed
```

