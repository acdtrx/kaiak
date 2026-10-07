# Structure review — 2026-10-07

**Question.** Many plans landed in sequence: group tree, tiered pricing, backend types,
cache-write units, OTel log export, Messages/Responses passthrough, control-plane replicas
and the stateless gateway. Each was built on top of the shape the previous one left. So,
knowing everything the code does today, where would a redesign from the current state be
simpler, smaller or more general? This is not a bug or security audit; those ran
separately.

**Method.**

1. **Heat map.** Non-test commits per file, counted over the full history (`private` +
   `main`). The hottest files: `cmd/kaiak/main.go` 48, `metrics/ops.go` 33,
   `limits/limits.go` 33, `server/upstream.go` 31, `server/pipeline.go` 25,
   `control/client.go` 24.
2. **Bottom-up module review.** Nine reviewers each took one group of modules, using the
   bar in [`CRITERIA.md`](CRITERIA.md). Their reports are in [`modules/`](modules/) and
   hold the full findings: file:line, current shape, history, proposed shape, payoff and
   cost.
3. **Relations pass.** The module summaries and cross-module hints were combined with the
   import graphs of both halves.
4. **Spot verification** of every claim the ranking rests on and every bug below.

5. **Independent review.** A separate reviewer (Codex) read a plain copy with no git
   history, against the same criteria:
   [`STRUCTURE-independent.md`](STRUCTURE-independent.md), findings `F01`–`F07`,
   `B01`. Where it agrees, disagrees or adds something is merged below and summarized
   in [Independent review](#independent-review).

Finding references read `<module report> <ID>`, e.g. `server S1` →
[`modules/gateway-server.md`](modules/gateway-server.md), finding S1.

## Verdict

No module needs a rewrite. The subsystem cuts are right in both halves: the Go
`internal/` graph and the `kaiak-control` subsystem graph are both acyclic and match the
concepts. The leaf packages are clean (`sse`, `clip`, `netfail`, `logattr`,
`otlplog`, `schemacheck`, `calendar`, `schemas`, `keys`, `protocol`, `store-contract`,
most of the sample).

Incremental change has left four kinds of mark:

1. **Vocabulary spread.** Some domain concepts are restated wherever they are used, so
   each feature that adds a value edits 7–10 places in lockstep. The concepts are limit
   types, usage units, attempt outcomes, endpoints/API formats and backend types. This
   is the main finding.
2. **The control ↔ limits seam glued in `main.go`.** Each outage clock, totals field
   and boot step was added as glue in the file with the most changes. One copy of that
   glue in the tests has already drifted.
3. **Leftovers of removed features.** The disk spool, the persisted limits, per-model
   limits, "start not ready", the fan-out with two sinks and the totals API shaped like
   a gateway each left fields, branches or API behind. All are cheap to remove.
4. **One job split by arrival order.** Per-attempt state sits partly on the request.
   The expiry sweep lives in `gateways` but runs usage work. Per-format stream rules are
   spread over three provider files.

## Cross-module themes

### T1 — Limit types: one table instead of ~10 restatements per half

- **Gateway.** A limit type's window (minute/hour/month), its measure
  (requests/tokens/cost) and whether the control plane counts it are restated in
  `config/schema.go` (two lists), `config/semantic.go` (the per-minute set and a literal
  `2`), `limits.shape` + `countedTypes` + 4 `kind != SlidingMinute` checks,
  `metrics/ops.go` `limitTypes`, `control/schema.go` (the totals enum + a window-start
  switch) and `server/limits.go` `tokenWindow`.
  - Sources: config F2, routing-limits F9, observability F3.
  - **Latent:** `limits.shape` falls through to "USD per month" for any type it does
    not name (verified, `limits/limits.go:55-65`).
- **Control.** The store interface passes `CurrentWindows {hourStart, monthStart}`, so
  the memory store, every database store and the GUIDE's SQL hard-code
  tokens_per_hour→hour and usd_per_month→month (control-core F3).
  - The sample page recomputes window starts and the counted-type rule, and reads
    spend through `totals("")` with a fake instance (control-core F2, edges F3,
    verified).
  - Window identity `JSON.stringify([group ?? null, type])` is written 5 times
    (control-core F7).
- **Proposed.**
  - Gateway: one `LimitType → {Window, Measure, Counted}` table in `config`, which
    everything reads.
  - Control: window starts as `Record<TotalsLimitType, number>`, one exported window
    identity, and hosts read `readTotals()`. `totals(instance)` and `countedThrough`
    leave the public API.
- **Future change it serves:** the backlog's *Fleet-wide per-minute token limits* (a
  per-minute type becomes control-plane counted, which today means 5 kind checks plus
  control's list), or any new type.
- **Cost:** small–medium. No protocol change. One public `kaiak-control` change (store
  window starts; `totals` removed). Feature-building mode allows both.

### T2 — Usage units: named sets instead of inline lists

- Four unit sets are written out as literal lists or sums: all token units, priced,
  input, and counted toward limits.
  - Gateway: 7 places, in `accounting.tokenUnits` (5 positional `int64`s, so swapping
    cached/cache-write compiles), `inputSize`, `Cost`, `limits.amountOf`, the log line,
    `config/schema.go` and `control/schema.go`.
  - Control: `usage/aggregate.ts` repeats the counted rule, and the sample page lists
    the unit columns.
  - Sources: observability F1, cross-module hints in config, control-main, control-core
    and edges.
- The **counted-tokens rule** (in + cache_write + out; cost in nano-USD) is written in
  both halves (`limits.go:629`, `aggregate.ts:46`) with no shared fixture. It changed by
  hand on both sides on 2026-10-05.
- **Proposed.**
  - Named sets next to `config.Unit`, with the price fallback as data; `Cost` becomes a
    loop with the same summation order.
  - A `protocol/fixtures/usage/` case set (record → counted amount per limit type)
    checked by both halves, like `config/resolved`.
- **Future change:** the per-lifetime cache-write units the spec defers "until Bedrock"
  (CONTROL-PROTOCOL.md). Today that is about 9 gateway places plus TS.
- **Cost:** small. No contract change.

### T3 — Attempt failure: one classification, a rule table, state on the attempt

- `retryReason` returns exactly `classifyAttempt`'s outcome when that outcome is
  retryable, and `""` otherwise. I verified this arm by arm
  (`server/upstream.go:259` vs `:491`), and the strings are identical.
- The 429 cooldown, `avoidAfter`, the `meter.Refused` code list, `errUpstream`,
  `errorCodeClass` and `metrics.retryReasons` each decode the same failure again.
  Adding `CodePathMissing` touched 7 server places plus 2 in metrics.
  - Sources: server S1, S6; observability F2.
  - `ErrorEventKind` is mapped in 4 server places, and "busy status (429/529)" in 4
    places across server and provider (provider hints).
- The backend's answer for an attempt (status, error, error code and type) lives on
  `request`, not `attempt`, and is valid only by call order (server S2).
  - **Bug:** the reset at `upstream.go:108` does not clear `upstreamErrorCode`. A
    request whose retry succeeds logs the failed attempt's error code next to status 200
    (verified by reading; log-only).
- **Proposed.**
  - `classifyAttempt(at)` is the one decoder.
  - A table keyed by outcome gives retry, backend-wide avoid and throttle.
  - Retry reasons are the retryable subset of `metrics.AttemptOutcome`.
  - Per-attempt fields move onto `attempt`. The independent review reached this
    separately (F03) and ranks it first.
  - Optionally, S7 (the attempt loop as a struct) and S9 (split `upstream.go` into
    attempts and relay) in the same change.
- **Payoff:** a new provider failure code goes from about 9 places to 1 table row. The
  stale-code bug becomes impossible by construction.
- **Cost:** about 150 lines in `upstream.go` and `errors.go`. Behavior is unchanged, and
  the retry, circuit, cooldown and metrics tests cover it.

### T4 — Endpoints and API formats: one descriptor table, one type per stream format

- **The server endpoint concept is scattered.** Route, metric name, GenAI operation,
  provider endpoint, error shape, output-limit keys and generates/counts live in about 7
  switches and 6 equality checks across 6 server files (server S3).
  - For body endpoints, it mirrors `provider.Endpoint` 1:1 through `providerEndpoint`
    (provider F6).
  - Each output-limit key is known in 3 places (server S4).
  - Adding an endpoint touches about 11 places in two packages.
- **Per-format stream rules in provider are split** over `stream_end.go`, `wire.go`
  (a type assertion picks the error-event fallback) and `body.go` (a second JSON decode
  per OpenAI usage chunk) (provider F1).
- **Inbound JSON walkers repeat one 8-line prologue 10 times and have drifted** (server
  S5).
  - **Bug:** a Messages message object that repeats a member is not refused
    (`inbound_messages.go:149`), unlike every sibling. Low impact.
- **Proposed.**
  - In server: one `endpointSpec` table keyed by `provider.Endpoint` (plus the 3 model
    routes); `providerEndpoint` goes.
  - One parse prologue that reads the output-limit keys from the table.
  - Two JSON-walk helpers.
  - In provider: a per-format `streamFormat` that owns completeness, error events, the
    nested model, the error fallback and the usage-only chunk.
- **Payoff is mostly future:** the backlog's *Image and audio models* adds endpoints,
  and the translation items add formats. A fourth format touches about 20 places in 4
  packages today. S5 and provider F1 pay off now.
- **Leave:** merging provider's and accounting's per-format stream readers
  (observability F9) and the two hand-written JSON scanners (provider F7). Both are
  high cost on the hot path with unmeasured gain. Revisit with a profile or a fourth
  format.

### T5 — The control ↔ limits seam, and `main.go`

The most-changed file collects glue that belongs to the two packages it connects.

- **Adapters in `main`.**
  - `controlContact` assembles 3 client getters into `limits.Contact`.
  - `limitsTotals` converts `control.TotalsWindow` field by field into the identical
    `limits.PushedWindow`.
  - Server tests carry a copy that has already drifted: `testLimitsTotals` never sets
    `Complete` (verified). So the controlled-gateway tests never exercise complete
    totals.
  - Sources: control-main F2, test-scaffolding F1.
- **Boot keeps the shape of the snapshot fetch it replaced** (control-main F4).
  - Two streams per boot and a `firstOnly` reader mode.
  - The first-totals wait is split over `main.waitFirstTotals`, a second boot deadline
    and `limits.FirstTotals`.
- **Live-gateway share in two modules.** The limiter rounds down and the router rounds
  up. The count passes control → main → limiter → `LiveGateways()` → router
  (routing-limits F1 and hints).
- **Pushed windows are stored twice** in the limiter (the `pushed` map and each window's
  base), beside a third store, `retained` (routing-limits F8).
- **"Hurry" is a channel watched by 3 hand-written goroutines plus a near-copy for
  boot.** A context collapses it (control-main F3).
- `run` checks the mode at 6 points and the file holds 4 jobs (control-main F6).
- **Proposed, in order.**
  1. `control` imports `limits` types (the `accounting.UsageRecord` precedent; no
     cycle). There is then one `LimitsContact()`, `OnTotals(limits.Totals, counted)`,
     and no adapters in `main` or the tests.
  2. Hurry as a context.
  3. Boot keeps its stream through the first totals and hands it to `Run`.
  4. Split `main.go` into `settings.go` and `controlplane.go`, plus `Client.Finish`.

  Optional: one pushed-or-retained counter store in limits (F8, medium confidence); the
  outage decision moving into the client (routing-limits F10, low-medium).
- **Payoff:** `run` loses about 100 lines. There is one connection per boot, adding an
  outage clock touches 2 places instead of 4, and the drifted test copy is gone.
- **Cost:** medium. It is the largest item but stays inside `control`, `limits` and
  `cmd/kaiak`. No protocol change. The boot diagram text in GATEWAY.md is touched.
- **Leave for now:** one owner for "this gateway's shares". Do it with *Demand-weighted
  shares* or *Live count excludes draining gateways* (backlog); both change both
  modules anyway.

### T6 — Applied-config propagation

- **`LoadObserver` is the de facto "config applied" bus**, but it does not carry the
  snapshot (config F5).
  - Its readers call `holder.Current()` 6 times.
  - They are correct only because the observer runs inside `Apply`'s lock. That rule is
    unstated, and an obvious tidy-up would break it.
- **Circuit series are prepared through a separate call from `main`** (observability
  F5).
- **Proposed:** `Load.Snapshot`; `NewOps` takes the `Circuits` and prepares both.
- **Leave** the limiter's pull model (`sync` against the holder). It is a separate,
  legitimate choice.
- **Cost:** very small.

### T7 — Gateway validation pipeline duplicated in `config` and `control`

- When `schemacheck` was extracted, two things were copied into `control` instead of
  moving with it: the stage glue (syntax → duplicates → walker → re-code issues →
  strict decode) and the error type (`Issue`, `ValidationError`, 3 codes).
- "4096.0 is an integer" is solved two different ways (`float64` in `document` vs
  `plainIntegers`), and `config` parses the bytes a third time (config F1).
- **Proposed:** `schemacheck.Validate` + `DecodeTyped[T]`, one `ValidationError`.
  - This also lets `Capabilities`/`OutputLimit` become one type shared by document,
    snapshot and `/v1/models` (config F4).
- **Cost:** medium-small. The fixtures pin codes, paths and messages.

### T8 — Backend-type knowledge

- **Provider modules repeat the body-edit step** and pass `stripUsage` out and back in
  (provider F2). Six of the seven also hand-build their credential header from the same
  two patterns, bearer or `api-key`.
- **"Can't tell which models a backend serves" is encoded twice:** a `listsModels` flag
  and an always-true probe. Because of this, `azure-openai`'s model check can never
  warn, and says nothing about it (provider F4).
- **backend-verify answers "what does this type mean" at 6 branch points**, each with an
  OpenAI-shaped default. So a new type compiles silently with `Bearer` + `/models`
  (edges F1).
- **Nothing ties the schema's backend-type enum to `provider.kinds`.** A type admitted by
  the schema but missing from `kinds` panics on first use (config hint). The TS side has
  this pin.
- **The URL layout + credential header is stated in 3 places with no shared check:**
  provider, backend-verify and the e2e tables (×3, test-scaffolding F2).
- **Proposed.**
  - Provider F2 and F4.
  - A `Record<BackendType, TypeRules>` in backend-verify.
  - A Go test tying the config enum to `kinds`.
  - A `protocol/fixtures/backend-types/` case set (type → models-list URL, credential
    header) read by both halves.
- **Rejected (user, 2026-10-07):** provider F3 (each module a *value* of one core
  struct), and the same move in the independent review's F02 ("one internal
  passthrough-module implementation for the five OpenAI-family cases"). Per-type
  modules that each read as a full provider are deliberate. What stays is F2: shared
  helpers that the modules call (the body-edit step inside `sendWire`, and bearer and
  `api-key` header helpers). Each module keeps its own struct, `Send`, `url`, `header`
  and probe.

### T9 — `kaiak-control` composition

- **The expiry sweep is the core's housekeeping** but lives in `gateways`. That forces
  `gateways` and `usage` to inject callbacks into each other, with a forward reference.
  `startExpirySweep`/`stopExpirySweep` are also public beside `start`/`stop`
  (control-core F1).
- **The listener-set pattern is hand-written 5 times**, with the same comment
  (control-core F4).
- **Two identical intake error types** plus `errorBody` that accepts neither; a
  `protocol_version: 5` literal beside `PROTOCOL_VERSION`; dead `libraryName`;
  `configHash` exported for tests only (control-core F7, edges F6).
- **`BatchCursors.latest`** makes every store compute "the batch counted last across
  epochs" for a log field (control-core F5, medium).
- **GUIDE.md restates contracts owned elsewhere** (store per-method contract, stream
  ordering, pricing/backend types) and opens with a dated change history. This is why it
  is the most-changed doc (control-core F6). **Your call:** how standalone the GUIDE
  must be for the real control plane's agent.
- **Leave for now:** moving stream sequencing from the Fastify adapter into the core
  (control-core F8, edges F7). This is the right placement in principle, but there is
  one adapter, and TECH-STACK says the real control plane starts from Fastify. Revisit
  if a second adapter appears or the next stream change can again only be tested over
  HTTP.

### T10 — Leftovers of removed features (removal discipline)

These are cheap and high-confidence. Each misleads a reader into looking for behaviour
that no longer exists.

| Leftover | From | Ref |
|---|---|---|
| `runSealer` seals the filling batch at stop; nothing sends it (verified); a comment claims it serves the drain | disk spool | control-main F1 |
| `instance` parameter on `get`/`post`/`send`, always the own ID; `usageSender.post` returns a discarded ack | disk spool | control-main F1 |
| `firstClosed` beside `totalsAt`; two hourly prune clocks | persisted limits | routing-limits F1 |
| `Subject.Model`, set by server, read by nothing (verified) | per-model limits | routing-limits F1 |
| `configChangedLocked` one-line wrapper; `Usage()` "for metrics", read only by tests; `LiveGateways()` read back by main | client/limits rework | routing-limits F1 |
| Status `starting`: unreachable since boot precedes `Run` | "start not ready", rejected | control-main F5 |
| Router "never configured" mode, test-only: `circuit.backend`, `probeTarget` fallback, 2 nil guards | pre-guard circuits | routing-limits F5 |
| Router `caps` map duplicates `backends[id].MaxInFlight` | per-backend cap | routing-limits F2 |
| `Group.Parent/Path/Root()`, `ModelSet.all`, `Snapshot.Keys`, `Identity.allowed` + `Key.AllowedModels()` | group-tree pointer tree | config F3 |
| `accounting.Fanout` (tests only), `OutOfRange` pointing at the same sink; GATEWAY.md and ARCHITECTURE still call the client a sink | two-sink design | observability F6 |
| `ControlPlane.totals(instance)`, sole caller the page with `""` | pre-feed stream read | control-core F2 |
| `sample/src/index.ts` + `exports`, imported by nothing | sample-as-library | edges F5 |
| fakecontrol `Totals` (no caller), `Hash`/`ConfigEvent`/`Counted` exported needlessly | — | test-scaffolding F10 |

The optional protocol trim of `starting` (schema, TS, fixture, CSS, spec) is a
both-halves change. Keep it as vocabulary unless you want it gone.

### T11 — Test scaffolding

- **Fixture runners:** the shared-fixture runner is written 4 times (2 Go, 2 TS)
  (test-scaffolding F3).
- **kaiak-control tests have no shared support.**
  - `startApp`, `openStream` and `nextTotals` are copied 4–5 times.
  - `"kaiak-protocol": "5"` appears 17 times; `7d51838` edited it by hand in 7 files.
  - Regression tests are filed by review round (`round-2`, `round-3`, `review`) rather
    than by subject, on both halves.
  - Sources: test-scaffolding F4, edges F4.
- **e2e:** three per-API passthrough scenarios each carry their own backend-type table
  and switch; `messagesConfig` and `responsesConfig` are the same function
  (test-scaffolding F2).
- **Server test config edits:** server tests edit a string config through about 63
  unchecked `strings.Replace` anchors, so a missing anchor fails silently
  (test-scaffolding F5). The cheap fix is a `replaceOnce` that fails.
- **Smaller:** three fake OTLP collectors; fakebackend `Reply` has 22 fault fields; the
  captures are read by other packages through copied paths (F6–F9).

### T12 — Routing observation, rebuilt by each consumer (independent review)

- **Main and metrics each rebuild the routing picture.**
  - `main.servingStatus` builds the set of configured backends, models and deployments,
    fills in closed circuits, then merges live counts for removed backends that still
    have requests running.
  - `metrics/ops.go` separately zero-fills configured backend and model counts, and
    walks deployment identities again for the circuit and cooldown gauges.
  - Both join sparse router getters (`InFlightByBackend`, `QueuedByModel`,
    `CoolingDown`, `Circuits`) with the config.
  - Sources: independent F05; this review's hints (routing-limits on
    `DeploymentID{…}` built by hand 5 times and the two circuit-state enums;
    observability F5).
