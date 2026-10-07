# Step 24 — kaiak-control core composition

**Status:** not started

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
