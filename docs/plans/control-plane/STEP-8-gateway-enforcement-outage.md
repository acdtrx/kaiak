# Step 8 — gateway: shared totals, shares and outage policy

**Status:** done (2026-09-24) — phase 3 complete (steps 6–8, suite green)

## Intent

In control-plane mode, enforce hourly and monthly limits from pushed totals plus the
gateway's own unacked usage, per-minute limits on their share, and fail money-limited
models closed after the outage grace.

## Files likely touched

- `gateway/internal/limits/` — control-plane mode for hour/month windows; share
  factor for minute windows
- `gateway/internal/control/` — feeding totals and live count to limits; connection
  state
- `gateway/internal/server/` — outage refusal (`503`, code chosen and added to the
  error table), metrics for control-plane connection state

## Decisions made during planning

- Hour/month counters in control mode: `base` = pushed used (for the window it names),
  `local` = own usage not yet acked; on ack, base := ack totals and local drops the
  acked batch's share in one step. A pushed window newer than the local one resets
  local counting for that window.
- Per-minute effective limit = limit ÷ live gateways (at least 1, rounded down, never
  below 1 unit unless the limit is 0).
- Outage = no successful contact (stream connected or ack) for longer than
  `control_outage_grace_ms`; then models covered by any USD limit that applies to the
  request's scopes are refused with `503`; others serve. Ends on the first successful
  contact.
- File mode keeps its P1 behavior unchanged.

## Acceptance criteria

- Tests: totals + local enforced; ack swap never double counts or drops; window
  rollover pushed mid-batch; share division; outage refusal after grace only for
  money-limited models and recovery.
- Phase 3 end: full suite green, committed.

## Result

- `scripts/check-gateway.sh` — gofmt, vet, staticcheck 2026.2.1, `go test -race ./...`
  (all packages ok; e2e with the new two-gateway test), live-test kit self-test:
  `gateway checks passed`. `go test -race -count=5 -cpu 1,4 ./internal/control
  ./internal/limits`: ok; the two-gateway e2e `-count=3`: ok.
- `control/`: `npm test` — 317 tests, 317 pass, 0 fail; `npm run lint` — `tsc` clean,
  `boundaries ok`.
- No expected reds: **phase 3 ends green.**

### Protocol fix landed first (own commit): ordered, per-gateway totals

- Totals gain `revision: { control_plane, sequence }` (32 random hex digits per
  control-plane process; a sequence from 0 bumped by every counted batch, publish
  and live-set change) and `counted_through: { epoch, sequence } | null` (the
  recipient instance's last counted batch: the stream's instance, or the
  acknowledged batch's). Schema, fixtures (every totals and ack fixture, 6 new
  invalid cases), both validators, spec text.
- The gateway applies totals only when newer (same control plane, higher sequence;
  or another control plane — adopted, ordering restarts). What a message shows
  counted applies whatever its revision.
- **Consistent snapshot** (the rule that makes the above safe): a totals message's
  windows include exactly the batches counted at its revision. `kaiak-control` takes
  batch counting (store write + sequence step) and totals reads (sequence,
  `counted_through`, windows) in turns — one promise chain in `usage`. Publishes and
  live-set changes bump the sequence from listeners the core registers first, so
  the bump precedes any stream's push for the change.
- `usage.totals(instance)` / `ControlPlane.totals(instance)` now take the recipient
  instance (the stream passes its checked instance).

### Decisions made while implementing

- **Usage generations** instead of record-keyed local usage: `control.Options.SealUsage`
  (the limiter's `SealUsage`) closes a generation as each batch seals, under the
  sender's lock, so a record's tag is never earlier than its batch; queue entries
  keep their generation. `TotalsUpdate{Totals *Totals (nil when not newer), Counted
  uint64}` replaces `Acked`: `Counted` is the max generation among queued batches at
  or below `counted_through` (and the acked batch). Memory stays one slice entry per
  generation per counter, not per record — a long outage costs nothing here.
- **Shared windows** (`window.shared`): `used + base-if-current`; `used` = held
  reservations + `local` (settled amounts by generation). Current window =
  max(clock window, pushed `window_start`, previous) — never backwards. Uncounted
  `local` usage **carries over** a window change (the control plane stamps on
  receipt, so it lands in its current window); reservations stay in the window they
  were made, as in file mode. The step's planning note "a pushed window newer than
  the local one resets local counting" is refined this way: dropping would
  under-count what the control plane is about to count.
- **Matching**: the limiter keeps the latest applied pushed windows; a counter not
  listed has base 0; a limit a reload adds takes its base from them.
- **Share**: `floor(limit ÷ live)`, live 0/none = 1, never below 1 unless the limit is
  0; recomputed on every applied totals, counts kept; headers and the 429 message
  show the share.
- **No `limits.json` in control mode**: `cmd/kaiak` neither restores nor writes it
  (`limits.NewShared` is only built in control mode).
- **Outage**: contact = snapshot fetched, stream bytes (heartbeats), an ack; an open
  stream is contact while open (else a grace under 15 s would flap between
  heartbeats — recorded as the rejected alternative). Status 204s do not count. The
  clock starts at client creation (≈ process start), so an LKG boot with the control
  plane down enters outage after the grace. Evaluated lazily in `Reserve` (no timer,
  no goroutine). Refusal: `503`, type `server_error`, code `budget_unavailable`, no
  `Retry-After`, no rate-limit headers; error class `budget_unavailable`
  (platform-side).
- **Metrics**: `kaiak_control_connected`, `kaiak_control_last_contact_timestamp_seconds`,
  `kaiak_control_totals_applied_timestamp_seconds` (absent before any; also what the
  e2e waits on), `kaiak_control_outage` — gauges read at scrape time through
  `metrics.ControlState`.
- **fakecontrol**: revision (new ID on `Restart`), per-instance `counted_through`,
  `SetWindows`, `SetLiveGateways`, `Totals(instance)`, `Revision()`,
  `PushCurrentTotals`, `PushTotalsOnChange` (push on connect and every change),
  `CloseStreams`; `SetAckTotals` removed (acks carry the model's totals). It does
  not aggregate usage: tests script the windows.
- e2e harness gained `metricValue`/`waitMetric` (polls `/metrics` every 50 ms: state
  inside another process has no event to wait on).

### Doubts

- A control plane run as several replicas (each its own `control_plane` ID) would
  make a gateway whose stream and acks reach different replicas re-adopt on every
  switch — correct but noisy; per-process revisions assume one control-plane process
  per store until a shared sequence is needed.
- A late message from a dead control plane (another ID) would be adopted — only
  possible if it is delivered after its successor's; not guarded.
- The global promise chain serializes all batch counting against totals reads —
  trivial for the in-memory store, a throughput bound for a slow database store
  (one batch at a time); revisit with a real store.
