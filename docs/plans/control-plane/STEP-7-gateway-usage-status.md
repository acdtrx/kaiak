# Step 7 — gateway: usage batches and status

**Status:** done (2026-09-24) — phase 3 continues with step 8

## Intent

Send usage records in batches with a spool, exactly as the protocol settles, and
report status.

## Files likely touched

- `gateway/internal/control/` — batch sender (a usage sink on accounting's fan-out),
  spool, status reporter
- `gateway/internal/state/` — spool files (format-versioned)
- `gateway/internal/server/` drain sequence — flush step

## Decisions made during planning

- Sink appends to the filling batch (non-blocking); sender sends every 5 s or at 500
  records; one outstanding; on ack drop the batch and hand the ack's totals to limits
  (step 8 consumes them — here, expose them).
- Spool: epoch + next sequence + outstanding batch + filling batch written on each
  change that matters (batch sealed, ack) — decide the write policy balancing
  durability and disk writes; document what a crash can lose (records settled since
  the last write).
- Resend the outstanding batch with the same ID until acked (backoff).
- Status on connect, on change, every 10 s: fields from step 1.
- Drain: after in-flight requests finish, seal and send the filling batch, wait for
  the ack within the drain timeout; whatever is left stays spooled.

## Acceptance criteria

- Tests: batching by time and size; one outstanding; resend keeps the ID; lost ack →
  control double counts nothing (against the kit, in step 11; here against the test
  double checking IDs); spool survives restart; drain flush; status cadence and
  content.

## Result

- `scripts/check-gateway.sh` — gofmt, vet, staticcheck 2026.2.1, `go test -race ./...`
  (all packages ok, the control-mode e2e included), live-test kit self-test:
  `gateway checks passed`. `go test -race -count=10 -cpu 1,2,8 ./internal/control/`:
  ok.
- `control/`: `npm test` — 307 tests, 307 pass, 0 fail; `npm run lint` — `tsc` clean,
  `boundaries ok`.
- No expected reds.

### Protocol fix landed first (own commit)

- `last_rejection` now means "the latest config received from the control plane was
  rejected": set on a rejection, cleared when a later config from the control plane is
  applied, whatever the versions (a restarted control plane counts from 1). The
  `rejection-not-newer` rule is gone from both halves; its fixture is now a valid
  status (`rejected-below-applied.json`). A last-known-good boot does not clear a
  rejection (that copy is not a config the control plane sent). Recorded in
  CONTROL-PROTOCOL.md, Messages → Status.

### Decisions made while implementing

- **All inside `control.Client`**: the sink (`Client.Record`), the sealer and sender
  goroutines and the status reporter run under `Client.Run` beside the config
  follower and are waited for; `FlushUsage(ctx, trigger)`, `SetDraining()` and
  `ReportStatus(ctx, trigger)` are the invocable mechanisms the drain calls.
- **Sealing**: every `BatchInterval` (5 s) and at `BatchMaxRecords` (500) — the size
  seal happens in `Record`, in memory; a separate sealer goroutine writes the spool,
  and a sender goroutine sends, so disk and network never wait on each other.
- **Spool format**: one file per sealed batch (`usage-batch-<epoch>-<seq>.json`, the
  batch message) plus an index (`usage-spool.json`: instance, epoch, next sequence),
  all format 1. The files are the queue; memory holds IDs and counts only. Chosen
  over one spool file: rewriting the whole queue on every seal grows with the outage
  (hundreds of MB an hour at high load) and would force a bound that drops billing
  data.
- **Write policy**: seal → batch file → index past it → queued; never sent before both
  are durable (a sequence is bound to one record set). Ack → file deleted. A failed
  write keeps the batch sealed in memory and is retried at the next tick. **Crash
  loss: records settled since the last seal (≤ 5 s or < 500 records).**
- **Filling batch not persisted** (deviation from the plan's "outstanding + filling
  written"): it is sealed every 5 s and at the drain, so writing it separately adds a
  second copy of the same records (and a de-duplication problem on restore) for no
  durability gain. CONTROL-PROTOCOL.md's Usage batches wording updated accordingly.
- **Queue bound**: none; a warning each 1000 batches; depth in metrics.
- **Retry classes**: network error, `5xx` (incl. `503 config-unavailable`), malformed
  ack or an ack for another batch → retry with the same ID on the step-6 backoff (the
  sender has its own backoff instance). Set aside only on known batch codes
  (`usage-batch-invalid`, `record-instance-mismatch`, `record-id-duplicate`,
  `timestamp-invalid`, `instance-mismatch`, `request-invalid`); any other refusal
  (401, protocol mismatch, unknown 4xx) → retried, logged at error. Rejected: set
  aside on any 400, which a misconfigured proxy would turn into data loss.
- **Set-aside**: renamed to `usage-rejected-<epoch>-<seq>.json`; the newest 10 kept.
- **Epoch**: fresh when no index, another format version, unreadable, or another
  instance's. Spooled batches are delivered whatever the index says, each under its
  own ID — another instance's batches go out with that instance in the
  `Kaiak-Instance` header (a docker container whose hostname changed keeps its
  spool). Older epochs are sent before the current one, one epoch at a time.
- **One totals path**: `Options.OnTotals func(TotalsUpdate)`; `TotalsUpdate{Totals,
  Acked}` — `Acked` is the acknowledged batch's records (nil for a stream event), so
  step 8 can swap base and local in one step. Calls are serialized by a mutex (the
  stream and the sender are different goroutines).
- **Status**: on client start, on a stream connecting, on change (apply, rejection,
  draining) and every 10 s; failures logged at warn when they start, then debug until
  delivered again. State: starting until a config is applied, ready, draining.
- **Drain**: `SetDraining()` before `drain.Run` (status `draining` at once); after
  it, `finishWithControlPlane`: `FlushUsage` bounded by what remains of grace +
  timeout from the drain's start (a hurry cancels it), then a final status (2 s
  bound, skipped when hurried); then `stopBackground()`. The sealer also seals and
  writes on stop, so even a hurried stop keeps every settled record in the spool.
- **Metrics**: `kaiak_usage_batch_sends_total{result=acked|rejected|failed}`,
  `kaiak_usage_spool_batches`, `kaiak_usage_spool_records`,
  `kaiak_usage_last_ack_timestamp_seconds` (control-plane mode), fed through a
  `control.UsageObserver` interface implemented by `metrics.UsageDelivery`.
- **`state`** gained `List(prefix)`, `Remove`, `Rename` (durable: directory synced).
- **fakecontrol**: `POST /v1/usage` (light batch checks, instance vs header, 503
  before a config, de-dup by last batch ID per instance, `FailUsage` queued faults,
  `SetUsageFault` persistent, `DropAck` counts then drops the connection,
  `SetAckTotals`), `POST /v1/status` (204), events channels, `Counted`,
  `CountedRecords`, `Statuses`, `Gets`; `Restart` also forgets the de-dup state.

### Deviations from the plan

- Filling batch not written to the spool (above).
- The two existing client tests that counted all requests now count GETs
  (`Gets()`): status POSTs now go to the same double.

### For later steps

- Step 8: wire `Options.OnTotals` (nil in `cmd/kaiak`); a `TotalsUpdate` with
  `Acked` is an ack — base := totals, local −= those records, in one step. A stream
  push may already include a batch whose ack has not arrived yet (temporary
  double count until the ack; conservative). Successful contact for the outage
  timer: a stream connecting, or an ack (`sendOutstanding`'s acked branch).
- Step 11: the sample control plane will see status every 10 s and batches every 5 s
  per gateway; a killed gateway's resend is the kit's `duplicate` outcome.
