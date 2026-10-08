# Step 3 — gateway: rerank

**Status:** not started

## Intent

The gateway serves `POST /v1/rerank` through every stage and reads
`global.max_rerank_documents`. The new endpoint is one row in each table that names
endpoints. Behaviour on the existing endpoints is unchanged.

## Files likely touched

- `gateway/internal/provider/`:
  - the `Rerank` endpoint (path `rerank`) and a rerank format;
  - `vllm` and `llama-server` serve it;
  - not a core endpoint for either (OVERVIEW decision 13);
  - the request edits for the rerank format: the model only — no `stream_options`;
  - the top-level `model` rewrite applies to its answer;
  - the endpoint-support table test (`endpointColumns`) gains the `rerank` column.
- `gateway/internal/server/`:
  - the `bodyEndpoints` row: name `rerank`, operation `rerank`, no output-limit keys;
  - the inbound parser for the format:
    - `model`;
    - `documents` counted against `max_rerank_documents` (`400 invalid_value`,
      `param: "documents"`);
    - `stream` not owned;
  - the error for the cap, in the shape and wording of the embeddings cap's.
- `gateway/internal/accounting/`:
  - **the input estimate:** the query's own estimate, media included, added once for
    each document beyond the first (decision 5);
  - **the usage reader for the format:** `usage.prompt_tokens` as `tokens_in`, no
    output, and no generated content to estimate (decision 11).
- `gateway/internal/config/`:
  - `max_rerank_documents`: schema check, document field, and snapshot value with
    default 1000.
- Tests beside each change. Server tests run on a `vllm`- or `llama-server`-typed fake
  backend: the existing fakes are `openai-compatible`, which does not serve rerank.

## Decisions made during planning

- **One format, one place each.** The format switches (inbound, meter, stream rules)
  gain a rerank case.
  - Where rerank and embeddings share a purpose, they may share the code: input-only
    usage in an OpenAI-shaped `usage` object is one example (CODING-RULES §2).
  - Where they only look alike, they stay apart. Counting `documents` is one example:
    `input` may hold token-ID lists, `documents` never does.
- **The query for the estimate is read from the body as sent.** A query that is not
  text (multimodal content parts) is estimated by the existing media rules.

## Acceptance criteria

- **Component tests:**
  - owned fields: model missing, documents not a list, documents over the cap,
    `stream` passed untouched;
  - the estimate: one document, many documents, a query with an image part;
  - the usage reader: reported, `usage` missing (estimated and flagged), malformed;
  - the route answers `405` to other methods;
  - `/v1/models` lists `rerank` for models on `vllm` and `llama-server` deployments;
  - the log line and the usage metrics carry operation `rerank`.
- A model whose deployments are all on types without rerank answers
  `400 endpoint_not_served`.
- The suite is green: `scripts/check-gateway.sh` and `scripts/check-all.sh`.

## Result

(filled in when the step is done)
