# gateway-provider — structure review

Modules: `gateway/internal/provider`, `gateway/internal/sse`, `gateway/internal/clip`,
`gateway/internal/netfail`. Branch `main` at `034329e`.

## Module summaries

### `gateway/internal/provider` (≈2,850 non-test lines; hot: `provider.go` 18, `openai.go` 18, `wire.go` 10 commits)

Responsibilities:
- The `Provider` contract (`provider.go`): `Request` in, `Response` of `Event`s out, in
  the client's format; failure vocabulary (`Code*`, `Error`, `ErrorEvent`/`Kind`,
  `ErrorEventEnd`, `RefusalError`, `ErrStalled`/`ErrResponseTimeout`/`ErrIncomplete`).
- The endpoint and format concepts: `Endpoint` (7 values, `path()`, `Format()`),
  `Format` (OpenAI, Messages, Responses).
- Registry: one module per backend type (`kinds` table, `provider.go:317`), connection
  pool per backend, credential lookup with the reserved-prefix guard, `Probe`,
  `Retain`, `Serves`, `ListsModels`.
- Seven backend modules (`openai.go`, `azure_openai.go`, `vllm.go`, `llama_server.go`,
  `openai_compatible.go`, `anthropic.go`, `azure_anthropic.go`, 67–87 lines each):
  endpoints served, URL layout, credential header, extra body edits, missing-model
  rule, unknown-path rule, core endpoints, probe.
- The wire core (`wire.go`, 811 lines): send with first-event/response timers, 401/403
  and 404/405 classification, first-event peek with preamble, the models-list fetch
  and `PathMissingError`, 404 body readers and the shared missing-model rules, the
  passthrough edit set and the standard-service-tier edit, upstream headers, the
  upstream response (stall timer, completeness, usage-chunk hiding, model rewrite,
  error-event relaying).
- Body splicing (`body.go`): `editObject`/`indexObject`, `setIncludeUsage`,
  `isUsageOnlyChunk`, the releasable upstream body.
- Model name rewrite (`model.go`): byte-level incremental JSON scanner.
- Per-format stream end (`stream_end.go`): completeness, error-event detection and
  kind, nested-model location — three implementations.
- Anthropic price-option refusal (`anthropic_price.go`): streaming `json.Decoder` walk.

Exported surface used outside, with importers:
- `server` (`upstream.go`, `errors.go`, `metrics.go`, `inbound.go`, `params.go`,
  `pipeline.go`, `api.go`): `Registry` (`For`), `Request`, `Param`, `Response`,
  `Event`, the 7 `Endpoint` constants, `Format*`, all `Code*`, `Error`, `ErrorEvent*`,
  `ErrorEventEnd`, `RefusalError`, `ErrStalled`, `ErrResponseTimeout`, `ErrIncomplete`,
  `Serves`.
- `accounting` (`meter.go`, `estimate.go`, `openai_usage.go`): `Endpoint` (+
  `Completions`, `Embeddings`), `Format*`, `Event`, `Request` (doc ref).
- `routing`: `Registry.Probe` via `ProbeFunc`; `PathMissingError` via a local
  interface (`BaseURLHint`), no import.
- `cmd/kaiak/main.go`: `NewRegistry`, `Registry.Probe`, `Registry.Retain`,
  `ListsModels`.

Dependencies: `config`, `netfail`, `sse`; stdlib only.

Domain concepts encoded here, and where:
- **Backend type** → `kinds` (`provider.go:317`) + one file per type. Also named outside:
  `config/snapshot.go:60` (constants), `config/schema.go:94,99` (enum, keyed types),
  `server/api.go:275` (`providerName`, GenAI provider name per type).
- **Endpoint** → `provider.go:44` (`Endpoint`, `path`, `Format`); per-type `serves`
  lists in each module file; mirrored by `server/pipeline.go:21` (`endpoint`) and
  mapped by `server/upstream.go:564` (`providerEndpoint`).
- **API format (protocol)** → `provider.go:80`; per-format rules in `stream_end.go`,
  `wire.go:464` (`passthroughBody`), `wire.go:764` (`relayedErrorEvent`), `body.go:158`
  (`isUsageOnlyChunk`), `wire.go:58` (`openAICore`). Mirrored per format in
  `accounting` (`usageReader` ×3, `estimate.go` `inputScan`) and `server/inbound.go:79`.
- **Owned passthrough edits** → `wire.go:464` (model, params, `store`,
  `stream_options`), modules (`service_tier` on `openai`, `azure-openai`, `anthropic`);
  output-limit params built in `server/params.go:118`.