- **Proposed:** one routing-owned observation taken against a config snapshot. It holds
  the configured entities plus removed-but-active ones, the configured cap next to this
  gateway's share, and each deployment's circuit and cooldown state. Main formats it as
  the status message; metrics formats it as gauges. The two output adapters stay,
  because the outputs really differ (status reports the configured cap, metrics the
  share — CONTROL-PROTOCOL.md "Status", checked).
- **Goes with the OTel metrics plan** (see Decisions): the gauge side is rewritten there
  anyway.
- **Cost:** medium across routing, metrics and main. No protocol change.

### Investigate only: one batch number instead of generation + sequence (independent F07)

- **What it is.** The usage sender numbers batches twice.
  - A process-local *generation* is given when a batch starts filling; limits uses it
    to retire own usage once totals cover it.
  - A wire *sequence* is given when a sealed batch passes its checks.
  - Totals coverage is translated from sequence back to generation by scanning the
    queued and acknowledged batches.
- **The proposal:** give the sequence when the batch starts filling, and drop the
  generation and the translation.
- **Why it's not on the list:**
  - It changes documented behavior: a batch emptied by its checks takes no sequence
    (GATEWAY.md, CONTROL-PROTOCOL.md sequence rules).
  - It touches accounting-sensitive interleavings: totals before the ack, a lost ack,
    memory drops.
