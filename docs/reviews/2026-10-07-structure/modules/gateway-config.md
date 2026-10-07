# gateway-config — structure review

Modules: `gateway/internal/config`, `gateway/internal/schemacheck`, `gateway/internal/auth`.
Read in full: every non-test file of the three; `fixtures_test.go`, `snapshot_test.go`,
`schemacheck_test.go` for intent; the mirrors (`protocol/schema/config.schema.json`,
`control/kaiak-control/src/config/*`, `control/kaiak-control/src/schemas/index.ts`) and the
callers (`cmd/kaiak/main.go`, `internal/control/{control,decode,schema,client}.go`,
`internal/limits`, `internal/metrics/ops.go`, `internal/server/{models,limits,pipeline}.go`,
`internal/provider/provider.go`, `internal/routing/routing.go`).

## Module summaries

### `internal/config` (snapshot.go 605, schema.go 362, semantic.go 270, loader.go 180, document.go 137, errors.go 88, holder.go 26)

Responsibilities
- The config document pipeline: bytes → generic tree (`schemacheck.Decode`, also the
  duplicate-member scan) → schema walker (`checkSchema`, schema.go) → strict typed decode
  into `document` (document.go; a second parse of the bytes) → cross-reference rules
  (`checkSemantics`, semantic.go) → `resolve` into the immutable `Snapshot` (snapshot.go)
  → backend credential presence (`checkCredentials`, loader.go).
- The apply path: `Applier` (one lock, swap, log, observer), `FileLoader` (file mode),
  `Holder` (atomic pointer, readiness).
- Shared `$defs` predicates for the protocol message walkers (`IsID`, `IsTimestamp`,
  `IsPublicModelName`, `IsBackendModelName`), reserved env prefixes.
- The domain constants: defaults (snapshot.go:18-39), bounds (41-55), backend types,
  limit types, usage units.

Exported surface used outside (non-test), by importer
- `Holder`, `Snapshot` (+ `KeyByHash`, `ModelNames`, `Models`, `Backends`, `Groups`,
  `GlobalLimits`, global settings): server, limits, metrics, routing, provider, control, main.
- `Backend`, `BackendType` + 7 consts, `Deployment`, `Model`, `OutputLimit`, `Capabilities`,
  `Queue`, `Circuit`: provider, routing, server, metrics.
- `Price`, `PriceTier`, `Unit` + 5 consts: accounting, provider, limits, server, control.
- `Limit`, `LimitType` + 4 consts: limits, metrics, server, control.
- `Group` (only `ID`, `PathIDs`, `AllowedModels`, `Limits` are read outside), `ModelSet`
  (`Allows`, `Names`): auth, limits, metrics, server.
- `Applier`/`NewApplier`, `Load`, `LoadObserver`, `FileLoader`/`NewFileLoader`, `Check`:
  main, control, metrics.
- `ValidationError`, `CodeSchema`: control (client.go:463-467).
- `IsID`, `IsTimestamp`, `IsPublicModelName`, `IsBackendModelName`, `MaxGroupDepth`:
  control/schema.go. `IsReservedEnvName`: provider. `DefaultFailureThreshold`,
  `DefaultProbeInterval`: routing.New. `Parse`: only tests outside the package.

Dependencies: `schemacheck`, `logattr`; stdlib.

Domain concepts encoded here
- Backend type: consts snapshot.go:57-67; enum list + "needs api_key_env" list schema.go:94-101.
- Limit type: consts snapshot.go:69-76; enum + "integer-valued" lists schema.go:102-108;
  "per-minute" set semantic.go:230.
- Usage / price unit: snapshot.go:78-92; priced-unit list schema.go:109-111.
- Group tree, levels, wildcard `"*"`, child_defaults merge: semantic.go:148-208,
  snapshot.go:462-556.
- Counter bound (what counters a config allocates): semantic.go:210-258, MaxCounters
  snapshot.go:54.