- **Deployment-failure 404s** (missing model, wrong path, missing endpoint) → per-module
  `missingModel`/`unknownPath` rules + core classification `wire.go:128`.
- **Error-event kinds** → `stream_end.go:133,218`; mapped by `server` in four places.

### `gateway/internal/sse` (165 lines)

- WHATWG SSE block reader returning raw bytes, data, data spans, event, id; size cap.
- Used by `provider/wire.go` (backend streams; uses `DataSpans`, `Event`, `ID`) and
  `control/stream.go` (config stream; uses `HasData`, `Event`, `Data`), plus
  `ErrTruncated`/`ErrTooLarge` in both.
- No dependencies. Clean: one job, two real consumers, nothing to change.

### `gateway/internal/clip` (27 lines)

- Bounds client-controlled strings for logs and error text. Used by `auth/auth.go`,
  `server/api.go`, `server/errors.go` (`String`). Clean.

### `gateway/internal/netfail` (62 lines)

- Names a transport failure's class in the gateway's own words (no remote text in
  logs). Used by `provider/wire.go`, `control/transport.go`, `control/stream.go`,
  `control/usage.go`, `otlplog/exporter.go`. Clean.

## Findings

Ranked by payoff vs cost. Settled decisions honoured: a type per server even if thin
(2026-09-30), no module embeds another, the core names no type (2026-09-29).

### F1 — Per-format stream rules are split over four places; gather them into one type per format

- **Kind** — cross-function.
- **Where** — `provider/stream_end.go:15` (`streamEnd`: `observe`, `complete`,
  `nestedModel`); `provider/wire.go:764-800` (`relayedErrorEvent`: which members carry
  a message, and the fallback payload chosen by
  `r.ending.(*responsesStreamEnd)` at `wire.go:785`); `provider/body.go:158`
  (`isUsageOnlyChunk`, OpenAI-only, called at `wire.go:671`);
  `provider/wire.go:470-476` (`passthroughBody`: `store:false` for Responses,
  `include_usage` for the OpenAI format); `provider/wire.go:58` (`openAICore`).
- **Now** — `streamEnd` began as "is the stream complete" and grew error-event
  detection (`d437d74`) and the nested model location (`bc70173`), so it is already
  the per-format stream reader in all but name. The rest of each format's stream rules
  stayed in the core: the error-event fallback is picked by a type assertion on the
  end reader, the message-carrying member paths of all formats are edited on every
  error event regardless of format, and the OpenAI usage-only chunk is recognized by
  a second, separate JSON decode (`isUsageOnlyChunk` unmarshals the chunk into a map
  right after `openAIStreamEnd.observe` unmarshalled it for `choices`).
- **How it got here** — Messages and Responses passthrough (`91e2c1e`, `7d23023`) added
  formats to a core built for OpenAI; each later fix put its per-format branch where
  the fix landed.
- **Proposed shape** — rename `streamEnd` to a per-format `streamFormat` (one value per
  `Format`, created by `newStreamFormat`) and let it own: `observe` returning
  `{errorEvent, usageOnly}` (the OpenAI implementation reads `usage` and `choices` in
  the decode it already does), `complete`, `nestedModel`, and `relayedError(payload)`
  (its own message paths and its own fallback). `relayedErrorEvent` keeps only the SSE
  framing; `isUsageOnlyChunk` goes. Optionally the format's request edits
  (`store:false`, `include_usage`) become `streamFormat`-adjacent `formatEdits(req)`
  so `passthroughBody` holds no format branch.
- **Payoff** — removes the type assertion and one JSON decode per hidden-usage OpenAI
  chunk; a change to one format's stream rules (or a fourth format) touches one type
  in one file instead of `stream_end.go` + `wire.go` + `body.go`. ≈20 lines removed.
- **Cost / risk** — small, provider-internal; tests go through the registry
  (`messages_test.go`, `responses_test.go`, `error_event_test.go`) and stay as they
  are. No contract change.
- **Confidence** — high.

### F2 — Every module's `Send` repeats the body-edit step and threads `stripUsage` back into the core

- **Kind** — cross-function.
- **Where** — `openai.go:42-51`, `azure_openai.go:42-51`, `vllm.go:43-52`,
  `llama_server.go:41-50`, `openai_compatible.go:43-52`, `anthropic.go:52-69`,
  `azure_anthropic.go:46-61`; `wire.go:38-41` (`wireCall.body`, `stripUsage`),
  `wire.go:464` (`passthroughBody` returning `(body, stripUsage, err)`),
  `wire.go:152`.
