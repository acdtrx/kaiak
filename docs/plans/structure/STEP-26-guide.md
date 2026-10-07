# Step 26 — GUIDE.md trimmed

**Status:** not started

## Intent

`control/kaiak-control/GUIDE.md` says what only it says (decision 3), so a store-contract
or pricing change stops editing it. Today it restates three contracts owned elsewhere,
opens with a dated change history, and is the most-changed doc.

## Findings

- control-core F6:
  - **keep:** division of labour, hard rules, the wiring, database-specific store advice
    (the SQL sketch, representation notes, restore steps, pitfalls), the ledger, the
    LiteLLM import mapping, operating, testing, "things that look wrong";
  - **per-method store contract:** point to `storage/types.ts` (its comments are
    complete) and the contract tests;
  - **backend types and prices:** one paragraph of what a UI must know, plus links;
  - **stream ordering:** one paragraph and a pointer (the rules live in
    `CONTROL-PROTOCOL.md` and the Fastify adapter);
  - **preamble:** one line naming the config format and protocol versions it matches.
- Everything earlier steps changed lands here once: `readTotals` for spend (step 8),
  window starts by limit type and the SQL join (step 13), `lastBatches` (step 24), the
  sweep's lifecycle (step 24).

## Files likely touched

- `control/kaiak-control/GUIDE.md`.

## Decisions made during planning

- `docs/specs/` does not ship in the package: where an app developer needs a contract to
  build against, the GUIDE keeps a short statement and names the shipped file (or the
  spec section) that owns it.

## Acceptance criteria

- No section restates a per-method store contract, a pricing rule or a stream-ordering
  rule beyond the one paragraph each; every link resolves.
- Every name the GUIDE mentions exists in the code (grep each exported name it uses).
- `npm test` green (the package's doc tests, if any).
