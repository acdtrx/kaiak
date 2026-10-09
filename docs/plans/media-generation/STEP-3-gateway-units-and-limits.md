# Step 3 — gateway: units, limits, caps, versions

**Status:** not started

## Intent

The gateway reads config format 6, speaks protocol 6, and carries the three new units
from a usage record through cost, limits, totals and metrics. It enforces the six new
limit types and the two caps. No endpoint produces the new units yet; the tests drive
them directly.

## Files likely touched

- `gateway/internal/config/`:
  - the units and the unit sets (`TokenUnits`, `PricedUnits`, `CountedUnits`…): decide
    where each new unit belongs (not priced, decision in step 1's backlog entry);
  - the limit-type table: new measures for images and the two audio units, minute
    windows local, hour windows counted;
  - the caps;
  - format 6.
- `gateway/internal/accounting/`: records carry the new units. Cost ignores them
  (unpriced).
- `gateway/internal/limits/`:
  - the new measures;
  - reservation: images reserve the request's count; audio generated reserves nothing
    and settles;
  - totals from the control plane per new window.
- `gateway/internal/control/`: protocol 6; the totals message.
- `gateway/internal/metrics/`: the new usage counters (step 1's names) with the usage
  labels.
- `gateway/internal/server/limits.go`: the reservation hook — what a request reserves
  per measure. The endpoint-specific amounts come in steps 5–8.
- Tests beside each.

## Decisions made during planning

- **The limit table stays the one place** a limit type is defined. A new measure is a
  new row and a new `Measure`, not a special case in the limiter.
- **A measure the request cannot know before it runs** (audio generated) reserves 0 and
  is counted at settle, as USD is.

## Acceptance criteria

- **Component tests:**
  - each new limit type refusing at its window, local and counted;
  - images reserved and settled;
  - audio settled after;
  - the caps refused;
  - records carrying the units through to the control plane's batch encoding;
  - the metrics series.
- `scripts/check-gateway.sh` and `scripts/check-all.sh` green — the phase-1 reds of
  steps 1–2 cleared, the cross-half e2e included.

## Result

(filled in when the step is done)