- The control-main reviewer did not raise it. Revisit when usage delivery next changes.

## Recommended order

Ranked by payoff against cost. Each package is independent unless noted.

| # | Package | Themes / findings | Size | Confidence |
|---|---|---|---|---|
| 1 | **Leftovers batch** (+ the endpoint-memory prune) | T10, independent B01 | small | high |
| 2 | **Attempt classification** (fixes stale-code bug) | T3: server S1, S2 (+S7, S9), observability F2 | small–medium | high |
| 3 | **Control ↔ limits seam + `main.go`** (fixes test drift) | T5: control-main F2, F3, F4, F6; routing-limits F1 rest | medium | high (F4 medium) |
| 4 | **Limit-type table + unit sets, both halves** | T1, T2: config F2, routing-limits F9, observability F1, F3 (limit types), control-core F3, F2, edges F3a, counted-rule fixture | small–medium | high |
| 5 | **Gateway config pipeline** | T6, T7: config F1, F4, F5; observability F5 | small–medium | high |
| 6 | **Provider + inbound** (fixes repeat-member gap) | T4 now-part: provider F1, F2, F4; server S5 | small | high |
| 7 | **kaiak-control composition** | T9: control-core F1, F4, F7, edges F1, F2, F6, F9 | small | high |
| 8 | **Routing internals** | routing-limits F3 (one eligibility pass), F4 (circuit transitions), F6, F7 | small | high |
| 9 | **Accounting tidy** | observability F7 (= independent F01), F10 | small | high |
| 10 | **Test support** | T11 + the backend-type fixture (T8) | medium | high |
| 11 | **Endpoint table** | T4 future-part: server S3, S4, provider F6 (= independent F04) | medium | medium-high; best done when an endpoint is added |
| — | **Moved to the OTel metrics plan** | observability F3, F4, F5, F8; T12 | — | — |

