# Step 3 — gateway: rerank

**Status:** done — reviewed and committed 2026-10-08

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
- `gateway/internal/fakebackend/`: the `rerank` path answering vLLM's shape (`id`,
  `model`, `usage`, sorted `results` with `document`), `OmitUsage` honoured — the
  server tests run through the fake backend. Step 4 adds llama-server's shape.
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

### What changed

- `gateway/internal/config/`: `global.max_rerank_documents` — document field, schema
  check `IntegerAtLeast(1)` (beside `max_embedding_inputs`), snapshot
  `MaxRerankDocuments` with `DefaultMaxRerankDocuments = 1000`. Tests: the schema
  default row, the default and `full.json`'s 250, and `TestBatchCapsRefuseZero` — the
  three batch caps' zero fixtures refused at `/global/<field>` by the bound ("must be
  between 1 and …"), so `max-rerank-documents-zero.json` no longer passes as an
  unknown field.
- `gateway/internal/provider/`:
  - `provider.go`: endpoint `Rerank` (path `rerank`), format `FormatRerank`.
  - `vllm.go`, `llama_server.go`: `Rerank` in the served lists. Core endpoints
    untouched: `openAICore` is the OpenAI format, so rerank is not core (decision 13).
  - `stream_format.go`: `rerankStream`, the format's case in `newStreamFormat`: no
    request edit (the model edit alone; `stream` passes untouched), and an event
    stream answering a rerank request is never complete (below).
  - Tests: `endpointColumns` maps the spec table's `rerank` column (checked: removing
    `Rerank` from `vLLMEndpoints` fails it); vLLM's and llama-server's not-found
    answers on rerank read as endpoint missing (current rule); rerank in
    `TestNoUsageEditOutsideTheOpenAIFormat`; new `rerank_test.go` — the body sent
    byte for byte with only `model` changed (`stream: "yes"` included), the answer's
    top-level `model` rewritten and a nested one left, completeness (whole JSON body,
    a body cut short, an event stream).
