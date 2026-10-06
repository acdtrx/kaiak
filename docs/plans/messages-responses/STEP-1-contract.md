# Step 1 — contract

**Status:** done (2026-10-06)

## Intent

Settle in the contract docs and the shared schemas everything this plan builds: the
new endpoints and their rules, the new backend types, the endpoint support per type,
the removal of `defaults`, and the version bumps. Every later step implements what
this step writes. Before writing, check the servers' real behaviour so the contract
rests on facts rather than on documentation alone.

## Research first (record findings in this file's Result)

- **Endpoint support**, from the current sources (record versions or commits):
  - vLLM: `/v1/messages`, `/v1/messages/count_tokens`, `/v1/responses`,
    `/v1/responses/input_tokens`.
  - llama-server: the same four.
  - OpenAI: `/v1/responses/input_tokens`.
  - Azure OpenAI v1: `/openai/v1/responses`, `/openai/v1/responses/input_tokens`.
  - Anthropic and Foundry: `/v1/messages/count_tokens`.
  - Decision 12's table is filled from this; a type claims only what its server has.
- **Recorded event sequences**, captured from the DGX's vLLM and llama-server for the
  fake backend and the tests:
  - Messages streamed and not, including a tool call and a thinking block where the
    model supports it.
  - Responses streamed and not.
  - The token-counting answers.
  - Each server's `404` for a missing model and a missing path on the new endpoints,
    and its error body shape.
  - Use `cria` entries per the working-style memory, and restore the user's model
    afterwards.
- **Anthropic and Foundry from documentation:**
  - the Messages event set and error shape, including `529` and `error` events
    mid-stream
  - the models list shape
  - a missing model's `404`
  - Foundry's answer to a wrong path
  - each price option (`service_tier`, `speed`, `inference_geo`, `cache_control.ttl`)
    and its values
- **Responses usage fields** on OpenAI and Azure: whether
  `input_tokens_details.cache_write_tokens` exists there.
- **OpenTelemetry GenAI `gen_ai.provider.name`** values for Anthropic and for Claude
  in Foundry.

## Files likely touched

- `docs/specs/GATEWAY.md`:
  - Client API:
    - the new endpoints and their routes
    - Anthropic-shaped `/v1/models` chosen by `anthropic-version`
    - `x-api-key`
    - the error shape on Messages endpoints, with the type-by-status table
    - new codes: `endpoint_not_served`, `upstream_endpoint_missing`, the stateful-field
      refusals, the hosted-tool refusal, the price-option refusal
    - owned fields per API
    - the `endpoints` member on model entries
    - `props` without `defaults`
  - Model metadata: defaults removed (dated, with the reason: the backends own
    defaults and they do not carry between APIs); what `context_length` still does.
  - Request pipeline: the inbound stage parses by endpoint format; the
    token-counting endpoints' path through the stages.
  - Providers: the two new types; endpoint support per type (the table); passthrough
    edits per API; service tier on Responses and on Anthropic types; Anthropic
    headers; URL layouts; wrong-model and wrong-path signatures for the new types and
    endpoints; `upstream_endpoint_missing`; completeness per format; model rewriting
    per format; the `azure-anthropic` probe rule.
  - Limits: the output-limit keys per API.
  - Accounting: the usage mapping per API; token-counting endpoints leave no record.
  - Observability: endpoint labels, `gen_ai.*` values, any new `kaiak.relay_end`
    value.
  - Data directory: the `last-known-good.json` format.
- `docs/specs/CONTROL-PROTOCOL.md`:
  - Config: backend types (two new, `api_key_env` required for them); `defaults`
    gone.
  - Protocol version 5, config `format_version` 5.
- `docs/specs/BACKEND-VERIFY.md`:
  - `anthropic`: its models list, header, what it can report.
  - `azure-anthropic`: no models list; a note, no check.
- `protocol/schema/config.schema.json` (types, `defaults` removed, format 5) and every
  `protocol_version` carrier. Then `npm run sync-schemas` from `control/`.
