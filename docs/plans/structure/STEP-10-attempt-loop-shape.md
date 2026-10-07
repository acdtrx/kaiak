# Step 10 — the attempt loop's shape

**Status:** not started

## Intent

With the attempt's result in one place (step 9), give the attempt loop the shape that
keeps it there: errors carry their class, the attempts stage is a struct, stage
applicability is declared once, and `upstream.go` splits into its two jobs.

## Findings

- server S6: `apiError` carries `class metrics.ErrorClass`, set where the error is
  built; upstream answers take it from step 9's table row; provider refusals are
  `invalid_request`; `errorCodeClass` and its silent `internal` default go. The
  spec-parity test collects the `(code, class)` pairs the constructors produce.
- server S7: `type attempts struct{router, recorder, providers, budget, missing, logger}`
  with `run(ctx, rq)` as the stage and `send`, `settle`, `end` as methods.
- server S8: `stage{name, run, bodyOnly}` (or a models flag) checked once in `API.serve`;
  the five `takesBody()` guards go; the models stage no longer runs as a no-op after
  every body request. Still one pipeline: model endpoints pass admission, auth, key
  concurrency and model access as now.
- server S9: `attempts.go` (loop, retry policy, classification and its table,
  settlement), `relay.go` (relay, peeked response, backend-fault answer, relay-end
  reasons), `requestlog.go` (the log line from `api.go`).

## Files likely touched

- `gateway/internal/server/{upstream,pipeline,api,errors,metrics,limits,drain,inbound,params,models}.go`,
  new `attempts.go`, `relay.go`, `requestlog.go`; `metrics_test.go`.

## Decisions made during planning

- S9 is a pure move and lands last in the step, in its own commit, so the diff of the
  earlier changes stays readable.
- The GATEWAY.md error table is unchanged; the parity test still ties it to the code.

## Removal checklist (clean at phase end)

- `git grep -n 'errorCodeClass' gateway/` → none.
- `git grep -n 'takesBody()' gateway/internal/server` → only the stage list's check.

## Acceptance criteria

- Every client error answer has the same code, status and class as before (the parity
  test, rewritten, covers all of them).
- No behaviour change; `scripts/check-all.sh` green. **Phase 2 ends here**: the removal
  checklists of steps 6–10 are clean.
