# Plan: Messages and Responses passthrough

## Goal

Serve Anthropic Messages and OpenAI Responses clients (Claude Code, Codex, the
Anthropic and OpenAI SDKs) through the full request pipeline, passing each request
through to a backend that speaks the same API natively. Add the backend types that
speak Messages: Anthropic's API and Claude in Microsoft Foundry. Drop the gateway's
model defaults, which do not carry over between APIs and which the backends already
own.

## Scope

- **Client API endpoints:** `POST /v1/messages`, `POST /v1/messages/count_tokens`,
  `POST /v1/responses`, `POST /v1/responses/input_tokens`, and Anthropic-shaped
  `GET /v1/models` and `GET /v1/models/{id}` (chosen by the `anthropic-version`
  request header).
- **Passthrough only:** a request goes to the deployments whose backend serves its
  endpoint natively, with only the edits the gateway owns.
- **New backend types:** `anthropic` (Anthropic's API) and `azure-anthropic` (Claude in
  Microsoft Foundry), both serving Messages only.
- **Inbound auth:** `x-api-key` is accepted beside `Authorization: Bearer`.
- **Gateway rules on the new APIs:**
  - Responses is stateless: `store` forced to `false`, and the stateful fields
    refused.
  - Tools the backend would run (hosted tools) are refused.
  - On Anthropic types, options that change the price are refused and the tier is
    forced to standard.
  - No client header reaches a backend.
- **Model defaults dropped:** the `defaults` config field goes, both halves.
  `context_length`, `reasoning_efforts` and `capabilities` stay as metadata, and the
  output limit (default and ceiling) stays applied.
- **Every pipeline stage learns the new formats:** owned fields, input estimate,
  usage reading, completeness, model-name rewriting, error shapes, wrong-model and
  wrong-path signatures.
- **Both halves:** contract docs, schemas, fixtures, `kaiak-control` types and
  validation, `backend-verify` for the new types, the sample, the GUIDE, examples.
- **Live-test kit:** `anthropic` and `azure-anthropic` kinds, plus Messages and
  Responses checks for the existing kinds. Live runs stay manual; only the self-test
  runs in the suite.

## Out of scope

- **Translation between APIs** (Messages ⇄ Chat Completions, Responses ⇄ Chat
  Completions): backlog. Until it lands, a model is reachable only through the APIs
  its deployments' backends serve natively. A Claude model is Messages-only, an
  OpenAI model chat/completions/embeddings/Responses.
- **Stateful Responses:** `previous_response_id`, `conversation`, `background`,
  `GET`/`DELETE /v1/responses/{id}`, the Conversations API. These stay out; a
  conversation store does not belong in the gateway (Kora note 2026-09-30: a separate
  agent runtime if ever).
- **Hosted tools of any kind**, on either API.
- **Forwarding `anthropic-beta` / `anthropic-version` from clients:** backlog, a
  decision is needed.
- **Health checks for `azure-anthropic`**, which has no models list: no config-time
  model check, and the circuit probe always passes so the half-open trial decides.
  Backlog.
- **Pricing the 1-hour cache write, fast mode or data-residency options:** they are
  refused, not priced.
- **Bedrock:** stays in the backlog.

## Decisions

Settled with the user (2026-10-06):

1. **Passthrough only; translation is backlogged.**
2. **Both APIs**, with the token-counting endpoints and Anthropic-shaped
   `/v1/models`.
3. **`anthropic` and `azure-anthropic` backend types**, built the way `openai` /
   `azure-openai` are. Claude in Foundry serves Claude models only, on
   `https://{resource}.services.ai.azure.com/anthropic/v1/*`.
4. **Responses is stateless.**
   - `store` is forced to `false` on every request.
   - `previous_response_id`, `conversation` and `background: true` are refused.
   - No `/v1/responses/{id}` routes.
5. **Hosted tools are refused** on both APIs.
6. **Standard price only.**
   - On Anthropic types, options that change the price are refused: `speed`, a
     non-default `inference_geo`, and a 1-hour cache write.
   - The tier is forced to standard.
7. **No client headers are forwarded**, `anthropic-beta` and `anthropic-version`
   included. A backlog entry records that a decision is needed.
