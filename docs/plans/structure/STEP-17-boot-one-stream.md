# Step 17 — one stream per boot

**Status:** not taken (user, 2026-10-08) — the boot keeps two streams

## Intent

Boot keeps the stream it opened through the first totals and hands it to `Run`. Today
every boot opens two streams (the first closed once the config arrives, the second
re-sends the same config, skipped by hash), the stream reader has a `firstOnly` mode,
and the first-totals wait is split over `main.waitFirstTotals`, a second copy of the
boot deadline and `limits.FirstTotals`.

## Findings

- T5 item 3, control-main F4:
  - Boot reads to the first config and applies it; for a control-plane config it keeps
    reading until the first totals have gone through `OnTotals`, or until the boot
    deadline; it logs the totals-wait lines;
  - it hands the open stream (response, `sse.Reader`, start time, first-totals flag) to
    `Run`, whose config follower resumes it before reconnecting;
  - `followStream`'s `firstOnly` parameter and `streamResult.first` go;
    `waitFirstTotals` and `main`'s `bootDeadline` go; `limits.firstTotals` /
    `FirstTotals()` go.

## Files likely touched

- `gateway/internal/control/{client,stream}.go`, `client_test.go`, `seed_test.go`.
- `gateway/internal/limits/{limits,shared}.go`.
- `gateway/cmd/kaiak/main.go`, `main_test.go`.
- `docs/specs/GATEWAY.md` (Control-plane mode → Boot / Readiness text and the boot
  diagram, where they mention the second stream).

## Decisions made during planning

- **Spike first** (decision 13): resume an already-open stream in the config follower.
  If the hand-off needs more than a small struct and one resume path, stop and report
  with what was found; the plan then keeps today's shape and records why.
- Behaviour to keep: no config published (stream open, nothing sent) ends at the boot
  deadline with `errNoConfigYet`; a stop signal during boot still cancels the totals
  wait; the seed fallback and its exits are unchanged.

## Removal checklist (clean at phase end)

- `git grep -nP 'firstOnly|streamResult\)?\.first\b|waitFirstTotals|bootDeadline|FirstTotals\(' gateway/` → none.

## Acceptance criteria

- A test counts one opened stream for a boot that gets config then totals (the existing
  `bootStreams` counters change from 2 to 1).
- Boot, seed, readiness and stateless tests pass; e2e startup tests pass.
- `scripts/check-gateway.sh` green, or reds named with the step that clears them.

## Outcome

**Not taken** (user, 2026-10-08). The spike's result below works and passed the full
suite, but it fails decision 13's bar: handing an open stream from Boot to `Run` did
not stay simple. Each stream request had to run on a detached context with its own
cancel, opening grew three timers, the totals wait moved into the client as
`WaitForTotals`, and production Go grew by ~100 lines where the review expected ~60
fewer. A second connection per boot costs little next to that. The code stays as it
was: two streams per boot, `followStream`'s `firstOnly`, `main.waitFirstTotals` and
`limits.FirstTotals`. `GATEWAY.md` is unchanged. The work was on a side branch
(`structure-step17-one-stream`, deleted); the Result below records what it found.

## Result (of the spike, not merged)

**Spike verdict: GO, with one deviation.** The step's bar holds: the hand-off is one
`*configStream` (body, `sse.Reader`, request context and its cancel, idle timer, start
time, first-totals flag) that Boot leaves in `Client.booted`, and `followConfig`'s
first `followStream(ctx, st)` resumes it before any reconnect; the reader has one mode.
Two things the report's sketch did not show, and that shaped the result:

- **The request outlives the context that opened it.** An HTTP request is bound to its
  context for life, and Boot's context ends at the boot deadline and on a stop signal.
  So every stream's request runs on `context.WithoutCancel(ctx)` with its own cancel;
  the opener's ctx bounds only the opening, and whoever reads the stream ends it with
  their own context (`watch`, a `context.AfterFunc`).
