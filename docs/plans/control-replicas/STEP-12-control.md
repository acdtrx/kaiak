# Step 12 — round-2 kaiak-control

**Status:** done (2026-10-07)

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

**Commits:** `635b126` (kaiak-control, the sample, their tests), `6ff61d5` (GUIDE, one
spec sentence), and this Result.

**What changed**

- **`src/config-publishing/`:**
  - The publish writes the config's text once (`JSON.stringify` of the validated
    document) and hashes it. The store keeps `{ text, hash, publishedAt }`, and the
    parents rule parses the current config's text.
  - `PublishedConfig` is the store entry with the parsed `config`; `currentConfig()`
    and `onConfigPublished` give it to hosts.
  - Every config read takes the next number when it is issued: `ConfigRead
    { published, read }`, from `readConfig()` (a stream's connect read) and
    `onConfigRead` (each delivery read).
  - **Removed:** the core-wide `delivered` hash (2M1) and `observeSequence`.
  - Delivery runs stay one at a time. Each run settles on its own (`deliveries =
    run.catch(…)`), so a failed run never stops the next (2L3).
- **`src/control-plane/`:**
  - **Removed:** `highestSequence`, `observeSequence`, `onRollback` and
    `RollbackListener`.
  - `onDeliveryFailed` listeners are called one by one. A throwing one goes to
    `onListenerError` as `{ type: "delivery-failed", error }` (2L3).
  - The core exposes `readConfig`, `onConfigRead` and `readTotals` beside the host
    API. `start()` announces `{ type: "catch-up" }`.
- **`src/usage/`:**
  - **Removed:** `limitedOf`, `limitedWindowsOf`, `LimitedWindow` and the config filter
    in `listedWindows`. The totals list every scope and type with usage in its current
    window (2H3).
  - `readTotals()` returns `TotalsRead { windows, cursors, liveGateways, windowStarts }`:
    one snapshot of the windows and every instance's cursors, with the live count read
    beside it (from `gateways()`, step 11's decision). The window re-read across a
    boundary is kept.
  - `totals(instance)` is the complete totals for one gateway. Its `counted_through`
    is that instance's cursor of each epoch (2M2), and it never returns undefined.
- **`src/fastify/`:**
  - New `totals-feed.ts`: one `readTotals` per push for every stream of the plugin.
    - Reads run one at a time, at most one per `totalsPushIntervalMs`, with a trailing
      read for changes during the interval or the read.
    - Each read is diffed against the previous one, and the changed windows go to
      every stream.
    - A window the earlier read had in the same current window, but the later one
      lacks (a store restored to less), is listed at `"0"`. One that ended with its
      hour or month is not listed. The spec now says so (`CONTROL-PROTOCOL.md`,
      Messages → Totals, `windows`).
    - A failed read is retried at the interval, logged once per run of failures.
  - `gateway-stream.ts`, rewritten:
    - **Configs** are sent only if their read number is above the last one sent there
      (configs among configs), and skipped when their hash is the last sent (2M1). The
      `pending` queue during the connect read and the sequence `lastSent` are removed.
    - The stored text goes out as it is:
      `{"config_hash":"…","config":<text>}` (2L1).
    - **Totals:** the first after the first config is complete, from the feed's latest
      read. Later ones carry the changes merged since, with the instance's cursors and
      the live count of the latest read.
    - Nothing is sent when no window, cursor or live count changed. While the socket
      waits to drain, changes gather, and they are sent on drain.
    - **Teardown** (2H2): `close()` is idempotent and marks the stream closed.
      Every ending runs it first: the plugin's `end()`, the stall timer, the `close`
      event, a failed connect read, and a response `error` (now logged; it never
      reaches the process). Every write and send checks the mark.
  - `index.ts`:
    - The rollback listener is removed.
    - The plugin creates the totals feed and closes it on close.
    - `totalsPushIntervalMs` now bounds the feed's reads, and so each stream's pushes.
- **`src/gateways/`:** the receipt-time check runs at the top of every attempt,
  including the first (2M5).
- **`src/messages/`:** `Totals.counted_through: BatchPosition[]`. The new rule
  `counted-through-epoch-duplicate` is in `checkTotals` (step 11's fixture now
  passes).
- **Sample:** `PublishedConfig` replaces `CurrentConfig`. The totals section no longer
  treats totals as optional. The page shows nothing of a sequence or rollback (it
  never did).
- **GUIDE:**
  - §4: `onDeliveryFailed` only, with how a host serving its own streams orders them
    (`readConfig` / `onConfigRead`, one `readTotals` per push).
  - §7: `publishConfig`'s result with `text`.
  - §9: `currentConfig()`'s shape; the snippet without "totals undefined";
    `delivery-failed` in the listener events; listeners may hear a config twice.
  - §1: totals per scope.
  - §12: a restore is sent once the store's channel catches up, and the app may
    publish its current config again.
  - The header names the round-2 update.

**Decisions made in this step**

- **Configs and totals are ordered apart, each by its own issue order.** Configs use
  the core's read numbers. Totals need no number: the plugin's feed runs its reads one
  at a time, so they complete in issue order by construction, and each stream sends
  only from the feed's latest read.
- **Host listeners are not de-duplicated.** With the core-wide `delivered` hash gone,
  `onConfigPublished` hears every read: a publish through this core twice (its own
  read and the store's announcement), a catch-up again. The GUIDE says so. Streams
  skip by their own hash. Keeping a host-only dedup would be the removed hash under
  another name.
- **The publish still reads back after its write.** So it still resolves only once
  this core's listeners have heard the config, whatever the store's notification
  latency.
- **A window that left a read within its window is listed at `"0"`.** That is the only
  case where usage goes down (a restored store). Without it, a gateway would keep the
  higher value until the window ends.
- **The feed reads on every change, with or without streams.** Reads are bounded by
  the push interval. A stream that joins before the first read waits for one; after
  that it starts from the latest read.

**Tests deleted** (each asserted removed behaviour):

- `control-plane.test.ts`: "a store sequence going back is a rollback the core
  announces; going on is not", and its `restorableStore` helper.
- `fastify.test.ts`: "a store going back ends every stream; the gateways reconnect to
  its current state".
- `usage.test.ts`:
  - "the store's sequence each totals read sees is heard, and never goes on the wire";
  - "a record whose last group was deleted is listed under its surviving ancestors and
    global" (listing filtered by the config);
  - "records count whatever the config's limits; a limit removed and added back shows
    all of it";
  - the "edited limits" suite's two tests (a limit value edited keeps its window; a
    limit added mid-window). All three asserted windows appearing or disappearing with
    the config's limits. They are replaced by one test that a publish changes no
    totals.
- Sequence assertions in `config-publishing.test.ts`: identical content's
  `again.sequence > first.sequence`; the "last stored by sequence" checks in both
  concurrent-publish tests.

**Tests changed** (the same behaviour, new shapes):

- `counted_through` as a list everywhere; totals never undefined.
- Windows sorted in the usage tests (the totals list them in no particular order).
- Listeners compared as runs of repeats (`runs()`), or counted per read.
- Stream tests wrap `readConfig` / `onConfigRead` instead of `currentConfig` /
  `onConfigPublished`.
- The publish-then-totals stream tests no longer wait for totals after a publish.

**Tests added:**

- `usage.test.ts`:
  - every window of a mixed batch, 19 of them, including unlimited and undefined
    groups;
  - a deep path's six scopes;
  - a publish changes no totals;
  - with no config, totals list the batch;
  - `counted_through` of two epochs.
- `config-publishing.test.ts`:
  - the store keeps the text byte for byte, hashed;
  - identical content is heard again;
  - reads take their place in issue order however late they complete.
- `status-totals.test.ts`:
  - the first totals are complete and later ones list only changed windows;
  - a publish pushes no totals, and the windows stay.
- `fastify.test.ts`:
  - the same config twice during the connect read is sent once;
  - a store restored to an older config is sent to the open streams after a catch-up,
    and republishing reaches them.
- **`fastify/round-2.test.ts`** (the AUDIT-2 regressions, 10 tests): see below.

**Regression tests and their failure before the fix.** Each test in
`fastify/round-2.test.ts` was run with its fix reverted in place (a scripted mutation,
restored afterwards), and with the fix:

| Test | Finding | Fix reverted → result |
|---|---|---|
| a stream ended during config delivery writes nothing into the ended response | 2H2 | `end()` without `close()` first → fails (`ERR_STREAM_WRITE_AFTER_END`) |
| a delayed first status read does not overwrite a newer status | 2M5 | receipt check only after a refused write → fails |
| republishing after a restore reaches a stream that connected to the restored config | 2M1 | a core-wide delivered hash → fails (config 2 never arrives) |
| a delivery read issued before a stream's connect read is not sent after it | order rule | no read-number check → fails (config 2 after 3) |
| a slow totals read is never applied after a later one | order rule | feed reads not one at a time → fails (100 after 200) |
| counted_through covers the acknowledged epoch after an older epoch's late write | 2M2 | one cursor (the last stored) → fails |
| a throwing delivery-failed listener does not stop later deliveries | 2L3 | listener errors not caught → fails |
| a config event carries the stored text as it is, its hash the hash of that text | 2L1 | config re-serialized at send time → fails |
| totals keep listing a window the current config no longer limits | 2H3 | — (the filter is gone; see below) |
| a catch-up sends a stream nothing it already runs, and the stream stays open | 2H1's replacement | — (see below) |

The last two have no in-place fix to revert. Codex's originals of them, and of 2M2,
2M5, 2L1 and 2H2, were run at the pre-fix commit `a1e9bfe` in a scratch worktree
(since removed). All nine failed:
- "totals still cover a running limit when the gateway rejects its removal";
- "catch-up after usage is not a store rollback";
- "an ordinary snapshot overtaken by a notification is not rollback";
- "a late previous-epoch commit must not hide coverage of the acknowledged epoch";
- "a delayed initial gateway read cannot overwrite a newer receipt";
- "a delayed totals result is not sent after a newer config";
- "a JSON object round-trip through reordered keys must preserve the advertised hash";
- "rollback closure cannot write another config into the ended response";
- "exported store contract rejects catch-up delivered to only one subscriber".

**Not ported, with the reason:**
- [B]'s two rollback reproductions (2H1, dissolved): replaced by the catch-up and
  order tests above.
- "a delayed totals result is not sent after a newer config" (2M4, dissolved: totals
  no longer depend on the config). Its ordering half is covered by the slow-totals
  test.
- The store-contract catch-up test (2M7): step 11's negative controls.
- The jsonb reorder model (2L1): replaced by the stored-text test.

**Removal checklist, control side** (greps over `control/` without `node_modules`):

```
highestSequence|observeSequence|CurrentConfig                         (none)
onRollback|[Rr]ollback                                                (none)
lastSent|\bdelivered\b
  GUIDE.md:288 "delivered only on commit"; windows.ts:34 "a backlog delivered";
  review.test.ts:161-181 a test's own variable; sample config-file.test.ts:253
    — the English word, not the removed hash
limitedOf|limitedWindowsOf|LimitedWindow|counted but not listed|only the windows the   (none)
config's limits
  GUIDE.md:198, 399; storage/types.ts:35 — "whatever the config's limits", the current rule
counted_through null / BatchPosition | null                           (none)
snapshot\.(config|liveGateways|last|sequence)                         (none)
JSON.stringify((current|published).config) | config: current.config   (none)
config-versions|config snapshot|snapshot fetch|acknowledged batch|for another config|resumes from|kaiak.config.version
  config/limits.ts:4, config/limits.test.ts:2 — "the gateway's config snapshot", the
  allowed meaning (a request's in-memory config)
sequence: number | .sequence - back | change.sequence
  messages/types.ts:49, 67 and the test helpers — a batch's sequence (allowed)
```

Left for step 14, as step 11 recorded: `docs/architecture/control-plane.html:278` (the
Rollback bullet) and `docs/ARCHITECTURE.md`.

**Suite** (2026-10-07):

- Control `npm test`: 605 tests, 604 pass, 0 fail, 1 skipped (the memory store's
  catch-up contract test, whose channel cannot drop a change; the lossy channel runs
  it).
- Control `npm run lint`: `tsc` clean, boundaries ok.
- `scripts/check-gateway.sh`:
  - gofmt, vet and staticcheck pass;
  - every package passes, the gateway e2e included (115.9 s);
  - except `kaiak/internal/control`, which has 5 failing tests:
    `TestTotalsEventsReachTheConsumer`, `TestValidMessageFixtures`,
    `TestInvalidMessageFixtures`, `TestValidMessageFixturesRoundTrip` and
    `TestTotalsAmountBeyondSafeInteger` (`counted_through` is a list).
  - **Expected, cleared by step 13.**
- Cross-half tests: not run. The gateway still decodes `counted_through` as one object,
  so they fail. **Expected red until step 13.**
- No `node --test` process left (`pgrep` empty). The scratch worktree and the logs are
  deleted.
