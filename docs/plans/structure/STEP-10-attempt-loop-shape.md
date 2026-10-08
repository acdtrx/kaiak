# Step 10 — the attempt loop's shape

**Status:** done (2026-10-08)

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

## Result

**What changed**

- S6: the error class is set where the error is built.
  - `apiError` gains `class metrics.ErrorClass`. Every literal sets it: the
    constructors in `errors.go`, plus `drain.go` (`errShuttingDown`,
    `errConfigNotLoaded`) and `limits.go` (`errLimited`, `errBudgetUnavailable`).
  - `errRefused` classes every provider refusal as `invalid_request`, whatever its
    code. Today's codes are `price_option_unsupported` and `duplicate_member`.
  - Upstream answers take their class from the rule row:
    `upstreamAnswer(status, code, class, message)`, one class per `failureRules` row.
    `errorEventRules`' answers use `errUpstreamFault`, `errUpstreamOverloaded` and
    `errUpstreamRefused`, which set their own class. It equals the row's `class`.
  - `errorClass` reads `rq.failure.class`. `errorCodeClass` and its silent `internal`
    default are gone.
  - `TestEveryErrorCodeHasItsClass` (`metrics_test.go`) is rewritten.
    - `errorAnswers()` is a table of every constructor's answers: each constructor
      once per code it can answer with, and every `failureRules` and
      `errorEventRules` row.
    - Each answer's `(code, class)` must match the spec map. The spec map keeps its
      old content, code by code.
    - Every code in the GATEWAY.md Client API table must be answered by some
      constructor. The map and the table must cover each other exactly.
    - `apiErrorConstructors` parses the package's non-test files (`go/parser`). Every
      function or package-level variable that builds an `apiError` literal must be
      listed in `errorAnswers`.
    - Checked by mutation, then reverted: a wrong class on `errServerBusy` and an
      unlisted `errProbe` constructor each failed the test with a message naming it.
- S7: the attempt loop is a struct.
  - `type attempts struct{router, recorder, providers, budget, missing, logger}`.
  - `run(ctx, rq)` is the stage (formerly `sendAttempts`, 8 parameters).
  - The methods are `send(ctx, rq, at)` (formerly `sendAttempt`, 6 parameters),
    `settle(rq, at)` (formerly `rq.settleAttempt(at, recorder)`) and `end(rq)`
    (formerly `rq.endAttempt(recorder)`).
  - `newPipeline` builds it once (`loop := &attempts{…}`); the stage entry is
    `loop.run`.
  - `releaseAttempt`, `retryRefused`, `routingRefusal` and the classification needed no
    collaborator and are unchanged.
- S8: stage applicability is declared once.
  - `stage{name, run, scope}`, with `stageScope` = `everyRequest`, `bodyRequests` or
    `modelRequests`. `scope.covers(ep)` holds the one `takesBody()` decision.
    `API.serve` skips a stage the scope does not cover.
  - The four `takesBody()` guards are gone: `readInbound`, `applyModelParams`,
    `checkLimits` and the attempts stage.
  - `authorizeModel`'s body-only half moved to its own `bodyRequests` stage,
    `endpoint_support` (`findServingDeployments`), right after `model_access`.
  - `models` is `modelRequests`, so it no longer runs as a no-op after body requests.
  - Stage list before/after:
    - Before, every request ran all 9 stages: admission, auth, key_concurrency,
      inbound, model_access, model_params, limits, attempts, models.
    - After, a model endpoint runs admission, auth, key_concurrency, model_access and
      models. Those are exactly the stages that did anything for it before, in the
      same order.
    - After, a body endpoint runs admission, auth, key_concurrency, inbound,
      model_access, endpoint_support, model_params, limits and attempts. This is the
      same work in the same order.
    - Still one pipeline. The model-endpoint tests (`visibility_test`,
      `endpoints_test`, `server_test`, `keylimit_test`, `drain_test`) pass unchanged.
- S9: a pure move. `upstream.go` is gone, and `api.go` keeps the handler, mux, request
  IDs and `statusWriter`.
  - `attempts.go`: `attempt`, `attempts`, `run`, the throttle-cooldown constants and
    `throttleCooldown`, `servingDeployments`, `serves`, `send`, `retryable`,
    `attemptRules`, `errorEventKind`, `avoidAfter`, `routingRefusal`, `retryRefused`,
    `canceledAnswer`, `lastAttempt`, `answeringAttempt`, `dropResponse`,
    `releaseAttempt`, `settle`, `end`, `classifyAttempt`, `failureRule`,
    `failureRules`, `errorEventRules`, `failureRuleOf`, `providerEndpoint`,
    `upstreamFailure`.
  - `relay.go`: `relayResponse`, `peekedResponse` and its `Next`,
    `answerBackendFault`, the `relay*` constants, `upstreamRelayEnd`,
    `endRelayCanceled`.
  - `requestlog.go`: `logRequest`, `methodAttrs`, `providerName`, `limitAttrs`,
    `triedAttempts` (from `api.go`), and `attemptOutcome` (from `upstream.go`; the log
    line is its only reader).
  - Verified pure: the 55 top-level declarations of the old `upstream.go` + `api.go`,
    doc comments included, are byte-identical to those of the four new files. Only the
    import blocks differ.
- Comments now state the scope instead of the guards (`answerModelEndpoint`,
  `readInbound`, `newPipeline`, `authorizeModel`).
- No spec change. GATEWAY.md's pipeline already lists the endpoint-support check right
  after model access.