Items 1–5 carry most of the value: two real fixes (the stale log code and the drifted
test copy) and most of the lockstep-edit cost. Items 6–9 are local tidy-ups. Items 10
and 11 pay off at the next protocol bump or endpoint; they are in scope too (Decision 7).

The metrics-facing findings move to the OTel metrics plan, which comes after items 2
and 4. Those items create the label lists the metrics read (attempt outcomes, limit
types, usage units), so each metric is reworked once. The 1–8 packages only touch
`metrics` where a list it copies goes away.

## Decisions

Taken (user, 2026-10-07 and 2026-10-08):

1. **Provider modules stay full per-type providers.** Provider F3, and independent
   F02's matching part, are rejected (see T8).
2. **Metric label lists are owned by the code that produces the values**, and exported
   for `metrics` to read. The metrics side of this is done together with **OpenTelemetry
   metrics export** alongside `/metrics`, the way OTel logs were added. That includes
   renaming metrics to the OTel semantic conventions where a convention exists. A
   separate plan follows the structure work (see Recommended order).
3. **GUIDE.md is trimmed** to what only it says (control-core F6; user, 2026-10-08).
   The parts an app developer needs that live in CONTROL-PROTOCOL.md stay, since
   `docs/specs/` does not ship in the package.
4. **Status `starting` is removed** from the protocol on both halves (control-main F5;
   user, 2026-10-08), in the leftovers batch, with no version bump: no current gateway
   can send it.

