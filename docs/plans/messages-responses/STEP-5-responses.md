# Step 5 — Responses

**Status:** done (2026-10-06)

## Intent

Serve `POST /v1/responses` and `POST /v1/responses/input_tokens`, stateless, through
the full pipeline. Requests pass through to `vllm`, `llama-server`, `openai` and
`azure-openai` backends, following step 1's table.

## Files likely touched

- `gateway/internal/server/`: routes; the Responses format's pieces.
  - Owned fields: `model`, `stream`, `max_output_tokens`.
  - `store` forced to `false` (a body edit, every backend).
  - Refusals: `previous_response_id`, `conversation`, `background: true`.
  - Hosted-tool allowlist: `function`, `custom`, `local_shell`, `shell`,
    `apply_patch`; a `tool_choice` naming another type is refused.
  - The output limit under `max_output_tokens`.
  - Errors in OpenAI's shape, as today.
  - The token-counting path, as in step 4.
- `gateway/internal/accounting/`: the Responses usage mapping (OVERVIEW decision 24,
  `cache_write_tokens` as step 1 found it) and the input estimate over Responses
  bodies:
  - `instructions`
  - `input` as a string or as items: messages, content parts, function calls and
    outputs, reasoning items
  - media
- `gateway/internal/provider/`:
  - the Responses wire: path `responses`, `responses/input_tokens`
  - completeness at `response.completed` / `response.incomplete`
  - `response.failed` and `error` events as backend failures
  - `response.model` rewriting in `response.*` events
  - `service_tier: "default"` on `openai` and `azure-openai`
  - Responses support in the four modules
- `gateway/internal/fakebackend/`: speaks Responses from step 1's recorded
  sequences, with the same fault modes as step 4.
- `gateway/e2e/`: Responses end to end.

## Decisions made during planning

- **A refused stateful field names the parameter** and says kaiak serves Responses
  stateless. A `null` value counts as absent and is not refused, as with every owned
  field.
- **`store: true` from the client** is overwritten to `false`, not refused. Clients
  default to `true` and work stateless, and refusing would break most of them.
- **`include` values** (for example `reasoning.encrypted_content`) pass untouched. The
  backend decides what it can include.

## Acceptance criteria

- **Component tests:**
  - owned fields
  - `store` forced on every backend type
  - each refusal
  - the tool allowlist and `tool_choice`
  - output-limit default and ceiling
  - estimate
  - usage mapping, streamed and not, cached and reasoning
  - completeness: `response.completed`, `response.incomplete`, `response.failed`,
    `error`, and a break
  - model rewriting at the top level and in events
  - service tier on `openai` / `azure-openai` only
  - input_tokens: no record
- **Gateway e2e against the fake backend, streamed and not:**
  - settles with the right units
  - a limit refusal in OpenAI's shape
  - `endpoint_not_served` for a Claude-only model
  - `upstream_endpoint_missing`
- `scripts/check-all.sh` green: **phase 3 ends here**, committed.

## Result

**What changed**

- **Provider** (`gateway/internal/provider/`):
  - `stream_end.go`: `responsesStreamEnd`. A Responses stream is complete at
    `response.completed` or `response.incomplete`; `error` and `response.failed` are
    the backend abandoning it (relayed, then incomplete; as the first event,
    `CodeErrorEvent`, as step 4 built). Events are read by their data's `type`, so
    overlapping output items change nothing. The nested model is `response.model`.
  - `wire.go`: `passthroughBody` sets `store: false` on every `/v1/responses`
    request (every module goes through it). `standardServiceTier` always sends
    `"default"` on Responses, as on chat, and leaves `responses/input_tokens` as the
    client sent it.
- **Accounting** (`gateway/internal/accounting/`):
  - `responses_usage.go`: the Responses usage reader — a body's `usage`, or
    `response.usage` in `response.completed` / `response.incomplete`; cached and
    written tokens come out of `input_tokens`, clamped as OpenAI's are; reasoning from
    `output_tokens_details`. Estimated output counts the content deltas (output text,
    refusal, reasoning text and summary, function-call arguments, custom tool input)
    or a body's output items (also tool-call names).
  - `estimate.go`: a Responses `content` list's `input_image` / `input_file` parts
    each count one media item, read whole whatever their member order.
