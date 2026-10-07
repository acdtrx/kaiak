# kaiak — independent structure review

Reviewed 2026-10-07. Source: this directory's plain copy, without git history.

The overall architecture still fits the product. I would keep the package graph, the single request pipeline, raw passthrough bodies, immutable resolved config, and the control plane's store contract. The best improvements are narrower: finish separating attempts from requests, share repeated provider mechanics, and consolidate endpoint descriptions. The hottest files contain some real structural debt, but their size and change frequency alone do not justify replacements.

This review covers every production package under `gateway/internal/`, `gateway/cmd/kaiak`, every subsystem folder under both control packages' `src/`, and their root entry points. Test scaffolding received a separate, lighter pass. Production modules were examined individually before tracing their relationships. The supplied heat map guided extra attention to main, server, limits, config, control delivery, ops metrics, and control-plane aggregation/storage. “How it got here” below is an inference from the current code and documented feature sequence, not a reconstruction of unavailable commits.

Read against `docs/kaiak.md`, `AGENTS.md`, `docs/CODING-RULES.md`, `docs/ARCHITECTURE.md`, relevant stack material, and the gateway, control-protocol and backend-verification contracts. No source changes were made. No dependency installation or full test suite was attempted. The one attempted repository check was:

```text
cd control && node scripts/check-boundaries.ts
Error [ERR_MODULE_NOT_FOUND]: Cannot find package 'typescript'
  imported from .../control/scripts/check-boundaries.ts
Node.js v26.9.0
exit 1
```

The import assessment below is source inspection, assisted by a static import inventory; it is not a successful run of the TypeScript parser-based boundary checker. Findings are structural recommendations, not tested patches. Line references identify this copy. Estimated savings are deliberately approximate; change-site counts refer to the named behavior, not all documentation and tests a feature requires.

## 1. Per-module review: gateway production

### `gateway/internal/accounting`

This package has a coherent job: estimate input, observe one attempt's response, normalize usage, price it, and fan the resulting record out to consumers. The format readers are justified: Messages has provisional output and disjoint cache counters, while OpenAI and Responses carry inclusive input totals. The bounded member scanner is also justified by passthrough and the absence of a full buffered-response requirement. Preserve those distinctions; there is one smaller shared policy worth extracting.

#### F01 / Normalize inclusive token reports in one place

- **Kind:** cross-function.
- **Where:** `gateway/internal/accounting/openai_usage.go:98`, `openAIUsage.parse`; `gateway/internal/accounting/responses_usage.go:38`, `responsesUsage.parse`; `gateway/internal/accounting/meter.go:15`, `tokenUnits`.
- **Now:** Both parsers independently clamp cached tokens to input, cache writes to input minus cached, reasoning to output, and subtract both cache classes from plain input. The equivalent arithmetic is at OpenAI lines 106–119 and Responses lines 46–55. Different JSON member names surround the same normalization rule.
- **How it got here:** Responses appears to have acquired its own reader by following the established OpenAI reader, including the already-added cache-write and reasoning rules. Separate readers were appropriate; copying the normalization policy was unnecessary.
- **Proposed shape:** Keep each format's decoding, missing-field decisions and event handling local. Pass the five decoded amounts through one purpose-named inclusive-token normalization helper. A small named input value can keep that call legible. Embeddings keeps its explicit input-only behavior; Messages keeps its different accounting semantics.
- **Payoff:** One clamping/subtraction policy instead of two. Changing precedence for overlapping cache details becomes **2 arithmetic implementations → 1**. Net line savings are modest, potentially only a handful after explicit field extraction; the value is removing a duplicated billing rule, not a generic parser framework.
- **Cost / risk:** Small, inside accounting. Exercise both formats' malformed, missing, negative and overlapping detail cases, plus embedding behavior. No protocol, spec or public library API change if outputs stay identical.
- **Confidence:** High. The arithmetic is visibly equivalent. A shared table of normalization cases applied to both existing readers would confirm preservation of their decoding differences.

### `gateway/internal/auth`

Authentication owns header precedence, digest lookup, expiry/disabled decisions, and an identity with resolved model access. It consumes the snapshot rather than rebuilding the group tree. The error type exposes safe key IDs rather than key material. **Clean — leave as is.** Splitting individual checks into services or an authentication strategy hierarchy would add concepts without a second authentication mechanism.

### `gateway/internal/clip`

A small UTF-8-aware bound for text included in diagnostics. The package has a specific shared purpose and no state. **Clean — leave as is.** Its size is not a reason to move it into an unrelated package.

### `gateway/internal/config`

The document, schema/semantic validation, resolved snapshot, loader and holder serve distinct phases. Document-to-snapshot conversion is not a redundant mirror: it resolves group ancestry and access, references, defaults, durations, and price representation once. The holder/apply path gives requests a stable snapshot while updates replace it. Retaining source-level types separately from operational types is appropriate despite the heat in `schema.go` and `snapshot.go`. **Clean — leave as is.** Do not collapse these into one mutable config object or generate a general configuration framework merely to reduce declarations.

### `gateway/internal/control`