8. **Defaults dropped.**
   - The backends apply their own defaults; config `defaults` goes.
   - `context_length`, `reasoning_efforts` and `capabilities` stay as information.
   - `context_length` also stays a check: an output limit above it is refused, and
     the injected default is fitted under it.
   - The output limit stays applied.
9. **`azure-anthropic` gets no health check for now** (see Out of scope).
10. **Live-test kit:** new kinds and checks, live runs opt-in; the self-test is in the
    suite.
11. **Released together with `docs/plans/control-replicas/`**, under one protocol
    version (5) and one config format (5).

Made while planning (confirm in review):

12. **Each backend type fixes the endpoints it serves**, in its module — no config
    field.
    - `openai-compatible` keeps OpenAI's three. A server that serves more gets its own
      type, following the type-per-server rule of 2026-09-30.
    - Step 1 checks each server's sources for the new endpoints and records what each
      type claims.

    | Type | Chat, completions, embeddings | Messages, count_tokens | Responses | input_tokens |
    |---|---|---|---|---|
    | `llama-server` | yes | yes | yes | yes |
    | `vllm` | yes | yes | yes | no (not in 0.30.0) |
    | `openai` | yes | no | yes | yes |
    | `azure-openai` | yes | no | yes | no (Azure's v1 API answers `404`) |
    | `anthropic`, `azure-anthropic` | no | yes | no | no |
    | `openai-compatible` | yes | no | no | no |

    The token-counting endpoints are claimed only where the server has them (step 1
    checked; `docs/specs/GATEWAY.md`, Providers → Endpoint support).
13. **Routing takes only deployments that serve the endpoint.** A model with none
    answers `400 endpoint_not_served`, naming the endpoint: it already passed the
    model-access check, so naming it leaks nothing.
14. **A server version missing an endpoint its type claims** (an older vLLM without
    `/v1/messages`) is not a wrong `base_url`.
    - It answers `502 upstream_endpoint_missing`.
    - It is retried on other backends.
    - It is not a circuit failure, so the deployment keeps serving its other
      endpoints, and it is logged as a warning.
15. **Inbound auth:** `Authorization: Bearer` first; `x-api-key` when there is no
    `Authorization` header. This applies on every endpoint.
16. **Error shapes.**
    - On Messages and count_tokens, every answer kaiak itself gives uses Anthropic's
      shape: `{"type": "error", "error": {"type", "message"}}`, plus kaiak's stable
      `code` inside `error`; SDKs ignore unknown members.
    - The `type` follows the status: 400 `invalid_request_error`, 401
      `authentication_error`, 403 `permission_error`, 404 `not_found_error`, 413
      `request_too_large`, 429 `rate_limit_error`, 503 `overloaded_error`, other 5xx
      `api_error`.
    - Limit refusals keep `Retry-After`.
    - Responses keeps OpenAI's shape.
17. **Owned fields.**
    - Messages: `model`, `stream`, `max_tokens`, `tools[].type`, `mcp_servers`,
      `container`, `service_tier`, `speed`, `inference_geo`, and `cache_control.ttl`
      wherever it appears.
    - Responses: `model`, `stream`, `max_output_tokens`, `store`,
      `previous_response_id`, `conversation`, `background`, `tools[].type`,
      `tool_choice`, `service_tier`.
18. **Hosted tools are refused by allowlist** in the inbound stage, on every backend
    (`400`, naming the tool type), so a new server tool is refused by default.
    - Messages allows tools with no `type`, `custom`, and the client-run Anthropic
      tools `bash_*`, `text_editor_*`, `computer_*`, `memory_*`; `mcp_servers` and
      `container` are refused.
    - Responses allows `function`, `custom`, `local_shell`, `shell` and `apply_patch`;
      a `tool_choice` naming another type is refused.
19. **Price options are refused by the Anthropic modules** (a caller `400`, never sent,
    not retried, not a circuit failure). Other types price none of them, so they pass
    the fields untouched, as with `service_tier` today.
20. **Service tier.**
    - `anthropic` always sends `service_tier: "standard_only"`, replacing the
      client's value. `azure-anthropic` adds none (Foundry has no Priority Tier; step
      1 changed this from "both Anthropic types").
    - `openai` and `azure-openai` force `"default"` on Responses as they do on chat.
