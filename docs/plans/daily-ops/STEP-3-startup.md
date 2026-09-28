# Step 3 — startup (D7, D8)

**Status:** done (2026-09-25) — D7, D8, and the 429 cooldown fix (never shortened)

- **D7** boot retries the control-plane snapshot with jittered backoff until the boot
  wait (default 60 s) ends; then LKG (opt-in), then the seed, then exit. A rejected token
  or rejected snapshot still exits at once.
- **D8** the listeners bind (readiness) only after the first totals for the applied
  config have arrived (they follow the config on stream connect), bounded by the boot
  wait; if the wait runs out without them, the gateway becomes ready but priced
  USD-limited requests are refused (`budget_unavailable`) until totals arrive. A seed
  boot (free models only) needs no totals. Test: [B]'s binary reproduction (delayed
  totals → spent budget refused, not admitted).

## Result

- **D7 boot warm-up** — `control.Client.Boot` asks again while the control plane is
  unavailable (`unavailable(err)`: connection errors, timeouts, any `5xx` incl. `503
  config-unavailable`, a missing/other `Kaiak-Protocol`), a full-jitter delay from 250 ms
  doubling to a 2 s cap, until `KAIAK_CONTROL_BOOT_WAIT_MS` ends (default now **60 000**,
  was 5 000). A `4xx` (refused token) or a snapshot the message rules refuse returns at
  once; a snapshot whose config is rejected goes straight to last-known-good/exit as
  before. The first failure logs `config snapshot not fetched at startup: retrying
  within the boot wait` (warn; error for a protocol mismatch) with `attempt` and
  `boot_wait_ms`; later attempts at debug. Boot returns ctx's error when cancelled.
- **D8 readiness waits for the first totals** — `cmd/kaiak`: the control client now runs
  before the listeners bind; after a boot from the control plane or last-known-good,
  `waitFirstTotals` holds the listeners back until `limiter.FirstTotals()` closes or the
  rest of the boot wait runs out (`waiting for the first totals` → `first totals
  received` / warn `first totals not received within the boot wait: priced USD-limited
  requests are refused until they arrive`). A seed boot skips it. `limits`: a "no totals
  yet" state (`totalsKnown` false until totals for the applied config are applied, or
  restored from `totals.json`) refuses priced USD-limited requests `Unavailable` through
  the existing outage/mismatch path → `503 budget_unavailable`; token limits keep
  counting from zero. The `budget_unavailable` message now says the spend is not known
  (unreachable or not reported yet).
- **429 cooldown never shortened** (main-session item) — `routing.throttle`: a later
  `429` on a cooling deployment keeps the later end; a longer one extends it.
- **Test tooling** — `fakecontrol` now sends the totals after every stream's replay, as
  the protocol says (`CONTROL-PROTOCOL.md`, Config stream) — before, only with
  `PushTotalsOnChange`; `HoldTotalsOnConnect(true)` withholds them (the delayed-totals
  scenario). The control package's harness holds them: its tests script every totals
  message.
- **Failing first** (red before, green after):
  - `e2e/TestBootWaitsForAControlPlaneComingUp` — control plane down, up 3 s after the
    gateway starts, default wait. Before: `gateway exited before logging the API
    listener` (one attempt, exit). After: boots from the control plane, serves.
  - `e2e/TestReadinessWaitsForTheFirstTotals` — [B]'s reproduction: stateless gateway,
    spent global budget on `priced`, connect totals held 1 s. Before (HEAD `e101bf5`,
    run in a scratch worktree with the new test and fake): `the API listener bound before
    the first totals; the priced request on a spent budget answered 200`. After: not
    bound while held; once pushed, binds, `/readyz` 200, priced → `429 budget_exceeded`.
  - `e2e/TestFirstTotalsLateRefuseBudgetsUntilTheyArrive` — wait 1.5 s, totals held:
    ready after the wait, priced → `503 budget_unavailable`, `chat` (no USD limit) → 200;
    totals pushed → `429 budget_exceeded`. Red before (no such state or log).
  - `limits/TestNoTotalsYetRefusesMoneyLimitedModels` — before: admitted with no totals.
  - `control/TestBootRetriesUntilTheControlPlaneComesUp` (unreachable; `503
    config-unavailable`) — before: `Boot returned … without retrying`. Delays 125 ms,
    250 ms at random 0.5.
  - `routing/TestLaterThrottleNeverShortensTheCooldown` — before: a 1 ms `429` after a
    1 h one set the end to 1 ms.
  - Guard (green before and after): `control/TestBootDoesNotRetryWhatTheOperatorMustFix`
    (401, rejected snapshot: no retry scheduled).
