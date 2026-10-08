# Step 19 — `main.go` split by job

**Status:** done (2026-10-08)

## Intent

`run` keeps the process lifecycle; settings parsing and the control-plane half live in
their own files behind one constructor. Today `main.go` holds four jobs, `run` is ~245
lines and checks the mode at six points.

## Findings

- control-main F6:
  - `settings.go`: env parsing; `readControlSettings` returns a partly filled
    `control.Options` instead of the `controlSettings` mirror; the seed variable is read
    once; the boot-wait and usage-memory defaults are set in one place (the client);
  - `controlplane.go`: `startControlPlane(ctx, stop, deps) (*controlPlane, error)` builds
    the shared limiter and client and boots; methods `run(bgCtx)`, `beforeDrain()`,
    `finish(hurryCtx, deadline)`; it also holds `servingStatus`, `controlState`,
    `ignoreReloads`;
  - `control.Client.Finish(ctx)`: the flush, then the final draining status bounded to
    2 s and skipped when `ctx` was hurried; `FlushUsage` and `ReportStatus` become
    unexported;
  - `run` branches on the mode at three points (setup, drain times, finish).
- control-main small items:
  - one `failureLevel(err)` for the five "log level by error class" copies (keep their
    small differences as explicit cases);
  - `durationMS` takes the `least` parameter `wholeNumber` has; the hand-written
    "= 0: want above 0" checks go; `otlplog/settings.go`'s third millisecond parser
    uses the same rule if it can without importing `main` (else note it).

## Files likely touched

- `gateway/cmd/kaiak/{main,settings,controlplane}.go`, `main_test.go`.
- `gateway/internal/control/{client,usage,status}.go`.

## Decisions made during planning

- No interface for the two modes: two modes do not need one.
- The tests drive `run` and `readSettings` as black boxes; they move only where a helper
  they call moved.

## Removal checklist (clean at phase end)

- `git grep -nP 'type controlSettings|func finishWithControlPlane|\bFlushUsage\b|\bReportStatus\b' gateway/` → none (lower-case forms inside `control` are fine).

## Acceptance criteria

- `main.go` holds the logger, `run` and the lifecycle helpers; `run` under ~150 lines.
- Every `cmd/kaiak` and e2e test passes unchanged.
- `scripts/check-all.sh` green. **Phase 4 ends here.**

## Result

**What changed** (file map)

- `cmd/kaiak/main.go` (730 → 345 lines): `main`, `newLogger`, `run`, and the
  lifecycle helpers `stopOnSignal`, `finishLogExport`, `reloadOnSignal`; plus
  `graph`/`newGraph`, the part of the dependency graph both modes share (holder,
  providers, metrics registry, router, ops, model checker, missing endpoints, and the
  applier with its applied-config hook), moved out of `run` as one block. `run`:
  226 → 157 lines (comments included). It checks the mode at three points: setup
  (file loader + local limiter + SIGHUP reload, or `startControlPlane`), drain times
  (`Reserve` + `cp.beforeDrain()`), finish (`cp.finish(hurryCtx, drainDeadline)`).
  The `kaiak starting` source is `settings.configSource()`; the recorder takes a
  `Batcher` set only in control-plane mode (a nil interface in file mode); background
  work starts through one `goBackground(func(ctx))`.
- `cmd/kaiak/settings.go` (new, 245 lines): `settings`, the defaults, `readSettings`,
  `configSource`, `readControlSettings`, `readSeed`, `wholeNumber`, `durationMS`.
  `settings.control` is a `*control.Options` with `URL`, `Token`, `BootWait`,
  `SeedConfig`, `SeedFile` set; `controlSettings` and the `defaultBootWait` alias are
  gone. `KAIAK_SEED_CONFIG_FILE` is read once in `readSettings` and handed to
  `readControlSettings`.
- `cmd/kaiak/controlplane.go` (new, 220 lines): `controlPlane{client, limiter}`,
  `controlPlaneDeps`, `startControlPlane(ctx, stop, deps)` (the shared limiter, the
  client with `Serving`/`Observer`/`OnTotals`, the control-state metrics, the
  serving-change hook, `Boot`, then `Run` in the background and the first-totals
  wait, as before), the methods `run(ctx)`, `beforeDrain()`, `finish(hurry,
  deadline)`; `bootStopped` (a stop signal during the boot); `waitFirstTotals`,
  `ignoreReloads`, `controlState`, `servingStatus`.
