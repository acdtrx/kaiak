# Step 19 — `main.go` split by job

**Status:** not started

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

- `git grep -nE 'type controlSettings|func finishWithControlPlane|\bFlushUsage\b|\bReportStatus\b' gateway/` → none (lower-case forms inside `control` are fine).

## Acceptance criteria

- `main.go` holds the logger, `run` and the lifecycle helpers; `run` under ~150 lines.
- Every `cmd/kaiak` and e2e test passes unchanged.
- `scripts/check-all.sh` green. **Phase 4 ends here.**
