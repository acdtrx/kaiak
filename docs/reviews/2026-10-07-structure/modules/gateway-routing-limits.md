# gateway-routing-limits — structure review

Modules: `gateway/internal/limits` (limits.go 673, shared.go 273, window.go 404,
headers.go 79), `gateway/internal/routing` (routing.go 719, circuit.go 444,
modelcheck.go 128). `persist.go` and `snapshot.go` are already gone (0a00dc6, "no data
directory"); the findings below include what that removal left behind.

## Module summaries

### limits

Responsibilities
- Checks and reserves a request against every applicable counter (global + each group
  on the key's path), all-or-nothing under one mutex (`Reserve`); settles with the
  request's usage records (`Settle`).
- Keeps an hour and a month count for **every** scope, limited or not, plus a sliding
  minute window per per-minute limit; matches counters to the live config lazily
  (`sync`, pulled from `config.Holder` on every entry point).
- Keeps a deleted scope's hour/month counts while own usage or a running request holds
  them (`retained`, `refs`, hourly `expireRetainedLocked`).
- Control-plane mode (`NewShared`, shared.go): hour/month windows = pushed base + own
  usage not yet counted, tagged by usage generation (`owning`, `counted`,
  `retireCountedLocked`); per-minute windows enforce `floor(limit ÷ live)`; outage
  decision and its log lines (`outageLocked`); first-totals readiness channel;
  ahead-of-clock and small-share warnings.
- Computes `x-ratelimit-*` header values and the `Rejection` (headers.go).

Exported surface used outside (importer)
- `New`, `NewShared`, `Limiter.ObserveSyncs`, `TakeTotals`, `LiveGateways`,
  `FirstTotals`, `Totals`, `PushedWindow`, `Contact` — `cmd/kaiak/main.go`.
- `Outage`, `TotalsAppliedAt` — `metrics` (through `controlState` in main).
- `Reserve`, `Settle`, `Reservation.Headers`, `Subject`, `Rejection`, `Headers`,
  `HeaderValues`, `Measure*`, `LogValue` — `internal/server` (limits.go, api.go,
  metrics.go, upstream.go).
- Exported but unused outside: `Usage`/`CounterUsage` (tests only), `Kind` and its
  constants, `ScopeGlobal`/`ScopeGroup` (server tests only).

Dependencies: `config`, `accounting` (UsageRecord, units), `logattr`.

Domain concepts encoded
- Limit type → (window kind, measure): `shape()` limits.go:55; "counted by every scope /
  by the control plane" = `countedTypes` limits.go:97 and, implicitly, every
  `kind != SlidingMinute` test (limits.go:298, 303, 350, 384).
- Measures (requests / tokens / nano-USD) and what each counts: `need`, `amountOf`,
  `applicable`, `blockedByRunning`, `Settle`, `headersFor`.
- Window kinds (sliding minute / UTC hour / UTC month): window.go, every method
  branching on kind.
- Scope (global / group) — `Scope`, `scopeOf`, `identityAttrs`.
- Usage generation (control-client batches) — window `local`, Limiter `counted`/`owning`.
- Live-gateway share (floor) — `share` limits.go:434.
- Control-plane outage — `Contact`, `outageLocked`.

### routing

Responsibilities
- Chooses a deployment (fewest in flight, ties rotate), counts in-flight per deployment
  and backend, enforces per-backend caps split `ceil(cap ÷ live)`.
- Per-model FIFO queues served by one dispatcher under the mutex (`dispatch`).
- Circuit breaker per deployment: failure threshold, response-timeout run, half-open
  trial decided at response start, per-backend probers (`RunProbers`, `ProbeNow`).
- 429 cooldowns per deployment (timer-ended), retry avoid sets.
- Config-time model check (`ModelChecker`) — a separate job sharing only `ProbeFunc`.

Exported surface used outside (importer)
- `New`, `Options`, `Configure`, `SetLiveGateways`, `OnServingChange`, `RunProbers`,
  `NewModelChecker`, `Check`, `Run` — main.go.
- `Acquire`, `Slot` (+ Release/Report/ResponseStarted/Throttled), `Avoid`, `Wait`,
  `DeploymentID`, `Err*`, `Outcome` consts — `internal/server` (upstream.go, pipeline.go).
- `InFlightByBackend`, `QueuedByModel`, `Circuits`, `CircuitReport`, `CircuitState`,
  `MaxInFlightByBackend`, `CoolingDown`, `Observer` — `metrics` (ops.go) and main
  (`servingStatus`).
- `ProbeNow` is exported but called only by `runProber` and tests.

Dependencies: `config`, `logattr`. No `provider` import (ProbeFunc and the
`pathMissing` interface are injected / mirrored).

Domain concepts encoded
- Deployment identity across snapshots (`DeploymentID`), backend caps and their share,
  eligibility (circuit) / usability (avoid + cooldown "warm" rule) / free slot.
- Outcome classes (`Outcome`), circuit states (implicit in `openedAt`/`halfOpen`/`trial`,
  explicit only as `CircuitState` for observers).

## Findings

Ranked by payoff vs cost.

### F1 — Limiter fields and API left behind by removed features
- **Kind** — cross-function
- **Where** — limits.go:441-454 `Subject.Model`; shared.go:189-195
  `configChangedLocked`; limits.go:159-165 + shared.go:62-69, 271-273
  `firstClosed`/`totalsAt`; limits.go:641-673 `Usage`/`CounterUsage`;
  limits.go:145-147 `retainedPrunedAt` + shared.go:170-172 `prunedAt`; shared.go:204-210
  `LiveGateways` (consumer main.go:414).
- **Now**
  - `Subject.Model` is set by server (server/limits.go:41) and read by nothing in limits.
  - `configChangedLocked` is a one-line wrapper around `warnSmallSharesLocked`.
  - "Totals were taken" is held three ways: `firstClosed`, `!totalsAt.IsZero()`, and the
    closed channel; `firstClosed` and `totalsAt` are always set together in
    `TakeTotals` and nowhere else.
  - `Usage()` is documented "for reading (metrics, tests)"; no production caller ever
    existed (introduced c55a65b); tests call `_ = l.Usage()` mostly to force a `sync`.
  - Two hourly-prune clocks: `retainedPrunedAt` (pruned from `sync`, added 4dc87df) and
    `prunedAt` (pushed windows, pruned only in `TakeTotals`).
  - main reads `limiter.LiveGateways()` back right after `TakeTotals` only to pass it
    to `router.SetLiveGateways`; both sides clamp to ≥1 on their own.
- **How it got here**
  - `Subject.Model` fed `c.limit.Covers(s.Model)` until per-model limits went
    (b44b13b, "limits by scope and type").
  - `configChangedLocked` applied waiting totals and tracked config mismatch until
    d5c45cd (broadcast-only client).
  - `firstClosed` replaced `totalsKnown` in 0a00dc6. `totalsKnown` was there to tell
    restored totals (from disk) from taken ones; with no disk the distinction is gone.
- **Proposed shape**
  - Drop `Subject.Model`; inline `warnSmallSharesLocked` in `sync`.
  - Replace `firstClosed` with `!l.totalsAt.IsZero()`.
  - Unexport `Usage` (or move it to a test helper) and drop the "metrics" claim.
  - One `housekeepLocked(now)` per hour from `sync` that prunes both retained counts
    and pushed windows (see F8, which removes the second one entirely).
  - In main, pass `u.Totals.LiveGateways` to the router directly and remove
    `LiveGateways()`.
- **Payoff** — about 35 lines, 3 fields, 2 exported identifiers, 1 wrapper. Every one of
  them misleads a reader into looking for behaviour that no longer exists.
- **Cost / risk** — trivial; limits tests that build `Subject{Model: …}` and read
  `Usage()` need edits; no contract touched.
- **Confidence** — high (each item checked against all callers and git history).

### F2 — Router `caps` map duplicates `backends[id].MaxInFlight`
- **Kind** — in-function / state
- **Where** — routing.go:74-76, 220-224 (`Configure`), 438-444 (`hasFreeSlot`), 473-483
  (`MaxInFlightByBackend`).
- **Now** — `Configure` builds `r.caps[id] = b.MaxInFlight` from the same
  `s.Backends` it stores in `r.backends`. `hasFreeSlot` reads `caps`, and falls back to
  `b.MaxInFlight` for a backend the applied config lacks.
- **How it got here** — `caps` came with the per-backend cap (d9f9995). `backends` was
  added later for probing (0ec83da).
- **Proposed shape** — drop `caps`. In `hasFreeSlot`, use
  `if ab, ok := r.backends[b.ID]; ok { b = ab }`. `MaxInFlightByBackend` ranges over
  `backends`.
- **Payoff** — one field and one map build gone; one source for "the cap in force".
- **Cost / risk** — about 10 lines; no test or contract changes.
- **Confidence** — high.

### F3 — Five deployment-eligibility walks where one pass would do
- **Kind** — cross-function
- **Where** — routing.go:372-392 `choose`, 396-399 `eligible`, 404-410 `usable`, 414-422
  `anyWarm`, 426-434 `anyUsable`, 594-602 `canServe`; callers `Acquire` (312, 317) and
  `dispatch` (568, 574, 585).
- **Now** — "may this attempt use d" is spread over `eligible` (circuit), `usable`
  (avoid, plus the cooldown "warm" rule) and `hasFreeSlot` (cap). Each of `choose`,
  `anyUsable` and `canServe` first calls `anyWarm`, which is a deployment loop of its
  own, then loops again. A dispatch grant walks the deployments 4 times; a queued
  `Acquire` walks them 4 times.
- **How it got here** — the 429 cooldown (2710969) added the "warm" rule. It was
  threaded through `usable(d, avoid, warm)` and the new `anyWarm`, and then copied into
  each existing helper.
- **Proposed shape** — one
  `pick(m, avoid) (best config.Deployment, free, usable bool)`. In a single walk it
  tracks:
  - the best candidate with a free slot among warm deployments;
  - the best candidate with a free slot among cooling ones;
  - whether any usable deployment exists, warm or not.

  It then resolves the warm rule once at the end. `choose`, `canServe`, `anyUsable` and
  `anyWarm` become this one function. `Acquire` and `dispatch` read the three results
  from one call.
- **Payoff** — about 40 lines and 3 helpers gone. The warm/cooldown rule lives in one
  place, so the next eligibility rule (backlog "provider budgets": spent deployments
  leave routing) touches 1 function instead of 4.
- **Cost / risk** — small; this is hot code. `dispatch_bench_test` and the
  queue/cooldown/circuit tests cover it. No contract touched.
- **Confidence** — high.

### F4 — Circuit transitions: log, observer and notify repeated at 5 sites, with lock handed to callee
- **Kind** — cross-function
- **Where** — circuit.go:122-129 (`report` open), 142-160 (`endTrial`: closed /
  re-open), 356-384 (`ProbeNow` failure: re-open), 423-428 (`ProbeNow`: half-open);
  `endTrial` "Called with r.mu held; it unlocks it" (132-134).
- **Now** — each transition site hand-writes the same three steps after unlocking: a
  `logger.Warn/Info("circuit …")`, `observer.CircuitChanged`, and `notify`. The opened
  line is written 3 times with slightly different attrs. `endTrial` unlocks the
  caller's mutex in all three branches.
- **How it got here** — half-open (c43b2df), the decision at response start (869f37e)
  and the "probe failed reopens half-open" rule each added a site.
- **Proposed shape** — under the lock, collect
  `[]transition{key, to, attrs}` (plus a `changed` flag). One
  `r.emit(transitions, changed)` after the unlock writes the log line, tells the
  observer and notifies. `endTrial` returns its transition and no longer unlocks.
- **Payoff** — about 20 lines; one place guarantees that every transition reaches the
  log, the metrics and the status report, and the unlock-in-callee pattern goes away.
- **Cost / risk** — small. Log attrs must stay as the spec lists them
  (GATEWAY.md:1072-1080); circuit tests assert transitions and lines.
- **Confidence** — high.

### F5 — Router "never configured" mode exists only for tests
- **Kind** — cross-function
- **Where** — routing.go:91-93 (`deployments` nil = "every deployment counts as
  configured"), 664 (`throttle`), circuit.go:80 (`report`), 58-60 + 104 (`circuit.backend`),
  432-444 (`probeTarget` fallback).
- **Now** — `report` and `throttle` guard with `r.deployments != nil && …`. Every
  failure report stores `c.backend` so that `probeTarget` can find a backend the
  applied config lacks. Once `Configure` has run:
  - circuits exist only for deployments of the applied config (`report`'s guard plus
    `dropRemovedCircuits`);
  - their backends are therefore always in `r.backends`.

  So the fallback is reachable only before the first `Configure`. In production that
  never happens: main's applier configures before any request.
- **How it got here** — `probeTarget` and `c.backend` predate the
  configured-deployments guard. 0ec83da added them; 2710969 and 021fb54 added the guard
  and dropped circuits on reload.
- **Proposed shape**
  - Make `Configure` the precondition (or have `New` take the first snapshot).
  - Delete `circuit.backend`, the `probeTarget` loop (use `r.backends[id]`) and the two
    nil guards.
- **Payoff** — 1 field, 1 fallback path and 2 guards gone; one fewer state to reason
  about in the circuit code.
- **Cost / risk** — about 15 lines in the code. The cooldown tests (8 routers built with
  `New(Options{})` and no `Configure`) and the bench need a `Configure` call.
- **Confidence** — medium-high. It would be high after confirming that no e2e test
  reports on an unconfigured router.

### F6 — `counter.limit config.Limit` carries a type the key already has; full limit recomputed 6×
- **Kind** — in-function
- **Where** — limits.go:105-112 (`counter`), 273, 286, 304 (resets to
  `config.Limit{Type: typ}`), 404, 429, 523-524, 545-546 (`effectiveLimit(c.limit)`),
  shared.go:129, 143; Reserve builds two near-identical `Rejection` literals (523, 545).
- **Now** — a counter keeps the whole `config.Limit` (type and float value) next to
  `key.typ`. An unlimited counter gets a dummy `config.Limit{Type: typ}` just so
  `c.limit.Type` reads right. `effectiveLimit` converts float USD to nano each time it
  is needed.
- **How it got here** — counters were per configured limit until 2a8590c ("count every
  scope, limits on top"). The unlimited count was then grafted on through a `limited`
  flag plus a placeholder `Limit`.
- **Proposed shape** — `counter{key, measure, limited bool, max int64}`, with `max` set
  once in `sync`; read `Type` from `c.key.typ`. Add a `c.rejection(…)` helper for the
  two literals.
- **Payoff** — the placeholder concept and 6 recomputations go; about 15 lines; one
  reading of "the full limit".
- **Cost / risk** — small, internal to limits.
- **Confidence** — high.

### F7 — Rejection log attrs built in server from limits internals
- **Kind** — cross-module
- **Where** — server/api.go:293-310 `limitAttrs`; limits.go:356-370 `identityAttrs`,
  exported `LogValue`; `Rejection.Scope` + `Rejection.Group` (headers.go:15-16).
- **Now** — limits writes the `kaiak.limit.scope/group` identity attrs for its own log
  lines (`identityAttrs`). server rebuilds the same two attrs and switches on
  `Measure` to format values. For that it needs the exported `LogValue`. `Scope` is
  stored on `Rejection` although `scopeOf(Group)` derives it.
- **Proposed shape** — `(*Rejection).LogAttrs() []slog.Attr` in limits, reusing
  `identityAttrs`; `Scope` becomes a method. `LogValue` becomes unexported.
- **Payoff** — the identity vocabulary has one writer; one exported helper and one
  redundant field go; server stops depending on `Measure`→unit formatting.
- **Cost / risk** — small. server tests that build `Rejection{Scope: …}` change. The
  log contract is unchanged.
- **Confidence** — high.

### F8 — Pushed windows held twice: `Limiter.pushed` and each window's `base`
- **Kind** — cross-function (state duplication)
- **Where** — limits.go:153-158 (`pushed`), 170-172 (`prunedAt`), 375-394
  (`applyLimit` copies `pushed[c.key]` into `c.w.setBase`), 139-147 + 316-343
  (`retained`); shared.go:70-93, 99-115 (`TakeTotals`, `prunePushedLocked`); window.go
  59-66 (`base`, `baseStart`).
- **Now** — a scope's control-plane count lives in the `pushed` map, which also holds
  scopes the config lacks, and is copied by hand into its counter's window. A deleted
  scope's own count lives in `retained`. Three structures are kept in step:
  - `counters`;
  - `retained`;
  - `pushed`, which has its own hourly pruning clock.

  There are also two rules for when a count survives its config: "own usage or refs"
  for `retained`, "window not ended" for `pushed`.
- **How it got here** — `pushed` predates "every scope is counted" (2a8590c), when
  counters existed only for configured limits and needed a side store to find their
  base on reload. `retained` came later (0a00dc6 / 4dc87df) for deleted groups.
- **Proposed shape** — one store of counts by key. A pushed window for a scope with no
  counter creates a retained counter with that base. `TakeTotals` sets bases on
  `counters ∪ retained` directly; complete totals zero the bases they do not list. A
  retained counter is pruned when it has:
  - no refs;
  - no own usage;
  - no base for its current window.

  `sync`'s `take()` already re-adopts retained counters, so "a reload that adds the
  scope finds its base" follows for free.
- **Payoff**
  - Removes the `pushed` map, `prunedAt`, `prunePushedLocked` (about 17 lines),
    `applyLimit`'s base branch and the hand copy (about 30 lines in all).
  - One survival rule instead of two, one place a count lives.
  - For the backlog's "Fleet-wide per-minute token limits", which pushes another window
    type, only the counter store changes, not two parallel stores.
- **Cost / risk** — medium. TakeTotals, sync and the prune rules get rewritten;
  shared_test (834 lines, behavioural) should stay valid. No spec or protocol change:
  the spec's "Windows of scopes the config does not have are kept for a reload that
  adds them" still holds.
- **Confidence** — medium. It would rise once the shared_test cases for
  totals-before-config and complete-totals-after-delete are checked against the new
  rule.

### F9 — Limit-type facts restated across 5 packages; "control-plane counted" inferred from window kind
- **Kind** — cross-module
- **Where**
  - limits: limits.go:55-65 `shape`, 97 `countedTypes`, and the window-kind tests at
    298, 303, 350, 384.
  - control: control/schema.go:30 `totalsLimitTypes` and 143-150 (hour vs month
    `window_start`).
  - config: config/schema.go:102-108 `limitTypes`/`countLimitTypes`, and
    config/semantic.go:230 (per-minute types).
  - metrics: metrics/ops.go:108 `limitTypes`.
  - server: server/limits.go:86-127 (`errLimited` hard-codes "per minute" / "per
    month"; `tokenWindow`).
- **Now** — each limit type's window, its measure and whether the control plane counts
  it are restated as lists, switches or messages in about 8 places across 5 packages.
  Inside limits, "shared with the control plane" is not a property: it is read as "kind
  is not the sliding minute" at 4 sites, plus `countedTypes`.
- **How it got here** — each package added its own list as it needed one: config
  validation, the protocol schema, metric pre-registration, client messages.
- **Proposed shape** — config owns one table: `LimitType → {Window: minute|hour|month,
  Measure: requests|tokens|usd, Counted bool}` (Counted = the control plane counts it).
  Everything else reads it:
  - limits' `shape` and the `countedTypes` / `shared` decisions;
  - config's schema lists and semantic check;
  - `control/schema.totalsLimitTypes` and the `window_start` check;
  - metrics' label list;
  - server's window wording.
- **Payoff**
  - Places to touch for a new limit type (e.g. `requests_per_hour`): about 8 → 2.
  - Places to touch for the backlog's "Fleet-wide per-minute token limits" (a
    per-minute type becomes control-plane counted): the 5 kind checks in limits plus
    control's list → one table flag, and the window code.
- **Cost / risk** — small to medium, mostly mechanical. It touches config (outside this
  group), control's schema walker, metrics and server. No contract change.
- **Confidence** — medium. The payoff depends on a new type or the fleet-wide minute
  item actually coming; today the four types are stable.

### F10 — Outage decision sits in limits, fed by a mirror of the control client's state
- **Kind** — cross-module
- **Where** — limits.go:203-217 `Contact`; shared.go:212-260 `Outage`/`outageLocked`;
  main.go:725-729 `controlContact`; main.go:745 `controlState.Outage` → `limiter.Outage`.
- **Now** — main copies 3 client accessors into a 4-field `limits.Contact` on every
  call. limits evaluates it against the grace and owns the start/end log lines. The
  metrics scrape reaches the outage through the limiter's mutex and `sync`. The only
  limiter state involved is `outageSince` and the applied grace.
- **How it got here** — the outage gates budget refusals, so it was placed with them.
  It grew two usage-waiting conditions over time (both control-client state).
- **Proposed shape** — `control.Client.Outage(now, grace) bool`, owning `outageSince`
  and the two log lines. limits takes `outage func(time.Time) bool` and keeps only the
  refusal. metrics asks the client.
- **Payoff** — the `Contact` type and the main adapter go (about 15 lines). The
  decision sits beside the state it reads, and a metrics scrape no longer takes the
  limiter lock.
- **Cost / risk** — medium-small. Both the client and metrics need the grace (main can
  close over `holder`). The spec text locating the outage under "Limits" stays
  accurate as behaviour.
- **Confidence** — low-medium. The win is placement more than size; weigh it against
  the control-module reviewer's view of `control.Client`'s breadth.

### F11 — ModelChecker is a provider job living in routing
- **Kind** — cross-module
- **Where** — routing/modelcheck.go (whole file); `pathMissing` interface (69-75)
  mirrors `provider.PathMissingError`; main.go:371 injects `providers.Probe` and
  `provider.ListsModels`.
- **Now** — `ModelChecker` uses no Router state. It needs `ProbeFunc`, a mirrored error
  interface and an injected `listsModels` predicate, all of which provider owns.
- **Proposed shape** — move it to provider (e.g. `provider.ModelChecker` over a
  `*Registry`). It then calls `Probe`, `ListsModels` and `PathMissingError` directly.
  routing keeps only `ProbeFunc` for its circuits.
- **Payoff** — the mirrored interface and one injected function go; routing becomes
  one job (the Router).
- **Cost / risk** — move 128 lines plus its two tests from circuit_test.go. The
  boundary stays acyclic: provider does not import routing.
- **Confidence** — medium. It would rise after checking that provider's own reviewer
  does not see the package as already over-broad.

### Considered, leave as is
- **window.go branching per kind** — 8 methods branch on SlidingMinute vs fixed.
  Splitting into two types would move the branches to `counter` (it reads
  `w.kind/used/local/limit`) without removing code. Adding a new fixed window kind
  touches `windowStart`/`windowEnd`/`previousWindow` only. One small trim:
  `previousWindow` can use `windowStart(kind, current.Add(-1ns))` instead of its own
  month/hour branch.
- **`dispatch`'s `firstServable` cache** — a measured optimisation (c50cdfb, the bench
  exists); it stays.
- **`Avoid` struct** — one field since `Tried` left (2710969). Collapsing it into a
  slice is cosmetic.
- **`ProbeNow(…, trigger)`** — exported and generalised with a single trigger
  (`interval`). It is speculative by the review's bar, but the spec settles it
  (GATEWAY.md:1069), so it stays unless the user drops that line.
- **`minute` index** (limits.go:136-138) — kept only for re-sharing on a live-count
  change. It could be a filter over `counters`; marginal.

## Cross-module hints
- **Live-gateway share is one concept in two modules.** `limits.share` (floor, ≥1,
  limits.go:434) and `routing.Router.share` (ceil, routing.go:448) each hold `live`. The
  count flows control → main → limiter → `LiveGateways()` → router. Both backlog items
  "Demand-weighted shares" and "Live count excludes draining gateways" change both. A
  single owner for "this gateway's shares" (pushed per gateway, or computed in one
  place) is worth a look for the relations pass.
- **Config-following differs by module.** Router is pushed (`Configure` from main's
  applier). Limiter pulls (`sync` → `holder.Current()` on every entry point, including
  metrics scrapes). Pushing to the limiter from the applier would make the pattern
  uniform; the limiter's hourly prune would still need a trigger. Low priority.
- **The scope-kind list appears twice:** `metrics/ops.go:105 limitScopeKinds`
  duplicates `limits.Scope` values. The same goes for metrics' `limitTypes` (see F9).
- **`DeploymentID{Backend: d.Backend.ID, Model: d.Model}` is built by hand** in main.go:764,
  metrics/ops.go:262 and 290, and server/upstream.go:327 and 332. routing has an
  unexported `keyOf(d)`; exporting it (`routing.IDOf`) removes 5 hand copies.
- **`servingStatus` in main.go:753-788** translates `routing.CircuitReport` and
  `CircuitState` into `control.DeploymentStatus` and `control.Circuit*`: two parallel
  enums for the same three states (open, half-open, closed).

## Bugs noticed in passing
- None confirmed. One thing to keep in mind if F3 lands: in `endTrial` Success
  (circuit.go:148), the result of `r.dispatch()` (a queue emptied) is discarded, while
  `notify(true)` fires anyway. That is harmless today, because every closing circuit
  notifies.