The client legitimately contains several independent lifecycles: config/totals stream following, status coalescing, and ordered usage delivery. Shared transport and backoff are already factored. Seal-time validation outside the request-path lock, retained acknowledged-batch metadata, and the distinction between acknowledgement and totals coverage all have current purposes. The remaining simplification candidate is the two batch order identities, examined as **F07** across control, accounting and limits. Otherwise **leave the stream/status/usage separation in place**; combining all three into a generic delivery engine would obscure their different durability and timing rules.

### `gateway/internal/limits`

This is complicated because its contract is complicated: inherited scopes, reservations, rolling minute windows, UTC hour/month windows, per-gateway shares, in-flight settlement, and remote totals plus uncounted own usage. These are not interchangeable counters with different durations. The code's retention of removed scopes while requests still reference them and its atomic retirement/application under the limiter lock are meaningful. **No independent restructuring finding.** F07 could simplify the token by which settled own usage is retired, but it must preserve the existing atomicity. Do not replace the current windows with a callback-heavy universal limiter.

### `gateway/internal/logattr`

The two duration helpers standardize seconds at the precision each caller requires. They are deliberately tiny and shared. **Clean — leave as is.**

### `gateway/internal/metrics`

The registry/text encoder, usage series, delivery/export adapters and ops instrumentation belong together. Billing records and approximate metrics remain separate consumers, as the philosophy requires. Much of `ops.go` is the unavoidable declaration of metric names, buckets, labels and prepared zero series. The repeated reconstruction of routing's observed universe is a narrower issue: **F05**. Preserve cumulative-series retention and the distinctions between request, attempt, usage and delivery metrics; they are not duplicate counters for the same event.

### `gateway/internal/netfail`

This package translates potentially sensitive low-level network errors into a bounded vocabulary. Provider traffic, control-plane traffic and log export really share that purpose. **Clean — leave as is.** Do not replace it with raw error-message reuse.

### `gateway/internal/otlplog`

Settings, slog capture, OTLP encoding and bounded export have distinct responsibilities. Capturing a record's values for asynchronous delivery and then encoding the OTLP wire representation is a justified conversion boundary. Its queue/drop/retry/shutdown contract differs from billable usage delivery. **Clean — leave as is.** A common “reliable sender” spanning this package and control usage would conflate two different loss policies.

### `gateway/internal/provider`

The raw-body editor, wire transport, stream termination inspection and provider-specific error recognition form a good boundary. Only this layer opens model-backend connections. Unknown fields remain raw except where the gateway owns edits; keep that design. However, the OpenAI-family modules duplicate substantial preparation mechanics around the shared wire core.

#### F02 / Share OpenAI-family module mechanics, retain named backend policies

- **Kind:** cross-function.
- **Where:** `gateway/internal/provider/openai.go:13`, `vllm.go:13`, `llama_server.go:12`, `openai_compatible.go:14`, `azure_openai.go:13`: module structs, constructors, `url`, `header`, `Send`, `probe`; `gateway/internal/provider/provider.go:305`, `backendKind` and `kinds`.
- **Now:** These five modules repeat the same three stored values, constructor, passthrough-body/error/send sequence, and model-list request setup. Four use the same URL join, bearer header and listed-model probe. vLLM, llama-server and openai-compatible even share the same body-edit and missing-model selection; their unknown-path predicates and endpoint support differ. `sendWire` has already centralized the difficult transport work, but the wrapper layer still repeats it by type.
- **How it got here:** Giving each backend a module made additions local. Later endpoint, usage, tier and error features added the same call assembly to several existing modules. The current variation is mostly policy selected before one common operation.
- **Proposed shape:** Use one internal passthrough-module implementation for the five OpenAI-family cases, built from each backend's named policy: URL prefix, credential header, body-edit function, missing-model/path recognition and model-list interpretation. Reuse the existing `kinds` registry rather than introduce a second registry. Keep genuinely different rules as named functions near their backend declaration. Start with the four identical URL/header/probe cases; include Azure only where doing so reduces code. Anthropic price-option validation and Azure Anthropic's absent probe need not join this extraction.
- **Payoff:** Common OpenAI-family request assembly becomes **5 implementations → 1**; identical bearer/listed-model setup becomes **4 → 1**. Likely tens to roughly a hundred net lines removed, depending on how narrowly the extraction is made. A common send-preparation change no longer requires edits in five provider files. Adding a backend still requires a deliberate capability/policy entry and tests.
- **Cost / risk:** Medium, contained within provider plus tests. Preserve per-type credentials, tier behavior, path/model error classification, raw unknown fields, endpoint support, and model-list interpretation. Existing provider matrix tests and recorded responses are essential. No new dependency, wire change, spec change or public control API change. Reject the extraction if it becomes a list of mode booleans or a large type switch.
- **Confidence:** High for the four identical mechanics; medium for including every Azure detail in the same implementation. A minimal prototype's net diff would establish the actual saving.

### `gateway/internal/routing`

Slot ownership, admission queues, deployment circuits, backend probes, model checks and quota cooldowns are related concerns, with distinct triggers. In particular, circuit probing and config-time model checks are not redundant health checks: one controls recovery and the other diagnoses declared models. Backend load and deployment health correctly have different keys. **Keep the routing algorithm and these distinctions.** Its observation boundary is the issue in F05. Endpoint-specific negative knowledge currently lives outside it; the incidental retention problem in that state is B01, but I would not move all endpoint policy into routing just to fix that bug.

### `gateway/internal/schemacheck`