- **Now** — all seven modules run the identical
  `body, stripUsage, err := passthroughBody(req, extra...); if err != nil { return nil, editError(err) }`
  and then copy `body` and `stripUsage` into `wireCall`, which hands `stripUsage` to
  `newUpstreamResponse`. `stripUsage` is never a module's choice: it is computed from
  `req` alone (`wire.go:473`) and only the core consumes it. Same for the credential
  header: four modules build an identical bearer header (`openai.go:33`, `vllm.go:34`,
  `llama_server.go:32`, `openai_compatible.go:34`), two an identical `Api-Key` header
  (`azure_openai.go:32`, `azure_anthropic.go:32`).
- **How it got here** — `passthroughBody` was a core helper of the single provider;
  when modules split out (`6fbf8cb`, `330507e`) each copied the call. The backend-types
  plan deliberately chose "no new shared helpers" (`docs/plans/backend-types/OVERVIEW.md`,
  decision 6).
- **Proposed shape** — `wireCall` takes `edits []memberEdit` instead of `body` +
  `stripUsage`; `sendWire` runs `passthroughBody` (and `editError`) itself and keeps
  `stripUsage` internal. Modules that refuse first (`refusePriceOptions`) still do so
  before calling `sendWire`. Two credential helpers in the core,
  `bearer(credential) http.Header` and `apiKey(credential) http.Header`, used by the
  modules' `header()` (the module still chooses which; the core still names no type).
- **Payoff** — ≈5 lines × 7 modules and one 3-value return removed; the "stripUsage
  follows the edit" invariant lives in one function instead of across 8. A future
  owned edit that changes what the core must do with the response (as `stripUsage`
  did) no longer needs a new `wireCall` field plus 7 module edits.
- **Cost / risk** — small; mechanical; no test or contract change. Within the settled
  rule (modules keep their own `Send`, the core reads no type).
- **Confidence** — high.

### F3 — Per-type constants are re-supplied on every call, and the module scaffolding is copied seven times

- **Kind** — cross-function.
- **Where** — the seven module files (struct + constructor + `url` + `header` + `Send` +
  `probe`, ≈30 lines each); `wireCall.missingModel`, `unknownPath`, `core`
  (`wire.go:44-52`); `core` computed two ways: `openAICore(req.Endpoint)` in five
  modules, `req.Endpoint == Messages` in two (`anthropic.go:67`,
  `azure_anthropic.go:59`); the missing-model rule
  `missingModelNamedOrCoded("model_not_found")` built per request in three modules.
- **Now** — each module is a type whose fields are always the same three
  (`backend`, `client`, `credential`) and whose `Send` fills a 9-field `wireCall` where
  only `url`, `edits`, `missingModel`, `unknownPath` and `core` differ by type — and
  those three rules are constants of the type. `wireCall` *is* already a per-type
  description made of functions; it is rebuilt per request.
- **How it got here** — the 2026-09-29 refactor tried "one provider reading a per-type
  description" (`8d0cd2a`, `dialect.go`) and replaced it within the hour by modules
  (`6fbf8cb`) because the description held *flags* the core branched on. The
  2026-09-30 plan then cloned `openai_compatible.go` into three thin modules.
- **Proposed shape** — keep one file per type and keep the core free of type names
  and flags, but make a module a *value* of one core struct built in that file:
  `type wireModule struct { backend; client; header http.Header; prefix string;
  core []Endpoint; edits func(*Request) ([]memberEdit, error); missingModel, unknownPath
  func...; probe func(ctx) (...) }` with one `Send` and one models-list `probe` helper in
  the core. `newVLLM` becomes ~12 lines returning that value with vLLM's rules;
  `core []Endpoint` sits beside `serves` so a type's endpoint row (served, core) is
  in one place. The core calls functions, never branches on a type's flag — the
  objection to `dialect.go` does not apply.