- Defaults: snapshot.go:18-39 (mirror of the schema's `default` keywords).
- Issue codes (syntax / duplicate-member / schema / 18 semantic codes / api-key-env-unset):
  errors.go.

### `internal/schemacheck` (457)

Responsibilities
- A hand-written JSON Schema subset as composable walker parts (`Checker.Object`,
  `CollectionOf`, `ArrayOf`, `Enum`, `Const`, `Integer*`, `Number*`, `String*`,
  `Nullable`), path-exact issues.
- `Decode` (one value, `json.Number`) + `FirstDuplicateMember` token scan.
- JSON Pointer building, JS-compatible whitespace class, `MaxSafeInteger`, number
  reading, calendar validity (`IsRealDate`, `IsRealTimestamp`).

Exported surface used outside: `config` and `control` only — `Checker`, `Field`, `Decode`,
`DuplicateMemberError`, `Pointer`, `MaxSafeInteger`, `Nullable`, `NumberValue`,
`IsInteger`, `IsRealDate`, `IsRealTimestamp`, `JSWhitespace`.

Dependencies: stdlib only. Concepts: JSON Schema keywords, ECMAScript-vs-RE2 regex gap,
JS safe integer, RFC 6901, proleptic Gregorian calendar.

Verdict: clean. It was extracted from `config` when the control messages arrived
(4322f74, private history) and does one job. The only gap is that the *stage pipeline*
around it was copied, not extracted (F1).

### `internal/auth` (120)

Responsibilities
- Key → identity: bearer / x-api-key extraction, SHA-256 lookup in the snapshot,
  disabled / expired refusals; refusal codes and client-safe messages.
- Model access: `Identity.AuthorizeModel`, `ModelNotFound` (one answer for "absent" and
  "not allowed").

Exported surface used outside: server only (pipeline.go, models.go, errors.go) —
`Authenticate`, `Identity` (`KeyID`, `Group`, `AllowedModels()`, `AuthorizeModel`),
`Error`, `Code` + consts, `ModelNotFound`.

Dependencies: `config`, `clip`. Concepts: key hash format `sha256:<hex>`, auth headers,
refusal codes, model-existence masking.

Verdict: clean apart from one duplicated field (part of F3).

### On the number of representations (the review's question)

bytes → generic tree → typed `document` → `Snapshot` → per-subsystem views (router maps,
limiter counters, metric series, provider pools). Each layer earns its place:
- The tree is the only way to give path-exact schema issues and to see duplicate members.
- `document` gives the semantic rules typed access and gives `resolve` the
  "absent vs set" distinction (pointers) that defaults need; `DisallowUnknownFields`
  catches walker/type drift.
- `Snapshot` is the resolved form (pointers, durations, defaults, merged limits,
  intersected model sets) every reader needs; it is the right single source.
- The per-subsystem views live in their subsystems and are rebuilt per applied snapshot.

What does not earn its place is smaller: the second (and third) bytes parse is avoidable,
the integer workaround is solved two different ways (F1), and some resolved fields have no
reader (F3). Validation by the schema walker and by semantic rules does **not** overlap:
every walker check transcribes a schema keyword, every semantic rule is one the schema
cannot express, and the 137 invalid + 15 valid + 5 resolved shared fixtures hold the walker
to the schema and the Go rules to the TypeScript ones. The hand-written walker (vs. a
generic JSON Schema interpreter over embedded schemas) is the price of the zero-dependency
rule; an interpreter would need a schema copy inside the Go module (`go:embed` cannot
reach `protocol/`), an ECMAScript→RE2 pattern translation, and `if/then`, `not`, `anyOf`,
`$ref`-across-files support — not worth it at 362 + 224 walker lines that the fixtures
already pin. Leave that as is.

## Findings

### F1 — The validation pipeline and its error type exist twice: `config` and `control`
- **Kind** — cross-module.
- **Where** — `config/errors.go:9-88` (`CodeSyntax`, `CodeDuplicateMember`, `CodeSchema`,
  `Issue`, `Issue.String`, `ValidationError`, `Codes`), `config/snapshot.go:292-327`
  (`Parse` stage switch, `decodeDocument`), `config/schema.go:115-123` (`checkSchema`
  re-coding `schemacheck.Issue` → `config.Issue`); mirrored by `control/control.go:19-82`
  (same three codes, same `Issue` + `String`, `ValidationError` + `Codes`) and
  `control/decode.go:61-102` (`decode`: same stage switch, same re-coding loop, same
  "walker accepted what the types cannot hold" fallback). `control/client.go:354` and
  `:463-467` must check both error types.
- **Now** — two copies of: `Decode` → duplicate-member issue → syntax issue → walker →
  convert issues to `CodeSchema` → rules → strict typed decode → schema-coded fallback.
  The integer problem ("4096.0 is an integer") is also solved twice, differently: `config`
  declares every integer field `float64` in `document` and casts in `resolve`
  (`int64(...)` ×9, `countOr`, `maxInFlight`); `control` rewrites the tree with
  `plainIntegers` and decodes strictly into `int64` (`decodeTyped`, decode.go:107-135).
  `config` also parses the bytes a third time (`decodeDocument`) where `control` decodes
  from the tree it already holds. `CodeTimestampInvalid` is declared in both packages.
- **How it got here** — `schemacheck` was extracted from `config` when the control
  messages were added (4322f74, 2026-09-24: "internal/schemacheck walker parts extracted
  from config"); the stage glue and the error type were copied into `control` instead of
  moving with it. kaiak-control did share it: one `ValidationIssue` for config and
  messages (`control/kaiak-control/src/schemas/index.ts:14`).
- **Proposed shape** — in `schemacheck`: `Issue{Code, Path, Message}`, `ValidationError{
  Subject string; Issues []Issue}` with `Error`/`Codes`, the three stage codes, and
  `func Validate(subject string, data []byte, walk func(*Checker, any)) (any, error)`
  (syntax, duplicates, walker; returns the tree) plus `DecodeTyped[T](tree)` (the current
  `plainIntegers` + strict decode). `config.Parse` = `Validate` → `DecodeTyped[document]`
  → `checkSemantics` → `resolve`; `document`'s integer fields become `int64` (Value and
  prices stay `float64`). `control.decode` keeps its per-message rule hook on top of the
  same two calls. `client.rejectionCodes` and the `invalid` check become one `errors.As`.
- **Payoff** — removes one error type pair, three duplicated code constants, one stage
  switch, one issue-recoding loop (~70 lines), the `float64`-for-integers convention and
  its casts, and one full parse of every config document. A new stage or a change to issue
  formatting (e.g. adding a field to `Issue`) touches 1 place instead of 2.
- **Cost / risk** — medium-small: ~150 lines touched across 3 packages; tests that name
  `config.ValidationError`/`CodeSchema` (config fixtures/loader/snapshot tests, control
  fixtures) switch to the shared names. No contract change: codes, paths and messages
  stay as the fixtures pin them (the log line's text prefix "config rejected:" is kept via
  `Subject`). The fixture suites prove it.
- **Confidence** — high on the duplication; medium on swapping `decodeDocument` for
  `DecodeTyped` (verify the strict-decode error paths in `loader_test.go` still read the
  same).

### F2 — A limit type's properties are re-derived in ~10 places across 6 files
- **Kind** — cross-module (owned here: `config.LimitType`).
- **Where** — `config/schema.go:102-108` (`limitTypes`, `countLimitTypes`),
  `config/semantic.go:230` (per-minute set) and `:238`/`:241` (the literal `2` = hour +
  month counters), `limits/limits.go:55-65` (`shape`: window kind + measure, **falls
  through to month/cost for any type it does not name**) and `:97` (`countedTypes`),
  `metrics/ops.go:108` (`limitTypes`), `control/schema.go:30` (`totalsLimitTypes`) and
  `:142-151` (window-start switch), `server/limits.go:123-128` (`tokenWindow`).
- **Now** — the same four facts per type — its name, its window (minute / hour / month),
  what it counts (requests / tokens / cost), whether every scope counts it unlimited — are
  restated as ad-hoc lists and `switch`es. The counter bound in `config`
  (`countCounters`) restates the limiter's allocation rule (`limits.build`: `countedTypes`
  per scope + one per limit) by hand.
