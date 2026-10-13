# Step 7 — Chat Completions content

**Status:** done

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

## Result

### What changed

- `gateway/internal/capture/`:
  - `genai.go` (new) — the conventions' content format every API maps into (the
    shared pieces, below), and `contentAttributes`, which writes a request's and an
    answer's content as the span's attributes.
  - `read.go` (new) — reading what clients and backends sent: by exact member name
    (maps of `json.RawMessage`, never struct fields, which encoding/json matches in any
    case), each value only when it has the type read; the typed parameter readers;
    the event-stream reader over `internal/sse`.
  - `chat.go` (new) — the Chat Completions mapping: the request (parameters,
    messages, tools), the answer as a JSON body, the answer as a stream.
  - `content.go` — `content` dispatches to an API's `mapping` (`request`, `answer`,
    `stream`); an error answer (status 400 or above) reads no answer at all. The
    Messages and Responses mappings are empty stubs for step 8.
- `gateway/internal/fakebackend/fakebackend.go` — `Reply.Reasoning`: reasoning pieces
  of a chat completion in vLLM's `reasoning` member, one stream event each before
  the text's (a `StreamFault`'s `At` counts the text events alone), joined in the
  non-stream message.
- `docs/specs/GATEWAY.md` (Request capture) — the cases the spec was silent on
  (below).
- No dependency added.

### The shared pieces (for step 8)

All in `internal/capture`, unexported:

- **The format** (`genai.go`): `message{Role, Parts, Name}` made by
  `newMessage(role, name, parts)` (nil parts written `[]`); the `part` interface
  with `newTextPart`, `newReasoningPart`, `newToolCallPart(id, name, arguments any)`,
  `newToolCallResponsePart(id, response json.RawMessage)` (nil → `null`),
  `newBlobPart(modality, mime, base64)`, `newURIPart(modality, mime, uri)`,
  `newFilePart(modality, mime, fileID)`, and `original(raw)` — a generic part (or a
  non-function tool), the JSON object as sent; `appendText` / `appendReasoning`
  (an empty text is no part); the `toolDefinition` interface with
  `newFunctionTool(name, description, parameters json.RawMessage)` and `original`.
  Member order follows the spec's examples.
- **Media**: `mimeType(s)` (type/subtype, lowercase, no parameters; `""` for no
  MIME type), `modality(mime, kind)` (`image/*`, `audio/*`, `video/*`, else
  `document`; `kind` — `modalityImage`, `modalityAudio`, `modalityVideo`,
  `modalityDocument` — when there is no MIME type), `dataURL(url)` (a base64
  `data:` URL, scheme and marker in any case → MIME type and base64 as sent),
  `newMediaByURL(url, kind)` (blob for a base64 data URL, else uri).
- **Tool arguments**: `toolArguments(raw)` — a string holding valid JSON is that
  JSON, any other string the string, any other value itself, absent/null none;
  `stringArguments(s)` for arguments assembled from stream fragments. Strings are
  carried as strings (not `json.Marshal`ed), so they are not HTML-escaped either.
- **Reading** (`read.go`): `members(raw)`, `elements(raw)` (both skip leading JSON
  whitespace — bodies and event data may start with some), `stringOf`, `isAbsent`
  (absent or `null`), `integerOf` (an integer written as one, within int64 — as the
  inbound stage reads its own integers), `numberOf` (a finite float64), `stringsOf`
  (a string or a list of strings); `parameters` with `addInteger(key, raws...)` (the
  first that is an integer — for an API with two names), `addNumber`, `addStrings`.
- **Streams**: `streamEvents(stream) iter.Seq[[]byte]` — each event's data, in
  order, to the end or to where it was cut (an event the stream ends inside is not
  yielded). It yields data only: Messages' and Responses' events name their type in
  the data.