- **Payoff** — ≈120–150 lines of scaffolding removed across 7 files; adding a type
  (the user's expected move: one module per server) goes from copying a 70-line file
  and editing 7 slots to filling one value; `core` written once per type.
- **Cost / risk** — medium: rewrites every module file and part of `wire.go`; tests go
  through `Registry` and survive. It touches the *spirit* of the settled 2026-09-29
  wording ("the core is not a provider") — needs the user's ruling; the spec's
  Providers paragraph would be re-dated. If declined, F2 alone captures most of the
  safe part.
- **Confidence** — medium: the line count is certain; whether the user values
  "each module reads as a full provider" over the saving is the open question.

### F4 — "Cannot tell which models a backend serves" is encoded twice

- **Kind** — cross-module.
- **Where** — `provider.go:310-325` (`backendKind.listsModels`), `provider.go:346`
  (`ListsModels`), `cmd/kaiak/main.go:371`, `routing/modelcheck.go:25,35,92`
  versus `azure_openai.go:67-72` and `azure_anthropic.go:81-83` (probes returning a
  `serves` that is always true).
- **Now** — two mechanisms for one fact. `azure-anthropic` has both: `listsModels:
  false` *and* a probe whose `serves` says yes to everything. `azure-openai` has only the
  second: its model check runs, can never warn about a model, and says nothing about
  that.
- **How it got here** — `azure-openai`'s always-true `serves` predates the Anthropic
  types; `azure-anthropic` (`d0d0165`) needed the model check to skip with an info
  line, so a flag and an exported function were added beside it.
- **Proposed shape** — `probe` returns `serves == nil` for "cannot tell" (both Azure
  modules); the model checker logs its info line when `serves` is nil (after the probe
  answered, so `azure-openai` keeps its wrong-path warning); the circuit treats nil as
  served. Drop `listsModels`, `ListsModels` and `NewModelChecker`'s second parameter.
- **Payoff** — one concept and one exported function removed; one place to say
  "unknowable" for a future type (Bedrock). `azure-openai` gains the honest info line.
- **Cost / risk** — small; touches `routing` (2 sites) and `main.go`; one spec sentence
  (Probe and model check) extends the info line to `azure-openai`.
- **Confidence** — high on the duplication; the `azure-openai` log change wants a nod.

### F5 — `wire.go` is a grab-bag; `sendWire` mixes 404 classification into the send

- **Kind** — in-function / cross-function.
- **Where** — `wire.go` (811 lines): send (`63-198`), models list + `PathMissingError`
  (`229-301`), 404 readers and missing-model rules (`303-435`), passthrough and
  service-tier edits (`456-500`), the upstream response (`508-811`). In `sendWire`, the
  release sequence `timer.Stop(); cancel(nil); return` is written four times
  (`107-109`, `121-124`, `143-147`, plus `fail`).
- **Now** — the file's header says the core "knows no backend type", yet it holds
  `anthropicModelMissing` (`wire.go:382`, Anthropic's rule, used by the two Anthropic
  modules) and `standardServiceTier` (`wire.go:490`, OpenAI's/Azure's rule).
- **How it got here** — `wire.go` is the 0.7.3 single provider (`openai.go`, 549 lines)
  renamed and grown; every per-concern helper landed in it.
- **Proposed shape** — split by domain: `send.go` (sendWire, readFirst, headers),
  `response.go` (upstreamResponse), `notfound.go` (404 readers, rules, `namesWord`),
  `probe.go` (fetchModelsList, listedModels, `PathMissingError`, hints); move
  `passthroughBody`/`standardServiceTier` to `body.go`; put rules shared by a family of
  types in a family file (`anthropic_rules.go`, `openai_rules.go`). Extract
  `deploymentFailure(resp, req, call) *Error` from `sendWire:128-149` so the early
  returns share one release.
- **Payoff** — no concept removed; locates each rule; `sendWire` shrinks by ~25 lines
  and has one release path. Do it together with F1–F3 or not at all.
- **Cost / risk** — small, mechanical.
- **Confidence** — medium (organizational payoff only).

### F6 — The endpoint concept exists twice, provider and server, joined by a mapping function

- **Kind** — cross-module.
- **Where** — `provider/provider.go:44-100` (`Endpoint`, `path`, `Format`);
  `server/pipeline.go:21-115` (`endpoint` with the same 7 body endpoints + 3 model
  endpoints; `path`, `name`, `operationName`, `bodyEndpoints`);
  `server/upstream.go:564-580` (`providerEndpoint`); `server/params.go:118`
  (`outputLimitKeys` by server endpoint).
- **Now** — adding an endpoint touches: provider const, `path`, `Format`, every module's
  `serves`; server const, `path`, `name`, `operationName`, `bodyEndpoints`,
  `providerEndpoint`, `outputLimitKeys`; plus accounting and inbound per format —
  ≈11 places in two packages for the endpoint alone. The upstream path is the client
  route minus `/v1/` for all seven.
- **Proposed shape** — the server's body endpoints *are* `provider.Endpoint`: the
  server's type wraps it (`endpoint{body provider.Endpoint}` or a separate small enum
  for the three model routes), `providerEndpoint` disappears, and per-endpoint facts
  the server owns (route, metric name, operation name, output-limit keys) sit in one
  table keyed by `provider.Endpoint`.