- **How it got here** — each feature added its own view: integer validation (schema),
  counter bound (round-3 fix f5cfcba), totals by scope and type (b44b13b), limit metrics.
- **Proposed shape** — one table next to the consts in `config`:
  `var limitTypes = map[LimitType]struct{ Window Window; Measure Measure }` with
  `Window` ∈ {Minute, Hour, Month} and `Measure` ∈ {Requests, Tokens, Cost} (the schema's
  own description of the limit type already defines these windows), exposed as methods
  `t.Window()`, `t.Measure()` and an ordered `LimitTypes()`. Derive from it: the schema
  enum, "must be an integer" (`Measure != Cost`), per-minute (`Window == Minute`), the
  always-counted set (`Window != Minute`, also the totals enum), `countCounters`' `2`,
  `limits.shape` (maps `Window` to its `Kind`), the metrics label list, `tokenWindow`.
- **Payoff** — a new limit type (e.g. `tokens_per_day`, `requests_per_hour`): ~10 Go
  places in 6 files → the table + the limiter's window mechanics (inherently new) + the
  totals window-start pattern. Removes `shape`'s silent fallback (an unlisted type is
  today treated as a USD-month budget). Removes 5 hand lists.
- **Cost / risk** — small-medium: ~60 lines across config, limits, metrics, control,
  server; no contract or schema change; behavior pinned by existing limits and fixture
  tests.