**Code → class mapping, before vs after**

- Captured before the change with a throwaway test, on the old code through
  `errorCodeClass`. Captured after through `apiError.class`.
- The capture covered every constructor answer (the `errorAnswers` list; 41 distinct
  `(status, code, class)` lines) and every code of the GATEWAY.md table (39).
- The two sorted captures (80 lines each) are **identical**. Every client error answer
  has the same code, status and class as before.

**Decisions made during the step**

- **Stage scope is a three-value `stageScope`**, not a `bodyOnly` bool. The models stage
  needs the opposite applicability, and one type states both.
- **`endpoint_support` is a new stage name**, split from `model_access`. It is the only
  way the body-only half loses its guard. Behaviour and order are unchanged, and stage
  names appear nowhere outside the list.
- **`errorEventRules` keeps its `class` column** for a stream an error event ended
  mid-relay. Its first-event answers carry the same class from their constructors.
- **The parity test also scans the package's source** for `apiError` literals. Without
  that scan, a new constructor of an existing code with a wrong class could go
  unlisted.
- **S9 is kept separable for review.** The index holds S6–S8 (staged), and the working
  tree adds the S9 move (unstaged: `upstream.go` deleted, three new untracked files,
  `api.go` changed). Commit the index first, then the rest. This step file is
  unstaged.

**Report vs code** (034329e; code at 2afc3f4)

- S6:
  - `errorCodeClass` was at `metrics.go:133-176`, not `:136-177`.
  - `errUpstream` was already gone (step 9 put `upstreamAnswer` in the rule rows).
  - The constructor count: 39 `apiError` literals across `errors.go` (35), `drain.go`
    (2) and `limits.go` (2), as the report's "about 35" suggests.
- S7:
  - `sendAttempts` had 8 parameters and `sendAttempt` 6.
  - `settleAttempt` and `endAttempt` were `request` methods taking `recorder`.
  - `NewAPI` has 10 parameters, not 9. It is unchanged: S7 is about the loop.
- S8:
  - The guards were at `inbound.go:44`, `params.go:26`, `limits.go:27`, `upstream.go:67`
    and `pipeline.go:284`.
  - The report's `pipeline.go:290` is the `namesModel()` check, which stays: list
    models names no model.
- S9: `upstream.go` was 797 lines (report: 783). `api.go`'s log line was lines 159–327.
- **Removal checklist item 2 is not literally clean.** `takesBody()` also appears in
  `requestlog.go:41`: `rq.modelAllowed && rq.endpoint.takesBody()` decides whether the
  log line carries `gen_ai.request.stream`. That is a log-field condition, not a stage
  guard. Replacing it would either change the log line or route it through
  `stageScope` cosmetically, so it stays. Flagged for review.

**Tests** (before → after)

| Package | `go test -list` | `-v` `=== RUN` |
|---|---|---|
| `internal/server` | 187 → 187 | 456 → 456 |
| `internal/metrics` | 12 → 12 | 12 → 12 |

- `TestEveryErrorCodeHasItsClass` was rewritten in place, with its spec map unchanged.
- No test was added or deleted, and no other assertion changed.

**Removal checklist** (step 10)

- `git grep -n 'errorCodeClass' gateway/` → none.
- `git grep --untracked -n 'takesBody()' gateway/internal/server` matches 4 lines:
  - `pipeline.go:112`, the definition;
  - `pipeline.go:239,241`, `stageScope.covers` (the stage list's check);
  - `requestlog.go:41`, the log-line field condition (see Report vs code).

**Phase 2 removal checklists** (steps 6–10, run on the final tree with
`git grep --untracked`, `-P` where `\b` is used)

- Step 6:
  - `limits.Subject` has no `Model` field.
  - `configChangedLocked|firstClosed|retainedPrunedAt|.LiveGateways()` → none.
  - `\bcaps\b` in routing → prose and one test message only, as step 6 recorded.
  - `circuit.backend|c.backend` → none.
  - `u.seal("stop")` → none.
- Step 7:
  - Item 1 → the same 10 false positives step 7 recorded (`Identity.AllowedModels()`,
    `groupDoc.Parent`, `doc.Keys[id]`, `ModelSet) Allows`).
  - `Fanout|OutOfRange` → none.
  - `DecodeUsageBatch|DecodeStatus` → `_test.go` only.
- Step 8:
  - `starting` → no status-state use. The same other-meaning lines step 8 listed.
  - `.totals(|countedThrough|PAGE_READER` → none.
  - `control/sample/src/index.ts` is not tracked.
- Step 9: both greps → none.
- Step 10: as above.

**Suite** (phase end): `scripts/check-all.sh` passed (exit 0) on the final tree.

```
==> gofmt / go vet / staticcheck 2026.2.1 (gateway)
==> go test -race -count=1 (gateway): ok cmd/kaiak, e2e (107s), accounting, auth, clip,
    config, control, limits, logattr, metrics, netfail, otlplog, provider, routing,
    schemacheck, server, sse
==> live-test kit: gofmt / vet / staticcheck; self-test passed for vllm, llama-server,
    openai, azure-openai, anthropic, azure-anthropic, vllm with two backends
gateway checks passed
==> npm test (control): tests 615, pass 614, fail 0, skipped 1
==> npm run lint (control): boundaries ok
==> cross-half e2e: ok kaiak/e2e 66s
all checks passed
```

Two comments (`newPipeline`'s and `attempts`') were reworded while the suite ran.
`gofmt -l` and `go vet` were clean after the edits.
