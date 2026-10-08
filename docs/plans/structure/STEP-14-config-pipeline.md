# Step 14 — one validation pipeline; one model metadata type; `Load.Snapshot`

**Status:** done (2026-10-08)

## Intent

The gateway validates a config document and a protocol message through one pipeline
with one error type; pass-through model metadata has one type; and the config-applied
hook carries the snapshot it announces.

## Findings

- T7, config F1: in `schemacheck`, `Issue{Code, Path, Message}`,
  `ValidationError{Subject, Issues}` with `Error`/`Codes`, the three stage codes, and
  `Validate(subject, data, walk) (tree, error)` (syntax, duplicates, walker) plus
  `DecodeTyped[T](tree)` (`plainIntegers` + strict decode). `config.Parse` =
  `Validate` → `DecodeTyped[document]` → `checkSemantics` → `resolve`; `document`'s
  integer fields become `int64` (limit values and prices stay `float64`); the third
  parse of the bytes goes. `control.decode` keeps its per-message rule hook on top of the
  same calls; `client.rejectionCodes` and the invalid check become one `errors.As`.
  `config`'s and `control`'s copies of the error type, codes, stage switch and re-coding
  loop go; `CodeTimestampInvalid` is declared once.
- config F4 / decision 14: one `config.Capabilities` and `config.OutputLimit` with JSON
  tags, used by `document`, `Model`, and the server's `/v1/models` entries;
  `capabilitiesDoc`, `outputLimitDoc`, `capabilitiesJSON`, `outputLimitJSON` and their
  copy blocks go.
- T6, config F5: `Load.Snapshot` (nil when rejected); the applied-config closure in
  `main` and `Ops.ConfigLoaded` read it instead of `holder.Current()`; the type and doc
  say it is the applied-config notification. The limiter's pull model stays.
- config hint: `control/schema.go` re-declares `publicModelNameWhat` (hard-coding
  `"/props"`) and `timestampWhat`; export check functions (predicate + description) from
  `config` so each is stated once.

## Files likely touched

- `gateway/internal/schemacheck/`, `config/{errors,snapshot,schema,document,loader}.go`,
  `control/{control,decode,client,schema}.go`, `server/models.go`, `metrics/ops.go`
  (`ConfigLoaded` reads `Load.Snapshot`; its series logic is the OTel plan's),
  `cmd/kaiak/main.go`, and the tests naming the old error types.

## Decisions made during planning

- Issue codes, paths and messages stay exactly as the fixtures pin them; the "config
  rejected:" log prefix is kept through `Subject`.
- observability F5 (the circuit series) is **not** here: it is in the OTel metrics plan.

## Removal checklist (clean at phase end)

- `git grep -nE 'type ValidationError|type Issue struct' gateway/internal/{config,control}` → none.
- `git grep -nE 'capabilitiesDoc|outputLimitDoc|capabilitiesJSON|outputLimitJSON|decodeDocument' gateway/` → none.
- `git grep -n 'holder.Current()' gateway/cmd/kaiak gateway/internal/metrics` → none in
  the applied-config path.

## Acceptance criteria

- Every config and message fixture gives the same codes, paths and messages on both
  halves; `/v1/models` responses unchanged (server tests).
- A config event's config is parsed one time fewer (state where, in the Result).
- `scripts/check-all.sh` green. **Phase 3 ends here.**

## Result

**What changed**

- One validation pipeline (T7, config F1), in `schemacheck`:
  - `Issue{Code, Path, Message}` (with `String`), `ValidationError{Subject, Issues}`
    (`Error` = `Subject + " rejected: " + issues`, `Codes`), and the three stage codes
    `CodeSyntax`, `CodeDuplicateMember`, `CodeSchema`. `Checker.Fail` records
    `CodeSchema` itself, so no stage re-codes the walker's issues.
  - `Validate(subject, data, walk func(tree any) []Issue) (any, error)`: one JSON
    value, then the duplicate-member scan, then the walker; returns the tree.
  - `DecodeTyped[T](subject, tree)`: `plainIntegers` + strict decode
    (`DisallowUnknownFields`); a failure is a `ValidationError` with `CodeSchema`
    (the walker accepted what the types cannot hold).
  - `Decode` and `DuplicateMemberError` are gone (folded into `Validate`, no other
    user).
- `config`:
  - `Parse` = `Validate` → `DecodeTyped[document]` → `checkSemantics` → `resolve`.
    `decodeDocument` (the third parse of the bytes) is gone.
  - `document`'s integer fields are `int64` (limit values and prices stay `float64`).
    In `resolve`, the nine `int64(...)` casts and `maxInFlight` are gone; a generic
    `valueOr(p, fallback)` replaces the five `if … != nil` blocks of the globals and
    the two metrics-label blocks. `countOr` (saturates at `MaxInt32`) and
    `millisecondsOr` (saturates at the longest duration) stay, now over `*int64`. The
    semantic rules' messages lose their three `int64(...)` casts.
  - `errors.go` keeps only config's own codes (`CodeAPIKeyEnvUnset`, the semantic
    codes) and `rejectedSubject = "config"`; `Issue`, `ValidationError` and the stage
    codes are gone. `CodeTimestampInvalid` is declared here only.
  - `checkSchema` returns the walker's issues as they are; `checkCredentials` and
    `Applier.reject` use the shared type.