21. **Token-counting endpoints** run the whole pipeline.
    - They count toward requests-per-minute limits and reserve no output.
    - They produce no usage record, because nothing is billed.
    - They are logged and counted in the ops metrics like any request.
22. **Anthropic-shaped `/v1/models`** (request carries `anthropic-version`):
    - It lists the models the key may use whose deployments serve Messages.
    - The shape is Anthropic's (`type: "model"`, `id`, `display_name` = id,
      `created_at` fixed, `has_more: false`, `first_id`, `last_id`), with kaiak's
      metadata added.
23. **Model entries gain `endpoints`**: the client API endpoints the model's
    deployments serve, sorted, so a client can see which API reaches it.
    Informational.
24. **Usage mapping onto kaiak's units.**
    - Messages:
      - `tokens_in` = `input_tokens`, which already excludes the cache.
      - `tokens_cached` = `cache_read_input_tokens`.
      - `tokens_cache_write` = `cache_creation_input_tokens`.
      - `tokens_out` = `output_tokens`.
      - `tokens_reasoning` = 0, because it isn't reported.
      - Streams read `message_start`, then every `message_delta`; the latest value of
        each field wins.
    - Responses:
      - `tokens_in` = `input_tokens` − cached − written.
      - `tokens_cached` = `input_tokens_details.cached_tokens`.
      - `tokens_cache_write` = `input_tokens_details.cache_write_tokens` when present.
      - `tokens_out` = `output_tokens`.
      - `tokens_reasoning` = `output_tokens_details.reasoning_tokens`.
      - Streams read `response.completed` and `response.incomplete`.
25. **Completeness.**
    - A Messages stream is complete at `message_stop`.
    - A Responses stream is complete at `response.completed` or
      `response.incomplete`. `response.failed` or an `error` event is a backend
      failure after the first event.
    - JSON bodies are complete when their top-level value closes, as today.
26. **Model-name rewriting:** the top-level `model` of JSON bodies, plus
    `message.model` in `message_start` and `response.model` in `response.*` events.
27. **Anthropic types on the wire.**
    - The gateway sends `anthropic-version: 2023-06-01`.
    - Credential: `x-api-key` for `anthropic`, `api-key` for `azure-anthropic`.
    - Base URLs: `anthropic` takes `https://api.anthropic.com/v1`; `azure-anthropic`
      takes the resource endpoint, and the gateway appends `/anthropic/v1/`.
    - Anthropic's `529 overloaded_error` counts as a `5xx`.
    - `api_key_env` is required for both types.
28. **Observability.**
    - Endpoint metric labels: `messages`, `messages_count_tokens`, `responses`,
      `responses_input_tokens`.
    - `gen_ai.operation.name` is `chat` for Messages and Responses.
    - `gen_ai.provider.name` follows the OpenTelemetry GenAI values for the new types
      (step 1 checks them).
    - Usage records gain no field: the deployment already says where a request went.
29. **Versions** (no backwards compatibility):
    - protocol 5 (the config in snapshots changes)
    - config `format_version` 5 (new types, no `defaults`)
    - `last-known-good.json` format bumped (it holds the config)
    - usage spool unchanged (records unchanged)

## Constraints

- One pipeline, no side doors: the new endpoints pass every stage.
- Passthrough preserves what it does not understand; edits only the owned fields.
- Only provider packages talk to backends; only `backend-verify` in `control/`.
- Nothing sensitive in logs: no prompt or response content. New refusals name
  parameters and tool types, never values the client wrote.
- Protocol changes land on both halves at once. The gateway stays dependency-free.

## Risks

- **Server endpoint support moves with versions.** vLLM and llama-server gained these
  endpoints recently and change them often.
  - Mitigation: decision 14 contains an old version to its endpoint.
  - Step 1 records the versions checked.
  - The live kit runs against current builds.
