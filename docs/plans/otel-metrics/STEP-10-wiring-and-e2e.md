# Step 10 — wiring and e2e

**Status:** not started

## Intent

The gateway exports its metrics when configured, from the first collect to the
final export at exit, and an e2e test proves the push and the scrape agree.

## Files likely touched

- `gateway/cmd/kaiak/main.go` — start the exporter after the registry's families
  exist; the exit order (decision 21): usage flush, final status, final metric
  export, `kaiak stopped`, log flush; bounded like the log flush (the drain's
  deadline, or 1 s from its start, whichever is later; a second signal cuts it); a
  start that fails once the exporter runs exports what it has.
- `gateway/e2e/` — a fake collector for metrics (beside the logs one in
  `fakeotlp`): a gateway in file mode and in control-plane mode serves traffic; the
  push and a scrape taken after `ForceFlush` agree family by family (names through
  the translation, units, attributes, values); nothing is sent with no endpoint or
  with `OTEL_METRICS_EXPORTER=none`; the final export arrives at exit; one endpoint
  variable feeds both signals; the resource is the logs'.

## Decisions made during planning

- e2e compares a push and a scrape at a quiet moment (no traffic in flight) so
  values match exactly.

## Acceptance criteria

- The tests above pass under `-race`, uncached.
- Phase 3 end: `scripts/check-all.sh` green. Suite recorded.

## Result