- `control`:
  - `control.go` keeps `ProtocolVersion` and the four message rule codes; its
    `Issue`, `ValidationError`, stage codes and `CodeTimestampInvalid` are gone.
    `semantic.go` and `usage.go` (`batchRefusals`) use `config.CodeTimestampInvalid`.
  - `decode.go`: `validate(subject, data, walk)` (`schemacheck.Validate` with the
    message's walker) and `decode[T](subject, data, walk, rules)` = `validate` →
    rules → `DecodeTyped[T]`. The stage switch, the re-coding loop, `decodeTyped` and
    `plainIntegers` are gone. `DecodeConfigEvent` is `validate` and the raw config.
  - `client.go`: `unavailable` and `rejectionCodes` both read
    `*schemacheck.ValidationError` through one `errors.AsType` each.
- One model metadata type (config F4, decision 14): `config.Capabilities` and
  `config.OutputLimit` carry the JSON tags. `document` decodes into them, `Model`
  holds them as decoded (`OutputLimit` is the decoded pointer), and the server's
  `modelEntry`, `anthropicModelEntry` and `modelProps` embed them.
  `capabilitiesDoc`, `outputLimitDoc`, `capabilitiesJSON`, `outputLimitJSON` and the
  three copy blocks are gone.
- `Load.Snapshot` (T6, config F5):
  - `Load.Snapshot` is the config swapped in, nil when rejected; `Load.Applied()`
    reads it (the `Applied` field is gone).
  - `LoadObserver` is gone: `NewApplier` takes `onLoad func(Load)`. `Load`'s doc says
    it is the applied-config notification that the config's followers take
    `Snapshot` from; the `Applier.mu` doc says `onLoad` hears loads one at a time, in
    swap order.
  - `cmd/kaiak/main.go`'s closure reads `load.Snapshot` (six `holder.Current()` reads
    gone); `Ops.ConfigLoaded` prepares the series from `l.Snapshot`.
- Shared `$defs` descriptions (config hint): `config.PublicModelNameWhat` (built from
  `reservedModelSuffix`) and `config.TimestampWhat` beside `IsPublicModelName` and
  `IsTimestamp`. `control/schema.go`'s `publicModelNameWhat` (with its literal
  `"/props"`) and `timestampWhat` are gone; config's key `expires_at` check uses
  `TimestampWhat` instead of its own literal.
- `fixturetest.RunInvalid` reads `*schemacheck.ValidationError`: its `coded` interface
  and its `schemaCode` parameter are gone.
- `docs/ARCHITECTURE.md`: the `schemacheck` entry says it holds the one pipeline and
  the one rejection error.

**Fixture output diff**

A throwaway test in each package (deleted) dumped, for every file, the full `Error()`
text (every issue's path, message and code) and `Codes()`, or the resolved snapshot /
decoded message as JSON:

- `config`: 160 documents — `protocol/fixtures/config/{valid,invalid}`, `examples/`,
  `protocol/fixtures/duplicate-members` — 143 rejections.
- `control`: 111 documents — every `messages/<kind>/{valid,invalid}` through the
  fixture decoders (a config event through `config.Parse` too), and the message
  duplicate-member fixtures — 92 rejections.

Before and after are identical. The only difference in the config dump was the
snapshot's own JSON encoding of `Capabilities`/`OutputLimit` (field names now from
the tags), normalized before the diff.

**Parse count** (a config event's config, end to end, at the API-call level)

- Before, 6: the event envelope's `schemacheck.Decode` (1) and duplicate scan (2),
  `json.Unmarshal` into `json.RawMessage` (3); then `config.Parse`'s
  `schemacheck.Decode` (4), duplicate scan (5) and `decodeDocument` (6).
- After, the sent bytes are read 5 times: 1–5 as before (`Validate` in place of
  `Decode`). The removed one is `decodeDocument` in `config.Parse`.
- Caveat: `DecodeTyped` re-encodes the tree and strictly decodes that encoding (as
  `control` always did), so the number of JSON decodes is still 6, plus one
  `json.Marshal`. What went is the third parse of the *bytes* and the second
  integer rule, not decoding work.

**Decisions made during the step**

- **`Validate`'s walker is `func(tree any) []Issue`**, not `func(*Checker, any)`:
  both walkers are their own types embedding `Checker` (`schemaCheck`, `walker`), so
  each caller builds its walker and returns `Issues()`.
- **`DecodeTyped` takes the subject**, since its fallback is a `ValidationError`.
- **`DecodeConfigEvent` does no typed decode**: the config is returned as sent, and
  the typed decode would hand over a re-encoding (sorted members, rewritten
  integers, a different size).
- **`CodeTimestampInvalid` lives in `config`**, the owner of the shared `$defs`
  (`IsTimestamp`); `control` imports `config` already.
- **Descriptions as exported constants** (`PublicModelNameWhat`, `TimestampWhat`),
  not a predicate+description type: the walker parts take the two as separate
  arguments (`StringWhere`, `CollectionOf`), and `allowedModels` builds its own
  message around the description. The backend-model-name descriptions were left:
  config's and control's differ, and messages stay byte-identical.
- **`Load.Applied` became a method over `Snapshot`**: one fact, stated once. The hook
  is a plain `onLoad func(Load)`, as `control.Options.OnTotals` is.
- **Integers in semantic messages print with `%d`.** With `float64` fields, `%v`
  printed values of 10^6 and above in exponent form ("1.047576e+06"); now they are
  digits, as kaiak-control's message prints them. No fixture reaches it (the dump is
  identical); it changes the log text of an `output-limit-*` rejection with such
  values.
