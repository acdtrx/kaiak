# Step 8 — Messages and Responses content

**Status:** done

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

## Result

### What changed

- `gateway/internal/capture/`:
  - `messages.go` (new) — the Messages mapping: the request (parameters, `system`,
    messages and every block kind, tools), the answer as a JSON body, the answer as a
    stream (blocks by `index`, interleaving allowed).
  - `responses.go` (new) — the Responses mapping: the request (parameters,
    `instructions`, `input` as a string or items, tools), the answer — a response
    object — and the stream: the terminal event's response object, else the items as
    the events built them.
  - `content.go` — the two stubs gone.
  - `genai.go` — `readFunctionTool(fn, schema)`: a function definition from a
    function tool's members — name, description and the schema member, `parameters`
    or Messages' `input_schema`; `chat.go` uses it too (the three APIs read the same
    thing).
- `gateway/internal/fakebackend/` — `Reply.Reasoning` now reaches Messages (a
  `thinking` block before the text — a `thinking_delta` per piece, then a
  `signature_delta`; the text block at index 1) and Responses (a reasoning item in
  vLLM's shape — no summary, the reasoning as `reasoning_text` content, a
  `response.reasoning_text.delta` per piece — before the message, at
  `output_index` 1). Nothing changes when it is unset; a `StreamFault`'s `At` still
  counts the text events alone.
- `docs/specs/GATEWAY.md` (Request capture → Messages, Responses, Streams) — the
  cases the spec was silent on (below).
- No dependency added.

### Decisions where the spec was silent (the spec amended, 2026-10-13)

- **A Messages `tool_result`** carries its `content` as sent — a string, or its list
  of blocks as their JSON (images' base64 included), as a Chat Completions tool
  message's list does; its `is_error` is not carried (the conventions' tool result
  has no place for it). An absent `content` is `null`.
- **Messages streams:** `message_start` opens the message (an event before it is not
  read) and `message_stop` ends it; a tool call's input is its `input_json_delta`
  fragments joined — or, with no fragment text (a call without arguments may send one
  empty fragment), the `input` its start gave; a block opens once (a second start for
  its index is not read) and takes no delta after its `content_block_stop`; a block
  of another type is carried as `content_block_start` gave it, its deltas not read —
  equal to the unstreamed block for one that has none (`redacted_thinking`); a
  `citations_delta` is not read, as an unstreamed text block's citations are not
  carried. (A server tool's block would show its start's `input: {}`, but the
  inbound stage refuses hosted tools, so none reaches a client.)
- **Responses reasoning entries** of another type than `summary_text` /
  `reasoning_text` are generic parts.
- **Responses streams without a terminal event** — cut short, ended by an `error`
  event or by nothing — give what arrived, with no finish reason: the `id` and
  `model` of `response.created` / `response.in_progress`; the output items in the
  order `response.output_item.added` added them, each as `response.output_item.done`
  finished it, else as its deltas built it — a message's text and refusal, a
  reasoning item's summary then reasoning text, a function call's `call_id` and name
  as added plus its argument fragments (half-written: the string), any other item as
  added (generic). Deltas are matched by `item_id`: llama-server's events carry no
  `output_index`, and its items interleave (the function call is added before the
  reasoning item is done). The terminal event wins where they differ: vLLM's
  terminal response names its function call by another `call_id` than its item
  events did (recorded; the test names it).
- Within the general rule (content without its API's shape is left out), not
  spelled out in the spec: an answer whose `content` / `output` is not a list has no
  output message and no finish reason; a terminal event whose `response` is not an
  object is not read as one; an output item without a string `id` cannot be followed
  through the stream and is left out of the item assembly; a Responses `input_file`
  whose `file_data` is not a base64 `data:` URL is left out (as on Chat Completions);
  `file_url` is media by URL (a base64 `data:` URL there is a blob, by the general
  rule). An in-progress reasoning item's summary deltas join into one reasoning part
  (`summary_index` is not tracked) where the finished item gives one per summary
  entry — only a stream cut inside a summary shows it.

### Tests

- `internal/capture/messages_test.go`:
  - `TestMessagesContent` — the corpus (9 cases), each given whole and streamed, both
    held to one exact expectation and to each other: plain text (every parameter,
    `<`/`&` unescaped, a `ping` event); system blocks (one with `cache_control`, an
    empty one, an unknown one) with images by base64 (uppercase MIME type, none) and
    by URL, documents by base64 and by URL, a text-source document (generic); a tool
    round trip (a custom tool without a type, one of type `custom`, `bash_20250124`;
    `tool_result` as a string, as blocks, absent with `is_error`; input fragments
    after an empty one; an argument-less call with an empty fragment); thinking
    (`signature_delta`; `redacted_thinking` in the history); unknown blocks
    (`search_result`, an `x_custom` with key order kept; `redacted_thinking` and a
    text block with citations in the answer, a `citations_delta` in the stream); and
    the **recorded** streams — vLLM's and llama-server's tool streams (llama-server's
    blocks interleave, its signature after the tool input) and output-limit streams
    — each against a hand-written unstreamed twin.
  - `TestMessagesRecordedAnswers` — both servers' recorded `messages.json`.
  - `TestMessagesCutShort` — cut inside a tool call's input (thinking, text and the
    half-written input as a string, no finish reason, the event cut inside not
    read); llama-server's recorded tool stream cut inside its third input fragment,
    its thinking block still open; cut right after `message_start` (an empty
    message); an `error` event; a JSON body cut short (no output).
  - `TestMessagesErrorAnswer` (a relayed 400, vLLM's recorded 404, the gateway's 429
    to a stream, answer- and stream-shaped bodies under 400/529),
    `TestMessagesSpecExample` (the spec's example through `Captures`: the span's name,
    every attribute shown, exactly, and the full key set), `TestMessagesMalformed`
    (23 cases), `FuzzMessagesContent`.
