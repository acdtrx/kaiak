# Step 4 — the core over the new contract

**Status:** not started

## Intent

Make the `kaiak-control` core hold no state that replicas must agree on. It counts,
publishes, sweeps and reads totals through the store's guarantees alone, and its
listeners are fed by the store's notifications. Several cores over one store then
behave as one control plane.

## Files likely touched

- `control/kaiak-control/src/usage/index.ts`:
  - The in-process `sequence` and `turn` go.
  - Counting counts every record toward each `tokens_per_hour` and `usd_per_month`
    window of every scope on its path, whatever the config's limits (step 3), and
    retries only when the instance's last batch moved.
  - Totals come from the consistent read, filtered to the limits of the latest config.
  - The carry-over and `onLimitCarriedOver` go (step 3).
  - `totalsChanged()` goes; publishes and live-set writes move the sequence in the
    store.
- `control/kaiak-control/src/config-versions/index.ts`: the in-process publish queue
  goes. A publish is conditional on the latest version only; one that loses to
  another publish retries, revalidated against the new latest version, so the
  parent-change rule still holds. Usage never refuses a publish (step 3).
- `control/kaiak-control/src/gateways/index.ts`: conditional status and sweep
  writes; the sweep runs on every core.
- `control/kaiak-control/src/control-plane/index.ts`:
  - The lease, its timers and options go; `controlPlaneId` goes.
  - `start()` subscribes to the store and starts the sweep; `stop()` undoes both.
  - Listener events come from the store's notifications, so a change made by another
    core reaches this core's streams.
- `control/kaiak-control/src/fastify/`: unchanged in behaviour. Pushes still follow
  the core's listener events, at most once per interval.
- `control/kaiak-control/src/index.ts`: exports (contract tests from steps 2–3;
  lease- and carry-over-related exports removed).
- Tests: two cores over one memory store, through the core API and through two
  Fastify instances, covering the OVERVIEW's two-core verification list. A contended
  run (many instances' batches split across two cores) records retry counts.

## Decisions made during planning

- **Publishes conflict only with publishes**, and batches only with a resend of the
  same instance's batch (step 3 removed every other link). The contended run measures
  batch retries across two cores and records them.
- **`recentRecords`, config history and `configsSince`** read the store as today; they
  need no change for replicas.

## Acceptance criteria

- `npm test` and `npm run lint` from `control/` pass, the shared totals fixtures
  included.
- The two-core tests pass, repeated (they race by design): run the suite five times
  and record it.
- The contended run's numbers are recorded here.
- No in-process state that replicas must agree on is left: the step's Result lists
  what state the core still holds, and why each item is per-process by nature.
- Suite run and recorded. Expected reds: the gateway's totals fixtures and the
  cross-half e2e (step 5).

## Result

(filled in when the step is done)