- **Boot cannot also hold the totals wait.** Boot must return at the boot deadline
  while the stream stays open and silent; a blocked `sse.Reader.Next` can only be
  interrupted by cancelling the request. Holding the wait inside Boot would need a
  goroutine reading the stream from Boot onward — Run's follower started early — and
  a stream that ends between the config and the totals must still be reconnected
  within the wait, as today, which only the follower's loop does. So the totals wait
  is `Client.WaitForTotals(ctx)`, which `main` calls right after starting `Run`; Run
  reads the totals on the boot's stream. This is the one deviation from the
  findings' "Boot keeps reading until the first totals" (decided below).

**What changed**

- `control/stream.go`: `followStream(ctx, firstOnly)` → `openConfigStream(ctx)` (the
  opening bound and the stream's own context), `configStream.watch(ctx)`,
  `closeStream`, `nextBlock` (read-error classes, contact, idle reset — shared by both
  readers), `followStream(ctx, st *configStream)` (resumes `st`, or opens one when
  nil) and `takeBlock` (config → apply path, totals → consumer). `streamResult.first`
  and the `firstOnly` branches are gone.
- `control/client.go`: `Boot` sets `bootEnd` (the one boot deadline), takes the
  stream and config from `bootConfig`, applies it and leaves the stream in `booted`;
  on any failure, or a cancelled ctx, it closes the stream. `bootConfig` returns the
  open stream; `firstConfig` (new) opens a stream and reads it to its first config
  event, handing other blocks to `takeBlock`. `followConfig` follows `booted` first
  (closing it even when ctx has ended). `takeTotals` closes `totalsTaken` after the
  first `OnTotals` call. `WaitForTotals(ctx)` is `main`'s `waitFirstTotals` moved: the
  same three lines, bounded by `bootEnd`, a seed boot waiting for nothing.
- `cmd/kaiak/main.go`: `bootDeadline`, `waitFirstTotals` and the seed check go; boot
  is `Boot`, `go Run`, `WaitForTotals(bootCtx)` (the stop-signal context of step 16,
  so a stop still ends the wait).
- `limits`: `firstTotals`, its close in `TakeTotals`, and `FirstTotals()` go.
- Docs: `GATEWAY.md` → Boot gains "One stream per boot (settled 2026-10-08)" with the
  rejected two-stream shape; Readiness says a stream that ends before the totals is
  reconnected within the wait. `docs/architecture/control-plane.html`: the boot
  sequence loses the second `GET /v1/stream` and the skipped config; the caption says
  one stream per boot. `DEPLOYMENT.md`, `gateway.html` and `ARCHITECTURE.md` describe
  behaviour that stays true: unchanged.

**Decisions made during the step**

- **The totals wait is a client method `main` calls, not part of Boot** — the reason
  above. It still leaves `main` (with its deadline copy) and lives with the signal it
  waits on; step 19's `startControlPlane` can call it as one line. Its log lines are
  the client's, so they now carry `kaiak.control.url` like every client line
  (`GATEWAY.md`, Logs: "every line of the control-plane client"); messages and the
  `totals_wait` / `totals_waited` attributes are unchanged. Named `WaitForTotals`:
  `WaitFirstTotals` would match the checklist's `FirstTotals\(`.
- **The first-totals signal is the client's** (`totalsTaken`, closed after the first
  `OnTotals` call): "the first totals went through `OnTotals`" is the client's fact,
  and it spans reconnects.
- **A stream that ends between the config and the totals: today's behaviour kept.**
  Run's follower logs the end, reconnects with backoff, and the totals wait goes on
  until the boot deadline; totals on the new stream end it
  (`TestTotalsWaitEndsWithTheBootWait/the boot's stream ended before them`).
- **Blocks before the first config go to `takeBlock`.** The boot reader used to skip
  totals before a config; on one stream, skipping them would mark the next totals
  complete when they are not. The protocol sends none before a config, so this
  changes nothing a conforming control plane can see.
- **The boot deadline hitting as the config arrives**: the config is applied as
  before; the stream, already cancelled, is closed rather than handed over, and Run
  opens a new one.
- **Opening errors keep their text.** A cancel through a `CancelCauseFunc` reads as
  "cancelled"; when the opener's ctx ended the opening, the error is built from its
  cause, so a boot deadline during the opening still logs `open config stream: timed
  out`.