- **Confidence** — medium-high. Raises to high if a further limit type is on the roadmap
  (check `docs/BACKLOG.md`).

### F3 — Resolved fields and copies no production code reads
- **Kind** — cross-function.
- **Where** — `config/snapshot.go:237-256` (`Group.Parent`, `Group.Path []*Group`,
  `Group.Root()`), `:272-284` (`ModelSet.all`, `All()`), `:128-131`/`:346`/`:411`
  (`Snapshot.Keys` by ID), `:267-268` (`Key.AllowedModels()`); `auth/auth.go:41-45,77,96-98,107`
  (`Identity.allowed`).
- **Now** — outside `config`, only `Group.ID`, `PathIDs`, `AllowedModels`, `Limits` are
  read (metrics/ops.go:408, server/limits.go:41, server/upstream.go:451,
  limits/limits.go:292, limits/shared.go:138). `Parent` and `Path` exist only to build
  `PathIDs` inside `resolveGroup`; `Root()` has had no caller outside tests since 0.7.3;
  `ModelSet.All()` and `Snapshot.Keys[...]` are read only by tests (`Keys` otherwise only
  for `len` in the "config applied" log, loader.go:75). `Identity.allowed` is a copy of
  `Identity.Group.AllowedModels`, fetched through `Key.AllowedModels()`, whose only caller
  is auth.go:77. Two representations of the same path (`Path`, `PathIDs`) are kept in step,
  and the resolution fixture test checks their agreement (fixtures_test.go:273-277).
- **How it got here** — the group-tree plan (e020e93) resolved a full pointer tree up
  front; readers ended up needing only IDs.
- **Proposed shape** — `Group{ID, PathIDs, AllowedModels, Limits}`; `resolveGroup` takes
  the parent's `PathIDs`/`AllowedModels` from the resolved parent. Drop `ModelSet.all`
  (the resolution fixture compares "all" against `s.ModelNames`). Drop `Snapshot.Keys`
  (log `len(keysByHash)`; tests use `KeyByHash`). Drop `Identity.allowed` and
  `Key.AllowedModels()`; `Identity` methods read `id.Group.AllowedModels`.
- **Payoff** — ~25 lines and 6 members/methods removed; one "two forms of the path"
  invariant and one duplicated field gone; the snapshot's surface says what readers use.
- **Cost / risk** — small; `snapshot_test.go` (lines ~228-239, 256-318) and
  `fixtures_test.go:273-281` adjust. No contract touched.
- **Confidence** — high (grep-verified: no non-test reader).

### F4 — Model metadata is mirrored three times: document → snapshot → `/v1/models` JSON
- **Kind** — cross-module.
- **Where** — `config/document.go:86-96` (`capabilitiesDoc`, `outputLimitDoc`),
  `config/snapshot.go:194-204` (`Capabilities`, `OutputLimit`) and the field copies at
  `:423-428`, `:436`; `server/models.go:34-39` (`capabilitiesJSON`), `:47-50`
  (`outputLimitJSON`) and the copies at `:74-79` and in the props handler (~`:192`).
- **Now** — four booleans and two integers declared three times with identical field
  names and two copy blocks; the schema itself says this metadata is "declared model
  metadata, served on /v1/models and /v1/models/{id}/props" — it passes through unchanged.
- **How it got here** — the document/snapshot split was applied uniformly, including to
  pass-through data; the model endpoints then added their own JSON types.
- **Proposed shape** — one `config.Capabilities` (and `config.OutputLimit`) carrying
  `json` tags, used by `document` for decoding, by `Model`, and embedded directly by the
  server's entries. (`OutputLimit` can share only once `document` uses `int64` integers —
  F1; `Capabilities` can share today.)
- **Payoff** — a new capability (e.g. `audio`, `structured_outputs`): Go places 6
  (doc type, snapshot type, resolve copy, JSON type, response copy, walker) → 2 (type,
  walker), plus schema + TS as today. ~25 lines removed.
- **Cost / risk** — small; puts the `/v1/models` field names on a config type. That is
  the documented contract (GATEWAY.md, model endpoints), so the coupling is real, not
  accidental. Server tests pin the response shape.