The schema walker implements the subset this zero-dependency gateway needs, while semantic checks remain with their owning contracts. That is a real architectural constraint, not gratuitous reinvention of a full JSON Schema library. **Clean — leave as is.** Keep the supported subset explicit and tested against shared fixtures; do not broaden it speculatively or replace it with handwritten validation scattered across consumers.

### `gateway/internal/server`

The package contains the client API, a single ordered pipeline, request admission/limits, retries, relay, errors and observability. These jobs belong in the server boundary; file splitting alone would not improve it. The main structural weakness is that retry attempts were introduced without fully moving attempt-local state out of the request. Endpoint metadata is another independently useful cleanup, described in F04. The per-key limiter, body budget, socket admission cap and retry budget constrain different resources and should remain separate.

#### F03 / Make an attempt own its complete result

- **Kind:** cross-function.
- **Where:** `gateway/internal/server/pipeline.go:131`, `request`, especially lines 179–188 and 201–224; `gateway/internal/server/upstream.go:36`, `attempt`; `upstream.go:71`, `sendAttempts`; `upstream.go:422`, `releaseAttempt`; `upstream.go:440`, `settleAttempt`; `upstream.go:491`, `classifyAttempt`.
- **Now:** An attempt owns a deployment, meter, slot, response and some outcome fields. The request separately holds the latest deployment/meter, provider error/status, backend error fields and relay result. Each attempt starts with `rq.deployment, rq.meter = at.deployment, at.meter` and a reset of request-level upstream fields. `releaseAttempt(at)` receives the attempt but classifies `rq`; settlement similarly combines `at.meter` with `rq.relayEnd`. Correctness depends on which attempt the request happens to represent at that moment.
- **How it got here:** The request object fits a single routed attempt. Retries added an attempt list and retained responses without completely transferring ownership of the original per-attempt fields. Later relay timing and outcome features continued to use the established request fields.
- **Proposed shape:** Store the provider result, backend error fields, meter, selected deployment and relay/timing result on `attempt`. Pass that attempt explicitly to send, relay, classification and release. Keep identity, snapshot, parsed input, reservations, aggregate queue wait and retry-budget state on the request. Request logging reads the final attempt plus aggregates. Use the slot's deployment directly where an additional stored copy is unnecessary. Preserve a distinct final client error, which can arise before any attempt or from refusing a retry.
- **Payoff:** Eliminate the two explicit latest-attempt mirrors and the reset protocol; classification no longer has an implicit “must still be current” precondition. Adding a new attempt observation goes into **one owner**, instead of choosing among `attempt`, `request`, their copying/reset code and consumers. This is chiefly a reduction in state concepts and temporal coupling, not a promise that `upstream.go` becomes dramatically shorter.
- **Cost / risk:** Medium to large within server; externally behavior-preserving. Important tests cover a retained error response when retry admission fails, client cancellation between attempts, first-event versus response timeouts, partial billing across retries, release exactly once, and request/attempt metrics. Preserve finisher order and the single pipeline. No protocol/public library API change; no need to alter settled contracts.
- **Confidence:** High. The duplicated assignments and `classifyAttempt(rq)` call directly expose the ownership problem. A refactor that leaves those mirrors in place would not address this finding.

### `gateway/internal/sse`

The reader exposes both raw blocks and parsed data/event metadata, including spans used for minimal edits. That dual representation is necessary for preserving wire bytes while observing protocol events. Control streams and provider streams share framing but not interpretation. **Clean — leave as is.** Do not merge their event-state machines merely because both use SSE.

### `gateway/cmd/kaiak`

The hottest file is mostly a legitimate composition root: environment settings, config modes, construction/wiring, listeners, reload, drain and final flush. Separate usage/log flush concerns are not a reason to build a generic lifecycle framework. The local adapters keep control messages out of the limiter and router; most are worth retaining. **F05 identifies one real responsibility to move out:** constructing the routing status universe. Otherwise **leave the composition root intact** unless a future concrete change establishes another shared responsibility; moving environment parsing to another file alone is not a structural finding.

## 2. Per-module review: `control/kaiak-control/src`

### `backend-verify/`

Verification validates input against the config schema, performs bounded GETs only, recognizes reported server data, and returns observations with provenance. The large single file follows a comprehensible workflow. Azure deployment semantics and the not-checkable Azure Anthropic result justify explicit branches. The reported-model shape, sources, and config metadata fragment are different products, not unnecessary mirrored DTOs. **Clean enough — leave as is.** `readVllmEntry` and `readAnthropicEntry` repeat a small context-length extraction, but that is lower value than F01/F02 and does not justify restructuring verification into a plugin framework.

### `calendar/`

Shared real-calendar checks support config and protocol validation. They have two current consumers and prevent date-shaped strings from being accepted as real dates. **Clean — leave as is.**

### `config/`

Types, schema validation, semantic rules, limit resolution and generic group-tree traversal are sensibly separated. The control-plane view resolves human/config concepts without importing gateway runtime types. Shared fixtures are the right way to keep the two languages in agreement. **Clean — leave as is.** Do not replace the generic group tree with level-specific organizational types or merge config validation with publication/storage policy.

### `config-publishing/`