- `internal/control`: `Client.Finish(ctx)` (`client.go`) — `flushUsage(ctx,
  "drain")`, then, unless `ctx` was cancelled, `reportStatus` on its own 2 s
  timeout (`finalStatusTimeout` moved here). `FlushUsage` → `flushUsage`,
  `ReportStatus` → `reportStatus`. `failureLevel(err, refusals)` (`transport.go`)
  replaces the five level-by-error copies (boot retry, `logFetchFailure`,
  `logStreamEnd`, usage `configProblem` — gone — and the status report's
  warn-or-debug choice).
- Tests: the four settings tests (`TestDrainTimeSettings`,
  `TestClientTimeoutSettings`, `TestListenAddressDefaults`,
  `TestControlModeSettings`) moved from `main_test.go` to `settings_test.go`;
  `TestServingStatusCoversTheAppliedConfig` to `controlplane_test.go`. Bodies
  unchanged but for `TestControlModeSettings` reading `s.control.URL/Token/BootWait`
  (the fields of the type that replaced the mirror; same values asserted). Control
  tests call `flushUsage`/`reportStatus`. New
  `TestFinishFlushesThenReportsDrainingUnlessHurried` (`status_test.go`).
- `docs/DEPLOYMENT.md`: the gateway variable table points at
  `gateway/cmd/kaiak/settings.go` instead of `main.go`.

**Decisions made during the step**

- **The boot wait and usage memory defaults stay filled by `readSettings`**, from
  `control.DefaultBootWait` and `control.DefaultUsageMemoryBytes` (the values are
  defined once, in `control`); `New` keeps its zero → default for its other callers
  (server and control tests). `TestControlModeSettings` asserts a 60 s boot wait and
  `TestUsageMemoryAndConnectionSettings` 64 MiB from `readSettings`, and
  `startControlPlane` needs the effective boot wait for the first-totals deadline, so
  leaving them zero until `New` would have weakened two assertions (decision 17).
  `main`'s `defaultBootWait` alias is gone.
- **`run` starts the client's `Run` through `deps.goBackground`.** With step 17 not
  taken, `Run` must run before the first-totals wait, inside the boot; so
  `startControlPlane` launches `cp.run` on run's background group itself, and a
  stop signal during the boot returns `bootStopped` — `run` logs `kaiak stopped`
  with its text (`signal terminated during boot`, as before) and returns nil.
  `waitFirstTotals` and the boot deadline stay in `main` (`controlplane.go`): moving
  them into the client is step 17's shape, which was not taken.
- **`Finish(ctx)` reads "hurried" as `ctx` cancelled** (`errors.Is(ctx.Err(),
  context.Canceled)`): `cp.finish` passes `context.WithDeadline(hurryCtx,
  drainDeadline)`, so a second stop signal or a cancelled run ctx cancels it, while
  the drain deadline passing gives `DeadlineExceeded` and the status is still sent —
  step 16's semantics. One edge differs and is unreachable: a run ctx that itself
  ends by a deadline would count as not hurried (step 16 skipped the status on any
  `hurry.Err()`); `main` passes `context.Background()` and no test passes a ctx with a
  deadline. The new control test pins both branches; mutation checks: without the
  hurry check, and with `ctx.Err() != nil` as the check, it fails.
- **`failureLevel(err, refusals)`** with two named values, `refusalWarns` (boot and
  stream) and `refusalIsError` (usage and status): the 4xx difference is real — a
  refused stream (401) logs at warning today and an unified rule would raise it to
  error. `errMalformedTotals` is in the common rule: only the stream can produce it,
  so the other callers see no change. Levels per error are as before at every site.
