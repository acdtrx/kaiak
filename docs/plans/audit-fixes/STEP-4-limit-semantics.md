# Step 4 — limit semantics

**Status:** done (2026-09-25) — `8e7a81f`, `c10745d`, `9a18b3e`, `72af353`, `b16cc14`, `aa98c8e`, `c29b355`. **Phase 1 complete.**

## Items

- **D4 (M6)** gateway: uncounted usage kept per window (no carry across windows);
  control plane: a record counts into its `gateway_time` window when that is the
  current or previous window, otherwise the current. Spec both.
- **D5 (M8)** carry-over when only a limit's model set changes (scope + owner + type),
  both halves (gateway counters and control-plane windows).
- **D6 (M9)** unpriced models never refused for budget reasons (outage and USD limits).
- **M5** persist last applied totals + uncounted amounts (format-versioned) and restore
  at boot; until first totals, USD limits count as in outage after grace.
- **M7** a reservation that fits the full limit is admitted when the share's window is
  empty; config-apply warning when output default × live > tokens/min.
- **M16** contact for money limits requires a recent ack when batches are pending.
- **H10** `n` / `best_of` / completion prompt arrays owned: reservation × sequences,
  `max_n` ceiling (default 8) → 400 above, overflow-checked.

## Acceptance criteria

- Tests per item incl. [A]/[B] scenarios; phase 1 end: `scripts/check-all.sh` green.

## Result

Each item's regression test was run against the unfixed code (or with the fix
switched off) and failed, then passed with the fix.

- **H10** (`8e7a81f`, both halves): owned `n` (chat, completions), `best_of` and the
  prompt count (completions); `Sequences` = max(n, best_of) per prompt × prompts;
  reservation = input estimate + output limit × sequences, saturating at int64 max;
  `global.max_n` (schema, Go walker, snapshot default 8, kaiak-control type) → `400
  invalid_request_error n_too_large` with `param` `n`/`best_of`; `n`/`best_of` refused
  as model defaults. Fixtures: `full.json`/`at-bounds.json`/snapshot `full.json` carry
  `max_n`; invalid `max-n-zero`, `defaults-n`, `defaults-best-of`. Tests:
  `TestOutputMultiplicityMultipliesTheReservation` ([B]'s probe: failed with 1× for
  `n=4`, prompt lists, `best_of`, and 200 for `n=64`),
  `TestOutputMultiplicityReservationSaturates`, `n` type case in
  `TestInboundValidation`, config default/explicit asserts.
- **D6 / M9** (`c10745d`): `applicable` drops USD counters when the model has no price
  in force (`accounting.PriceAt` on the live config at now). Tests:
  `TestUnpricedModelsAreNeverRefusedForBudgets` (limits: outage, spent budget,
  mismatch — failed with m2 unavailable), `TestOutageRefusesOnlyPricedModels`
  (server: global `usd_per_month` + outage → `open` served, `pair` 503; failed with
  the fix off). `TestExhaustedBudgetIs429BudgetExceeded` changed as D6 requires: the
  unpriced model is now served under the spent all-models budget. Limits test model
  `m1` is priced, `m2` not.
- **M7** (`9a18b3e`): `counter.admits` admits a reservation above the share but within
  the full limit when the window is empty; `Rejection.Max` (full value) — "request too
  large" only above it; `warnSmallSharesLocked` logs once per (applied config, live
  count) per limit × model with default output × live > limit. The limiter takes a
  logger (`New(holder, now, logger)`, `NewShared(..., logger)`). Tests:
  `TestShareNeverMakesARequestImpossible` ([A]'s 60 000 / 16 384 / 4: failed with
  "request 1 refused … Limit:15000"), `TestTooLargeMeansAboveTheFullLimit`.
- **M16** (`72af353`): `limits.Contact{Connected, Last, UsageWaitingSince}`; outage
  also when usage batches have waited past the grace; the control client's ack clock
  (`UsageWaitingSince`: since the first sealed/queued batch or the last answer, reset
  on the answer before its totals are delivered). Metric
  `kaiak_usage_last_ack_timestamp_seconds` already existed; alert guidance in
  GATEWAY.md Observability. Tests: `TestUsageAcksFailingPastTheGraceRefusePricedBudgets`
  (server + real control client + fakecontrol: stream up, `/usage` 500 → 503, free
  model served, ack → served; failed with 200 before), `TestUsageWaitingPastTheGraceIsAnOutage`.
- **D4 / M6** (`b16cc14`, both halves): gateway — a shared window's uncounted usage is
  dropped when its window is left behind (no carry), and a record whose
  `GatewayTime` is in the previous window is not charged to the current one; control
  plane — `recordWindowStart` (own window if current or previous, else current),
  previous windows kept (`dropPastWindowTotals` gets the previous windows). Tests:
  `TestUncountedUsageStaysInItsOwnWindow` ([A]'s 60%/hour over 2 h: failed with
  "request 1 refused … Used:1000"), `TestPushedWindowRolloverMidBatch` rewritten
  for D4; kaiak-control "a record counts in its gateway_time window when that is the
  current or previous one" (the [A] backlog at 12:30), "a record from last month
  counts in last month's window" (month boundary, older-than-previous → current) —
  both failed before; rollover/drop tests adjusted to the kept previous window.
