# Step 7 — Chat Completions content

**Status:** not started

## Intent

Fill in the content of a Chat Completions capture: the request's messages and tools,
the request parameters, and the answer — a JSON body or an assembled stream — in the
conventions' message format (OVERVIEW decisions 14, 15, 19, 20).

## Files likely touched

- `gateway/internal/capture/chat.go` (or the API's own file name in the package):
  - **request:** `messages` → `gen_ai.input.messages` (every role kept, `system` and
    `developer` included; `name` kept); content strings → `text`; content parts:
    `text`, `image_url` (a `data:` URL → `blob` image with its MIME type; else `uri`),
    `input_audio` → `blob` audio (the format as MIME type), `file` (`file_id` →
    `file`; `file_data` → `blob`), anything else → a generic part with its JSON;
    assistant `tool_calls` → `tool_call` parts; `tool` messages →
    `tool_call_response` (id, content); `tools` → `gen_ai.tool.definitions`;
  - **parameters:** `max_completion_tokens` or `max_tokens` → `gen_ai.request.max_tokens`;
    `temperature`, `top_p`, `top_k` (when sent), `frequency_penalty`,
    `presence_penalty`, `stop` → `stop_sequences`, `seed`, `n` → `choice.count` (not
    1), `stream`;
  - **answer, JSON:** `id`, `model`, each choice → one output message (`content` →
    `text`, `reasoning_content` or `reasoning` → `reasoning`, `tool_calls` →
    `tool_call`, `refusal` → `text`), each `finish_reason` → `finish_reasons`;
  - **answer, stream:** the SSE bytes the client received, split with
    `internal/sse`; deltas gathered per choice index — content, reasoning, tool
    calls per index (id, name, argument fragments concatenated), finish reasons;
    `[DONE]` ends it; a stream cut short yields what arrived;
  - **error answers:** no output messages (a `4xx` relayed or the gateway's own
    error body).
- Tests with a corpus of requests and answers, and each streamed answer assembled
  equal to the same answer unstreamed (the fake backend can produce both).

## Decisions made during planning

- **Tool-call arguments** are parsed when they are valid JSON, else kept as the string
  — a cut stream's half-written arguments stay a string.
- **The content attributes' JSON strings are built without HTML escaping**
  (`json.Encoder` with `SetEscapeHTML(false)`, as `otlptrace.Encode` does): with
  `json.Marshal` every `<`, `>` and `&` in a prompt becomes `\u003c`, escaped again
  inside the span — larger, and not what the client sent (found in step 4).
- **`refusal`** is model text: a `text` part.
- **Unknown members of a message** (beyond role, content, name, tool calls, tool call
  ID) are not carried: the conventions' message has no place for them. Unknown
  content parts are, as generic parts.

## Acceptance criteria

- The corpus covers: plain text; multi-part user content with an image by `data:` URL
  and by URL; audio input; a tool round trip (call, result, follow-up); reasoning
  from vLLM's and llama-server's fields; `n` > 1; a refusal; an unknown part type;
  a cut stream mid tool call.
- Every streamed case assembles to the same messages as its unstreamed twin.
- `scripts/check-gateway.sh` green.