5. **Outbound-only Go validators move into `_test.go`** (control-main F7; user,
   2026-10-08): they stay the oracle for the fixture tests and leave the binary.
6. **Embeddings and completions stop type-checking output-limit keys they ignore**
   (server S4; user, 2026-10-08), matching GATEWAY.md's settled output-limit keys.
   Passthrough is unchanged.
7. **Every package is in scope, 10 and 11 included** (user, 2026-10-08): the point of
   the work is cheaper future changes, and a "next change that needs it" trigger would
   only move that cost into a feature. The plan is `docs/plans/structure/`.

## Bugs and gaps noticed in passing

| Item | Status | Ref |
|---|---|---|
| A successful retry logs the failed attempt's `kaiak.upstream.error.code` | verified by reading (reset at `upstream.go:108`); not reproduced | server S2 |
| Controlled-gateway server tests never exercise complete totals (`testLimitsTotals` drops `Complete`) | verified | test-scaffolding F1 |
| Sample page feed has no `error` listener and no closed mark; a write after `end()` at shutdown crashes with exit 1 | verified no listener; the write-after-end crash confirmed on Node 26; the timing window not reproduced | edges, bugs |
| Messages message objects with a repeated member are not refused | verified by reading | server S5 |
| `limits.shape` treats any unnamed limit type as USD per month | verified; latent (schema admits only four) | config F2 |
| `azure-openai` model check can never warn, and says nothing about it | by reading | provider F4 |
| GATEWAY.md:1780-1786, :2064 and ARCHITECTURE.md:146,164 describe the control client as a sink on a fan-out (it is the `Batcher`) | doc drift | observability F6 |
| `ControlPlane.stopExpirySweep()` on a started core leaves `started` set | no caller; goes with control-core F1 | control-core |
| Missing-endpoint memory never forgets a backend that a reload removed. Its entries are only deleted lazily, for deployments still in a model. Bounded by backend IDs ever named × 7 endpoints, so tiny at target scale. Fix: prune on config apply | verified by reading (`server/endpointmemory.go:33-52`) | independent B01 |

