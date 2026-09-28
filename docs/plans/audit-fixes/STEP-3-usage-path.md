# Step 3 — usage path

**Status:** done (2026-09-25) — `1f9653e`, `84810fa`

## Items

- **H4** settle limits before publishing the record; a per-record generation assigned
  at publication under one lock with the batch append. Tests: the 500th record,
  interval seal racing settlement, fast ack, no later traffic.
- **H5** clamp units/cost to 2^53−1 at settlement (flag + warn log); the sender checks
  each record at seal time and sets aside only a bad record.
- **H6 / D1** the provider reports "request fully sent"; any end before the first event
  after that records estimated input (`estimated` + `partial`); connect failures zero.
  Spec: Accounting / Usage across attempts.
- **H3** the limiter applies totals only when their `config_version` equals the applied
  version; otherwise keeps bases and local usage; a mismatch past the outage grace
  treats USD limits as in outage. Test: the [B] scenario (rejected v2, acks, month).
- **L16** during sustained spool write failure: keep records, log/alert, metric —
  decide the bound and record it.

## Acceptance criteria

- Regression tests reproduce H3/H4/H5/H6 first, then pass; spec updated.

## Result

Each item's regression test was run against the unfixed code (or with the fix
switched off) and failed, then passed with the fix.

- **H4** (`1f9653e`): the control client is accounting's `Batcher` —
  `Client.Record(rec) uint64` appends and returns the filling batch's generation
  under the lock that seals; `UsageRecord.Generation` (not sent) carries it; the
  limiter settles each record into its generation and skips a record whose batch was
  already shown counted with applied totals (`countedLocked`). `SealUsage` is gone.
  Local usage is one entry per generation in any order (a late older generation
  still leaves with its batch). Tests: `TestRecordFillingABatchIsCountedOnce`
  (server, real control client + fakecontrol, `BatchMaxRecords` 1: failed with
  "hour used 1200 … want 600", next request 429), limits
  `TestUsageSettledAfterItsBatchIsCountedIsNotCountedTwice` (fast ack),
  `TestOlderGenerationSettledLateLeavesWithItsBatch` (interval seal / out-of-order).
- **H3** (`1f9653e`, both halves): totals carry `config_epoch` (schema, 28 fixtures
  updated, invalid `config-epoch-missing` / `config-epoch-uppercase`, kaiak-control
  `totals`, fakecontrol, Go walker). `config.Snapshot.Version {Epoch, Number}` set by
  `Applier.ApplyPublished` (control and last-known-good applies). Test:
  `TestTotalsOfAnotherConfigKeepTheSpentBudget` ([B]'s scenario: failed with "team
  month used 0 after v2 totals" when the gate is removed), plus snapshot-version
  asserts in the client boot/LKG tests and `TestLimitsTotalsCarryTheirConfig`.
- **H5** (`1f9653e`): `accounting.clampToProtocol` at settlement (warn log with
  request and record IDs and what was clamped; `kaiak_usage_clamped_records_total`);
  `usageSender.checkSealed` validates each record with `DecodeUsageRecord` before a
  batch is written, sets a failing one aside to `usage-rejected-record-<id>.json`
  (pruned with refused batches), logs it, counts
  `kaiak_usage_dropped_records_total{reason="invalid"}`; an emptied batch takes no
  sequence. Tests: `TestSettlementClampsUsageToTheProtocolBound`,
  `TestBackendReportingTooManyTokensDoesNotLoseItsBatch` (server: failed with "1
  records counted, want both"), `TestInvalidRecordIsSetAsideAloneAtSeal` (failed:
  the batch broke the protocol).
- **H6 / D1** (`1f9653e`): `provider.Request.Sent` (httptrace `WroteRequest`);
  `Meter.Sent` (atomic), `Meter.Refused` (credential refusal), `SentUnanswered`;
  `Meter.Answered` moved to when the provider returns a response, so a retried
  `5xx`/`429` stays unbilled. Tests: `TestClientGoneBeforeTheFirstEventBillsTheSentPrompt`
  and the drain-cut subtest of `TestDrainTimeoutCutsOffHungRequests` (both failed
  with zero units), `TestSentReportsTheRequestWrittenInFull` (provider),
  `TestSentUnansweredCountsTheEstimatedInput` (meter); connect refused stays zero
  (`TestRequestsWithoutABackendAnswerRecordNoUnits`).
- **L16** (`1f9653e`): `boundSealed` after a failed spool write drops the oldest
  sealed batches past 10 000 records, error log,
  `kaiak_usage_dropped_records_total{reason="spool_unwritable"}`. Test:
  `TestSealedRecordsInMemoryAreBoundedWhileTheSpoolCannotBeWritten` (read-only data
  directory, bound 4: failed with no drop), then the kept newest records are sent.
- **Cursor retention** (`84810fa`, kaiak-control): `CountedBatch.countedAt`; the
  store's `deleteGateways` keeps cursors; `dropBatchCursorsCountedBefore(cutoff)`
  run by the expiry sweep with `batchCursorRetentionMs` (default 7 days), reported
  as `batchCursorsDropped` in the sweep run (the sample logs it). Test: "a gateway
  forgotten while silent keeps its last counted batch for the cursor retention"
  (failed: the resend was `first`, counted twice), memory-store test.
- **Specs**: GATEWAY.md — Limits → Control-plane mode (generations per record;
  Totals follow their config), Usage across attempts (D1), Accounting (Billed from
  the moment the request was sent; Protocol bound), Usage spool (Sustained write
  failure; Record checks at seal time), metrics table. CONTROL-PROTOCOL.md — Messages
  table and Totals (`config_epoch`), Matching totals, Usage records, Status intake
  (Batch cursor retention). ARCHITECTURE.md — accounting, limits, control, gateways.

## Decisions made during implementation

- **H4 order**: the record goes to the batch first and limits settle after, from
  the generation the batch returned — not settle-then-publish as sketched. The
  generation is only known at the append; the "fast ack" race is closed by skipping
  a record whose generation the limiter has already retired (the applied base holds
  it). Same agreement, no pending-tag bookkeeping.
- **H3 mismatch rule**: a totals message applies only when `(config_epoch,
  config_version)` equals the applied snapshot's. Otherwise: the live-gateway count
  applies; bases unchanged; the message's `counted` generations are held
  (`pendingCounted`) and retired only once totals are applied (they include them);
  the message waits and applies the moment its config is applied (a config event
  after an early ack), replaced by newer totals. A counted generation shown by a
  not-newer message retires only while the newest totals are applied. "Mismatch" =
  the newest totals' config ≠ the applied config; past `control_outage_grace_ms` of
  it, USD-limited requests get `503 budget_unavailable` as in outage (step 4's D6
  will refine which). `kaiak_control_outage` is not raised by a mismatch (the totals
  timestamp stops moving; status reports the rejection).
- **D1 scope**: a connection lost after the request was sent is billed like the
  other unanswered ends (retried or not) — D1's "zero only when the request never
  reached the backend"; a first-byte timeout before the request was written in full
  is now zero.
- **H5**: the record is unchanged apart from the clamp (no flag); warn log + metric.
- **L16**: bound 10 000 records, enforced by the spool writer (the only remover of
  sealed batches) after a failed write.

## Suite

`scripts/check-all.sh` (2026-09-25, at `84810fa`) → gofmt, vet, staticcheck clean;
`go test -race ./...` ok; live-test kit self-test passed; control `npm test` 429
pass, 0 fail; lint + boundaries ok; cross-half e2e `ok kaiak/e2e 38.4s`; "all checks
passed". New server/control/provider tests re-run `-count=10 -race`: stable.