- **`durationMS(…, least)`**: a value below 0 or unparsable keeps `"%s=%q: want a
  whole number of milliseconds, 0 or more"`; a whole number below `least` gives
  `"%s=%d: want a whole number of milliseconds above %d"` with `least-1`, which is
  the old hand-written `=0: … above 0` text for `least` 1, byte for byte (checked for
  `KAIAK_IDLE_TIMEOUT_MS=0`, `KAIAK_WRITE_TIMEOUT_MS=00`, `KAIAK_CONTROL_BOOT_WAIT_MS=0`,
  `-1`, `x`).
- **`otlplog`'s millisecond parser is left alone**: it already applies the "above 0"
  rule (`ms <= 0` with one "above 0" message), and sharing `durationMS` would need a
  package both import — not worth one caller.
- **`newGraph`** was not in the plan: without it `run` was 187 lines. The shared
  graph is construction, not lifecycle, and moves out as one block unchanged.

**Report vs code** (034329e line numbers; code at aefa3df)

- control-main F6: as reported, with the mode checks at six points (log source,
  setup, batcher, SIGHUP, drain times, finish) → three in `run` (source moved to
  `settings.configSource`, batcher and SIGHUP into the setup branch). The report's
  "run shrinks by about 100 lines": 226 → 157 with `newGraph`; the control-plane
  half is bigger than the report assumed because step 17 kept the boot's totals
  wait in `main`.
- Small items: five copies found as listed (`client.go` boot retry,
  `logFetchFailure`, `logStreamEnd`; `usage.go` `configProblem`; `status.go`). The
  guard item was step 8's. `durationMS`: the hand-written checks were at the
  client bounds loop and `KAIAK_CONTROL_BOOT_WAIT_MS`, as reported.

**Tests** (`go test -count=1 -v`, `--- PASS`, subtests included; before → after, no
skips, no fails)

- `cmd/kaiak` 32 → 32 (5 tests moved file, none changed in what it asserts);
  `internal/control` 240 → 241 (+1: `TestFinishFlushesThenReportsDrainingUnlessHurried`).
- `go test -race -count=3 ./cmd/kaiak/ ./internal/control/`: ok (6.8 s, 22.5 s).

**Line counts**: `main.go` 730 → 345; `settings.go` 245; `controlplane.go` 220
(`cmd/kaiak` non-test 730 → 810: file headers, `graph`, `controlPlaneDeps`,
`bootStopped`). `control` non-test: `client.go` 523 → 534, `usage.go` 526 → 512,
`status.go` 209 → 210, `transport.go` 165 → 192.

**Phase-4 removal checklists** (`git grep`)

- Step 15 — `func limitsTotals|func controlContact|testLimitsTotals` → none;
  `UsageWaitingSince|UsageUncountedSince` → only the `limits.Contact` fields, their
  use in `shared.go`/`shared_test.go`, `LimitsContact` filling them and
  `totals_test.go` reading them (as accepted at step 15); `type TotalsWindow` → none.
- Step 16 — `hurry\s*(:)?=\s*make\(chan|close\(hurry\)|endSignalWatch` in
  `gateway/cmd/kaiak` → none.
- Step 17 — not taken; checklist skipped.
- Step 18 — `\.pushed\b|prunedAt|prunePushedLocked|effectiveLimit` in
  `gateway/internal/limits` → none.
- Step 19 — `type controlSettings|func finishWithControlPlane|\bFlushUsage\b|\bReportStatus\b`
  in `gateway/` → none. Also `configProblem`, `defaultBootWait` → none; docs
  (outside plans and reviews) name none of the removed identifiers.

**Suite**: `scripts/check-all.sh` passed (exit 0) on the final code. Phase 4 ends
green.

```
==> gofmt / go vet / staticcheck 2026.2.1 (gateway)
==> go test -race -count=1 (gateway): ok cmd/kaiak, e2e (107s), accounting, auth, clip,
    config, control, limits, logattr, metrics, netfail, otlplog, provider, routing,
    schemacheck, server, sse
==> live-test kit: gofmt / vet / staticcheck; self-test passed for vllm, llama-server,
    openai, azure-openai, anthropic, azure-anthropic, vllm with two backends
gateway checks passed
==> npm test (control): 617 tests, 616 pass, 0 fail, 1 skipped
==> npm run lint (control)
==> cross-half e2e: ok kaiak/e2e 65.3s
all checks passed
```