- **The attributes**: `requestContent{parameters, systemInstructions, messages,
  tools}` and `answerContent{id, model, messages, finishReasons}` →
  `contentAttributes` writes `gen_ai.response.id`, `.model`, `.finish_reasons`
  (when any is non-empty; one per output message, `""` for none),
  `gen_ai.system_instructions`, `gen_ai.input.messages`, `gen_ai.tool.definitions`,
  `gen_ai.output.messages` (each only when non-empty), the JSON built by
  `appendJSON` without HTML escaping. `gen_ai.system_instructions` is wired and
  unused by Chat Completions.
- **The dispatch** (`content.go`): `mapping{request, answer, stream}`; step 8
  replaces `messagesMapping` and `responsesMapping`.
- **Test helpers** (`content_test.go`): `events(data...)` (an event stream),
  `plainAttributes`, `compact`, `expectAttributes` (exact key set; JSON
  expectations compacted first), `jsonAttributes`. `chat_test.go`'s
  `FuzzChatCompletionsContent` is the pattern for a per-API fuzz target.

### Decisions made while implementing (the spec amended, 2026-10-13)

- **An empty text or reasoning is no part**, in requests and answers alike: OpenAI's
  stream opens with `"content": ""` where the unstreamed answer has `null`, so the
  twins would differ; a message left with no parts has `"parts": []`.
- **Reasoning under both names**: `reasoning_content` when it has text, else
  `reasoning` — a server may send the same text under both (the accounting meter
  counts it once for the same reason).