This module combines validation, content hashing, conditional publication and ordered delivery of observed current configs. Publication CAS, serialized listener delivery, and ordering of overlapping reads address different races. Removing one merely because several ordering mechanisms exist would be misleading. **Clean — leave as is.** The public content hash is an identity, not an ordering sequence; imposing a monotonic revision in the gateway would contradict restore/current-config behavior.

### `control-plane/`

The core is a small composition facade over publishing, usage and gateway membership, with one store subscription and adapter-neutral request checks. That is appropriate. Its one conspicuous construction-order dependency is the gateway sweep callback referencing `usage` before usage is constructed. This is not an initialization bug under the current lifecycle, but it reveals misplaced maintenance ownership. **F06** describes the improvement.

### `fastify/`

HTTP checking/routing, gateway SSE delivery and shared totals-read coalescing are separated. A shared totals snapshot and each stream's last-sent windows are different state: one avoids repeated reads, the other defines that stream's delta. Initial config/totals ordering and bounded pending output are genuine concerns. **Clean — leave as is.** The SSE client helper is test scaffolding considered separately below. Do not combine the gateway feed with the sample browser feed: their payloads, sequencing and backpressure contracts differ.

### `gateways/`

Status intake, conflict detection, live membership and conditional expiry belong here. Cursor retention and usage-window housekeeping do not: neither depends on whether a gateway remains in the live set. **F06** separates those responsibilities without adding another periodic reconciler. Keep the conditional writes against observed gateway revisions; they are necessary when another core receives a status during a sweep.

### `keys/`

Key creation, ID validation and hashing form a small reusable public capability. The sample's presentation and CLI validation remain outside it. **Clean — leave as is.**

### `messages/`

Wire types and message-specific semantic checks belong at this boundary. Batch record identity, unique epochs in totals, timestamps and schema validation cannot all be replaced by TypeScript static types. Config types reused in messages avoid a separate redefinition where their meanings really coincide. **Clean — leave as is.** Keep wire string money and stored bigint amounts distinct.

### `protocol/`

Version/header/auth checking and protocol error shape are isolated from Fastify. This is a reusable boundary with a clear contract. **Clean — leave as is.** The facade's request-check wrapper supplies the configured token; it is not an unearned abstraction.

### `schemas/`

Schema loading, compiled validators and definition-level checks have real consumers in config, messages, keys and verification. The package's schema copies support distribution; the repository already checks them against the canonical protocol copies. **Clean — leave as is.** Do not make the published package reach outside itself into the repository at runtime.

### `storage/`

The public store interface specifies atomic counting, consistent totals/cursor snapshots, conditional config/gateway writes and change notifications. The in-memory implementation is only one implementation in the repository, but external stores are an explicit product seam with an executable contract. **Clean — leave as is.** Removing the interface because it currently has one shipped implementation would remove the replica architecture. Likewise, window additions, stored totals and protocol totals differ in role and representation, even when several fields line up.

### `usage/`

Intake validation, batch de-duplication, aggregation and windows are coherent. The core correctly trusts gateway-priced usage instead of reconstructing provider billing. Instance-local serialization limits local contention while store CAS protects cross-core correctness; these are distinguishable purposes, so I do not recommend deleting the queue on source shape alone. **No independent finding.** F06 moves usage retention policy next to this owner. The small repeated window-key encoding in aggregation and memory storage could be shared during related work, but it does not justify a standalone larger cleanup.

### Root `index.ts`

The root declares the intended package surface; a separate store-contract entry keeps `node:test` out of normal applications. The broad type exports are deliberate API surface, not an internal dependency cycle. **Clean — leave as is.** They do mean F06 must account for exported gateway option/result types even if the main facade's runtime API stays unchanged.

### `store-contract/`

Reviewed with test scaffolding in section 4, rather than as production business logic.

## 3. Per-module review: `control/sample/src`

### `app/`

Composition of a memory store, core, adapter, config-file watcher and status page belongs here. Optional additional protocol cores intentionally share the store to exercise replica behavior. Logging adapters are sample concerns. **Clean — leave as is.** Do not promote this sample's process layout into the reusable library.

### `config-file/`

File reading, publication, last-run status, directory watching and debounce form one input adapter. Watching the parent directory supports replaced/projected files; serializing reloads protects publication order. Local unchanged-content suppression is useful for noisy filesystem events in this single file publisher. **Clean — leave as is.** Its remembered text is not a general control-plane authority: should the sample acquire a second config-writing source, that assumption would need revisiting. That feature does not exist here, so this is not a current finding.

### `keygen/`

CLI option parsing and human-readable output wrap the library's key creator. No key generation policy is duplicated. **Clean — leave as is.**

### `logging/`

The module selects JSON or human-oriented Fastify/Pino settings from a simple sample setting. **Clean — leave as is.** Keeping logger construction out of the reusable control core is appropriate.

### `page/`

Markup safety, formatting, document/CSP construction, section rendering and event delivery are separated. `sections.ts` grows with displayed domain data but is still a presentation module; the group tree uses the library resolver rather than reimplementing inheritance. The server and browser relative-time formatters are a small duplication, but removing it would require a browser code-sharing mechanism disproportionate to the code saved. **Clean enough — leave as is.** No frontend framework or general component system is warranted. Keep browser feed behavior distinct from control-protocol delivery.

### `settings/`

Environment validation and listen/replica-port parsing have one clear consumer and produce a checked settings value. **Clean — leave as is.** Do not generalize these few parsers into a settings framework.

