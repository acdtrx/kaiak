# Step 14 — one validation pipeline; one model metadata type; `Load.Snapshot`

**Status:** not started

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