- **`schemacheck_test`**: `TestDecodeRefusesADuplicateMember` →
  `TestValidateRefusesADuplicateMember`, the same two assertions through `Validate`
  (a duplicate at `/a/0/b`; a syntax error is not a duplicate, now asserted as the
  `syntax` code).
- **Tests adapted, none weakened**: tests naming `ValidationError`/`Code{Schema,
  Syntax,DuplicateMember}` use the `schemacheck` names; `Load` literals of an
  applied config in `metrics_test.go` carry `Snapshot: holder.Current()` (the holder
  held it already); `control/client_test.go` reads `Applied()`.
  `TestApplierReportsEveryLoadToTheObserver` → `TestApplierTellsOnLoadOfEveryLoad`,
  plus one assertion: the applied load's `Snapshot` is the one `Apply` returned and
  the holder holds.

**Report vs code** (034329e line numbers; code at 0d36e50)

- config F1: as reported. `Parse`/`decodeDocument` had moved to
  `snapshot.go:371-406` (steps 11–12 added the tables above), `decodeTyped`/
  `plainIntegers` to `decode.go:93-125`. The stale `decodeTree` comment ("Bugs
  noticed") went with `decodeDocument`.
- `client.go`: `unavailable` (`:354`) checked only `control`'s type. Config
  rejections never reach it (the boot applies outside `followStream`'s result), so
  matching the shared type changes nothing it decides.
- config F5: main's closure read `holder.Current()` six times, not five
  (`missingEndpoints.Retain` was added since the review), plus `ops.go`'s one.
- config F4 and the hint: as reported.

**Tests** (`go test -count=1 -v`, `--- PASS`; before → after, no skips, no fails)

- `internal/schemacheck` 13 → 13; `internal/config` 208 → 208;
  `internal/control` 239 → 239; `internal/server` 456 → 456; `cmd/kaiak` 33 → 33;
  `internal/metrics` 12 → 12.

**Removal checklists** (phase 3; `git grep`, untracked files included where the
command allows)

- Step 11: `countLimitTypes|countedTypes|totalsLimitTypes` in `gateway/` → none.
  Limit-type names outside tests → the constants in `config/snapshot.go` and
  `fakecontrol/fakecontrol.go:317` (kept by step 11's decision).
- Step 12: `tokenUnits(` in `gateway/` → none.
- Step 13: `hourStart|monthStart|CurrentWindows` in `control/` → none.
  `JSON.stringify([` in `control/` → `messages/totals.ts` (`scopeTypeKey`) and
  `storage/window-key.ts` (`windowKeyOf`) only.
- Step 14: `type ValidationError|type Issue struct` in `config`, `control` → none.
  `capabilitiesDoc|outputLimitDoc|capabilitiesJSON|outputLimitJSON|decodeDocument`
  in `gateway/` → none. `holder.Current()` in `cmd/kaiak` and `metrics` → none in
  the applied-config path (left: the status `Serving` callback in `main.go`, the
  scrape-time gauges, `NewOps`' first preparation, `CountRequestError`, the usage
  metrics, and tests). Also gone: `LoadObserver`, `publicModelNameWhat`,
  `timestampWhat`, `decodeTyped`, `schemacheck.Decode`, `DuplicateMemberError`,
  `maxInFlight(`.

**Suite** (phase 3 end): `scripts/check-all.sh` passed (exit 0) on the final code.
Only this step file was edited after it started.

```
==> gofmt / go vet / staticcheck 2026.2.1 (gateway)
==> go test -race -count=1 (gateway): ok cmd/kaiak, e2e (107s), accounting, auth, clip,
    config, control, limits, logattr, metrics, netfail, otlplog, provider, routing,
    schemacheck, server, sse
==> live-test kit: gofmt / vet / staticcheck; self-test passed for vllm, llama-server,
    openai, azure-openai, anthropic, azure-anthropic, vllm with two backends
gateway checks passed
==> npm test (control): 617 tests, 616 pass, 0 fail, 1 skipped
==> npm run lint (control): boundaries ok
==> cross-half e2e (sample control plane + two gateways; two cores over one store)
ok  	kaiak/e2e	65.357s
all checks passed
```