### `verify/`

The CLI validates its own flags/env-name syntax, invokes the library verifier, and explains which metadata still needs an operator decision. This is a useful split between reusable backend observation and sample UX. **Clean — leave as is.** The library still validates the actual API options because it has callers other than this CLI.

### Root entry points

`index.ts` exposes reusable sample construction for tests; `main.ts`, `keygen-cli.ts` and `verify-cli.ts` provide process/stdio behavior. They are thin and their separate exits match distinct commands. **Clean — leave as is.**

## 4. Lighter pass: test scaffolding and shared helpers

### `gateway/internal/fakebackend`

The fake models request capture, scripted faults, pacing and protocol-shaped responses. Its shared `streamWriter` already removes duplicated pacing/interruption mechanics while separate Messages/Responses emitters retain independent wire expectations. The many `Reply` options are test fault controls, not product feature flags. Recorded backend captures supplement the synthetic answers. **Leave as is.** Importing production provider classifiers or accounting normalizers into this fake would reduce code but weaken test independence.

### `gateway/internal/fakebackend/cmd/fakebackend`

The standalone fake selects a handful of useful profiles and credential layouts, starts the same fake, and reports its URL. This is an appropriate reuse boundary for e2e/manual tooling. **Clean — leave as is.**

### `gateway/internal/fakecontrol`

The raw-JSON fake intentionally imports no gateway production packages and scripts acknowledgements, stream events and faults. It repeats only the protocol behavior needed to exercise the client, while totals are explicitly scripted rather than a second full aggregator. Its large file is still one fake with a coherent public control surface. **Leave as is.** The real sample/cross-half tests are necessary precisely because this fake is not a complete control implementation; do not make it one.

### `gateway/e2e`

The harness builds a real gateway process and uses log events, response bodies, metrics and fake arrival channels for observation. Shared process/HTTP/log helpers belong in the harness; sample-process and totals-watch helpers serve the cross-half tests. Replica, startup/stateless, protocol passthrough, retry and zero-series cases provide useful protection for the proposed refactors. **Leave the scenario-oriented layout as is.** A generic step-script DSL would hide the ordering that these tests need to make visible.

### Control `store-contract/`

The reusable store contract tests are particularly valuable. Separate handles, notification waits, reconnect behavior, torn snapshots and deliberately broken stores exercise the external seam rather than only the memory implementation. The negative-control wrappers remove confidence that would otherwise be based merely on a test passing against its own assumptions. **Clean — leave as is.** Keep these tests separate from the library's normal entry point.

### Shared fixtures and local helpers

The two halves consume canonical config/message fixtures; package schema-copy checks protect distribution. Within control tests, the Fastify SSE reader centralizes stream mechanics while scenario constructors stay near their tests. Go config/message fixture readers and e2e process helpers have different assertion and runtime roles. **No actionable scaffolding finding from this lighter pass.** A few repeated fixture constructors are cheaper than a shared mutable test-world object. This was not an exhaustive audit of every test assertion.

## 5. Cross-module structure

### Import graph and boundary assessment

The observed Go graph has the expected direction: server composes routing, providers, limits and accounting; control depends on accounting/config but not server or limits; main supplies the adapters. Routing does not import provider or perform backend I/O. Accounting imports provider for endpoint/event types; that is a small genuine dependency, not currently a reason to invent a new common-domain package. Metrics consumes accounting/config/routing observations and does not become the billing source. Fakes are independent leaves outside production wiring.

The library's observed subsystem edges are:

```text
control-plane -> config-publishing, gateways, protocol, storage, usage
fastify       -> control-plane, config-publishing, messages, protocol
publishing    -> config, schemas, storage
gateways      -> messages, storage
usage         -> messages, storage
storage       -> messages
messages      -> calendar, config, schemas
config        -> calendar, schemas
backend-verify-> config, schemas
keys/protocol -> schemas
```

Here `publishing` abbreviates `config-publishing`; root re-exports and the separate store-contract entry are omitted. The sample consumes the library's public entry and has no reverse dependency. No production cycle was found in the inspected graph. The more useful finding is a runtime responsibility loop that an acyclic import graph cannot show: gateways receives a callback to usage housekeeping while usage receives gateways' live-count function (F06).

### F04 / Declare endpoint metadata once per owning layer