- **Payoff** — one enum and one mapping function removed; adding an endpoint drops
  from ≈11 places to ≈7 (one table row in server, provider const/path/format, the
  modules that serve it).
- **Cost / risk** — medium; mostly `server`; no contract change.
- **Confidence** — medium; the combining pass should weigh it against the server
  reviewer's view of `endpoint`.

### F7 — Two hand-written incremental JSON scanners read the same non-stream body

- **Kind** — cross-module.
- **Where** — `provider/model.go:30-190` (`modelRewriter`: depth, strings, escapes,
  watched keys, `closed()` for completeness) and `accounting/scan.go:19-240`
  (`memberScanner`: phases, strings, escapes, top-level keys, captures). Both run on
  every piece of every successful JSON body (`wire.go:686`, `accounting/meter.go:155`).
- **Now** — two byte-level lexers with their own key-capture limits (`maxKeyCapture` 32,
  `maxScanKey` 64), escape handling and repeated-key semantics (rewrite every one vs.
  keep the last).
- **Proposed shape** — one small top-level-member tokenizer (an internal package, e.g.
  `internal/jsonscan`) emitting key/value-span events per piece, driven by a rewriting
  consumer (provider) and a capturing one (accounting). Two real uses today.
- **Payoff** — ~120–150 lines and one set of lexing rules removed; JSON-escape and
  key-match rules fixed in one place.
- **Cost / risk** — medium-high: both are hot-path, subtle and heavily tested; the
  nested-member watch (`message`, `response`) must fit the shared tokenizer. Not urgent.
- **Confidence** — low-medium; worth a spike only if one of the two needs changing
  anyway.

### F8 — "SSE read error in the gateway's own words" is written by both consumers

- **Kind** — cross-module (small).
- **Where** — `provider/wire.go:708-713` (`readFailure`) and `control/stream.go:89-95`.
- **Now** — both pass `sse.ErrTruncated`/`ErrTooLarge` through and map anything else
  through `netfail.Class`.
- **Proposed shape** — `sse.Reader.Next` returns the transport's error already as
  `errors.New(netfail.Class(err))` (sse imports netfail); both call sites lose their
  switch. `readFailure` stays only for the non-stream body path.
- **Payoff** — ~6 lines, one rule in one place (the reader is where remote bytes
  enter).
- **Cost / risk** — trivial.
- **Confidence** — medium (marginal payoff; fine to skip).

`sse`, `clip`, `netfail`: otherwise clean — leave as they are.

## Cross-module hints

- Backend type named outside `provider`'s `kinds` despite the spec's "one place a type
  is named": `server/api.go:275` (`providerName`, GenAI value per type) and
  `config/schema.go:94,99` (type enum, keyed types). The config ones are contract;
  `providerName` could be a field of `backendKind` (`provider.ProviderName(t)`).
- `ErrorEventKind` is mapped to answers/outcomes/labels in four `server` places:
  `errors.go:291`, `metrics.go:84`, `upstream.go:119`, `upstream.go:553` — one table
  would do.
- Per-format logic outside provider, each with its own `switch ep.Format()`:
  `accounting/meter.go:97` (usage readers ×3), `accounting/estimate.go:54,205-209`,
  `server/inbound.go:79`, `server/errors.go` (Anthropic-shaped errors). "Inbound API
  format" is a seam in `docs/kaiak.md` (principle 5) but has no single plug: a fourth
  format touches ≈20 places in 4 packages.
- Each stream event is decoded 3–4 times: `streamEnd.observe` (provider),
  `isUsageOnlyChunk` (provider, OpenAI when hiding), the nested model rewrite (bytes),
  and the usage reader (`accounting/*_usage.go`). F1 removes one; merging provider's
  end reading with accounting's usage reading would cross the provider/accounting
  boundary — probably not worth it, noted for completeness.
- `routing/modelcheck.go:74` reads `PathMissingError` through an interface to avoid
  importing provider; `PathMissingError`'s exported fields (`Backend`, `URL`, `Hint`)
  are then unused outside and `BaseURLHint()` just returns `Hint`.

## Bugs noticed in passing

- None found. (`upstreamBodyReader.held()` at `body.go:250` exists only for
  `release_test.go`; harmless.)