- **A `refusal` content part** (an assistant message's) is text, as the message's
  `refusal` member is.
- **Output role**: the role the choice names, `assistant` when it names none.
- **Finish reasons** are positional: one per output message, `""` for a choice that
  gave none; written when any choice gave one (a single cut stream: none written).
- **Tool calls of another type** (OpenAI's `custom`) are generic in a request and an
  unstreamed answer; a stream cannot rebuild them from fragments and leaves them out
  — the one named exception to the twin rule.
- **Stream tool calls**: the `id` and name from the first fragments that carry them.
- **Integers** are integers written as one (`2.0`, `1e3` are left out), as the
  inbound stage reads `n` and the output limits; numbers must be finite float64s
  (`1e400` is left out). `stop: []` is carried as an empty list.
- **Media**: only a base64 `data:` URL is a blob; any other URL — a non-base64
  `data:` URL included — is a uri. MIME types lowercase, without parameters; a value
  with no `type/subtype` is no MIME type (the API's kind gives the modality). A
  `file_id` file's modality is document. A `file_data` that is not a base64 `data:`
  URL is left out (visible as absent rather than a blob of unknown content).
- **A choice or a stream fragment without an integer `index`** is left out (the
  general rule: content without its API's shape).
- **A JSON body answering a stream request** (status below 400) gives no output: the
  answer is read as the request asked, never sniffed.
- **The cost** (measured, not in the spec): a 4 MiB request with a 3 MiB inline image
  reads and encodes in about 18 ms with about 42 MB of short-lived garbage — each
  nesting level's `json.RawMessage` copies its subtree (seven levels down to an
  image URL), and the encoder's buffer; a 2000-chunk stream about 4 ms. One build
  runs at a time. A non-copying raw type would cut the garbage to about 4× the body,
  but relies on encoding/json handing `UnmarshalJSON` subslices of the input,
  against its documented "must copy" — not done.

### Tests

- `internal/capture/chat_test.go`:
  - `TestChatCompletionsContent` — the corpus, every case given whole and streamed,
    both held to one exact expectation and to each other: plain text (OpenAI shapes,
    `system`, `developer`, `name`, every parameter, `<`/`&` unescaped, a usage
    chunk); images by `data:` URL (an uppercase MIME type, one with none) and by URL
    (vLLM); audio with and without a format and files by ID and by data (PDF, image);
    two parallel tool calls with argument fragments (vLLM); tool results — a string
    and a list — with the follow-up, a call whose arguments are not JSON
    (llama-server); reasoning from vLLM's `reasoning`, llama-server's
    `reasoning_content`, and both names at once; `n` = 2 answered out of order and
    streamed interleaved (one chunk carrying both); a refusal (and a `refusal`
    content part); unknown parts and a custom tool kept as sent (key order
    preserved), the stream with a `: ping` comment and CRLF line endings.
  - `TestChatCompletionsCutShort` — a stream cut mid tool call keeps the text and
    the half-written arguments as a string, no finish reason, the block cut inside
    not read; a JSON body cut short gives no output.
  - `TestChatCompletionsErrorAnswer` — a relayed 400, the gateway's 429 to a stream,
    and answer-shaped bodies under 400/502: the request's content alone.
  - `TestChatCompletionsSpecExample` — the spec's Chat Completions example, built
    through `Captures` (the stream appended in 7-byte pieces) into a span: its name
    and every attribute it shows, exactly, and the full key set.
  - `TestChatCompletionsMalformed` (23 cases) — empty, truncated, non-object
    requests; every wrong shape of messages, parts, tool calls, tools, parameters,
    choices and stream chunks; members differing in case not read; leading
    whitespace; `[DONE]` first; a JSON body for a stream; data after extra spaces.
  - `FuzzChatCompletionsContent` — seeds from the corpus: never panics, every
    content attribute a non-empty JSON list, finish reasons one per output message.
    Fuzzed 45 s locally (9.9M executions, nothing found).
- `internal/capture/genai_test.go` — `dataURL` (14 cases), MIME type and modality,
  media by URL, tool arguments, no HTML escaping and the empty parts list, the
  finish reasons rule, `streamEvents` (line endings, comments, multi-line data, cut
  inside an event, early stop).
- `internal/server/capture_test.go`:
  - `TestCaptureSpanAttributes` — the exact key sets gain the answer's ID, model,
    finish reasons and output messages (success, stream), and ID, model and output
    without a finish reason for the cut stream; refusals and upstream errors gain
    none.
  - `TestCaptureChatCompletionsContent` (new) — the spec's example through the
    gateway (model `msg` on a vLLM backend, the fake backend's reasoning): the input
    messages and tools as sent, the temperature, no `max_tokens` though the gateway
    injects one, the output with reasoning and text, the public model name in
    `gen_ai.response.model` (`kaiak.deployment.model` `msg-back`); the streamed
    answer equal to the unstreamed; a stream cut after the first text chunk keeps the
    reasoning and `"A "`, no finish reason; no content in the log.
- Mutation checks (reverted): counting both reasoning names fails the "both names"
  twin; keeping empty texts fails ten corpus cases; iterating stream choices
  unsorted is caught when Go's map order puts index 1 first (it is randomized).

### Suite

- `go test -race` on `internal/capture`, `internal/server`: green.
- `scripts/check-gateway.sh`: green — gofmt, vet, staticcheck, the telemetry
  boundary, `go test -race -count=1 ./...`, the live-test kit's lint and self-test.
- `scripts/check-all.sh`: green. Control `npm test`: 679 tests, 678 pass, 0 fail, 1
  skipped (as before); lint clean. Final lines:

  ```
  ==> cross-half e2e (sample control plane + two gateways; two cores over one store)
  ok  	kaiak/e2e	80.417s
  all checks passed
  ```

### For steps 8–10

- Step 8: fill `messagesMapping` and `responsesMapping` (each may move to
  `messages.go` / `responses.go`) from the shared pieces; Messages' base64 sources →
  `newBlobPart(modality(mimeType(media_type), kind), mimeType(media_type), data)`,
  `tool_use.input` → `toolArguments` (an object is itself), `tool_result.content` →
  `newToolCallResponsePart`; Responses' `input_image` → `newMediaByURL`,
  `function_call.arguments` → `toolArguments`. Real answers of both servers on both
  APIs, streams cut by the output limit and tool streams included, are in
  `internal/fakebackend/captures/` (`fakebackend.Captured`) — corpus material.
- Step 9: `fakebackend.Reply.Reasoning` gives Chat Completions reasoning; Messages
  thinking and Responses reasoning items are not in the fake backend yet.
- Step 10: the builder's short-lived garbage (about 10× a deeply nested inline
  image's request, one build at a time) may deserve a line beside the `GOMEMLIMIT`
  advice in `docs/DEPLOYMENT.md`.

