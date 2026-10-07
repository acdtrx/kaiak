# Step 13 — counted units and window starts, both halves

**Status:** not started

## Intent

What counts toward a limit is pinned across the two halves by a shared fixture, and the
control plane's store holds no limit-type knowledge. Today the counted-tokens rule is
written in Go and TS with no shared case (it changed by hand on both sides on
2026-10-05), every store hard-codes tokens_per_hour→hour and usd_per_month→month, and
the window identity is written five times.

## Findings

- T2: `protocol/fixtures/usage/` — cases of a usage record → counted amount per limit
  type (tokens, nano-USD), read by a Go test over `limits`' amount (and step 12's
  `CountedUnits`) and a TS test over `usage/aggregate.ts`.
- control-core F3: `type WindowStarts = Record<TotalsLimitType, number>` replaces
  `CurrentWindows {hourStart, monthStart}`; stores filter
  `total.windowStart === current[total.type]` and drop `< oldest[total.type]`;
  `windowStartFor` becomes indexing; `currentWindows`/`previousWindows` return the
  record; store-contract constants follow; the GUIDE's SQL sketch joins on
  `(type, window_start)`.
- control-core F7 (window keys), edges hint: `windowKeyOf` lives in `storage`
  (`aggregate.ts` imports it); one exported window identity from `messages`
  (`scopeTypeKey` or similar) for `messages/semantic.ts`, `fastify/totals-feed.ts` and
  the sample page.
- The sample page's counted-type `if` reads the library's counted set (`COUNTED_TYPES`
  or step 11's TS twin), so a new counted type does not touch the page.

## Files likely touched

- New `protocol/fixtures/usage/`; a Go test in `gateway/internal/limits`; a TS test in
  `kaiak-control/src/usage/`.
- `kaiak-control/src/{storage,usage,messages,fastify,store-contract}/`;
  `control/sample/src/page/sections.ts`.
- `control/kaiak-control/GUIDE.md` §5 (the store table and SQL; step 26 trims around it).
- Where `protocol/fixtures/` layout is described (`CONTROL-PROTOCOL.md` or
  `TECH-STACK.md`).

## Decisions made during planning

- The public store interface changes (decision 11); no external store exists yet.
- The fixture's numbers come from the spec's counting rule (`CONTROL-PROTOCOL.md`,
  Units), not from either implementation's output.

## Removal checklist (clean at phase end)

- `git grep -nE 'hourStart|monthStart|CurrentWindows' control/` → none.
- `git grep -n 'JSON.stringify(\[' control/` → only the one exported identity (and
  `windowKeyOf`).

## Acceptance criteria

- The fixture covers each counted type with plain, cached, cache-write, output and
  reasoning units present; both halves pass it; altering the rule in one half fails it
  (checked once by hand, noted in the Result).
- Store-contract tests pass on the memory store and the negative controls still fail as
  named.
- `scripts/check-all.sh` green, or reds named with the step that clears them.