- **Kind:** cross-module.
- **Where:** `gateway/internal/server/pipeline.go:21`, endpoint enum and its `path`, `name`, `operationName`, `bodyEndpoints`, `counts`; `gateway/internal/server/upstream.go:564`, `providerEndpoint`; `gateway/internal/provider/provider.go:44`, `Endpoint`, `path`, `Format`; `gateway/internal/server/errors.go:95`, `errorShapeOf`; `gateway/internal/server/params.go:119`, `outputLimitKeys`.
- **Now:** The same seven forwarded endpoints are represented by two enums, two path descriptions and an explicit mapping switch. Server labels, body-route registration and operation names live in additional lists/switches. This is metadata distributed by the functions that consume it rather than grouped by the endpoint it describes.
- **How it got here:** The original OpenAI endpoints acquired separate extensions for Messages, Responses, token-counting, metrics and GenAI log attributes. Each extension naturally appended a branch to its current consumer.
- **Proposed shape:** Keep provider's semantic endpoint identity and one provider descriptor for suffix/format. Let a server descriptor contain its provider identity, client route/label, operation name and counting/body properties. Derive forwarded client paths from the common suffix where appropriate. Keep local model routes in server. Iterate descriptors for registration and zero-series labels. Use format metadata for existing format-level decisions, but retain purpose-specific parsers and validation; this is a small closed table, not endpoint plugins or a universal request representation.
- **Payoff:** For another generating Messages-family body endpoint with a named GenAI operation, the current metadata path involves at least **9 sites**: server enum, path, metric name, operation name, body list; provider enum, path, format; server-to-provider mapping. An OpenAI-format addition can inherit the format default and avoid that particular edit. A descriptor design reduces the listed metadata work to roughly **4 sites**: two identities and two rows, or fewer if existing provider identities can be reused directly. Parser/output-limit/error-shape/capability behavior and tests still need explicit changes. Roughly dozens of switch lines and the mapping function can disappear.
- **Cost / risk:** Medium across server/provider, with accounting's endpoint consumers checked. Preserve unknown-route error shape, local model endpoints, unbilled token-counting behavior, metric labels and pipeline registration. No wire schema/public library API change. Client API and observability contracts must remain identical.
- **Confidence:** High. The enumerations and switches are concrete. The precise row layout should be judged on an endpoint-by-endpoint diff; avoid putting functions into the table merely to remove every switch.

### F05 / Routing should supply the observed backend/model/deployment universe

- **Kind:** cross-module.
- **Where:** `gateway/cmd/kaiak/main.go:753`, `servingStatus`; `gateway/internal/metrics/ops.go:218`, backend/queue/circuit/cooldown gauge closures; `gateway/internal/routing/routing.go:630`, `InFlightByBackend`, plus `QueuedByModel` and `CoolingDown`; `gateway/internal/routing/circuit.go`, `Circuits`.
- **Now:** Main builds configured backends, models and distinct deployments, fills closed circuits, then merges live counts for removed-but-still-active identities. Metrics separately zero-fills configured backend/model counts and independently enumerates deployment identities for both circuit and cooldown gauges. Consumers reconstruct which routing entities exist before formatting their output.
- **How it got here:** Control status, prepared zero metrics, shared deployment circuits and cooldown observability were added incrementally to separate consumers. Sparse router getters were sufficient initially, so each consumer learned to join them with config.
- **Proposed shape:** Provide one routing-owned observation operation taking the relevant config snapshot and collecting live routing values together. It should explicitly represent configured entities plus still-active removed entities, configured backend caps versus effective shared caps, and deployment circuit/cooldown state. Main formats that observation as control status; metrics formats it as gauges. Do not expose control wire types from routing, add a second mutable config store, or require a generic metric snapshot framework.
- **Payoff:** The configured/live-union policy moves from **2 consumer modules → 1 owner**, and the repeated distinct-deployment walk for gauges becomes one observation construction. Future routing status fields are read once at their owner instead of adding more sparse getters and consumer joins. The wire and metrics adapters remain because their outputs differ. Expect tens of net lines removed, not elimination of the whole status builder.
- **Cost / risk:** Medium across routing, metrics and main. Preserve intentionally different outputs: status's configured cap versus the metric's enforced share; cumulative series retention versus current routing gauges; old in-flight backend identities after reload. Check status, zero-series, circuit, queue and reload tests. No protocol shape change is required. Coherent reads are a useful consequence, not a claim that today's separate gauge reads violate a contract.
- **Confidence:** High on repeated ownership; medium on exact net size until the observation type is designed. Two real consumers justify the boundary.

### F06 / Compose housekeeping in the core, not inside gateway membership

- **Kind:** cross-module.
- **Where:** `control/kaiak-control/src/gateways/index.ts:88`, `GatewaysOptions`; `gateways/index.ts:195`, `sweep`, especially lines 217–218; `control/kaiak-control/src/control-plane/index.ts:123`, `createGateways` wiring; `control-plane/index.ts:135`, `createUsage`.
- **Now:** The gateways module expires gateway records, drops usage batch cursors and invokes `dropPastWindows`. Its options include both usage cursor retention and a callback into usage. The core must construct that callback before the referenced `usage` object exists. Meanwhile usage legitimately asks gateways for the current live count. The import graph is acyclic, but the runtime ownership of housekeeping is not cleanly separated.
- **How it got here:** Gateway silence originally supplied the periodic sweep. Cursor retention and old-window cleanup appear to have been attached to the existing timer rather than composed beside it as independent store-maintenance concerns.
- **Proposed shape:** Gateways exposes its conditional membership-expiry operation. Usage owns cursor/window pruning. The core composes both into the existing externally invocable sweep and owns its one timer, trigger/result reporting and error handling. No additional polling mechanism or standalone maintenance subsystem is needed. Preserve the existing facade methods so callers can still invoke or schedule the same sweep.
- **Payoff:** Remove `dropPastWindows` from gateway options and the deferred cross-reference at construction. Gateway membership no longer owns usage retention policy. Changing cursor/window housekeeping becomes **usage + core wiring**, without also editing gateway membership. The main benefit is correct responsibility and fewer cross-module callbacks; moving the timer by itself saves few lines.
- **Cost / risk:** Small to medium across the three modules and sweep/lifecycle tests. Preserve one reported run, failure behavior, ordering, concurrent-core conditional writes and timer start/stop semantics. No protocol/schema change. `GatewaysOptions`, `Gateways` and `ExpirySweepRun` types are re-exported from the public root: internal reshaping must explicitly decide which public declarations remain at the facade and which change. This is not automatically a private-only type refactor.
- **Confidence:** High. Usage cursor retention is explicitly independent of remembered gateway membership in the current option contract. The existing callback makes the misplaced responsibility concrete.

