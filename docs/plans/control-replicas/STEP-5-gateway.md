# Step 5 — gateway

**Status:** not started

## Intent

Order totals by the store's sequence within the config epoch, and remove what
existed only to tell control-plane processes apart. Bring the gateway's limits to
step 3's shape: no model sets, a limit identified by its scope and type.

## Files likely touched

- `gateway/internal/control/messages.go`: `Revision` is the integer. Totals apply
  only when their `config_epoch` is the running config's, and then only with a higher
  sequence (decision 10 as changed in step 1).
- `gateway/internal/control/client.go`: the retired-process list and its log lines
  go. A totals message from an older sequence is ignored, with a debug line if one
  exists today.
- `gateway/internal/control/schema.go`: the totals schema.
- `gateway/internal/fakecontrol/`: speaks the new revision; it can stand in for two
  processes over one sequence where the tests need it.
- `gateway/internal/config/` and `gateway/internal/limits/`: limits without `models`;
  identity (scope, type); one limit per type per scope (config refuses a repeat);
  `child_defaults.limits` merged by type; totals windows without `models`; the
  file-mode usage snapshot and the totals cache keyed by the new identity (their
  format versions bump if their shape changes).
- Tests: ordering within an epoch; another epoch's totals not applied; an older
  sequence from any connection ignored (push read before ack, ack before push);
  `counted_through` handling unchanged; limits by scope and type, a scope's usage
  counted whatever the model; an edited limit value keeping its window.

## Decisions made during planning

- **`counted_through`'s rule is unchanged.** Any totals message, newer or not, stops
  counting the batches it covers as the gateway's own. It never depended on which
  process sent it.
- **The data directory's totals cache** holds no revision, but its counters are
  keyed by limit identity, which step 3 changes: its format bumps with the snapshot's
  if the stored shape changes.

## Acceptance criteria

- `scripts/check-gateway.sh` green.
- `scripts/check-all.sh` green, the cross-half e2e included: **phase 1 ends here**,
  committed.

## Result

(filled in when the step is done)
