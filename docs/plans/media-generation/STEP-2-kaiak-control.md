# Step 2 — kaiak-control

**Status:** not started

## Intent

`kaiak-control` reads and validates config format 6 and speaks protocol 6. Its usage
intake, totals and storage carry the three new units and the per-hour image and audio
windows. It knows the `vllm-omni` type, and the sample shows a speech model and an
image model.

## Files likely touched

- `control/kaiak-control/schema/` — synced from `protocol/schema/`.
- `control/kaiak-control/src/config/types.ts`:
  - the units (`UsageUnit`);
  - the limit types;
  - the caps;
  - `BACKEND_TYPES`.
- `control/kaiak-control/src/usage/`:
  - aggregation: each new unit summed into its own window, beside tokens and cost;
  - windows.
- `control/kaiak-control/src/messages/` — the totals message.
- The protocol and format version constants.
- `control/kaiak-control/src/storage/` and `store-contract/`: the store keeps the new
  windows' totals. Check whether the stored shape changes, and what the contract
  tests need.
- `control/kaiak-control/src/backend-verify/`: `vllm-omni`, read like `vllm` (its
  models list).
- `examples/config.json`:
  - a `vllm-omni` backend with Qwen3-TTS;
  - a `vllm-omni` backend with Qwen-Image;
  - image and audio limits on a group, so the example shows them.
- `control/sample/`: the page shows the new totals, if it shows token totals.
- Tests beside each.

## Decisions made during planning

- **Each new limit type has its own window total,** summed from the records' units, as
  `tokens_per_hour` is from tokens. No shared "media" total.

## Acceptance criteria

- `kaiak-control` passes the step 1 fixtures.
- Usage records with the new units aggregate into the new hourly totals, and the totals
  message carries them.
- The store contract tests pass for the new windows.
- `npm test` and `npm run lint` are green.
- `scripts/check-all.sh` run and recorded. Expected reds: the gateway's fixture and
  version tests, and the cross-half e2e — both cleared by step 3.

## Result

(filled in when the step is done)
