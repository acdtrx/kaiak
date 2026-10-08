# Step 24 — kaiak-control core composition

**Status:** done (2026-10-08)

## Intent

The core owns its housekeeping, the library has one listener-set helper, stores compute
no domain rule for a log field, and the small duplicated rules go. Today the expiry
sweep lives in `gateways` and pulls usage work in through an injected callback with a
forward reference, and the listener-set pattern is written five times with the same
comment.

## Findings

- control-core F1: the core has the timer and one `housekeeping(trigger)` run:
  `gateways.expireSilent(at)` → `{expired, forgotten}`, then
  `store.dropBatchCursorsCountedBefore(at - retention)`, then
  `usage.dropPastWindows()`, reported as one `ExpirySweepRun`. `GatewaysOptions` loses
  `batchCursorRetentionMs`, `sweepIntervalMs`, `onExpirySweep`, `dropPastWindows`;
  `Gateways` loses the timer methods; `ControlPlane` loses
  `startExpirySweep`/`stopExpirySweep` (`start`/`stop` are the lifecycle). The live
  count moves to the core's `readTotals` composition so usage and gateways do not inject
  into each other. The `stopExpirySweep`-leaves-`started` bug goes with it.
- control-core F4: `src/listeners/index.ts` (a leaf), `createListeners<T>(onError)` →
  `{add, emit}`; the five copies and their copied comment go; each module keeps its
  `onX` signature and error mapping.
- control-core F5: `lastBatches(instance)` returns the instance's cursors (one per
  epoch, with `countedAt`); the core picks the epoch's and the latest; the refused
  `saveCountedBatch` returns the same list; `BatchCursors.latest` goes from the store
  interface, the memory store, the contract tests and GUIDE §5.
- control-core F7, edges F6:
  - `libraryName` and `index.test.ts` go;
  - `configHash`: the publish uses it, or it moves to a test helper;
  - one `IntakeError` type and one helper for the two identical intake prologues;
    `errorBody(error: {code, message})` so both routes send `errorBody(e)`;
  - `protocol_version: typeof PROTOCOL_VERSION` in `messages/types.ts`.
- control-core small item: `ALL_MODELS = "*"` defined once.

## Files likely touched

- `kaiak-control/src/{control-plane,gateways,usage,storage,store-contract,config-publishing,protocol,messages,fastify,listeners}/`
  and tests; `src/index.ts`.
- `control/sample/src/app/index.ts` (the sweep log hook, unchanged in meaning).
- `docs/specs/CONTROL-PROTOCOL.md` only if it names the removed members (it describes
  one run per process already).

## Decisions made during planning

- Public API removals per decision 11; the sample's `onExpirySweep` use keeps working.

## Removal checklist (clean at phase end)

- `git grep -nP 'startExpirySweep|stopExpirySweep|batchCursorRetentionMs|libraryName|StatusError|UsageBatchError|\blatest\b.*BatchCursors|BatchCursors.*\blatest\b' control/` → none.
- `git grep -n 'so the same function subscribed twice' control/` → one hit (the helper).

## Acceptance criteria

- Store-contract tests pass on the memory store; the negative controls fail exactly as
  named.
- `npm test` and `npm run lint` (boundaries) green; `scripts/check-all.sh` green, or reds
  named with the step that clears them.

## Result

**What changed**

- The sweep in the core (control-core F1):
  - `control-plane` owns the run and its timer. `expireSilentGateways(trigger)` (the
    public name kept, so the manual run stays): `gateways.expireSilent(at)` →
    `{ expired, forgotten }`, then `store.dropBatchCursorsCountedBefore(at -
    batchCursorRetentionMs)`, then `usage.dropPastWindows()`, reported to
    `onExpirySweep` as one `ExpirySweepRun` (shape unchanged; the type moves from
    `gateways` to `control-plane`). `start` sets the interval, `stop` clears it.
  - `GatewaysOptions` loses `batchCursorRetentionMs`, `sweepIntervalMs`,
    `onExpirySweep`, `dropPastWindows`; `Gateways` loses `expireSilentGateways`,
    `startExpirySweep`, `stopExpirySweep` and gains `expireSilent(at)`
    (`GatewayExpiry`). `ControlPlane` loses `startExpirySweep`/`stopExpirySweep`; the
    `stopExpirySweep`-leaves-`started` bug goes with them.
  - The live count is added by the core: `usage.readTotals()` returns
    `CountedTotals` (windows, cursors, window starts); the core's `readTotals()`
    reads it and `gateways.liveGateways()` together and returns `TotalsRead` (now in
    `control-plane`, same shape as before for hosts). `UsageOptions.liveGateways` and
    the forward reference are gone: `usage` and `gateways` are independent siblings.
  - The core validates its own `batchCursorRetentionMs` and `expirySweepIntervalMs`
    (code `control-plane-option-invalid`); `gateways` still validates its two
    timings (`gateways-option-invalid`).
