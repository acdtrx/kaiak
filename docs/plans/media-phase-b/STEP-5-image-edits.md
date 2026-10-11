# Step 5 — image edits

**Status:** not started

## Intent

Serve `POST /v1/images/edits` on `vllm-omni` deployments: an upload format of its own,
counted in `images` as generations are.

## Files likely touched

- **The names, on both halves at once:**
  - `protocol/schema/config.schema.json`: `images_edits` in the endpoint names;
  - `protocol/fixtures/backend-types/vllm-omni.json`: `images_edits` in its endpoints;
  - `control/kaiak-control`: `ENDPOINT_SUPPORT`, `MODEL_ENDPOINTS`, the types, the
    packaged schema, tests;
  - `gateway/internal/config`: the endpoint name and the support table.
- `gateway/internal/provider/`:
  - the endpoint (path `images/edits`), an upload format;
  - `vllm_omni.go` serves it;
  - vLLM-Omni's image `400 Model mismatch` read as the model missing, as on
    generations;
  - the answer relayed as generations' is.
- `gateway/internal/server/`:
  - the `bodyEndpoints` row: `images_edits`, operation `generate_content`, output type
    `image`, an upload endpoint;
  - the owned parts (OVERVIEW decisions 16 and 27):
    - `n` against `max_images_per_request`, reserved;
    - `response_format` `b64_json` only;
    - `stream` true refused;
    - `lora` refused;
    - `url`, `url[]`, `mask_image`, `reference_image` strings refused unless they are
      `data:` URLs (`external_reference_unsupported`);
    - `image`, `image[]`, `url[]` may repeat; a file part is never inspected.
- `gateway/internal/accounting/`:
  - the input estimate for an edit: the prompt's text, plus each input image as a
    media item;
  - the image count reused from generations (the answer's `data[]`, never kept).
- `gateway/internal/fakebackend/`: an edits handler that parses the rebuilt body and
  checks it (the model replaced, the parts intact), answering like vLLM-Omni.
- Tests beside each.

## Decisions made during planning

- **The image counter is shared with generations:** one reader, one rule (a layered
  model's answer may hold more images than `n`; the settle corrects it).
- **An edit's `n` defaults to 1** for the reservation, as vLLM-Omni's does.

## Acceptance criteria

- **Component and server tests:**
  - each owned part and refusal: `stream`, `lora`, each URL-ish field as `http…`, as
    a path-like string and as a `data:` URL, `response_format` other than `b64_json`,
    `n` over the cap;
  - the JSON form of an edit refused;
  - the rebuilt body reaching the fake backend intact;
  - `images` on the record from the answer's count; `tokens_in` estimated and
    flagged;
  - the image limits refusing and settling;
  - the operation and output type on the request line and metrics;
  - an edit to a model whose list leaves `images_edits` out refused before any
    backend.
- The shared fixtures pass on both halves with the new name.
- `scripts/check-all.sh` green — **phase 2 ends green.**