- **Clients send options we refuse by default.**
  - Codex attaches its hosted `web_search` tool by default (`web_search = "cached"`).
    If it does so with custom providers too, Codex users must set
    `web_search = "disabled"`.
  - Claude Code may ask for 1-hour cache writes, which the Anthropic types refuse.
  - Mitigation: the live client checks (phase 4) show it before release. The refusal
    message names the field so the fix is one config line, and the runbook documents
    it.
- **Stream formats are richer than OpenAI's.** Completeness, usage and model
  rewriting read typed events.
  - Mitigation: the fake backend speaks both formats with recorded event sequences
    from real servers (step 1 captures them from the DGX vLLM and llama-server).
- **The client-API seam (step 3) touches the core of the pipeline.**
  - Mitigation: step 3 moves the OpenAI format behind it with no change in behaviour,
    and the existing suite must stay green unchanged except for the defaults tests
    that go.
- **No direct access to Anthropic or Foundry.** The `anthropic` and
  `azure-anthropic` modules are built from documentation.
  - Mitigation: the live kit plus the runbook's tester section, run by someone with
    access before the release.

## Tag

Tag `main` right before step 1 begins (AGENTS.md → Git): `v0.10.1`, message "before
Messages/Responses passthrough and control-plane replicas". Local and `origin` only.

## Branch and worktree

Branch `messages-responses`, worktree `.claude/worktrees/messages-responses`. It
rebases onto `main` before the ff merge. `docs/plans/control-replicas/` follows on
its own branch after this one merges; the release waits for both.

## Phases and steps

- **Phase 1 — contract and groundwork** (steps 1–3). Green at the end. The new
  endpoints do not exist yet; the new types exist and serve nothing reachable.
  1. `STEP-1-contract.md`: research, specs, schemas, fixtures, versions, backlog.
  2. `STEP-2-kaiak-control.md`: types, validation, defaults gone, `backend-verify`,
     sample, examples, GUIDE.
  3. `STEP-3-gateway-groundwork.md`: defaults removed, versions, the client-API
     seam, endpoint support per type, `x-api-key`, the Anthropic modules.
- **Phase 2 — Messages** (step 4). Green at the end.
  4. `STEP-4-messages.md`
- **Phase 3 — Responses** (step 5). Green at the end.
  5. `STEP-5-responses.md`
- **Phase 4 — live kit, end to end, docs** (steps 6–7). Green at the end.
  6. `STEP-6-live-kit.md`
  7. `STEP-7-e2e-and-docs.md`

Expected reds inside phase 1:
- After step 1, both halves fail the new fixtures and the version checks. Step 2
  clears `kaiak-control`'s and step 3 the gateway's.
- The cross-half e2e stays red until both halves speak protocol 5 (step 3).

## Verification

- **Component tests per format:**
  - owned fields and refusals (stateful fields, hosted tools, price options)
  - output-limit default and ceiling under `max_tokens` / `max_output_tokens`
  - input estimate
  - usage mapping, streamed and not
  - completeness
  - model rewriting, nested included
  - error shapes for every gateway answer on each API
- **Routing:** endpoint support per type; `endpoint_not_served`;
  `upstream_endpoint_missing` retried and not counted toward the circuit.
- **Anthropic modules:** URLs, headers, `standard_only`, price-option refusals, 529,
  wrong-model and wrong-path signatures, probe behaviour.
- **Shared fixtures run by both halves:** the new types, a config with `defaults`
  refused, the version checks.
- **Gateway e2e against the fake backend:**
  - each new endpoint, streamed and not, through the whole pipeline
  - usage records with the right units
  - limits refusing in each API's shape
  - the token-counting endpoints leaving no record
  - Anthropic-shaped `/v1/models`
- **Cross-half e2e:** a Messages request and a Responses request settle at the
  sample.
- **`scripts/check-all.sh` green at every phase end.**
- **Live, after phase 4** (`docs/testing/LIVE-BACKENDS.md`):
  - The kit against the DGX's vLLM and llama-server: Messages and Responses.
  - The `anthropic`, `azure-anthropic`, `openai` and `azure-openai` kinds, run by a
    tester with access.
  - Claude Code and Codex each running one task through a local kaiak.