- `gateway/internal/server/`:
  - `pipeline.go`: row `{api: Rerank, name: "rerank", operation: "rerank"}`, no
    output-limit keys; comments on `operation` and `outputLimitKeys`.
  - `inbound.go`: `stream` is read unless the endpoint counts tokens or its format is
    Rerank; `parseRerankFields` (the format's case) refuses documents above the cap;
    `rerankDocuments` counts one per list element, one for any other value.
  - `errors.go`: `errTooManyDocuments` — `400 invalid_value`, `param: "documents"`,
    the embeddings cap's wording (`'documents' holds N documents; the gateway's
    maximum is M per request.`).
  - `limits.go`, `inbound.go`: `saturatingMul` moved to `accounting.SaturatingMul`
    (beside `SaturatingAdd`), which the estimate needs too — one helper, not two.
  - Tests: new `rerank_test.go` (`withRerankModels` adds `vl`/`ls`/`vl-slow` backends
    and three priced rerankers, cap 3, without touching what other tests see):
    the pipeline on a vllm and a llama-server deployment (body sent, answer, record
    `tokens_in` 40 and cost, operation on the log line and usage metrics,
    `http.route` `/v1/rerank`, `gen_ai.request.stream` false); refusals in OpenAI's
    shape (no key, `endpoint_not_served` on `openai-compatible` and `azure-openai`,
    model missing, model not a string, not JSON, documents over the cap — counts
    named, no document text —, `GET` → 405 with `Allow: POST`, body too large);
    accepted shapes passed byte for byte (documents at the cap, a string, `stream`
    true or an object); a backend 5xx as `upstream_error`; usage missing → estimate,
    flagged; the non-stream timers despite `"stream": true` (response timeout, not
    retried); `/v1/models/{id}` endpoints. Rerank rows in
    `TestBatchSizeIsCappedPerRequest` (1000 accepted, 1001 refused, 1001 numbers are
    1001 documents, a string is one). `messages_test.go`'s pinned vLLM list gains
    `rerank`; `errTooManyDocuments` in `errorAnswers`. Checked: owning `stream` on
    rerank again fails the pipeline, refusals and timer tests.
- `gateway/internal/accounting/`:
  - `estimate.go`: on rerank, `query` is scanned into a part of its own (folded into
    the body's figure as any value) and `documents` counted while scanned
    (`documentList`); the total adds `(documents − 1) × query.tokens()`, saturating.
    `LargestPrompt` stays the body's (no output limit uses it).
  - `meter.go`: `NewMeter`'s rerank case; a reader may name no content member (`""`),
    and `Answered` then keeps only `usage`.
  - `openai_usage.go`: `promptOnlyUnits` (and `decodeUsageReport`), the
    prompt-tokens-only reading embeddings already had, now shared with rerank.
  - `rerank_usage.go`: `rerankUsage` — body `usage` through `promptOnlyUnits`, no
    content, nothing read from a stream.
  - Tests: `rerank_usage_test.go` — usage reported (before or after `results`),
    prompt tokens only, missing / null / malformed / neither count → estimated,
    output 0 with 1 KB of result text; the estimate: one document, many, the query
    after the documents, documents as a string, none, a query with an image part, a
    document with an image part, and `query`/`documents` as plain text on
    embeddings.
- `gateway/internal/fakebackend/`: `rerank` path, under `/v1/` only (neither Azure
  layout has one); new `rerank.go` answers vLLM's shape — `id`, `model`, `usage`
  (`prompt_tokens`, `total_tokens`), `results` with `index`, `document` (`text`, or
  `multi_modal` for a non-string document) and `relevance_score` (the share of the
  query's words the document holds), sorted by score, ties in order, `top_n` > 0
  applied, `documents` that is not a list one document; `OmitUsage` honoured;
  `stream` ignored, as both servers do.
- `scripts/live/apis.go`: `epRerank` in `vllm`'s and `llama-server`'s lists — the
  self-test's `models` check compares the chat model's `endpoints` with them. No
  rerank checks (step 6).

### Design choices

- **Stream ownership** is decided in `parseOwnedFields` by the format, beside the
  token-counting rule, rather than by a new row flag: "rerank has no stream" is the
  format's property (spec, Client APIs), as the Anthropic error shape is
  (`answersAnthropic`).
- **Two document counts, one rule.** The cap counts in the server
  (`rerankDocuments`), the estimate while it scans (`documentList`) — the estimate's
  one-pass rule forbids reading `documents` again, as `promptCount`/`promptList`
  already do for prompts. Both: one per list element, one for anything else; they can
  differ only between 0 and 1 (absent or `null`), which neither the cap (≥ 1) nor
  the estimate (repeats from the second document) sees.
- **Shared by purpose:** the prompt-only usage reading (embeddings, rerank) and
  `SaturatingMul`. **Kept apart:** document counting from `promptCount` (a document
  list holds no token IDs); `rerankUsage` from `openAIUsage` (another format, no
  choices, no stream).
- **The query's repeat** is its own estimate, `EstimateTokens(its bytes) + media`,
  once per extra document — the spec's wording — not its bytes summed into the body's
  text first; the two differ only in rounding. Its span, like every value's in the
  scan, includes the `:` before it.
- **`rerankStream` returns a value**, not a pointer: it holds no state.

### Spec points to review

- **An event stream answering a rerank request** — the spec says a rerank answer is
  never a stream but not what an SSE answer means. The code reads it as never
  complete: `upstream_incomplete`, a circuit failure, usage partial (estimated). The
  alternative was the OpenAI stream rules (the default) — complete on `[DONE]` — which
  would accept a shape no rerank server sends. Worth a sentence in `GATEWAY.md` →
  Rerank answers if the reading stands.
- **Absent `documents`** counts as one for the cap (no value at all); the spec names
  lists and other values only. Harmless (cap ≥ 1, the backend refuses it).
- No contradiction found between the spec and the code for this step.
- Left for later steps: `CodeEndpointMissing`'s comment ("the server's version
  predates the endpoint") and the vllm core endpoints are step 5's;
  `docs/TECH-STACK.md` → Testing describes the fake backend as speaking "the OpenAI
  API, Messages and Responses" — it now answers rerank too (step 6's docs pass).

### Suite (2026-10-08)

The step-1 reds are cleared (`cmd/kaiak` 1, `internal/config` 12, `internal/control`
2): they failed only for the unknown field.

- `scripts/check-gateway.sh`: green — gofmt, vet, staticcheck 2026.2.1, telemetry
  boundary; `go test -race -count=1 ./...` every package `ok` (`e2e` 122 s,
  `internal/server` 15 s); the live kit's lint and self-test: `self-test passed for
  vllm, llama-server, openai, azure-openai, anthropic, azure-anthropic, vllm with two
  backends` (vllm's `models` check lists `rerank`); `gateway checks passed`.
- `scripts/check-all.sh` (3 m 26 s): green —
  - gateway stages as above, `gateway checks passed`;
  - control `npm test`: `tests 630, suites 45, pass 629, fail 0, skipped 1`;
  - `npm run lint`: `boundaries ok`;
  - cross-half e2e: `ok kaiak/e2e 65.381s`;
  - `all checks passed`.
