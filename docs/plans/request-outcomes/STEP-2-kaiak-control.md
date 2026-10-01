# Step 2 — kaiak-control

**Status:** not started

## Intent

`kaiak-control` speaks protocol 4: it accepts records with `status`/`error_code` and
batches with refusal counts, stores them with the batch, and exposes them to the
host.

## Files likely touched

- The protocol version constant and its tests.
- `control/kaiak-control/src/messages/`: the record and batch types; validation is
  the schema's.
- `control/kaiak-control/src/storage/types.ts`: `ReceivedRecord` gains the two
  fields; `CountedBatch` gains `refusals` (required — a store that drops it fails to
  type-check); a `ReceivedRefusal` type; `recentRefusals(limit)` on the store.
- `control/kaiak-control/src/storage/memory.ts`: keeps the most recent refusal counts,
  bounded like records (`recentRecordsSize`, or its own size if one reads better —
  decide and say why), copies on read and write.
- `control/kaiak-control/src/usage/`: intake passes the batch's refusals to the store
  with its records; totals untouched; a duplicate or straggler batch stores nothing
  (as today).
- `control/kaiak-control/src/control-plane/index.ts`: `recentRefusals()` beside
  `recentRecords()`.
- `control/kaiak-control/GUIDE.md`: the store section (what to keep, a Postgres
  sketch for refusal counts and the two record columns), reading outcomes and
  refusals per key and group, the protocol 4 upgrade note.

## Decisions made during planning

- `recentRefusals()` returns newest batch first, counts in the order the batch
  carried them — the same convention as `recentRecords()`.

## Acceptance criteria

- kaiak-control passes the shared message fixtures with the verdicts `cases.json`
  names.
- Tests: a batch with refusals only, records only, both — stored once, totals
  unchanged; a duplicate batch stores no refusals twice; `recentRefusals()` bounded
  and newest first; the memory store shares no state with callers for refusals.
- `npm test` and `npm run lint` green. Suite recorded; expected red: the gateway (step
  3) and the cross-half e2e.

## Result

(to be filled when the step is done)
