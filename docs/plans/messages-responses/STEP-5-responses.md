# Step 5 — Responses

**Status:** not started

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

(filled in when the step is done)
