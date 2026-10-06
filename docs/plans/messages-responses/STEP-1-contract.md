# Step 1 — contract

**Status:** not started

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

(filled in when the step is done)