- **Confidence** — medium (judgment call on letting config types carry the response's
  JSON tags; the alternative is to accept the mirror).

### F5 — `LoadObserver` doubles as the "config applied" hook, and readers re-fetch the snapshot it does not carry
- **Kind** — cross-module.
- **Where** — `config/loader.go:17-33` (`Load` has no snapshot), `:59-79` (`report` is
  called under `a.mu`, after `Swap`); `cmd/kaiak/main.go:375-387` (the observer
  reconfigures router, circuits series, provider pools, model check, body-cap warning —
  five `holder.Current()` calls); `metrics/ops.go:478-495` (a sixth: "the holder holds it
  by then").
- **Now** — the hook documented as "the ops metrics" is the bus that propagates a new
  config to four subsystems. Correctness rests on an unstated invariant: the observer runs
  while `Apply` still holds `a.mu`, so `holder.Current()` is the snapshot just applied.
  Moving `report` out of the lock (an obvious tidy-up) would let a concurrent apply slip
  in between.
- **How it got here** — the observer started as the ops-metrics feed; later features
  (routing caps, model check, connection-pool retention) hooked into the same callback.
- **Proposed shape** — `Load.Snapshot *Snapshot` (nil when rejected); the closure and
  `Ops.ConfigLoaded` use it instead of `holder.Current()`; rename the type/doc to say it is
  the applied-config notification. (The limiter's pull model — `sync` compares
  `holder.Current()` with `l.applied`, limits.go:250-255 — is a separate, legitimate
  choice; leave it.)
- **Payoff** — six re-reads and one implicit lock-ordering invariant removed; the hook's
  name matches its job.
- **Cost / risk** — very small (~10 lines, main + metrics + loader); no contract.
- **Confidence** — high.

### Not findings (checked, leave as is)
- Hand-written walker vs JSON Schema: no duplication with semantic rules; see the
  representation section.
- Go `semantic.go` vs TS `semantic.ts`/`tree.ts`/`limits.ts`: same rules in two languages
  by necessity, pinned by the shared fixtures and resolution fixtures.
- `Holder` / `Applier` / `FileLoader` split: read side, write side, file source — each
  small and earning its place.
- `schemacheck`: clean.
- `auth`: clean apart from F3's duplicate field.

## Cross-module hints

- `control/schema.go:38-41` re-declares `publicModelNameWhat` (hard-coding `"/props"`
  instead of `config.reservedModelSuffix`) and `timestampWhat`, which `config/schema.go:68`
  and `:331` also state. The shared `$defs` predicates are exported from `config`; their
  descriptions are not. Exporting check functions (predicate + description) would leave
  one statement each.
- Schema `default` keywords (`protocol/schema/config.schema.json`) vs
  `config/snapshot.go:18-39`: 17 defaults stated twice with no test tying them (all match
  today). A test reading the schema's defaults and comparing with `resolve` of
  `valid/minimal.json` would pin them; only the gateway applies defaults (kaiak-control
  never does).
- `config.backendTypes` (schema.go:94) vs `provider.kinds` (provider.go:317): no test
  ties them; a type admitted by the schema but missing from `kinds` panics in `kindOf`
  (provider.go:329) on first use. TS has the equivalent pin (`BACKEND_TYPES` vs the schema
  enum). "Needs api_key_env" (`keyedBackendTypes`) is also a per-type property that sits
  outside provider's `backendKind` table.
- Usage units: priced set at `config/schema.go:109`, full set at `control/schema.go:33`,
  plus literal unit lists in `accounting/accounting.go:215` and `limits/limits.go:635` —
  for the accounting reviewer.
- `routing.New` (routing.go:191) builds a `config.Circuit` from config defaults before
  any config is applied — the only non-config reader of `Default*`; the other exported
  `Default*` constants have no outside reader.
- A config event's config is parsed ~5 times end to end (control envelope `Decode`,
  duplicate scan, `json.Unmarshal` into `RawMessage`, then config's own 3 parses). At
  3–7k keys (~1 MB) this is load-time only; F1 removes one of them.

## Bugs noticed in passing

- None in behavior. Two stale leftovers:
  - `config/fixtures_test.go:248-251`: `resolvedGroup.Limits[].Models` — a field from
    per-model limits, removed in 42b7513; no resolution fixture carries `models`
    any more.
  - `config/snapshot.go:318`: the comment names `decodeTree`, which does not exist
    (it is `schemacheck.Decode`).