Checked and not a bug: status `max_in_flight` is the configured cap and the metric is
the per-gateway share. That is deliberate, and CONTROL-PROTOCOL.md "Status" says so.

The sample page's quadratic render at thousands of keys/groups is already in the backlog
(*Sample status page render time*). It is not repeated here.

## Independent review

[`STRUCTURE-independent.md`](STRUCTURE-independent.md) was written on a plain copy with
no git history and no plans or reviews.

**Agrees** (found separately):

| Independent | Here |
|---|---|
| F03 — an attempt owns its whole result (ranked 1st) | T3, server S2 |
| F04 — endpoint metadata declared once per layer | T4, server S3, provider F6 |
| F06 — housekeeping composed in the core, not in `gateways` | T9, control-core F1 |
| F01 — one inclusive-token normalization | observability F7 |
| F02 — shared provider mechanics | provider F2 (kept), F3 (rejected) |
| The package graphs, the single pipeline, config document vs snapshot, the store triad, `schemacheck`, `otlplog`, `sse`/`clip`/`netfail` are right | same verdict |

**Adds:** F05 → T12 (routing observation); F07 → investigate only; B01 → the bugs table
and package 1.

**Differs:**

- **`main.go` adapters.** It prefers keeping the short `control → limits` adapters in
  `main` over coupling limits to the client. T5 keeps that direction: `control` imports
  `limits` types, and `limits` stays unaware of `control`. What T5 removes is the
  adapter's location in un-importable `main`. That location is why the test copy
  drifted, and the drift is verified.
