# Step 4 — the core over the new contract

**Status:** done (2026-10-06)

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

**What changed** (`control/kaiak-control`, `control/sample`)

- `src/usage/`:
  - `aggregate.ts`: `batchAdditions` counts every record toward the
    `tokens_per_hour` and `usd_per_month` window of global and of every group on its
    path, whatever the config. `limitedWindowsOf` lists the (scope, type) windows a
    config limits. The carry-over (`carryOvers`, `CountedLimit`) is gone.
  - `index.ts`: the in-process `sequence`, `turn`, `publishing()`, `totalsChanged()`,
    `controlPlaneId`, `liveGateways` and `onLimitCarriedOver` are gone.
    - A batch's write is conditional on the instance's last batch only; a refused
      write is decided again against the batch that won.
    - Totals come from `store.totalsSnapshot`, with `revision` the store's sequence
      and the windows filtered to the latest config's limits.
    - `onTotalsChanged` hears every `batch-counted` change from the store, whichever
      process counted.
- `src/config-versions/index.ts`:
  - The in-process publish queue and `BeforeSave` are gone.
  - A publish is conditional on the latest version only. A lost race is checked again
    against the version that won, so the parent rule holds.
  - Listeners are fed from the store's `config-published` changes: each version is
    read back and handed out in order. `publishConfig` resolves once this process's
    listeners have heard of it.
- `src/gateways/index.ts`:
  - The in-process queue is gone.
  - A status is judged against the stored record and written conditionally on its
    revision, judged again on a refusal.
  - The sweep writes each expiry and forget conditionally, so it runs on every core.
  - Listeners are fed from the store's `gateways-changed` changes.
- `src/control-plane/index.ts`: the lease, its timers and options, `controlPlaneId`
  and `onLimitCarriedOver` are gone. `start()` starts the sweep; `stop()` stops it.
  `ListenerEvent` loses `limit-carried-over`.
- `src/config/`: `Limit` has no `models`. `limitIdentity` and `limitCovers` are gone
  (and gone from the package exports). `mergeLimits` and the effective-limit count
  work by type. `limit-model-unknown` is gone; `limit-duplicate` is by type.
- `src/messages/`: `TotalsWindow` has no `models`; `Totals.revision` is a number; the
  duplicate-window rule is by scope and type.
- `src/fastify/index.ts`: the lease comment only. Stream behaviour is unchanged:
  pushes follow the core's listener events.
- Sample: no `onLimitCarriedOver`; the totals table loses its Models column and keys
  limits by scope and type.
- GUIDE: the §4 example and options without the lease and carry-over options; the
  limits section's "editing a limit keeps its spend" and "a limit added mid-window
  starts with the window's usage"; listener events. §2 rule 1, §5 (the store table and
  its Postgres sketch), §10 and §12 still describe one process and the lease: they are
  step 6's rewrite.
- **Tests:**
  - Usage tests rewritten for counting by scope: the mixed batch's expected totals,
    region counted without a limit, a removed and re-added limit showing all of it,
    edited limits keeping their windows, a limit added mid-window, revision and
    listeners across two processes.
  - Config-versions: consecutive versions from two processes, every listener hearing
    every version, a lost race checked again.
  - Control-plane "several cores over one store": both start; 8 instances × 3 batches
    split and resent to both cores counted once; one revision through either core; a
    publish reaching the other core's listeners; publishes and batches racing without
    refusing each other; an edited limit across cores; two sweeps, and a sweep racing
    a status.
  - Fastify: two apps over one store, a stream on one hearing the other's publish,
    batch and status.

**Deviations**

- The core subscribes to the store when it is created, not in `start()`. A core's
  listeners work before `start()` (tests and hosts call the core directly), and
  `start()` only runs the sweep.
- `publishConfig` resolves after this process's own listeners heard the version. If
  reading it back fails, the publish still succeeds (it is stored); streams get it
  with the next announcement or from the replay when they reconnect.

**Contended run** (scratch script, two cores over one memory store whose writes yield
to the event loop before landing, so the cores interleave):
- 40 instances × 25 batches, each batch sent to both cores at once, plus 20
  concurrent publishes and 1000 statuses.
- Three runs, identical:

  | Write | Attempts | Refused |
  |---|---|---|
  | `saveCountedBatch` | 2000 | 1000 |
  | `publishConfig` | 211 | 190 |
  | `saveGateway` | 1000 | 0 |

- The 1000 refused batch writes are exactly the resends: each lost once, then read
  as a duplicate, with no further retry.
- The 190 refused publishes are 20 publishes landing at once: the n(n+1)/2 worst case
  of publishes conflicting only with publishes. Usage never refused one.
- Totals: carol 1000 of 1000 nano-USD; config version 21. About 195 ms per run.

**Per-process state the core still holds**, none of it a decision replicas share:
- usage:
  - the per-instance batch queue, which only orders this process's own intake (across
    processes the conditional write decides);
  - the limited-windows cache, derived from the store's config by version;
  - the hour last pruned, since pruning is idempotent.
- config versions: its listeners, and the version last handed to them (the order of
  this process's own deliveries).
- gateways: its listeners and the sweep timer.
- the core: its started flag.
- Fastify: open streams and their push timers, per connection.

**Suite** (2026-10-06)

- Control `npm test`: 593 tests, 593 pass. `npm run lint`: passes (`tsc`,
  boundaries ok).
- The two-core files (control-plane, config-versions, usage, Fastify totals, store
  contract) run five times: 115 pass, 0 fail, each time.
- `scripts/check-all.sh`: stops at the gateway. gofmt, vet and staticcheck pass.
  Expected reds, cleared by step 5:
  - `internal/control`: `TestTotalsEventsReachTheConsumer`, `TestValidMessageFixtures`,
    `TestInvalidMessageFixtures`, `TestValidMessageFixturesRoundTrip`,
    `TestTotalsAmountBeyondSafeInteger` (the integer revision).
  - `internal/config` (uncached): `TestInvalidFixtures/limit-models.json`,
    `TestChildOverrideMatchesOnTypeAndModelSet`, `TestLimitCovers` (limits without
    model sets).
- Cross-half e2e, run on its own: fails. The gateway still speaks the old totals and
  limits; step 5 clears it.