- `protocol/fixtures/**`, by a one-off script outside the repo:
  - config `format_version` 4 → 5, `protocol_version` 4 → 5
  - `defaults` removed from every config fixture
  - old-version cases renamed as in earlier bumps
- New fixtures:
  - valid configs with an `anthropic` and an `azure-anthropic` backend
  - invalid: either type without `api_key_env`
  - invalid: a model with `defaults` (an unknown field now)
- `docs/BACKLOG.md`:
  - Replace the two inbound entries with **Messages ⇄ Chat Completions translation**
    and **Responses ⇄ Chat Completions translation**. Trigger: a client must reach a
    model whose backends do not serve its API.
  - Add **Client `anthropic-beta`/`anthropic-version` forwarding**: a decision is
    needed. Trigger: a client feature needs a beta on an Anthropic backend.
  - Add **`azure-anthropic` health checks**: a probe and a config-time model check
    without a models list. Trigger: Foundry deployments misconfigured unseen, or a
    circuit that closes on a trial too late.
  - Add **Pricing Anthropic price options**: 1-hour cache writes, fast mode, data
    residency. Trigger: a client needs one of them.

## Decisions made during planning

- One fixture transform script, plus a second scratch script that checks every
  fixture equals its `HEAD` version under exactly that transform. Neither is
  committed.
- The spec records the endpoint table and the capture versions as facts, dated, so a
  later server upgrade has something to compare against.
- Recorded event sequences are committed as test data where the tests of steps 4–5
  need them. They hold no prompt content beyond the test prompts.

## Acceptance criteria

- Every decision in the OVERVIEW appears in its owning spec section, dated
  2026-10-06. Settled ones are marked settled; planning ones also carry what was
  rejected where that matters.
- The research findings are in Result, with sources and versions; decision 12's
  table is filled.
