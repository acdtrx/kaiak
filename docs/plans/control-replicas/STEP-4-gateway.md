# Step 4 — gateway

**Status:** not started

## Intent

Order totals by the store's sequence within the config epoch, and remove what
existed only to tell control-plane processes apart.

## Files likely touched

- `gateway/internal/control/messages.go`: `Revision` is the integer. Its newer-than
  rule takes the epoch into account: the same epoch and a higher sequence, or another
  epoch.
- `gateway/internal/control/client.go`: the retired-process list and its log lines
  go. A totals message from an older sequence is ignored, with a debug line if one
  exists today.
- `gateway/internal/control/schema.go`: the totals schema.
- `gateway/internal/fakecontrol/`: speaks the new revision; it can stand in for two
  processes over one sequence where the tests need it.
- Tests: ordering within an epoch; adoption on an epoch change; an older sequence
  from any connection ignored (push read before ack, ack before push);
  `counted_through` handling unchanged.

## Decisions made during planning

- **`counted_through`'s rule is unchanged.** Any totals message, newer or not, stops
  counting the batches it covers as the gateway's own. It never depended on which
  process sent it.
- **The data directory's totals cache** holds no revision (`internal/limits/persist.go`
  stores counters and the config identity), so its format is unchanged.

## Acceptance criteria

- `scripts/check-gateway.sh` green.
- `scripts/check-all.sh` green, the cross-half e2e included: **phase 1 ends here**,
  committed.

## Result

(filled in when the step is done)
