# Step 27 — routing internals; rejection log attrs

**Status:** done (2026-10-08)

## Intent

"May this attempt use this deployment" is decided in one walk, every circuit transition
reaches the log, the metrics and the status report through one function, and a limit
refusal's log attributes are written by `limits`, which owns them.

## Findings

- routing-limits F3: one `pick(m, avoid) (best config.Deployment, free, usable bool)`
  walks the deployments once (best free warm, best free cooling, any usable) and
  resolves the warm rule at the end; `choose`, `canServe`, `anyUsable`, `anyWarm` become
  it; `Acquire` and `dispatch` read its three results. Note the review's caution about
  `endTrial`'s discarded `dispatch()` result.
- routing-limits F4: under the lock, collect `[]transition{key, to, attrs}`; one
  `r.emit(transitions, changed)` after the unlock writes the line, tells the observer and
  notifies; `endTrial` returns its transition and no longer unlocks the caller's mutex.
- routing-limits F7: `(*Rejection).LogAttrs() []slog.Attr` in `limits`, reusing
  `identityAttrs`; `Scope` becomes a method; `LogValue` unexported; server's
  `limitAttrs` uses it.
- routing-limits hint: export `routing.IDOf(d)` for the hand-built
  `DeploymentID{Backend: d.Backend.ID, Model: d.Model}` copies in `main` and `server`
  (`metrics`' copies are the OTel plan's).

## Files likely touched

- `gateway/internal/routing/{routing,circuit}.go`, `dispatch_bench_test.go`.
- `gateway/internal/limits/headers.go`, `limits.go`; `gateway/internal/server`
  (`limitAttrs`, upstream/attempts).

## Decisions made during planning

- Circuit log lines keep the attributes `GATEWAY.md` lists (circuits section).
- The dispatch bench runs before and after; record both in the Result. A slowdown is a
  finding to report.

## Removal checklist (clean at phase end)

- `git grep -nP 'func \(r \*Router\) (anyWarm|anyUsable|canServe|choose)\b|func LogValue' gateway/` → none.

## Acceptance criteria

- Queue, cooldown, circuit and routing tests pass unchanged; circuit log lines identical.
- `scripts/check-gateway.sh` green, or reds named with the step that clears them.

## Result

**Dispatch bench** (`BenchmarkDispatchFullScan`, `-count 6`, Apple M5 Max)

- Before: 5178–5459 ns/op (5378, 5291, 5178, 5459, 5340, 5323).
- After: 4913–6170 ns/op in a first run (noisy). Run side by side, interleaved, against
  a detached worktree of the step's base (3 × 5 runs each): base 5190–6236 (median
  ≈ 5400), new 4866–5578 (median ≈ 5200). No slowdown; slightly faster. The bench's
  full-scan round asks `pick` once per model; its cost is mostly the queue walk.

**What changed**

- F3 — one eligibility walk:
  - `pick(m, avoid) (best int, usable bool)` walks m's deployments once from the
    model's next turn. It tracks the best free deployment among the warm ones, the
    best free one among the cooling ones, whether any usable one exists, and whether
    any of those is warm. The warm rule is resolved at the end. `best` is an index
    (−1 when none is free), not a `config.Deployment`: the turn (`r.next`) must move
    only when a slot is taken, never when `dispatch` merely asks.
  - `takeTurn(m, at)` moves the model's turn past `at` and takes the slot.
  - `choose`, `usable`, `anyWarm`, `anyUsable` and `canServe` are gone.
    `eligible` takes a `DeploymentID`.
  - `Acquire` makes one `pick` call: `best ≥ 0` takes it; `!usable` →
    `ErrNoHealthyDeployment`.
  - `dispatch` keeps its `firstServable` cache. It now caches `pick`'s index rather
    than a yes/no, and keeps the winner's index with the winner. The grant takes that
    index instead of calling `choose` again: nothing changes between a round's picks
    and its grant, so the result is the same, with one walk fewer per grant.
