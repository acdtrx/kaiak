# Plan: Rerank

## Goal

Serve reranker models (Qwen3-Reranker and the like) through the full request
pipeline: a client sends a query and a list of documents to `POST /v1/rerank` and
gets each document's relevance score, passed through to a vLLM or llama-server backend
that serves the endpoint natively, metered, limited and priced like any request.

On the way, fix how the gateway reads a self-hosted server's answer to an endpoint its
loaded model does not serve: today a chat request sent to a reranker or embedding
model on vLLM counts as a failure of a healthy deployment, and enough of them open its
circuit for everyone.

## Scope

- **Client API endpoint:** `POST /v1/rerank`, the request and answer shape vLLM and
  llama-server both serve (Jina's rerank API, which Cohere's v1 matches): `model`,
  `query`, `documents`, `top_n`, plus each server's own fields, passed through.
- **Backend types:** `vllm` and `llama-server` serve it. The cloud types have no rerank
  API; `openai-compatible` keeps OpenAI's three endpoints.
- **Every pipeline stage learns the endpoint:** owned fields, a cap on documents per
  request, the input estimate, usage reading, the operation name on logs and metrics.
- **Both halves:** a new optional config field, `global.max_rerank_documents`, in the
  contract docs, schemas, fixtures, `kaiak-control` types and validation, and the
  sample. No version bump (decision 16).
- **Wrong-endpoint answers** (phase 2): a self-hosted server's answer that its model
  does not serve the endpoint reads as the endpoint missing — neutral for the circuit
  — instead of a wrong `base_url` or a backend failure.
- **Live-test kit:** rerank checks and a reranker server of its own; the
  wrong-endpoint check. Live runs stay manual; only the self-test runs in the suite.
- **Docs:** how to run Qwen3-Reranker on vLLM and llama-server in `DEPLOYMENT.md`.

## Out of scope

- **Other rerank routes:** `/rerank`, `/v2/rerank` (Cohere v2) and vLLM's own
  `/v1/score`, `/classify` and `/pooling`.
- **Cloud rerank services** (Cohere, Jina, Voyage, Cohere on Microsoft Foundry): no
  backend type until one is asked for.
- **Translating between the servers' answers:** vLLM's results carry `document` and
  an `id`, llama-server's do not; both carry `index` and `relevance_score`. The client
  gets its backend's own answer.
- **A per-document size cap** (decision 4).
- **`/v1/models` listing endpoints per loaded model:** `endpoints` stays per backend
  type, so a vLLM chat model lists `rerank` and a vLLM reranker lists
  `chat_completions`. Backlog (step 1).
- **TEI's rerank format** (`texts`, `return_text`).

## Decisions

Settled with the user (2026-10-08):

1. **`POST /v1/rerank` only.**
2. **Served by `vllm` and `llama-server`.** OpenAI, Azure OpenAI, Anthropic and Claude
   in Foundry have no rerank API. `openai-compatible` stays at OpenAI's three, by the
   type-per-server rule (`GATEWAY.md`, Endpoint support).
3. **A cap on documents per request:** `global.max_rerank_documents`, default 1000.
   Above it, `400 invalid_value`, `param: "documents"`. It works the way
   `max_embedding_inputs` does.
4. **No per-document size cap in kaiak.**
   - Each document is scored with the query and the model's template, and that pair
     must fit the model's context. The backend refuses a longer one with a `400` the
     caller can act on: vLLM above `--max-model-len`; llama-server above a slot's
     context.
   - The body cap (`max_request_body_bytes`) bounds a request's total text.
5. **Input estimate:** the body as today, plus the query's estimate once more for each
   document beyond the first, since every pair repeats the query.
   - The model's template is not counted.
   - The backend's reported usage settles the record.
6. **`gen_ai.operation.name` is `rerank`.**
   - OpenTelemetry's GenAI conventions have no well-known value for reranking, and
     they allow a custom value when none applies. `retrieval` names a vector-store
     search, not reranking.
   - `GATEWAY.md`'s rule of well-known values only gets a dated amendment.
7. **The wrong-endpoint fix lands in this plan** (phase 2).
8. **Live runs on the DGX:** vLLM and llama-server, once the user says the DGX is free.

Made while planning (confirm in review):

9. **Rerank has a request format of its own**, not OpenAI's.
   - No `stream`, no `stream_options` edit, no output limit and no sequences.
   - Gateway errors use OpenAI's error shape, as on every endpoint but Messages; vLLM
     answers its rerank errors in that shape.
   - Rejected: reusing the OpenAI format. It would make rerank a core endpoint, so a
     server without it would read as a wrong `base_url` and count toward the circuit.
