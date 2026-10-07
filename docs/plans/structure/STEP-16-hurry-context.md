# Step 16 — "hurry" as a context

**Status:** not started

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