- **Things it did not find,** because it reviewed modules without following the copies
  into other packages: T1 (limit types), T5's boot shape and hurry, T7 (the config
  pipeline copied into `control`), and most of T10. Each was verified here, against
  code and history.
- **Usage units.** It warns that iterating "all units" would erase the priced/recorded
  distinction (reasoning is recorded but not priced separately). T2 names separate sets
  for exactly that reason, so there is no conflict. It's worth stating in the plan.
- **Test scaffolding.** It found nothing actionable and would leave fakebackend's
  `Reply` and the e2e layout alone. T11 stays low priority except the drifted copy,
  which T5 fixes.
- **backend-verify:** "clean enough". edges F1 (an exhaustive `Record` keyed by type, so
  `tsc` refuses a new type until it's decided) stays: it closes a silent-default gap,
  and the change is small.
- **The sample's root `index.ts`:** it says the file serves tests. That is wrong, and
  checked: the tests import `./app/index.ts` etc., and nothing imports the root entry.
  edges F5 stands.

## Considered and left as is

- **Hand-written schema walker in Go** instead of a JSON Schema interpreter: the price of
  zero dependencies, and the fixtures pin it (config).
- **`kaiak-control/schema/` byte copy:** settled in TECH-STACK 2026-09-25, guarded by a
  test.
- **TS message types mirrored by hand:** generating them needs a dependency.
- **Store interface + memory store + contract tests:** the minimum for a pluggable store.
  What can shrink is the domain logic inside it (T1, control-core F5).
- **`window.go` branching per kind; `dispatch`'s measured cache; `listener.go`/`drain.go`
  staying in `server`; `otlplog`'s intermediate value type:** each was checked, and each
  either earns its keep or moving it would not remove code.
- **backend-verify duplicating the gateway's URL/header knowledge:** necessary across
  languages and processes. The fix is a shared fixture (T8), not sharing code.
- **Provider/accounting stream-reader merge, shared JSON tokenizer:** see T4.
