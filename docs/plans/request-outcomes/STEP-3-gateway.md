# Step 3 — gateway

**Status:** not started

## Intent

The gateway speaks protocol 4: every usage record says how its request ended, and
every error answer that leaves no record is counted into the next batch.

## Files likely touched

- `gateway/internal/accounting/`: the record's `Status` and `ErrorCode`, set where
  the request settles — from the same values the request log line takes (one source,
  overview decision 3); a retried attempt's record carries its failure's code and no
  status.
- `gateway/internal/server/`: where the request log line and `kaiak_errors_total` are
  written for an answer with no record — the one place a refusal is counted
  (key ID and groups when the key was valid, the public model only when it is
  configured, the code).
- `gateway/internal/control/usage.go`: refusal counts held beside the filling batch,
  merged by (code, key, model), sealed into the batch with its records; a batch seals
  at 500 records **or** 1 000 refusal counts, and the interval seal fires when either
  is non-empty; batches with only refusals are sent and acked like any other.
- `gateway/internal/control/spool*.go`: the spool carries refusal counts; its format
  version bumps (the old format discarded with its log line).
- The protocol version constant (and `fakecontrol`'s); `fakecontrol` accepts and
  records refusal counts for tests.
- File mode (no control plane): refusal counts are dropped as records are — check
  what file mode does with records today and do the same; say what that is.

## Decisions made during planning

- Counting a refusal is an increment under the usage sender's lock, never I/O on the
  request path.
- A refusal during the drain (after new requests are refused) is not counted: the
  drain's own refusals are the gateway's lifecycle, not the caller's — confirm against
  how `kaiak_errors_total` treats them and follow it.

## Acceptance criteria

- The gateway passes the shared message fixtures; the records and batches it emits
  validate against the schemas (round-trip tests).
- Per ending — success, backend `5xx`, timeout, wrong path, client gone mid-stream,
  a retried attempt — the record's `status`/`error_code` equal the log line's (one
  test comparing them).
- Per refusal kind — unknown key, unknown model, forbidden model, body too large,
  invalid JSON, a limit, queue full, unknown path — one count with the right key,
  groups, model and code; repeats merge into one count with first/last times;
  a batch seals at 1 000 counts; a batch of refusals only is sent and acked.
- A spool of the old format is discarded with its log line.
- `scripts/check-all.sh` green — the cross-half e2e included now both halves speak
  protocol 4 — or every red named with step 4.

## Result

(to be filled when the step is done)