- **Tests updated to the new contract**: `limits` outage tests take a first (empty)
  totals before testing the outage; `TestBootWithoutTheControlPlaneIsAnOutageAfterTheGrace`
  — without restored totals, refused from the start (was: until the grace);
  `server/TestUsageAcksFailingPastTheGraceRefusePricedBudgets` waits for the first
  totals rather than the stream alone; `cmd/kaiak` default boot wait 60 s; e2e boots
  with the control plane down set `KAIAK_CONTROL_BOOT_WAIT_MS` (1000/2000 ms) instead of
  riding the 60 s default; `TestNoConfigAtBootExits` checks the retries and the 2 s wait.
- **Docs**: `GATEWAY.md` — `KAIAK_CONTROL_BOOT_WAIT_MS`, listeners, Boot (retries,
  table, the reversal of E2's one-attempt rule), new "Readiness waits for the first
  totals" and "No totals yet" entries, the `budget_unavailable` row, Lifecycle →
  Readiness, 429 cooldown ("later, never earlier"). `CONTROL-PROTOCOL.md` — "Not an
  outage, same refusal: no totals yet". `DEPLOYMENT.md` — startup probe row and sizing
  (`periodSeconds: 2`, `failureThreshold: 35` for the 60 s wait; `initialDelaySeconds`
  alternative), Boot (retries, first totals, what operators see during a control-plane
  restart), the env table, the budget-refusal alert's third cause.
- **Suite**: `GOFLAGS=-count=1 scripts/check-all.sh` — green (the first run caught a
  data race in the new control test's own delay channel, fixed in the test):

  ```text
  ok  	kaiak/cmd/kaiak	2.924s
  ok  	kaiak/e2e	84.765s
  ok  	kaiak/internal/control	13.408s
  ok  	kaiak/internal/limits	3.244s
  ok  	kaiak/internal/routing	3.059s
  ok  	kaiak/internal/server	11.904s
  ...
  boundaries ok
  ==> cross-half e2e (sample control plane + two gateways)
  ok  	kaiak/e2e	48.942s
  all checks passed
  ```

## Decisions made during the step

- **Hourly token limits do not wait for totals** (USD only), matching the outage policy:
  an hour token limit's overshoot is at most one hour's share and it corrects itself,
  a budget's is money. Recorded in `GATEWAY.md` (No totals yet).
- **Restored totals (`totals.json`) count as known**: a data-directory restart serves
  its restored spend at once (the M5 design); the readiness wait still runs, for fresh
  totals, bounded by what is left of the boot wait.
- **Totals for another config end the readiness wait** (not the refusal): after a boot
  whose snapshot this gateway rejected (last-known-good boot, control plane reachable),
  the matching totals may never come until the operator fixes the config; waiting the
  full 60 s would delay every free model for nothing.
- **The totals wait also follows a last-known-good boot** — normally with ~0 left of
  the wait (the control plane was unavailable throughout), so it binds at once.
- **Boot retry step 250 ms → 2 s, full jitter** (as briefed), separate from the 500 ms →
  30 s reconnect backoff.
- **A later `429` keeps the later end** — this reverses step 2's "a later 429 replaces
  the cooldown's end".
- **Status during the totals wait**: the client runs, so status reports `ready` (config
  in force) a few milliseconds before the listeners bind. No protocol change.
- **A stop signal during the boot wait** is acted on once the listeners bind (the
  signal stays queued), as before; with a 60 s wait a pod stopped during startup may be
  killed at its grace period — nothing is lost (no traffic, no usage yet).

## Decisions for the user to confirm

- Boot wait default **60 s** and the startup-probe sizing in `DEPLOYMENT.md`
  (`failureThreshold: 35` × 2 s).
- Hourly token limits enforce from zero while totals are unknown (USD only refused).
- Totals for another config end the readiness wait (the no-totals refusal stays).
- Restored `totals.json` lifts the no-totals refusal (outage rule unchanged).
- A later `429` never shortens a cooldown (the reverse of step 2's recorded choice).