### F07 / Consider one process-local batch order instead of generation-to-sequence translation

- **Kind:** cross-module.
- **Where:** `gateway/internal/control/usage.go:109`, `usageSender.generation`; `usage.go:183`, `Record`; `usage.go:305`, `countedGeneration`; `usage.go:361`, `ackedBatch`; `gateway/internal/control/queue.go:33`, `queuedBatch`, and `queue.go:65`, `queueSealed`; `gateway/internal/accounting/accounting.go:45`, `UsageRecord.Generation`; `gateway/internal/limits/shared.go:169`, `retireCountedLocked`.
- **Now:** Records enter a filling batch and receive a process-local generation. Later, after validation, a nonempty sealed batch receives a separate wire sequence. Queued and acknowledged batches retain both. Totals coverage is translated from epoch/sequence into generation by scanning those structures. There is now only one usage epoch during a gateway process, but there remain two orders and a translation between them.
- **How it got here:** The code separates settlement-time attribution from later delivery identity, and the documented feature sequence includes the move to a completely stateless process. This makes the remaining translation worth reconsidering. There is no git evidence here proving that it is simply obsolete disk-queue code. Its current concrete justification is that an all-invalid batch consumes a generation but no wire sequence.
- **Proposed shape:** Evaluate assigning the eventual sequence to the filling batch under the same lock that accepts records, and use that number as the local settlement token. A batch discarded by validation consumes a number and creates a gap, just as dropping queued batches can already do. Match incoming totals only to the active epoch and valid issued range. Retain acknowledged timestamps and the oldest-uncovered acknowledgement clock: they serve outage policy independently of identity translation. Preserve atomic record attribution and totals retirement.
- **Payoff:** **2 monotonic batch identities → 1**, no separate sequence-to-generation mapping, fewer fields on sealed/queued/acknowledged values, and a simpler totals coverage operation. The acknowledged history cannot all be deleted: timestamps, bounded retention and lag policy still matter. This potentially simplifies future delivery/drop changes across accounting, control and limits more than it saves raw lines.
- **Cost / risk:** Medium to large and accounting-sensitive; rank below the behavior-preserving changes. It changes documented behavior: `docs/specs/GATEWAY.md:2107` says an empty-after-validation batch takes no sequence, and `docs/specs/CONTROL-PROTOCOL.md:278` describes sequence progression. Control intake already accepts/logs gaps, but both contracts and gap observability must be reconciled deliberately; the first transmitted batch may also follow discarded numbers. JSON shapes need not change, but whether the semantic change warrants a protocol-version update must be settled for both halves together. Test totals-before-ack, lost ack/resend, forgotten acknowledged entries, memory drops, all-invalid batches, late settlement after totals, retained old-epoch cursors and process restart. Keep checks/encoding off the request path.
- **Confidence:** Medium. The opportunity is real, but the documented no-sequence-on-empty behavior may be worth retaining. A focused state-transition comparison including every drop/ack/totals interleaving would raise confidence. Do not undertake it just to rename `Generation` to `Sequence` or leave two counters behind a new wrapper.

### Concepts that cross boundaries: counted change sites and justified mirrors

The main repeated concepts are not all debt. These counts exclude tests, docs and distribution copies unless stated, and are illustrative change paths rather than a claim that every possible feature touches the same set.

| Named change | Current sites / boundaries | Assessment |
| --- | --- | --- |
| Add a generating Messages-family endpoint | At least 9 metadata sites enumerated in F04, plus real parser/limit/error/capability behavior | Accidental metadata spread; consolidate descriptions, retain behavior-specific code. |
| Change OpenAI-family common send preparation | 5 module `Send` wrappers around one wire core | Real repetition; F02 has a confined extraction. |
| Add a backend kind | At least 7 declaration/policy sites: canonical config schema, Go backend constants, Go schema backend enum, TS backend list, provider registry, provider policy/module, verifier behavior; a credential-required kind also affects schema conditions | Some lockstep work is the cross-language contract. F02 reduces wrapper work, not the need to explicitly declare support. Existing type-enum and provider/verifier tests matter. Do not build cross-language code generation solely for seven kinds. |
| Add a billable usage unit | At least 8 areas: config schema prices, usage-record schema, Go unit/schema declarations, accounting production/normalization, accounting price/total semantics, TS unit declarations, control aggregation semantics, sample usage display | The unit's meaning must be decided at each real boundary. Generic metrics already consume units; avoid making every unit a plugin with pricing/limit callbacks. F01 only shares an arithmetic rule already identical today. |
| Observe another routing state | Router data/getter, main status reconstruction, metrics reconstruction; protocol/sample only if exposed remotely | F05 centralizes collection/universe construction; each output contract still deserves an explicit adapter. |
| Add a protocol message | Canonical schema/fixtures, Go decoder/types and owning sender/consumer, TS types/validator and owning route/core | Intentional independent implementations of one versioned contract. Keep fixture parity and cross-half tests rather than hiding the boundary. |

