# Step 4 — Messages

**Status:** not started

## Intent

Serve `POST /v1/messages`, `POST /v1/messages/count_tokens` and Anthropic-shaped
`GET /v1/models` (`/{id}`) through the full pipeline. Requests pass through to
`vllm`, `llama-server`, `anthropic` and `azure-anthropic` backends, following
step 1's table.

## Files likely touched

- `gateway/internal/server/`: routes for the new endpoints; the Messages format's
  pieces.
  - Owned fields: `model`, `stream`, `max_tokens`, plus the tool and option checks.
  - Hosted-tool allowlist: tools with no `type`, `custom`, `bash_*`,
    `text_editor_*`, `computer_*`, `memory_*`; `mcp_servers` and `container` are
    refused.
  - The output limit under `max_tokens`. The field is required by the API, so the
    default fills it when absent.
  - The Anthropic error shape for every gateway answer on these routes, limit
    refusals and `Retry-After` included.
  - The Anthropic-shaped models list.
  - The token-counting path: rpm limits only, no reservation, no usage record.
- `gateway/internal/accounting/`: the Messages usage mapping (OVERVIEW decision 24)
  and the input estimate over Messages bodies:
  - `system` as a string or as blocks
  - content blocks
  - images and documents as media
  - tool definitions and results
- `gateway/internal/provider/`:
  - the Messages wire: path `messages`, `messages/count_tokens`
  - completeness at `message_stop`
  - `event: error` mid-stream as a backend failure
  - `message_start.message.model` rewriting
  - Messages support in the `vllm` and `llama-server` modules
- `gateway/internal/fakebackend/`: speaks Messages from step 1's recorded sequences,
  plus fault modes: break mid-stream, error event, missing model, missing path.
- `gateway/e2e/`: Messages end to end.

## Decisions made during planning

- **No `include_usage` equivalent:** Messages always reports usage, so nothing is
  hidden from the client.
- **The `cache_control.ttl` check** (Anthropic types only, step 3's module refusal)
  reads every `cache_control` in `system`, `messages[].content[]` and `tools[]`. The
  input estimate's pass already walks those, so the check rides along rather than
  adding a second scan if the code allows it cleanly.
- **`thinking.budget_tokens` above a lowered `max_tokens`** is left to the backend to
  refuse. The gateway does not edit `thinking`.

## Acceptance criteria

- **Component tests:**
  - owned fields and each refusal (hosted tools by type, `mcp_servers`, `container`)
  - output-limit default and ceiling
  - estimate
  - usage mapping, streamed and not, with cache read and write
  - completeness: `message_stop`, break before it, error event
  - model rewriting at the top level and in `message_start`
  - every gateway error on these routes in Anthropic's shape, with kaiak's `code`
  - the Anthropic `/v1/models` shapes and filtering
  - count_tokens: no record, rpm counted
- **Gateway e2e against the fake backend, each streamed and not:**
  - a request settles with the right units
  - a token limit refuses in Anthropic's shape
  - a model with only OpenAI-only backends answers `endpoint_not_served`
  - an old-server `404` answers `upstream_endpoint_missing` and leaves the circuit
    closed
- `scripts/check-all.sh` green: **phase 2 ends here**, committed.

## Result

(filled in when the step is done)
