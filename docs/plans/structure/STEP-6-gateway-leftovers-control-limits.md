# Step 6 — gateway leftovers: control client, limits, routing

**Status:** done (2026-10-08)

## Intent

Remove what removed features left in the control client, the limiter and the router.
Each leftover misleads a reader into looking for behaviour that no longer exists.

## Findings

- control-main F1 (disk spool):
  - `runSealer` returns on `ctx.Done()` without sealing; fix the `Run` doc and the
    comment that says the stop seal serves the drain's flush;
  - `get`/`post`/`send` lose the `instance` parameter (they set `Kaiak-Instance` from
    `c.opts.Instance`); `usageSender.instance` goes;
  - `usageSender.post` returns `error` only.
- routing-limits F1 (persisted limits, per-model limits, client rework):
  - drop `Subject.Model`;
  - inline `configChangedLocked`;
  - `firstClosed` → `!l.totalsAt.IsZero()`;
  - `Usage`/`CounterUsage` unexported (or a test helper), the "metrics" claim gone;
  - one hourly housekeeping clock from `sync` (`retainedPrunedAt` and `prunedAt` today;
    step 18 removes `pushed` itself);
  - `main` passes the pushed live count to the router directly; `LiveGateways()` goes.
- routing-limits F2: the router's `caps` map goes; `backends[id].MaxInFlight` is the cap.
- routing-limits F5: the router's "never configured" mode goes — `Configure` (or `New`
  with the first snapshot) is the precondition; `circuit.backend`, the `probeTarget`
  fallback and the two nil guards go; the cooldown tests and the bench configure first.
- routing-limits "Considered": `previousWindow` uses `windowStart(kind, current.Add(-1ns))`.

## Files likely touched

- `gateway/internal/control/{usage,client,transport,status}.go` and tests.
- `gateway/internal/limits/{limits,shared}.go` and tests; `gateway/internal/server/limits.go`
  (`Subject` literal).
- `gateway/internal/routing/{routing,circuit}.go`, `cooldown_test.go`,
  `dispatch_bench_test.go`.
- `gateway/cmd/kaiak/main.go` (`LiveGateways` readback).

## Decisions made during planning

- `firstTotals` / `FirstTotals()` stay until step 17 removes the boot wait they serve.
- If F5's "no e2e test reports on an unconfigured router" turns out false, stop and
  report: that would make the mode live.

## Removal checklist (clean at phase end)

- `limits.Subject` has no `Model` field (read the type).
- `git grep -nE 'configChangedLocked|firstClosed|retainedPrunedAt|\.LiveGateways\(\)' gateway/`
  → none.
- `git grep -nP '\bcaps\b' gateway/internal/routing` → none.
- `git grep -n 'circuit.backend\|c\.backend' gateway/internal/routing` → none.
- `git grep -n 'u.seal("stop")' gateway/` → none.

## Acceptance criteria

- Each item above gone, its callers and tests adjusted; no behaviour change (existing
  assertions unchanged).
- `scripts/check-gateway.sh` green, or reds named with the step that clears them.

## Result

**What changed**

- Control client (control-main F1):
  - `runSealer` returns on `ctx.Done()` without sealing; its comment and the `Run` doc
    now say that usage not delivered when ctx ends is not sent — `FlushUsage`, called
    before, delivers it. Confirmed in `cmd/kaiak/main.go`: `finishWithControlPlane`
    (which calls `FlushUsage(ctx, "drain")`) runs before `stopBackground()`.
  - `get`/`post`/`send` lose the `instance` parameter: `send` sets `Kaiak-Instance`
    from `c.opts.Instance`. `usageSender.instance` is gone; `queueSealed` builds the
    batch ID from `u.c.opts.Instance`. `usageSender.post` returns `error` only (it
    still checks the ack names the batch sent).