10. **Owned fields:**
    - `model` is required and rewritten.
    - `documents` is counted for the cap: a list counts its elements; anything else
      counts as one, and the backend judges its shape.
    - `query` is read only for the estimate.
    - Everything else passes untouched: `top_n`, `return_documents`, `instruction`,
      `chat_template_kwargs`, `max_tokens_per_doc`, `truncate_prompt_tokens`,
      multimodal content parts, and `stream`, which neither server reads.
11. **Usage:**
    - `usage.prompt_tokens` maps to `tokens_in`; there is no output.
    - Both servers report it: vLLM 0.30.0 and current send `{prompt_tokens,
      total_tokens}`, and llama-server sends the same.
    - Missing usage is estimated and flagged, by the existing rule.
    - Pricing goes through the model's `prices` on `tokens_in`, as for embeddings.
      No new unit.
12. **The answer is relayed as the backend sends it.**
    - Its top-level `model` is rewritten to the public name.
    - It is complete when its JSON value closes, as today.
13. **Rerank is not a core endpoint.** A server without it answers
    `502 upstream_endpoint_missing`, which is neutral for the circuit.
14. **Wrong-endpoint answers.** Step 1 settles these from the servers' sources:
    - **vLLM:**
      - vLLM creates its routes from the loaded model's tasks (`vllm/entrypoints/
        launchers/api_server/routers.py`, `pooling/factories.py`, main on
        2026-10-08). A reranker or embedding server has no chat route, and a chat
        server has no embeddings or rerank route.
      - So vLLM's route-missing answer (`{"detail": "Not Found"}`, or `Method Not
        Allowed`) reads as the endpoint missing on every endpoint. `vllm` keeps no
        core endpoints.
    - **llama-server:**
      - llama-server registers every route whatever its model, so its `404` rule
        stays.
      - Its `501 not_supported_error` on embeddings or rerank means the server was not
        started in that mode (`--embeddings`, `--reranking`). It reads as the endpoint
        missing.
      - Step 1 listed llama-server's `501`s: on kaiak's endpoints there are only
        these two. A chat request carrying media its model lacks answers `500`, not
        `501` (b9917 to b11513) — recorded under the existing backlog entry for
        llama-server's client errors answered `500`, out of this plan.
    - **What is remembered:** a missing endpoint is remembered per deployment, not
      per backend, and a request's retries refuse only that deployment.
      llama-server's router mode runs several models with their own flags behind one
      backend (step 1 confirmed).
    - **A wrong `base_url` on a `vllm` backend** stops counting toward the circuit. It
      still shows in two ways:
      - the config-apply warning when the models list answers `404`;
      - the endpoint-missing warning on each request.

      The endpoint memory keeps traffic off that backend.
    - **`openai-compatible` is unchanged:** its server is unknown. `DEPLOYMENT.md`
      already points vLLM to the `vllm` type.
15. **`DEPLOYMENT.md` documents serving Qwen3-Reranker:**
    - **vLLM:** `--runner pooling`, the `--hf_overrides` for the original checkpoint,
      and the score template (`--chat-template`). Without the template, scores are
      worse.
    - **llama-server:**
      - Start it with `--embedding --pooling rank --reranking`, on build b11223 or
        later (corrected at step 6, 2026-10-09). From that build a causal reranker
        such as Qwen3-Reranker is processed in chunks, so a pair over a slot's context
        answers `400 exceed_context_size_error` whatever `-ub` is. On older builds,
        and for encoder rerankers (bge-reranker-v2-m3) on any build, a pair longer
        than the physical batch (`-ub`) answers `500`, read as the backend failing:
        raising `-ub` to a slot's context does not help, because the batch check
        comes first.
16. **No version bump** (changed at step 1 review, 2026-10-08): config format and
    protocol stay 5. `max_rerank_documents` is optional and additive, the case the
    settled "Types bump no version" rule covers (`CONTROL-PROTOCOL.md`, 2026-10-01):
    every current config stays valid, an older gateway rejects a config carrying the
    field with a schema error the control plane sees like any rejection, and no
    protocol message changes. Usage records are unchanged: no new unit, and the
    operation is not on the wire.

## Constraints

- **One pipeline, no side doors:** rerank passes every stage.
- **Passthrough** preserves what it does not understand, and edits only the owned
  fields.
- **Only provider packages talk to backends.**
- **Nothing sensitive in logs:** no query or document text. New refusals name
  parameters and counts, never values.
- **Protocol changes land on both halves at once.**
- No new dependency in either half.

## Risks

- **Score quality depends on the backend's setup** (vLLM's score template, the
  `--hf_overrides`), which kaiak cannot see.
  - Mitigation: `DEPLOYMENT.md` gives the exact flags.
  - The live run checks that a relevant document outranks an irrelevant one.
- **llama-server answers a long pair with a `500`** on builds before b11223 and for
  encoder rerankers, which counts toward the circuit.
  - Mitigation: `DEPLOYMENT.md` names the build (decision 15).
  - The live run sends an oversize document to both servers.
- **Weaker wrong-`base_url` signal on `vllm`** (decision 14).
  - Mitigation: the config-apply warning and the per-request endpoint-missing warning
    stay.
  - The live kit's wrong-path check is updated to what `vllm` now answers.
- **The estimate leaves out the template**, about 70 tokens per pair for Qwen3. Token
  limits check a lower figure before the request.
  - Mitigation: none needed. The reported usage settles the record, as on every
    endpoint.
- **Server APIs drift:** both projects change these endpoints often.
  - Mitigation: step 1 records the versions checked.
  - The live kit runs against current builds.

## Tag

`v0.12.1` on `main`, "before the rerank plan", immediately before step 1 starts. It is
not pushed to `github`: an anchor, not a release.

## Branch and worktree

Branch `rerank`, worktree `.claude/worktrees/rerank`. It rebases onto `main` before
the ff merge.

Steps are implemented by subagents (the same model as the main session), one step per
brief. The main session reviews each against its acceptance criteria and commits at
the step boundary. The branch merges after step 8. Releasing is the user's call. A
gateway older than the release refuses a config that sets `max_rerank_documents`, so
upgrade gateways before setting it.

## Phases and steps

- **Phase 1 — Rerank** (steps 1–4). Green at the end: `/v1/rerank` works end to end
  against the fakes, with the new field on both halves.
  1. `STEP-1-contract.md` — research recorded, specs, schema, fixtures, backlog.
  2. `STEP-2-kaiak-control.md` — the field, the sample.
  3. `STEP-3-gateway-rerank.md` — the endpoint through every stage, the field.
  4. `STEP-4-e2e.md` — the fake backend's rerank, gateway e2e, cross-half e2e.
- **Phase 2 — Wrong-endpoint answers** (step 5). Green at the end.
  5. `STEP-5-wrong-endpoint.md`
- **Phase 3 — Live kit, docs, review, live run** (steps 6–8). Green at the end.
  6. `STEP-6-live-kit-and-docs.md`
  7. `STEP-7-review-and-green.md`
  8. `STEP-8-live-run.md` — waits for the DGX.

Expected reds inside phase 1:
- After step 1, both halves fail the fixtures that carry the new field, and
  `kaiak-control`'s schema copy differs from `protocol/schema/`.
- Step 2 clears `kaiak-control`'s, and step 3 the gateway's.

## Verification

- **Component tests:**
  - owned fields, the cap, and the estimate (query repeated per document, media in
    the query);
  - the usage reader (reported, missing, malformed);
  - the operation on the log line and the usage metrics;
  - the route and its `405`;
  - endpoint support per type;
  - each module's wrong-endpoint reading.
- **Gateway e2e against the fake backend:**
  - rerank through the pipeline on a `vllm`-typed and a `llama-server`-typed backend;
  - the usage record (`tokens_in` from `prompt_tokens`) and its cost;
  - the cap refusal;
  - `endpoint_not_served` for a model with no rerank-serving deployment;
  - a chat request to a reranker deployment answering the route-missing shape:
    neutral, and the deployment keeps serving rerank.
- **Cross-half e2e:** a rerank request settles at the sample.
- **`scripts/check-all.sh`** green at every phase end, and 3× in a row at the end.
- **Live (step 8):**
  - Qwen3-Reranker on the DGX's vLLM and on llama-server;
  - rerank answers, with usage reported, not estimated;
  - an oversize document refused with `400`;
  - wrong-endpoint requests neutral.

**Review outcome** (step 7, 2026-10-09): two agent reviews of the branch and an
independent Codex review of a plain copy. Eleven findings: the high one — llama-server
reads `texts` in place of `documents` and answers it without usage, so the cap and
the usage could be bypassed — and five lower ones fixed with tests (`texts`
refused; documents counted without allocating; the endpoint-missing warning once per
interval; usage without `prompt_tokens` estimated, embeddings included; untested
helpers and wiring); four doc corrections; two pre-existing llama-server behaviours
(router mode's missing-model `400`, chat to an encoder model answering `500`) and
embeddings' `promptCount` allocation recorded in `docs/BACKLOG.md`.
`STEP-7-review-and-green.md` has the table.

**Verification status:** component, e2e and cross-half tests green, `check-all.sh` 3×
in a row (2026-10-09). The live run (step 8) waits for the DGX.
