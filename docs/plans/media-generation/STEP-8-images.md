# Step 8 — image generation

**Status:** not started

## Intent

Serve `POST /v1/images/generations` on `vllm-omni` deployments:
- `n` is capped by `max_images_per_request` and reserved against the image limits;
- `response_format` is `b64_json` only — `file` and `url` are refused (OVERVIEW
  decision 7) — and `lora` is refused (decision 11);
- the answer's `data[]` is counted into `images` as it streams through, never held;
- it is never a stream.

## Files likely touched

- `gateway/internal/provider/`: the endpoint (path `images/generations`) and its
  format; `vllm-omni` serves it; the `model` edit; an event-stream answer is
  incomplete (as rerank's).
- `gateway/internal/server/`:
  - the row (`images_generations`, operation `generate_content`, output type image);
  - the inbound parser:
    - `n`, capped and reserved;
    - `response_format` and `lora` refused;
    - `stream` not owned.
- `gateway/internal/accounting/`:
  - the input estimate (the prompt; `negative_prompt` counted as text);
  - an images reader that counts the top-level `data[]` elements without keeping them.
    The member scanner gains a count-only mode, or the reader counts during its single
    pass.
- The fake backend: images, multi-MB base64 bodies, a layered model's answer (more
  images than `n`).
- Tests beside each.

## Decisions made during planning

- **No `usage` is expected** from vLLM-Omni's image answer. If one appears (a future
  version), it is ignored: images are what this endpoint counts.
- **The prompt's tokens are recorded** as `tokens_in`, estimated and flagged, so token
  limits still see the prompt.

## Acceptance criteria

- **Component tests:**
  - owned fields and refusals;
  - the `n` cap;
  - reservation and settle of `images`;
  - counting over a body of several MiB with no allocation growing with it;
  - a layered answer (more than `n`) settled at its real count;
  - an event-stream answer incomplete;
  - the operation and output type.
- **Phase 3 end:** `scripts/check-all.sh` green and recorded.

## Result

(filled in when the step is done)