- **D5 / M8** (`aa98c8e`, both halves): gateway `carryOver` in `sync` — a new identity
  with a dropped same scope/owner/type predecessor takes its counter (count, pushed
  base, uncounted usage); several predecessors → the largest used, warn log; a
  predecessor claimed twice is copied without reservations. Control plane:
  `Usage.publishing` runs every publish (the core's `publishConfig`) in the totals'
  turn and raises the new identity's current windows to the largest predecessor's
  amount; batches count under the config current in their turn;
  `store.addWindowTotals`; `onLimitCarriedOver` (core option; the sample logs info /
  warn when ambiguous). Tests: `TestModelSetEditKeepsTheSpend` (spent USD month edited
  [m1] → [m1,m2], failed with 0 used; ambiguous merge carries 500), the reload test's
  last case now asserts the carry; kaiak-control "a limit whose only change is its
  model set keeps its spend", "several predecessors carry the largest spend and are
  reported" (both failed with the carry switched off).
- **M5** (`c29b355`): `totals.json` (format 1): per hour/month counter base + base
  window, uncounted + window, config identity, live count; `SaveShared` on every
  applied totals message (`TakeTotals` now reports applied) and at shutdown;
  `LoadShared` after `client.Boot`, before listeners; another config → discarded and
  logged. Restored spool batches take generations 1..k (`RestoredGeneration`), and
  restored uncounted usage is tagged k (0 → leaves with the first applied totals).
  Tests: e2e `TestRestartWithTheControlPlaneDownKeepsASpentBudget` (spent budget
  pushed, SIGTERM, control plane closed, LKG boot → still 429 `budget_exceeded`;
  failed with 200 before), `TestSharedStateSurvivesARestart`,
  `TestBootWithoutTheControlPlaneIsAnOutageAfterTheGrace` (the existing
  "outage after grace since start" rule, with and without a restored file).
- **Specs**: GATEWAY.md — error table (`n_too_large`, priced), owned fields, Output
  multiplicity, Unpriced models, Model-set edits keep the spend, Per-minute shares
  (never impossible + warning), Uncounted usage stays in its own window, Restart
  keeps the last totals, Usage acks count for money limits, Observability alert,
  data directory, defaults. CONTROL-PROTOCOL.md — defaults refused (`n`, `best_of`),
  Usage records (`gateway_time` picks the window), Usage intake (Counted in its own
  window, past windows), Totals, Budgets (Model-set edits), Control-plane outage.
  ARCHITECTURE.md (limits, usage), TECH-STACK.md (persistence list).

## Decisions made during implementation

- **H10**: sequences per prompt = max(n, best_of) (vLLM/OpenAI: best_of candidates
  generated, n returned); a prompt list of strings or token-ID lists is one prompt per
  element, a single token-ID list one prompt; `max_n` applies to `n` and `best_of`
  each, not to the prompt count (the token limits bound that); values below 1 count as
  1 (the backend refuses them); `n`/`best_of` became refused model defaults (owned
  fields are not defaults — also removes a default-driven multiplier).
- **D4**: the gateway mirrors the control plane's rule at settlement (a record in the
  previous window is not charged to the current one); usage older than the previous
  window is in neither count until its batch is counted — accepted, documented.
- **D5**: carry = raise the new window to the largest predecessor's amount (so a
  limit that returns within the window does not add its old amount twice); the
  carry runs inside the publish's totals turn to avoid a batch counting between the
  publish and the carry.
- **D6**: "price in force" = a `prices` entry effective now on the limiter's live
  config.
- **M5**: written on every applied totals message (at most ~1/s of pushes plus acks),
  not per request; no config at boot → file not used; no separate "no totals yet"
  outage condition was added — the existing contact clock (from process start)
  already refuses after the grace, and a second net for the same concern is what
  AGENTS.md rules out (tested instead).
- **M7**: the warning also fires when the live-gateway count changes (shares change
  with it), once per (config, count).
- **M16**: "pending" = sealed or queued batches (not the filling one); any answer
  (ack or refusal) restarts the clock; `kaiak_control_outage` now also reads 1 while
  acks fail past the grace (its meaning: priced money-limited models refused).

## Decisions for the user to confirm

1. **`n` / `best_of` refused as model defaults** (schema + both halves). Conservative;
   the alternative is counting a declared default in the reservation.
2. **`max_n` global only**, default 8, applied to `n` and `best_of` separately; the
   prompt count is not capped by it.
3. **D4 under-count**: a backlog more than one window old counts in the control
   plane's current window but the gateway no longer holds it — in neither count until
   its batch is acked.
4. **Store interface change** (D5): `addWindowTotals(additions)` joins
   `ControlPlaneStore`; `dropPastWindowTotals` now receives the *previous* windows
   (keep them for late records). A real store must implement both.
5. **M5 file** is named `totals.json`; AGENTS.md "Deployability" lists the data
   directory's files and does not name it yet — an agent may not edit AGENTS.md, so
   the line needs your edit ("control-plane mode's last applied totals").
6. **M16** also raises `kaiak_control_outage` on unanswered usage (not only a lost
   stream).

## Suite

`scripts/check-all.sh` (2026-09-25, at `c29b355`) → gofmt, vet, staticcheck clean;
`go test -race ./...` ok (gateway e2e 36.7 s); live-test kit self-test passed; control
`npm test` 436 pass / 0 fail; lint `boundaries ok`; cross-half e2e `ok kaiak/e2e
48.3s`; "all checks passed". New timing-sensitive tests re-run `-race -count=10`
(server), `-count=3` (e2e M5), `-count=5` (limits): stable. **Phase 1 ends green.**