- Limiter (routing-limits F1):
  - `Subject.Model` gone (server's `checkLimits` stops setting it).
  - `configChangedLocked` inlined into `sync` (`warnSmallSharesLocked`, with its
    reason as a comment).
  - `firstClosed` gone: `TakeTotals` closes `firstTotals` when `totalsAt` is still
    zero, and `noTotalsLocked` reads `l.totalsAt.IsZero()`. `firstTotals` /
    `FirstTotals()` stay (step 17).
  - One hourly clock: `housekeepLocked` (was `expireRetainedLocked`), run from `sync`,
    prunes the retained counts and the pushed windows on `housekeptAt`;
    `retainedPrunedAt` and `prunedAt` are gone, and `prunePushedLocked` no longer
    keeps a clock of its own nor runs from `TakeTotals` directly (`TakeTotals` syncs
    first, so the hour's first totals still prune).
  - `Usage()` / `CounterUsage` gone, with the "metrics" claim. In their place one
    exported read, `Used(group, typ) (int64, bool)`, documented as read by tests only
    (see Decisions). limits' tests: `used` reads `Used`; the "removed limit still
    counted" check asks `Used` for the removed counter; the eleven `_ = l.Usage()`
    calls (10 in `limits_test.go`, 1 in `shared_test.go`) become a test helper
    `readAll(l)` that does what they relied on — sync, then roll every window to the
    clock.
  - `LiveGateways()` gone: `main` passes `u.Totals.LiveGateways` to
    `router.SetLiveGateways`; each side still clamps to ≥ 1 on its own (`TakeTotals`:
    `max(t.LiveGateways, 1)`; `SetLiveGateways`: `max(min(n, MaxInt32), 1)`).
  - `previousWindow` reads `windowStart(kind, start − 1ns)`. Window starts are always
    UTC hour/month boundaries (the gateway's own, or a pushed start, which the control
    schema checks is the top of a UTC hour / first of a UTC month).
- Router (routing-limits F2, F5):
  - `caps` gone: `hasFreeSlot` uses `r.backends[b.ID]` when the applied config has
    the backend, else the one the request carries; `MaxInFlightByBackend` ranges
    `backends` (its local map is named `shares`).
  - The "never configured" mode gone: `New`'s doc makes `Configure` the precondition
    ("until then it knows no deployment, and tracks no outcome"); `circuit.backend`,
    `probeTarget` (`ProbeNow` reads `r.backends[backend]`) and the two
    `r.deployments != nil` guards are deleted. `New` no longer seeds the default
    circuit-breaker setting — it only served the unconfigured router; `Configure`
    sets it from the snapshot, which carries the defaults.
  - Tests: six cooldown-test routers now call
    `Configure(circuitSnapshot(5, time.Hour, m))` (`TestCoolingDeploymentIsSkipped`,
    both "all cooling" and "a single deployment" subtests,
    `TestQueuedRequestGetsTheDeploymentWhoseCooldownEnds`,
    `TestWaitersGetACoolingDeploymentWhenTheLastOtherCoolsDown`,
    `TestLaterThrottleNeverShortensTheCooldown`); four of them failed without it, as
    expected. The server harness `buildTestGateway` now calls
    `router.Configure(holder.Current())`, as `cmd/kaiak`'s applier does.
- Comments touched in passing: the two `AUDIT-…` references on the two
  `totals_test.go` tests whose `usageSender{instance: …}` literals changed.

**Decisions made during the step**

- `Usage` could be neither unexported nor moved into limits' test files: server's
  tests read counts through it (`counterUsed`, 10 assertions in 5 files:
  `limits_test`, `drain_test`, `queue_test`, `retry_test`, `usage_path_test`). The
  narrowest form that keeps those assertions is one exported `Used(group, typ)`; the
  list API, its struct type and the sort are gone. It is an entry-point method aimed
  at one consumer (tests) — flagged per CODING-RULES §7 for review. The alternative,
  observing the counts through response headers, does not cover hour/month counts.
- F5: "`Configure` is the precondition" rather than "`New` takes the first
  snapshot". It is the smaller change: production already configures from the
  applier before any listener binds, and ~70 `New(Options{…})` call sites (routing,
  metrics, `main_test`, server) would otherwise all change; only the routers that
  report outcomes need a `Configure`.
- F5 precondition check: no production or e2e path reports on an unconfigured router.
  File mode returns on a failed `loader.Load("startup")` and control-plane mode on a
  failed `client.Boot`, both before `server.Listen`; the applier calls
  `router.Configure` on every applied load; e2e runs the binary. The server component
  tests did run on an unconfigured router (the harness never configured it); every
  server test that asserts circuit or cooldown tracking already configured through
  `g.apply`, so they stayed green, and the harness now configures at build so no
  server test runs outside the precondition. Routers that only serve reads
  (`metrics`, `cmd/kaiak` tests: `InFlightByBackend`, `MaxInFlightByBackend`, …) stay
  unconfigured; an unconfigured router reports empty maps there, as before.
- Merging the two prune clocks is not test-visible: the one place pruning moved is
  the order inside `TakeTotals` — the hour's first totals prune at its start (in
  `sync`) rather than after adding their windows, so an already-ended window pushed in
  that message waits for the next hour instead of being dropped at once. An ended
  pushed window never counts (`window.pushed` only reads a base naming the current
  window; `roll` never goes back), so only memory sees it. Pushed windows are now
  also pruned on an hour's first request when no totals come.

**Report vs code** (034329e; code at d0fa3f6)

- control-main F1: as reported; one more site — `queue.go` builds `BatchID.Instance`
  from `u.instance` (now `u.c.opts.Instance`). Line numbers had shifted by 0–3.
- routing-limits F1: "`Usage`/`CounterUsage` (tests only)" holds, but the tests
  include server's, in another package — so unexporting is not available (above).
  `_ = l.Usage()` occurs 11 times (10 + 1).
- routing-limits F5: the report counted 8 cooldown routers built without `Configure`;
  8 `New` calls exist in `cooldown_test.go`, 2 already configured, so 6 needed it.
  The bench already configured (`dispatch_bench_test.go` calls
  `r.Configure(circuitSnapshot(…))`), so it needed nothing. The report did not
  mention the server harness's unconfigured router.
- Step checklist: `git grep -nP '\bcaps\b'` matches nothing on this machine whatever
  the code holds (git's ERE does not honour `\b`); run with `-P` (below).

**Tests** (before → after; names diffed, identical)

| Package | `go test -list` | `-v` `=== RUN` |
|---|---|---|
| `internal/control` | 76 → 76 | 240 → 240 |
| `internal/limits` | 58 → 58 | 80 → 80 |
| `internal/routing` | 50 → 50 | 52 → 52 |
| `internal/server` | 184 → 184 | 432 → 432 |
| `cmd/kaiak` | 29 → 29 | 33 → 33 |

No test deleted; no assertion changed.

**Bench** — `BenchmarkDispatchFullScan` (`go test -run XXX -bench Dispatch
./internal/routing/`, Apple M5 Max):

- Before (3 runs): 5137, 4975, 5020 ns/op. After (3 runs): 6314, 5453, 5516 ns/op.
- The two runs were minutes apart, so both were rebuilt and interleaved (HEAD from
  `git archive`, `-test.cpu 1`, 6 rounds × 3): before mean 5215 ns/op, after mean
  5395 ns/op (18 each; spread 4916–5554 vs 4866–5936). About 3% slower, within the
  run-to-run spread; `hasFreeSlot` now reads the cap through `*config.Backend`
  rather than an `int64` map value.

**Removal checklist**

- `limits.Subject` has no `Model` field (read: `Groups`, `Priced`, `RequestsOnly`).
- `git grep -nE 'configChangedLocked|firstClosed|retainedPrunedAt|\.LiveGateways\(\)' gateway/`
  → none.
- `git grep -nP '\bcaps\b' gateway/internal/routing` → prose only ("caps them per
  backend", "other caps", "their caps", "backend caps" ×2, one test message); no
  identifier (`git grep -nP 'r\.caps|\bcaps\b\s*(:=|\[|=)'` → none).
- `git grep -n 'circuit.backend\|c\.backend' gateway/internal/routing` → none.
- `git grep -n 'u.seal("stop")' gateway/` → none.
- Also none: `prunedAt`, `expireRetainedLocked`, `CounterUsage`, `Usage()`,
  `u.instance`, `probeTarget`, `r.deployments != nil`.

**Suite** — `scripts/check-gateway.sh`:

Green on its own; no red is carried to a later step.

```
==> gofmt / go vet / staticcheck 2026.2.1 (gateway)
==> go test -race -count=1 (gateway): ok cmd/kaiak, e2e (116s), accounting, auth, clip,
    config, control, limits, logattr, metrics, netfail, otlplog, provider, routing,
    schemacheck, server, sse
==> gofmt / go vet / staticcheck (live-test kit)
==> live-test kit self-test: passed for vllm, llama-server, openai, azure-openai,
    anthropic, azure-anthropic, vllm with two backends
gateway checks passed
```

