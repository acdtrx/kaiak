# Step 6 — gateway leftovers: control client, limits, routing

**Status:** not started

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
- `git grep -nE '\bcaps\b' gateway/internal/routing` → none.
- `git grep -n 'circuit.backend\|c\.backend' gateway/internal/routing` → none.
- `git grep -n 'u.seal("stop")' gateway/` → none.

## Acceptance criteria

- Each item above gone, its callers and tests adjusted; no behaviour change (existing
  assertions unchanged).
- `scripts/check-gateway.sh` green, or reds named with the step that clears them.
