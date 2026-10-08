# Step 16 — "hurry" as a context

**Status:** done (2026-10-08)

## Intent

"Stop waiting: a second stop signal came" is one context, not a channel watched by three
hand-written goroutines plus a near-copy for boot.

## Findings

- control-main F3:
  - `hurryCtx, endHurry := stopOnSignal(ctx, stop, onSignal)` reuses the boot helper,
    with a log callback for the "second stop signal" line;
  - `drain.Run(api, times, hurryCtx.Done(), logger)` (`server.Drain.Run` unchanged);
  - `finishWithControlPlane`: `context.WithDeadline(hurryCtx, drainDeadline)`, then
    `FlushUsage`, then return if hurried, then the final status — no goroutine;
  - `finishLogExport` cuts its deadline with `context.AfterFunc(hurryCtx, …)`;
  - before a drain starts, `context.Background()` instead of a nil channel;
  - the `ctx.Err() != nil → close(hurry)` special case goes.

## Files likely touched

- `gateway/cmd/kaiak/main.go`.

## Decisions made during planning

- Step 19 later moves the control-plane finish into `Client.Finish`; this step keeps it
  in `main` so each step's diff stays one idea.

## Removal checklist (clean at phase end)

- `git grep -nE 'hurry\s*(:)?=\s*make\(chan|close\(hurry\)|endSignalWatch' gateway/cmd/kaiak` → none.

## Acceptance criteria

- `TestSecondStopSignalSkipsTheRemainingDrain`, `TestSecondSignalCutsTheFinalLogFlush`,
  `TestStopSignalDrains` and the e2e drain tests pass unchanged.
- `scripts/check-gateway.sh` green, or reds named with the step that clears them.

## Result

**What changed** (`gateway/cmd/kaiak/main.go` only; 780 → 730 lines)

- `stopOnSignal(ctx, stop, onSignal)`: the boot helper takes an optional callback,
  called with the signal taken from `stop` before the context ends. Boot passes nil.
- `run`: `hurryCtx, endHurry` start as `context.Background()` and a no-op (never
  hurried before a drain starts); at the drain's start
  `hurryCtx, endHurry = stopOnSignal(ctx, stop, …)`, whose callback logs
  `second stop signal: skipping the remaining drain`. Since `hurryCtx` derives from
  `ctx`, a cancelled parent hurries it at once: the `ctx.Err() != nil → close(hurry)`
  special case, the hand-written watcher goroutine, its `WaitGroup` and `endWatch`
  are gone. `endHurry` still runs in the first deferred function after the final log
  flush, so the watch covers that flush as before.
- `drain.Run(api, drainTimes, hurryCtx.Done(), logger)`; `server.Drain.Run`
  unchanged.
- `finishWithControlPlane(hurry, client, deadline)`: `context.WithDeadline(hurry,
  deadline)`, `FlushUsage`, return if `hurry.Err() != nil`, then the final status
  (its own 2 s timeout on `Background`, as before). No goroutine, no `WaitGroup`.
- `finishLogExport(hurry, e, by)`: two `Flush` calls instead of one plus a watcher —
  first until the floor (nothing cuts it), then, if not done, until `by` on
  `context.WithDeadline(hurry, by)`. `later` (only user) is gone.

**Decisions made during the step**

- **Two `Flush` calls over `context.AfterFunc` for the log flush.** The deadline
  is "the floor, then until `by` unless hurried": the floor can't be cut, so
  "hurried" only matters after it. `context.AfterFunc(hurry, …)` would need a
  second step that waits for the floor (a nested `AfterFunc` or a `time.AfterFunc`),
  and its callback goroutine can still run after `finishLogExport` returns.
  `TestStopSignalDrains` holds that nothing `run` started may still run once it returns,
  so that would need a wait again. The two-call shape has no goroutine at all.
  It relies on `Flush`'s documented contract ("The sender carries on either way"):
  a `Flush` whose ctx ends does not stop the export in flight, so the second call
  waits for the same work, plus one empty pass of the sender. Cases checked: no
  drain (`by` zero) → floor only; hurried before or during the floor → ends at
  the floor; hurried past the floor → ends at once; never hurried → ends at
  `later(by, floor)`, or when the queue is empty.
- **Context first.** `finishWithControlPlane` and `finishLogExport` take `hurry` as
  their first parameter, as `waitFirstTotals(ctx, …)` and Go convention do.
- **`endHurry` cancels.** The old `endSignalWatch` ended the watch without closing
  `hurry`; `endHurry` cancels `hurryCtx`. It runs after the last reader (the log
  flush), so nothing sees the difference.

**Report vs code** (034329e line numbers; code at d2e967a)

- control-main F3: as reported. The constructs had moved to `main.go:316-330`
  (variables and defer, unchanged), `:516-537` (watcher), `:620-646`
  (`finishWithControlPlane`), `:659-682` (`finishLogExport`) and `:560-577`
  (`stopOnSignal`). The "about 40 lines" estimate: 50 net.
- One edge that is not identical, and can't be reached from `main`: if the parent ctx is
  already cancelled when the drain starts *and* a stop signal is queued, the
  watcher's `select` may take the signal and log the "second stop signal" line
  (before, no watcher started and nothing was logged). `main` passes
  `context.Background()`, so this happens only in tests, and none queues a signal
  that way. The line would still be true.

**Tests** (`go test -count=1 -v ./cmd/kaiak/`, `--- PASS`): 32 → 32, no skips, no
fails; no test file changed.

- `go test -race -count=5 ./cmd/kaiak/`: ok (10.4 s). The six signal/ctx tests
  (`TestSecondStopSignalSkipsTheRemainingDrain`, `TestSecondSignalCutsTheFinalLogFlush`,
  `TestStopSignalDrains`, `TestStopSignalDuringTheBootWaitExits`,
  `TestRunLoadsConfigAndReturnsWhenContextIsCancelled`,
  `TestSignalTriggersReloadAndStopsWithContext`) at `-race -count=20`: ok.
- Mutation check: with the second `Flush` on `context.Background()` instead of
  `hurry`, `TestSecondSignalCutsTheFinalLogFlush` fails; restored.

**Removal checklist** (`git grep`)

- `hurry\s*(:)?=\s*make\(chan|close\(hurry\)|endSignalWatch` in `gateway/cmd/kaiak`
  → none.
- Also checked: `watch\.(Go|Wait)|endWatch|later\(` in `gateway/cmd/kaiak` → none;
  `sync.WaitGroup` left only for `background` and `listeners` in `run` (and one in
  `main_test.go`). `hurry`/`endSignalWatch` in `docs/specs`, `docs/ARCHITECTURE.md`
  → none (no doc edit needed).

**Suite**: `scripts/check-gateway.sh` passed (exit 0) on the final code.

```
==> gofmt / go vet / staticcheck 2026.2.1 (gateway)
==> go test -race -count=1 (gateway): ok cmd/kaiak, e2e, accounting, auth, clip,
    config, control, limits, logattr, metrics, netfail, otlplog, provider, routing,
    schemacheck, server, sse
==> live-test kit: gofmt / vet / staticcheck; self-test passed for vllm, llama-server,
    openai, azure-openai, anthropic, azure-anthropic, vllm with two backends
gateway checks passed
```