- **Server** (`gateway/internal/server/`):
  - `/v1/responses` and `/v1/responses/input_tokens`: endpoints, metric names
    `responses`, `responses_input_tokens`; `gen_ai.operation.name` `chat` for
    Responses, none for counting; OpenAI's error shape; no `/v1/responses/{id}`
    routes (`404 unknown_url`).
  - `inbound_responses.go`: `model`; `stream` and `max_output_tokens` on
    `/v1/responses`; `previous_response_id`, `conversation` and `background: true`
    refused (`400 stateful_responses_unsupported`, null absent, `background` must be
    a boolean); the hosted-tool allowlist on `tools` and `tool_choice`, on both
    endpoints.
  - `inbound.go`: the tool-list check is shared by Messages and Responses
    (`refuseHostedToolTypes`, one purpose: refusing a tool the backend runs).
  - `params.go`: Responses' output-limit key is `max_output_tokens`.
  - **Messages allowlist correction** (step 4's review): a client tool's prefix must be
    followed directly by its version date (`computer_20250124`); `computer_toolset_…`
    and a bare `bash_` are refused.
- **Fake backend:** `responses.go` serves Responses and `responses/input_tokens` under
  every layout, streamed (created, in_progress, one message item, `output_text`
  deltas, `response.completed` / `response.incomplete`) and not, with Responses-shaped
  usage, the existing fault modes, and an `error` event for `ErrorEvent`;
  `HonorMaxTokens` reads `max_output_tokens`.
- **Spec** (`docs/specs/GATEWAY.md`): the dated Messages allowlist rule;
  `allowed_tools` in `tool_choice`; `responses/input_tokens` refuses the stateful
  fields; the Responses estimate's content list in full.
- **Tests:**
  - provider: every recorded Responses stream and body from vLLM and llama-server
    relays whole (only the model names change, byte count checked, every occurrence
    rewritten); each stream cut before `response.completed` is incomplete;
    `response.incomplete` ends whole; `error` and `response.failed` mid-stream and
    first; `store` sent by every module and absent on counting; service tier per
    module and in `standardServiceTier`.
  - accounting: usage from the six recorded streams, `response.incomplete` with cache
    read and write, clamping, body usage; estimated output from deltas and from body
    items; the media-part estimate (5 cases).
  - server: refusals in OpenAI's shape (stateful fields on both endpoints, not
    served, input_tokens on vllm, output limit, 405, stored-response paths), hosted
    tools in `tools` and `tool_choice` on both endpoints and the client tools passing,
    passthrough edits with default and ceiling, the Azure tier, streams (public name
    in each event, `response.incomplete` cut by the limit), error events and an ended
    stream, input_tokens with no record, limit refusals.
  - e2e (`gateway/e2e/responses_test.go`): vllm, llama-server, openai and
    azure-openai, streamed and not — path, credential, `store: false`, the tier only on
    the OpenAI types, public model name, usage on the log line; input_tokens on the
    types that have it and `endpoint_not_served` elsewhere; refusals in OpenAI's shape;
    an old vLLM's `404` failed over 8 times with the circuit closed.

**Decisions made in this step** (please review)

1. **`responses/input_tokens` refuses the stateful fields too.** The spec gave the
   counting endpoints only `model` and the tool fields; but OpenAI's counting endpoint
   takes `previous_response_id` and `conversation`, and since the gateway never lets a
   backend keep a conversation, one there could only reference another client's
   stored state (on a shared OpenAI project). Recorded in the spec.
2. **`tool_choice` of type `allowed_tools` passes,** its own `tools` list held to the
   allowlist: it narrows the declared tools and names none of its own; refusing it
   would break clients using only client tools. Recorded in the spec.
3. **A non-string, non-object `tool_choice` is `invalid_type`;** a string passes (the
   backend judges `none` / `auto` / `required`).
4. **Responses estimated output counts refusals, tool-call names and custom tool
   input** beside the spec's list, as chat counts refusals and names; the spec's list
   now names them.
5. **`store` is not type-checked:** it is always overwritten with `false`.

**Discrepancies:** none between spec and code beyond the gaps closed in 1–2.

**Suite** (2026-10-06):
- `scripts/check-all.sh` passed: gofmt, vet, staticcheck, `go test -race` with the
  e2e test (`ok kaiak/e2e 111.005s`), the live kit's lint and self-test, control
  `npm test` (pass 576, fail 0), `npm run lint`, the cross-half e2e (`ok kaiak/e2e
  43.900s`), ending "all checks passed".
- The new server, provider and accounting tests ran 5× and the Responses e2e 3× under
  `-race`, all green.
- **Phase 3 ends green.**
