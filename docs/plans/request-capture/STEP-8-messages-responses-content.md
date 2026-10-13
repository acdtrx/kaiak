# Step 8 — Messages and Responses content

**Status:** not started

## Intent

Fill in the content of Messages and Responses captures, as step 7 did for Chat
Completions (OVERVIEW decisions 14, 15, 19, 20).

## Files likely touched

- `gateway/internal/capture/messages.go`:
  - **request:** `system` (a string or text blocks) → `gen_ai.system_instructions`
    (text parts); `messages` → the input: `text` → `text`; `image` (`base64` source →
    `blob`; `url` → `uri`); `document` (`base64` → `blob` modality `document`; `url` →
    `uri`); `tool_use` → `tool_call`; `tool_result` → `tool_call_response` (its
    content as sent); `thinking` → `reasoning` (the signature left out);
    anything else (`redacted_thinking`, server tool blocks, …) → a generic part;
    `tools` → tool definitions (`input_schema` as `parameters`);
  - **parameters:** `max_tokens`, `temperature`, `top_p`, `top_k`, `stop_sequences`,
    `stream`;
  - **answer, JSON:** `id`, `model`, `content` blocks → one output message,
    `stop_reason` → `finish_reasons`;
  - **answer, stream:** `message_start` (id, model), `content_block_start` /
    `content_block_delta` (`text_delta`, `input_json_delta` fragments,
    `thinking_delta`; `signature_delta` ignored) / `content_block_stop` by index,
    `message_delta` (`stop_reason`); a cut stream yields what arrived.
- `gateway/internal/capture/responses.go`:
  - **request:** `instructions` → system instructions; `input` as a string → one user
    text message; as items: messages (`input_text`, `output_text` → `text`;
    `input_image` → `blob` or `uri` or `file`; `input_file` → `blob` or `file`),
    `function_call` → a `tool_call` in an assistant message, `function_call_output` →
    a `tool_call_response` in a `tool` message, `reasoning` items → `reasoning`
    parts (their summaries), anything else → a generic part; `tools` (function
    tools) → tool definitions;
  - **parameters:** `max_output_tokens`, `temperature`, `top_p`, `stream`;
  - **answer, JSON:** `id`, `model`; `output` items → one output message (messages'
    text, `function_call` → `tool_call`, `reasoning` → `reasoning`, others →
    generic); `status` and `incomplete_details.reason` → `finish_reasons`;
  - **answer, stream:** the terminal event's response object (`response.completed`,
    `response.incomplete`, `response.failed`) is the answer; a stream cut before it
    yields the items finished so far (`response.output_item.done`) plus the text of
    the one in progress (`response.output_text.delta`).
- Tests: a corpus per API, streamed cases assembled equal to their unstreamed twins.

## Decisions made during planning

- **One output message per answer** on both APIs: they return one generation.
- **Responses' function calls in the input** become assistant and tool messages, so a
  tool round trip reads the same in the tool as on Chat Completions.

## Acceptance criteria

- The corpora cover, per API: plain text; a system prompt; an image by base64 and by
  URL; a document (Messages); a tool round trip; thinking or reasoning; an unknown
  block or item; a cut stream mid tool call.
- Every streamed case assembles to the same messages as its unstreamed twin.
- `scripts/check-gateway.sh` green.
