# Step 1 — contract

**Status:** not started

## Intent

Write the record fields, the refusal counts and protocol version 4 into the contract
both halves test against. Every later step implements what this step writes.

## Files likely touched

- `docs/specs/CONTROL-PROTOCOL.md`:
  - Usage record: `status`, `error_code` — meaning, when each is absent (overview
    decision 3), that they are the request log line's values;
  - Usage batch: `refusals` — the count's fields (decision 4), the bounds and the
    records-or-refusals rule (decision 5), that nothing counts toward totals;
  - Usage intake: what the control plane does with them (decision 6), the store's
    `CountedBatch.refusals` (decision 7);
  - protocol version 4, dated, with the reason.
- `docs/specs/GATEWAY.md`: Accounting — the record's `status`/`error_code` from the
  request's outcome; a new "Refusal counts" decision — what is counted, the key and
  model rules, the 1 000 cap and sealing; Usage spool — its format version bumps.
- `protocol/schema/usage-record.schema.json`: the two fields (`status` an integer in
  100–599; `error_code` a code-shaped string, bounded); the stale "informational"
  description of `gateway_time` corrected while there (review K6).
- `protocol/schema/usage-batch.schema.json`: `refusals` (0–1 000 items), `records`
  0–500, at least one of the two non-empty; the refusal count's shape
  (`additionalProperties: false`).
- The protocol version wherever schemas or fixtures carry it. Then
  `npm run sync-schemas` from `control/`.
- `protocol/fixtures/messages/usage-record/`, `…/usage-batch/`: the valid and
  invalid cases of the overview's Verification list, with `cases.json` entries.

## Decisions made during planning

- `error_code`'s pattern follows the gateway's codes (lowercase, digits,
  underscores), bounded at 64 characters; the schema does not list them — the gateway
  owns its codes, and a new one must not need a protocol change.
- `first_time` and `last_time` use the record's `gateway_time` format.

## Acceptance criteria

- The specs state every decision of the overview that touches the contract, dated
  2026-10-01, with reasons and what was rejected.
- Schema copies in sync; the new fixtures listed in `cases.json`.
- Suite run and recorded. Expected reds: both halves fail the new fixtures and the
  version checks (step 2 clears kaiak-control's, step 3 the gateway's); the
  cross-half e2e until step 3.

## Result

(to be filled when the step is done)