- `internal/capture/responses_test.go`:
  - `TestResponsesContent` — the corpus (7 cases), each given whole, streamed, and
    streamed **without its terminal event** (equal but for the finish reason — the
    item assembly held to the same output): plain text, a string input and
    instructions; message items — with and without `type`; system, developer and
    assistant roles — with images by `data:` URL, URL and file ID, files by data, URL
    and ID, an unknown part; a function call round trip (arguments that are not
    JSON, a list output, a custom tool's call and output); reasoning items (OpenAI's
    summary with encrypted content, vLLM's `reasoning_text`) and a two-part summary
    streamed; unknown items, content parts and tools; an incomplete response
    (`response.incomplete`, `max_output_tokens`); a failed one (`response.failed`).
  - `TestResponsesRecordedStreams` — vLLM's and llama-server's tool and output-limit
    streams: as streamed; their terminal response given as a body (the twin); and
    without the terminal event (vLLM's call ID from its item events, llama-server's
    without a model; llama-server's length-capped stream says `completed`).
  - `TestResponsesRecordedAnswers`, `TestResponsesCutShort` (cut mid tool call; every
    item kind in progress at once with interleaved deltas, ended by an `error` event;
    llama-server's recorded tool stream cut inside the arguments with its reasoning
    item not yet done; a JSON body cut short), `TestResponsesErrorAnswer`,
    `TestResponsesSpecExample`, `TestResponsesMalformed` (23 cases),
    `FuzzResponsesContent` (seeds include each stream without its terminal event).
- `internal/capture/content_test.go` — the per-API pieces now shared: `contentCase`,
  `apiContent`, `expectCorpus`, `typedEvents`, `expectWellFormed`, `with`,
  `expectSpecExample`; `chat_test.go` uses them.
- `internal/server/capture_test.go`:
  - `TestCaptureMessagesContent`, `TestCaptureResponsesContent` (new) — through the
    gateway to the fake backend (model `msg`, vLLM, with reasoning), whole, streamed,
    and streamed and cut after the first text chunk: the system prompt /
    instructions, the input with its inline image, the tools, the temperature; the
    client's `max_tokens` on Messages and none on Responses though the gateway
    injects one; the output with the thinking / reasoning item as reasoning, the
    public model name (`kaiak.deployment.model` `msg-back`), `end_turn` /
    `completed`, the cut stream's reasoning and `"A "` with no finish reason; no
    content in the log.
  - `TestCaptureSpanAttributes` — the exact key sets grow with a Messages and a
    Responses request, whole and streamed (provider `vllm`, finish reasons
    `end_turn` and `completed`, span name `chat msg`); `postCaptured` posts for it
    and the three content tests.
- Mutation checks (reverted): reading `signature_delta` as thinking text fails three
  Messages twins; ignoring `response.output_item.done` fails the without-terminal
  checks (the two-part summary) and two malformed cases; not stopping at the
  terminal event fails every Responses corpus case.
- Fuzzing, 45 s each, nothing found: `FuzzMessagesContent` 5.2M executions,
  `FuzzResponsesContent` 3.7M executions. No `testdata/` written.

### Suite

- `go test -race` on `internal/capture`, `internal/server`, `internal/accounting`:
  green.
- `scripts/check-gateway.sh`: green — gofmt, vet, staticcheck, the telemetry
  boundary, `go test -race -count=1 ./...` (`kaiak/e2e` included), the live-test
  kit's lint and self-test (35 passed, 0 failed, 3 skipped, as before).
- `scripts/check-all.sh`: green. Control `npm test`: 679 tests, 678 pass, 0 fail, 1
  skipped (as before); lint clean. Final lines:

  ```
  ==> cross-half e2e (sample control plane + two gateways; two cores over one store)
  ok  	kaiak/e2e	80.504s
  all checks passed
  ```

### For steps 9–10

- Step 9: `fakebackend.Reply.Reasoning` now gives Messages thinking and Responses
  reasoning items (vLLM's shape) as well as Chat Completions reasoning. The fake
  backend answers no tool calls on any API and no Responses summaries — an e2e with
  tool calls in the answer needs them added. The server tests already cover each
  endpoint's content through the gateway (whole, streamed, cut); the e2e adds the
  session header, the budget, the drain and the cross-half path.
- Step 9 and the live run: vLLM's Responses terminal event names a function call by
  another `call_id` than its item events — a comparison of a capture with what a
  client assembled must take the terminal event as the answer (the spec says so).
- Step 10: the spec's Request capture section now covers the three APIs' edge cases;
  nothing in it is left for the docs step beyond the guide's examples.