- One listener set (control-core F4): `src/listeners/index.ts` (a leaf),
  `createListeners<T>(onError)` → `{ add, emit }`, with `listeners.test.ts` (5 tests:
  order, a function twice is two subscriptions, unsubscribe idempotent, the snapshot
  at emit, a throwing listener isolated and reported with the value). Used by the
  memory store (each listener still gets its own copy of the change; errors rethrown
  from a microtask), `config-publishing` (error mapped to `read.published`),
  `gateways`, `usage`, and the core's delivery-failed listeners. Each `onX` keeps its
  signature.
- No `latest` in the store (control-core F5):
  - `BatchCursors { inEpoch, latest }` → `BatchCursor { batch, countedAt }`;
    `lastBatch(instance, epoch)` → `lastBatches(instance)` (the instance's cursors,
    one per epoch, in no particular order); a refused `saveCountedBatch` returns
    `{ saved: false, cursors: BatchCursor[] }`, the same list.
  - Memory store: one map of `BatchCursor` per instance and epoch; the max-by-
    `countedAt` and the insertion-order tie rule are gone.
  - `usage`: `inEpochOf` picks the epoch's cursor (the expected last batch, the
    outcome); `countedLastOf` picks the instance's latest for `previous` on a new
    epoch (ties: either). `outcomeOf` reads "first" from an empty list.
  - Contract tests restated: each assertion now names the cursor list with its
    `countedAt` (sorted where the list has more than one), so "the one counted last"
    is the cursor with the greatest `countedAt`, and the refusal lists every epoch's
    cursor. The snapshot test's reconcile loop finds the epoch's cursor in the list.
  - GUIDE §5's row names `lastBatches` and `{ batch, countedAt }` (minimal edit).