- F4 — circuit transitions emitted once:
  - `transition{key, to, attrs}` is collected under the lock.
  - `emit(ts, changed)` runs after the unlock. For each transition it writes the log
    line (`circuit opened` warn; `circuit half-open` / `circuit closed` info; backend
    and deployment model first, then the transition's attrs) and tells the observer.
    It then notifies when `changed` or a circuit opened or closed. Half-opening alone
    does not notify, as before.
  - `report` = lock, `reportLocked` (the old body, early returns now plain
    `return`s), unlock, `emit`.
  - `endTrial` returns its transitions and `changed`, and no longer unlocks the
    caller's mutex. `responseStarted` emits what it returns.
  - `ProbeNow` collects `reopened` / `halfOpened` as transitions; its `probe failed`
    / `probe succeeded` lines and the `circuit kept open` lines keep their place
    before the transition lines.
- F7 — rejection log attrs written by limits:
  - `(*Rejection).LogAttrs() []slog.Attr` in `headers.go`.
  - `Rejection.Scope` is now a method (`scopeOf(Group)`); the field is gone, and so is
    `counterKey.scope()`, whose only caller was the rejection literal.
  - `LogValue` → `logValue`.
  - `identityAttrs` returns `[]slog.Attr`, so `LogAttrs` reuses it. Its three
    existing call sites (counter went negative, per-minute share below the default
    output, pushed window ahead) use `logger.LogAttrs` with typed attrs.
  - server: `limitAttrs` deleted. `logRequest` appends `rej.LogAttrs()`; `errLimited`,
    `errBudgetUnavailable` and `metrics.go` call `Scope()`.
- Hint: `keyOf` exported as `routing.IDOf` (renamed throughout routing and its tests).
  `cmd/kaiak/controlplane.go` (`servingStatus`) and `server/attempts.go`
  (`avoidAfter`, 2 sites) use it. `metrics`' two copies are left for the OTel plan.

**Decisions made during the step**

- `pick` returns an index rather than the brief's `(best config.Deployment, free,
  usable bool)`. The index carries "free" (−1) and lets the one caller that takes a
  slot move the turn.
- `endTrial`'s discarded `dispatch()` result (Success): it now feeds `changed`. The
  outcome is the same — a closing circuit always notifies — but nothing is discarded.
- `emit` keeps the old notify rule: a circuit opening or closing notifies; going
  half-open notifies only when the dispatch it allowed emptied a queue (as
  `ProbeNow` did before). Notifying on half-open would add status reports the spec
  does not ask for ("Opening and closing send a status report").
- Log output is unchanged: same message, level and attribute order. The limit log
  values go from `slog.Any` of the named string types (`Scope`, `LimitType`) to
  `slog.String`, and ints to `slog.Int64`. Each handler writes them the same way:
  JSON, text, and the OTLP handler, whose `KindAny` path goes through `jsonText`.
- `limitAttrs` was deleted rather than kept as a one-line wrapper around `LogAttrs`.
- Server/routing tests that hand-build `DeploymentID{…}` were left as they are; they
  check against an independent spelling.

**Report vs code**

- F3: the helpers the review names (`choose`, `eligible`, `usable`, `anyWarm`,
  `anyUsable`, `canServe`) were all there, as described. `caps` and the never-configured mode
  were already gone (step 6), so `hasFreeSlot` reads `r.backends` only.
- F4's sites matched; `ProbeNow` reads `r.backends[backend]` (step 6).
- F7: `limitAttrs` lives in `server/requestlog.go` (step 22/23), not `api.go`.
- The hint's main.go copy is now in `cmd/kaiak/controlplane.go` (step 19).
  `server/upstream.go`'s two copies are in `server/attempts.go` (steps 9/10).

**Test counts** (top-level tests from `go test -list`; in brackets, passes including
subtests)

- `internal/routing`: 48 (51) → 48 (51).
- `internal/limits`: 58 (86) → 58 (86).
- `internal/server`: 189 (462) → 189 (462).
- Changes to tests are renames only: `keyOf` → `IDOf` in routing's tests, and
  `rej.Scope` → `rej.Scope()` (7 sites in limits' tests). Two server
  `Rejection{Scope: limits.ScopeGroup, Group: "research", …}` literals lose the field
  (`Group` gives the same scope). No assertion changed. Queue, cooldown, circuit and
  routing tests pass unchanged; `go test -race -count=3 ./internal/routing/
  ./internal/limits/` ok.

**Removal checklist**

- `git grep --untracked -nP 'func \(r \*Router\) (anyWarm|anyUsable|canServe|choose|usable)\b|func LogValue' gateway/`
  → none.
- Also clean: `limits\.LogValue|limitAttrs` and routing's `keyOf` (limits keeps its
  own unexported `keyOf` for counter keys); `Rejection{…Scope:` and `.Scope` as a
  field. Hand-built `DeploymentID{Backend: x.Backend.ID, …}` outside tests remains
  only in `routing.IDOf` itself and `metrics/ops.go` (2, the OTel plan's).

**Suite**: `scripts/check-gateway.sh` passed (exit 0). `scripts/check-all.sh` was not
run: step 24 was in progress under `control/` in the same worktree.

```
==> gofmt / go vet / staticcheck 2026.2.1 (gateway)
==> go test -race -count=1 (gateway): ok cmd/kaiak, e2e (106s), accounting, auth, clip,
    config, control, limits, logattr, metrics, netfail, otlplog, provider, routing,
    schemacheck, server, sse
==> live-test kit: gofmt / vet / staticcheck; self-test passed for vllm, llama-server,
    openai, azure-openai, anthropic, azure-anthropic, vllm with two backends
gateway checks passed
```