**Report vs code** (034329e line numbers; code at a01351f)

- control-main F4: as reported (`followStream` at `stream.go:46`, `bootConfig`,
  `waitFirstTotals`, `bootDeadline`, `limits.firstTotals`). The report's "about 60
  lines removed over 3 packages" did not hold: non-test Go went +259/−161 lines
  (code without comments or blank lines: +193/−120). The growth is the detached
  request context and its watch, the separate first-config reader, and
  `WaitForTotals` (moved, ~35 lines, net zero across files). The payoffs that held:
  one connection per boot, one reader mode, one boot deadline, no `FirstTotals`.
- The report's hint that `FirstTotals` is "used only by `waitFirstTotals`": two tests
  also read it — `limits/shared_test.go` (`TestNoTotalsYetRefusesMoneyLimitedModels`,
  two selects asserting the channel's state: removed with it, decision 16; the rest
  of the test stands) and `server/usage_path_test.go`
  (`TestUsageAcksFailingPastTheGraceRefusePricedBudgets`, a wait for the first
  totals: it now waits on the test's own `OnTotals` channel, `g.counted`, filled
  after `limiter.TakeTotals` — the same event).

**Tests**

- New: `TestBootHandsItsStreamToRun` (one stream opened for a boot that gets config
  then totals; the totals complete; the config not received twice; the wait ends with
  the totals and logs both lines); `TestTotalsWaitEndsWithTheBootWait` (totals held →
  the warning at the deadline, late totals still the stream's first; the boot's
  stream ended before the totals → the reconnect's totals end the wait; a seed boot
  waits for nothing).
- Changed for one stream (the intended change): the client harness's `boot` no longer
  counts the stream it hands to Run, so `nextStream` returns it;
  `TestStreamAppliesUpdatesAndIgnoresHeartbeats` asserted the booted config was
  received again and skipped — now it asserts it is not received again, and
  `TestReconnectTakesTheCurrentConfig` asserts the skip line on a real reconnect;
  `TestReconnectDelaysGrowWhileDownAndResetAfterAHealthyStream` closes the boot's
  stream after `SetDown` (it used to end with the boot); `TestStreamOpenIsBounded`
  calls `followStream(ctx, nil)`; `totals_test.go`'s bare `Client` literal gets the
  channel. e2e: `control_test.go` and `stateless_test.go` wait for 1 `config stream
  connected` instead of 2, and `control_test.go` asserts exactly one after a push.
- Counts (`go test -count=1 -v`, `--- PASS`; before → after, no skips, no fails):
  `internal/control` 240 → 245 (+2 tests, +3 subtests); `internal/limits` 85 → 85;
  `cmd/kaiak` 32 → 32; `internal/server` 457 → 457.
- `go test -race -count=5 ./internal/control/ ./cmd/kaiak/`: ok (40.9 s, 10.7 s).

**Removal checklist** (`git grep -nP 'firstOnly|streamResult\)?\.first\b|waitFirstTotals|bootDeadline|FirstTotals\(' gateway/`)

- One hit, a false positive: `e2e/startup_test.go:71`
  `func TestReadinessWaitsForTheFirstTotals(t` — the name of the D8 readiness test,
  which stays (decision 17). No code name matches.

**Suite**: `scripts/check-all.sh` passed (exit 0) on the final code.

```
==> gofmt / go vet / staticcheck 2026.2.1 (gateway)
==> go test -race -count=1 (gateway): ok cmd/kaiak, e2e (108s), accounting, auth, clip,
    config, control, limits, logattr, metrics, netfail, otlplog, provider, routing,
    schemacheck, server, sse
==> live-test kit: gofmt / vet / staticcheck; self-test passed for vllm, llama-server,
    openai, azure-openai, anthropic, azure-anthropic, vllm with two backends
gateway checks passed
==> npm test (control): 617 tests, 616 pass, 0 fail, 1 skipped
==> npm run lint (control): boundaries ok
==> cross-half e2e: ok kaiak/e2e 65.3s
all checks passed
```

