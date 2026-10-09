# Step 7 — speech batch

**Status:** not started

## Intent

Serve vLLM-Omni's `POST /v1/audio/speech/batch` on `vllm-omni` deployments:
- the items are counted against `max_speech_batch_items`;
- each item follows speech's rules for `response_format`, `ref_audio` and
  `max_new_tokens`;
- the answer's per-item usage is summed;
- each successful item's base64 audio is measured toward `audio_seconds_out`.

## Files likely touched

- `gateway/internal/provider/`: the endpoint (path `audio/speech/batch`) and its format;
  `vllm-omni` serves it.
- `gateway/internal/server/`:
  - the row (`audio_speech_batch`, operation `generate_content`, output type speech);
  - the inbound parser: items counted and capped; per-item and batch-level owned
    fields, with the batch's defaults applying where an item sets none.
- `gateway/internal/accounting/`:
  - the input estimate over the items;
  - a batch reader: the `results[]` scanned once, summing each item's `usage`, and
    measuring each item's `audio_data` from its encoded size and its format (the
    item's `media_type`);
  - errored items add nothing.
- The output limit: a batch reserves the output limit once per item (the items run as
  that many sequences), as completions' prompts do.
- The fake backend: the batch endpoint, mixed success and error items.
- Tests beside each.

## Decisions made during planning

- **The answer is relayed as vLLM-Omni sends it**, with its top-level `model`
  rewritten if present. Its base64 audio is counted, never held.

## Acceptance criteria

- **Component tests:**
  - the items cap;
  - per-item refusals (format, `ref_audio`), naming the item's position;
  - usage summed;
  - audio length per item;
  - errored items;
  - the reservation per item.
- **Phase 2 end:** `scripts/check-all.sh` green and recorded.

## Result

(filled in when the step is done)