- Small items (control-core F7, edges F6):
  - `libraryName` and `index.test.ts` are gone.
  - `configHash` moves to `test-support` (the tests' helper); the publish keeps hashing
    the one text it writes (`hashOf(text)`) — calling `configHash(config)` there would
    stringify twice.
  - `messages` holds `IntakeError` and `checkIntake(rules, instance, doc)` (validate,
    schema issue → the message's `*-invalid` code, messages joined, then
    `instance-mismatch`); `gateways` (`STATUS_INTAKE`) and `usage` (`BATCH_INTAKE`)
    each state their rules once. `StatusError`, `UsageBatchError` and both `invalid()`
    helpers are gone.
  - `errorBody(error: { code, message })`; both fastify intake routes send
    `errorBody(intake.error)`.
  - `GatewayStatus.protocol_version: typeof PROTOCOL_VERSION` (`messages` → `protocol`,
    a type-only edge, acyclic).
  - `ALL_MODELS` once, in `config/types.ts` (not re-exported from the package).
- Docs: `ARCHITECTURE.md` gets `listeners` (bullet and graph edges), `messages`'s
  intake check, the `messages → protocol` edge, and the sweep moved from the `gateways`
  bullet to `control-plane`. `DEPLOYMENT.md`'s upgrade note names `lastBatches`.
  `CONTROL-PROTOCOL.md` names none of the removed members: unchanged.

**Decisions made during the step**

- **`expireSilentGateways(trigger)` stays the public manual run** (§7); no new
  `housekeeping` name, so hosts and tests keep calling what they call.
- **`ControlPlaneOptions.batchCursorRetentionMs` stays.** It is the host's knob and
  now belongs to the core that runs the drop; only `GatewaysOptions` loses it, as F1
  says. The checklist's first grep therefore still finds it (below).
- **New error code `control-plane-option-invalid`** for the two core-level options. A
  bad `expirySweepIntervalMs`/`batchCursorRetentionMs` passed to `createControlPlane`
  used to throw `gateways-option-invalid` naming `sweepIntervalMs`; it now names the
  host's option. The moved option test asserts the new code.
- **`TotalsRead` lives in `control-plane`**, `CountedTotals` in `usage`. Hosts see the
  same `TotalsRead` shape.
- **`IntakeError` and `checkIntake` live in `messages`**, which both intakes already
  import; `protocol` cannot hold them (it would import `messages` for the validator
  types while `messages` imports `protocol` for `PROTOCOL_VERSION`).
- **The store-contract test stores keep their own listener code.** `lossy-channel.ts`
  forwards each subscription to a store subscription and announces to all or the
  first subscriber; `early-announcement-store.ts` is deliberately broken (no
  isolation). Neither is the same pattern.

**Report vs code** (034329e line numbers; code at eeca1d6)

- `windowKeyOf` was already in `storage` (step 13); nothing to do there.
- The finding's "five copies" held; the store-contract stores are not copies (above).
- The usage tests' expected snapshots carried `liveGateways: 0` from an injected fake;
  with the live count out of `usage`, those two `liveGateways: 0` members go from the
  expected objects, and `currentTotals` validates the totals message with
  `live_gateways: 0`. The live count is asserted by the core's "the live set's size is
  the live-gateway count in totals".

**Tests** (before → after)

- Total (`npm test` from `control/`): 617 (616 pass, 1 skipped) → 624 (623 pass, 1
  skipped).
- `index.test.ts`: 1 → deleted ("the entry names the library" asserted the removed
  `libraryName`).
- `listeners/listeners.test.ts`: new, 5.
- `gateways/gateways.test.ts`: 15 → 14. Moved to `control-plane.test.ts`: "the
  scheduled sweep runs on its timer with the schedule trigger until stopped" (now "…
  on the core's timer … while started"; also checks a stop and a start sweep again);
  the run reporting of "a gateway silent for the live timeout leaves the live set at
  the next sweep" (now "every sweep run reports its trigger, time and result to the
  host", same run objects asserted, `runs` equal to `[early, due]`); "a failed sweep
  is reported with its trigger and the next one runs" (same name); the
  `sweepIntervalMs`/`batchCursorRetentionMs` lines of "options must be positive
  integers" (now "the sweep's options must be positive integers"). Stayed, on
  `expireSilent(now())`: the live-timeout test (its `{expired, forgotten}`, live
  counts, change list), forget, never-swept, draining; "an expiry the store fails
  rejects, and the next one runs" (the failed-sweep test's gateway half); options
  (`liveTimeoutMs`, plus `forgetAfterMs`).
- `control-plane/control-plane.test.ts`: 30 → 34 (the four above).
- `usage/usage.test.ts` 28, `config-publishing` 23, `store-contract/memory` 25 (1
  skipped), `lossy-channel` 25, `negative-control` 4, `fastify` 33, `status-totals`
  22, `usage-route` 6, `protocol` 11, `messages` 117: unchanged.
- Negative controls: each broken store fails exactly its named tests (torn: 2,
  early announcement: 1, silent reconnect: 1, catch-up to one subscriber: 1).

**Removal checklist** (step 24)

- `git grep -nP 'startExpirySweep|stopExpirySweep|batchCursorRetentionMs|libraryName|StatusError|UsageBatchError|\blatest\b.*BatchCursors|BatchCursors.*\blatest\b' control/`
  (with `--untracked`): only `batchCursorRetentionMs`, all the core's host option
  (`control-plane/index.ts`, its two tests, GUIDE §4's option list) — kept by design
  (above). Every other term: none.
- `git grep -n 'so the same function subscribed twice' control/` → one hit,
  `listeners/index.ts`.
- Also none: `BatchCursors`, `lastBatch\b`, `sweepIntervalMs`, `dropPastWindows:`,
  `configHash` outside `test-support` and its users (the sample's `configHash` log
  field is unrelated).

**Suite**: `npm test` (624 tests, 623 pass, 1 skipped, 0 fail) and `npm run lint`
(`tsc` + "boundaries ok") from `control/`, green. `scripts/check-all.sh` was not run
here (step 27 is in flight under `gateway/` in this worktree); it runs at phase end.

