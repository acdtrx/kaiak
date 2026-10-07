# Step 27 — routing internals; rejection log attrs

**Status:** not started

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

- `git grep -nE 'func \(r \*Router\) (anyWarm|anyUsable|canServe|choose)\b|func LogValue' gateway/` → none.

## Acceptance criteria

- Queue, cooldown, circuit and routing tests pass unchanged; circuit log lines identical.
- `scripts/check-gateway.sh` green, or reds named with the step that clears them.
