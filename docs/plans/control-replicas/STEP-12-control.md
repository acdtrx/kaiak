# Step 12 — round-2 kaiak-control

**Status:** not started

## Intent

Implement step 11's contract in `kaiak-control` and the sample, and fix the
control-side round-2 findings (`docs/reviews/2026-10-07/AUDIT-2.md`). This is a
removal step: nothing removed survives under another name, and tests that assert
removed behaviour are deleted.

## Scope

- **Ordering by issue, no rollback** (decision 21; 2H1, 2L2):
  - Remove `observeSequence`, the highest-sequence state, `onRollback` and the plugin's
    end-all-streams on rollback.
  - Each config or totals read gets a ticket from a per-core counter when it is
    issued. A stream sends a result only if its ticket is above the last ticket of
    the same kind sent there. Configs and totals have separate tickets.
- **Config delivery** (decisions 24, 27; 2M1, 2L3):
  - The core-wide `delivered` hash is removed. Each stream skips a config whose hash
    is the last it sent.
  - The core sends the stored JSON text verbatim. The publish produces the text and
    its hash once.
  - A throwing host listener (`onDeliveryFailed` and any other the delivery chain
    calls) cannot stop later deliveries. Each run gets its own catch, and listener
    errors go to `onListenerError`.
- **Totals** (decisions 22, 23, 26; 2H3, 2M4):
  - `listedWindows`' config filter and `limitedOf` are removed. Totals carry every
    window with usage.
  - Each push interval, the core takes **one** snapshot for all its streams. It diffs
    the snapshot against its previous one and merges the changes into each stream's
    pending totals.
  - When a stream can be written, it sends its pending totals with its instance's
    cursors from the latest snapshot. A new stream starts from the full snapshot.
  - The live count is read separately, on a live-set change and at connect.
  - `counted_through` is the recipient's cursors, one per epoch still kept.
- **Stream teardown** (2H2):
  - Ending a stream marks it closed, unsubscribes, and cancels its timers and
    pending reads before ending the response. It is idempotent.
  - Every write path checks the closed mark.
  - A response `error` (transport) goes through the same teardown and is logged. It
    never becomes an unhandled event.
- **Status intake** (2M5): the receipt-time check runs on every attempt, including
  the first.
- **Sample:** follows the API changes. The page shows nothing of a sequence or
  rollback.
- **GUIDE:**
  - §1–§4, §7–§12 wording: no rollback;
  - a restore is the current state;
  - totals carry every window;
  - the store keeps the config text.
- **Regression tests:** port each control-side [B] reproduction from the session
  scratchpad (`audit-b3/audit-b.test.ts`) and rewrite [R]'s from their descriptions
  in AUDIT-2. Port only those whose finding survives:
  - 2H2: the rollback trigger is gone, so drive the same write-after-end overlap with
    a stream ended during a delivery;
  - 2H3 (control half), 2M2, 2M5, 2L3;
  - 2M1: republishing after a restored store reaches the streams;
  - a config read and a totals read completing out of issue order;
  - a late notification after a newer read sends nothing older.

  Reproductions of dissolved findings (2H1's rollback counts, 2M4) are replaced by
  tests of the new rule, not ported. Each ported test fails before its fix; say so
  in Result.

## Files likely touched

- `src/control-plane/index.ts`, `src/config-publishing/`, `src/usage/`,
  `src/fastify/{index.ts,gateway-stream.ts}`, `src/gateways/`, `src/messages/`,
  `src/index.ts`.
- `control/sample/**`, `GUIDE.md`.
- Tests next to each.

## Acceptance criteria

- `npm test` and `npm run lint` from `control/` pass.
- The removal checklist items on the control side grep clean. Record the commands and
  their output in Result.
- The gateway and the cross-half tests may still fail (step 13). Name them.

## Result