- The schema copy is in sync (`npm test`'s byte-for-byte check passes).
- The fixture diff is exactly the scripted transform plus the new fixtures.
- Suite run and recorded. Expected reds:
  - `kaiak-control`'s fixture and version tests (cleared by step 2)
  - the gateway's fixture and version tests (cleared by step 3)
  - the cross-half e2e (cleared by step 3)

## Result

**What changed**

- `docs/specs/GATEWAY.md`:
  - **Client API**: the endpoints; Client APIs (three formats, passthrough only, the
    translation rejected); Responses is stateless; hosted tools refused (the
    allowlists); token-counting endpoints; `x-api-key`; the Anthropic error shape on
    Messages endpoints with the type-by-status mapping (529 → `overloaded_error`);
    five new codes in the table; owned fields per format; `endpoints` on model
    entries; the Anthropic-shaped model list; `props` without `defaults`.
  - **Model metadata**: "Metadata is information" and "No model defaults"
    replace the defaults rules (with the rejected option and its cost).
  - **Request pipeline**: inbound per format; routing among the deployments that
    serve the endpoint; the provider stage passes through.
  - **Providers**: the two types; the endpoint support table; base URLs, credentials
    and `anthropic-version`; a provider's caller-error refusal; passthrough edits per
    format; service tier on Responses; Standard price on Anthropic types; usage chunk
    OpenAI-only; headers (no `anthropic-beta`); nested model rewriting; backend error
    shapes; wrong-model and wrong-path signatures for the new types; An endpoint
    missing from a server; completeness per format and error events; probe and model
    check for the new types.
  - **Limits**: the input estimate over Messages and Responses bodies; the output-limit
    keys `max_tokens` (Messages) and `max_output_tokens` (Responses).
  - **Accounting**: no record for the token-counting endpoints; Messages and Responses
    usage mappings; estimated output per format.
  - **Observability**: endpoint labels; `endpoint_missing` (retry reason, attempt
    outcome, neutral class); `gen_ai.operation.name` and `gen_ai.provider.name`;
    `kaiak.backend.type` values. **Data directory**: `last-known-good.json` format 6.
- `docs/specs/CONTROL-PROTOCOL.md`: protocol 5; config `format_version` 5; "Strict
  everywhere" replaces the `defaults` open map (and its number rule); backend types
  with the Anthropic ones and their `api_key_env` requirement.
- `docs/specs/BACKEND-VERIFY.md`: the `type` option; `anthropic` (models list with
  `?limit=1000`, `x-api-key` + `anthropic-version`, `context_length` from
  `max_input_tokens`); `azure-anthropic` (no request, a note); recognition and the
  Azure listing rule extended.
- `protocol/schema/config.schema.json` (format 5, the two types and their
  `api_key_env` rule, `defaults` and its `$defs` removed),
  `protocol/schema/status.schema.json` (protocol 5); copies synced into
  `control/kaiak-control/schema/` (`npm run sync-schemas`).
- `protocol/fixtures/**`: the scripted transform — `format_version` 4 → 5 and
  `protocol_version` 4 → 5 everywhere; every `defaults` member removed;
  `format-version-3` → `format-version-4` and `protocol-version-3` →
  `protocol-version-4`; the eight `defaults-*` invalid config fixtures and their cases
  deleted (their rules are gone). A second script checked every fixture equals its
  `HEAD` version under exactly that transform: no mismatch. New fixtures:
  `config/valid/anthropic-types.json`, `config/invalid/anthropic-without-api-key-env.json`,
  `config/invalid/azure-anthropic-without-api-key-env.json`,
  `config/invalid/model-defaults.json` (a model with `defaults`: unknown field), with
  their `cases.json` entries.
- `gateway/internal/fakebackend/captures/`: recorded answers for steps 4–5 (README
  says what and from where).
- `docs/BACKLOG.md`: the two inbound entries replaced by the two translation entries;
  new entries for `anthropic-beta` forwarding, `azure-anthropic` health checks and
  pricing Anthropic price options.
- `OVERVIEW.md`: decision 12's table and decision 20 updated (below).

**Research findings** (2026-10-06)

- **Endpoint support**, called on the servers or from their references:
  - vLLM 0.30.0 (`dgx.local:11434`, its `/openapi.json` route list): `/v1/messages`,
    `/v1/messages/count_tokens`, `/v1/responses` (+ `GET /v1/responses/{id}`,
    `/cancel`) — **no `/v1/responses/input_tokens`**: a `POST` there answers
    `405 {"detail": "Method Not Allowed"}` (it lands on the `{id}` route).
  - llama-server build b9917 (`llama-embed.local:11435`, each route called): all four
    — `messages`, `messages/count_tokens` (`{"input_tokens": n}`), `responses`,
    `responses/input_tokens` (`{"input_tokens": n, "object":
    "response.input_tokens"}`).
  - OpenAI: `POST /v1/responses/input_tokens` exists (API reference). Azure OpenAI's
    v1 API answers `404` for it (Microsoft Q&A, the .NET SDK's
    `GetInputTokenCountAsync`): **`azure-openai` does not claim it**.
  - Anthropic: `/v1/messages/count_tokens`; Foundry supports token counting but has
    no Models API, no Batches, no `inference_geo`, no fast mode (Anthropic's platform
    availability table, the Foundry page).
- **Shapes recorded** (the captures): vLLM's Messages stream with thinking
  (`thinking_delta`, `signature_delta`) and `tool_use` (`input_json_delta`), its
  Responses stream with reasoning items and function calls, both cut by the output
  limit; llama-server's four endpoints (an embedding model: shapes only).
  - Messages usage: vLLM reports `input_tokens`/`output_tokens` only (no cache
    fields) and repeats `input_tokens` in `message_delta`; llama-server reports
    `cache_read_input_tokens` beside an `input_tokens` that **excludes** it (9 + 3 =
    12 prompt tokens) and only `output_tokens` in `message_delta`.
  - Responses usage: both put cached tokens **inside** `input_tokens`
    (`input_tokens_details.cached_tokens`), and vLLM reports
    `output_tokens_details.reasoning_tokens`.
  - Completeness: neither format sends `[DONE]`; Messages ends with `message_stop`;
    vLLM ends a Responses stream cut by `max_output_tokens` with
    `response.completed` whose `status` is `incomplete` (OpenAI sends
    `response.incomplete`).
  - Errors: vLLM's missing model on Messages is Anthropic-shaped (`{"type": "error",
    "error": {"type": "NotFoundError", "message": "The model `nope` does not
    exist."}}`), on Responses OpenAI-shaped; a Messages request failing vLLM's
    validation is answered in **OpenAI's** shape. llama-server's wrong path is its
    usual `File Not Found`; an invalid JSON body there is a `500`.
  - Model name: the top-level `model` in bodies; `message_start.message.model` and
    `response.model` in `response.*` events (as planned).
  - Self-hosted servers accept the Anthropic price options and `store: false`
    untouched (vLLM and llama-server both answered normally with `service_tier`,
    `speed`, `inference_geo` and a `ttl: "1h"` cache marker).
  - vLLM's Responses objects carry **no `store` member**: step 6's planned check "the
    answer's `store` is `false`" cannot hold on vLLM — check the forced field at the
    fake backend (step 5) and live only where the answer carries it (OpenAI, Azure).
- **Anthropic price options** (Anthropic's service-tier and data-residency pages, the
  claude-api reference): `service_tier` takes `auto` (default; uses Priority Tier
  capacity where committed) or `standard_only`; Priority Tier is no longer sold and
  is excluded on the newest models. `inference_geo` takes `global` (default) or `us`
  (1.1× on Claude 4.6 and later; a workspace may default to `us`). Fast mode is
  `speed: "fast"` (about 2×, Claude API only). Cache writes cost 1.25× input for 5
  minutes and 2× for 1 hour (`cache_control.ttl: "1h"`); `usage.cache_creation`
  splits writes by lifetime.
- **Anthropic API facts** used in the spec: error shape `{"type": "error", "error":
  {"type", "message"}}` with types by status (529 `overloaded_error`); the Models
  API is paged (`limit` up to 1000) and carries `max_input_tokens`; the missing-model
  and wrong-path `404` texts are from the documentation, **not verified live**
  (`anthropic`, `azure-anthropic`) — step 6's live kit confirms them.
- **OpenTelemetry**: `gen_ai.provider.name` well-known values include `anthropic`,
  `azure.ai.openai`, `azure.ai.inference`; Claude in Foundry gets `anthropic` (no
  Azure value names it).
- **OpenAI Responses usage**: `input_tokens_details.cache_write_tokens` exists beside
  `cached_tokens` (OpenAI SDK types) and is read.

**Changes to planning decisions (please review)**

1. Decision 12: `vllm` does not claim `responses_input_tokens` (not in 0.30.0);
   `azure-openai` does not claim it either (Azure's v1 API lacks it).
2. Decision 20: only `anthropic` sends `service_tier: "standard_only"`;
   `azure-anthropic` adds none and passes the client's untouched — Foundry has no
   Priority Tier, and an unknown value there is a risk with no benefit.
3. Decision 19, made precise: refused are `speed` other than `"standard"`,
   `inference_geo` other than `"global"`, and any `cache_control.ttl: "1h"`.
4. Decision 14, made precise: an endpoint missing is **neutral** for the circuit
   (outcome `endpoint_missing`), retried on other backends' deployments; vLLM's `405`
   on these endpoints reads the same way.
5. Decision 25, made precise: an error event as the **first** event is a backend
   failure the attempt loop may retry (like a `5xx`); after the first event it ends
   the stream as incomplete.
6. Code names chosen: `stateful_responses_unsupported`, `hosted_tool_unsupported`,
   `price_option_unsupported` (all `400`), beside `endpoint_not_served` and
   `upstream_endpoint_missing`.
7. Hosted-tool refusals also apply on the token-counting endpoints (they own their
   format's tool fields), so one rule covers a format.
8. The Anthropic-shaped model list lists the key's models whose `endpoints` include
   `messages`; `created_at` is the fixed epoch.
9. `responses/input_tokens` gets no service-tier edit (nothing is billed there).

**DGX**: nothing was started, stopped or swapped. The captures come from what was
already running: vLLM 0.30.0 serving `unsloth/Qwen3.8-27B-NVFP4` on
`dgx.local:11434`, and llama-server b9917 serving the embedding model
`/models/qwen3-embedding-0.6b-q8_0.gguf` on `llama-embed.local:11435`. A swap to a
llama-server chat model (for llama-server captures with thinking and tool calls)
was allowed by the user but blocked by the session's auto-mode classifier on the
`ssh dgx.local` call; the llama-server captures are shapes only (no tool call, no
thinking). Steps 4–5 can use the vLLM captures for those, or a later capture once
the user runs the swap.

**Suite** (`scripts/check-all.sh`, which stops at the first failing half; the control
tests, lint and the cross-half e2e were then run on their own):

- gateway (`check-gateway.sh`): gofmt, vet, staticcheck pass; `go test -race` fails
  in `cmd/kaiak` (its test configs are format 4), `internal/config` (fixtures,
  format 5, the new types' `api_key_env` rule), `internal/control` (protocol 5;
  inline configs format 4) and `internal/server` (`TestEveryErrorCodeHasItsClass`:
  the five new codes in the spec's table have no class yet). `e2e`, `accounting`,
  `auth`, `limits`, `metrics`, `provider`, `routing`, `sse`, `state` and the rest
  pass. **Cleared by step 3** — including all five codes' classes, even those whose
  endpoints arrive in steps 4–5, so phase 1 ends green.
- control `npm test`: 574 tests, 568 pass, 6 fail — the backend type enum
  (`BACKEND_TYPES` vs the schema), `examples/config.json` and
  `examples/local-config.json` (format 4, `defaults`), two slow-reader stream tests
  and the keys-entry test (their inline configs are format 4). **Cleared by step 2.**
- control `npm run lint`: passes.
- cross-half e2e: fails — the sample's config is format 4 (`/format_version must be
  equal to constant`). **Cleared by step 3** (with step 2's examples and sample
  configs).

**Sources**

- vLLM 0.30.0 and llama-server b9917, called on 2026-10-06 (routes, captures above).
- [Claude in Microsoft Foundry](https://platform.claude.com/docs/en/build-with-claude/claude-in-microsoft-foundry)
- [Service tiers](https://platform.claude.com/docs/en/api/service-tiers)
- [Data residency](https://platform.claude.com/docs/en/build-with-claude/data-residency)
- [Token counting](https://platform.claude.com/docs/build-with-claude/token-counting)
- [OpenAI: Counting tokens](https://developers.openai.com/api/docs/guides/token-counting.md)
- [OpenAI: Responses input tokens](https://developers.openai.com/api/reference/cli/resources/responses/subresources/input_tokens/index.md)
- [Azure v1 API without `responses/input_tokens` (Microsoft Q&A)](https://learn.microsoft.com/en-us/answers/a/12772446)
- [OpenAI Responses usage: `InputTokensDetails`](https://rubydoc.info/gems/openai/OpenAI/Models/Responses/ResponseUsage/InputTokensDetails)
- [OpenTelemetry: Anthropic conventions](https://opentelemetry.io/docs/specs/semconv/gen-ai/anthropic),
  [Azure AI Inference conventions](https://opentelemetry.io/docs/specs/semconv/gen-ai/azure-ai-inference)
- [vLLM PR #22627 (Anthropic `/v1/messages`)](https://github.com/vllm-project/vllm/pull/22627)
- [llama.cpp: Anthropic Messages API](https://huggingface.co/blog/ggml-org/anthropic-messages-api-in-llamacpp)
- [llama.cpp: Responses API](https://www.simplified.guide/llama-cpp/server-call-responses-api)
- The claude-api skill's reference (error codes, prompt caching, platform
  availability), 2026-09-25 cache.