Specific mirrors to preserve:

- **Raw config versus resolved snapshot:** external shape versus request-time references/defaults/derived state. They are not field-for-field duplicates in purpose.
- **Go versus TypeScript config/message validation:** separate runtimes enforcing the same contract. Schema fixtures and semantic cases are the shared artifact; neither side should trust the other to have validated correctly.
- **Go limits versus control aggregation:** a reservation/enforcement view versus authoritative accumulated totals. Share agreements and fixtures, not their algorithms.
- **`control.Totals` versus `limits.Totals`:** the former carries protocol metadata; the latter carries limiter input, including complete/delta interpretation. The short main adapter is preferable to coupling limits to the control client.
- **Usage units versus priced units:** reasoning is recorded as a share of output but not priced twice. Replacing every explicit unit decision with an iteration over “all units” would erase that distinction.
- **Gateway probing versus `verifyBackend`:** both must agree on URL/header/model-list interpretation, but runtime recovery and operator metadata discovery are different operations. They cannot literally share Go/TypeScript code, and verification must remain explicitly invoked. Keep their existing behavior matrices, fixtures and type-coverage tests aligned.
- **Acknowledgement versus counted totals:** acknowledgement releases delivery payloads; totals coverage retires own usage from limit accounting. These are two events with different meanings, not two stacked safety nets. F07 does not merge them.

No proposed change requires third-party Go dependencies, backend I/O outside providers, a second request path, control-plane I/O on requests, disk state, or parsing/rebuilding unknown passthrough fields.

## 6. Ranked top list: payoff versus cost

| Rank | Finding | Payoff | Cost / principal risk | Recommendation |
| --- | --- | --- | --- | --- |
| 1 | **F03 — Complete attempt ownership** | Removes mirrored state and implicit current-attempt dependence in the hottest request code | Medium–large; retry/relay/settlement ordering | Highest long-term payoff. A confined server refactor with existing scenario protection. |
| 2 | **F02 — Shared provider mechanics** | Removes 4–5 copies of common preparation/probing; useful whenever provider behavior evolves | Medium; preserve backend-specific policy | Strong cleanup. Start with the four identical mechanics; measure net deletion before widening. |
| 3 | **F04 — Endpoint descriptions** | Roughly 9 metadata edit sites become 4; removes a redundant mapping | Medium; route/error/metric behavior | Strong cleanup ahead of another endpoint addition, without inventing plugins. |
| 4 | **F05 — Routing observations** | One owner for configured/live state joins, used by status and metrics | Medium; zero/current/historical entities and cap meanings | Worth doing when touching routing observability; preserve the output adapters. |
| 5 | **F06 — Core-owned housekeeping composition** | Removes a construction-order callback and places retention with usage | Small–medium; exported types and sweep lifecycle | Clear responsibility fix, limited raw line savings. |
| 6 | **F01 — Inclusive usage normalization** | One shared billing normalization rule rather than two | Small; malformed/missing field differences | Good small change independently, or alongside accounting work. |
| 7 | **F07 — One batch order token** | Removes a cross-package identity translation | Medium–large; contract and accounting interleavings | Investigate only with an explicit semantic decision. Do not bundle into a cosmetic cleanup. |

This is not a recommendation for one large rewrite. F01, F02 and F06 can be independent. F03 and F04 touch overlapping server code and are easier to review sequentially. F05 should keep its scope to observation. F07 has the lowest confidence-to-risk ratio and should not hold up the clearer improvements. B01 below is a separate small correctness fix, not justification for a routing redesign.

## 7. Bugs noticed in passing

### B01 / Missing-endpoint memory retains removed backend identities indefinitely

- **Where:** `gateway/internal/server/endpointmemory.go:33`, `remember`; `endpointmemory.go:45`, `exclude`, specifically lines 49–52; `gateway/internal/server/upstream.go:76` and `upstream.go:229`, its call sites.
- **Mechanism:** `remember` inserts `(backend ID, endpoint)`. The only deletion is the lazy expiry check inside `exclude`, and that check visits only deployments in the requested model for that endpoint. If config removes a backend after such a failure, future requests cannot visit its key. Its TTL can expire while the map entry remains forever. No apply-time pruning or other deletion path appears in the inspected source.
- **Impact:** Low-severity retention under long-running config churn: repeated missing-endpoint failures on newly named backends accumulate obsolete keys, despite a short behavioral TTL. A single static configuration keeps this small; no claim of an immediate outage or disk-state violation.
- **Evidence / confidence:** High from the insertion/deletion paths, not a runtime reproduction. A focused reproducer would remember N different backend IDs, remove them from the model, advance time beyond the TTL, call `exclude` for the remaining model, and inspect that N old entries remain. No test or source edit was made for this review.
- **Fix direction:** Give this existing cache a bounded retirement path, preferably pruning removed identities when config is applied, with explicit behavior for in-flight requests using an older snapshot. Do not add a second polling loop solely for cleanup. A targeted test should demonstrate reclamation after removal and expiry.

No other incidental behavior is asserted as a bug. The review was not a bug or security audit, and “clean” above means no structural change met the requested payoff bar, not proof of defect freedom.
