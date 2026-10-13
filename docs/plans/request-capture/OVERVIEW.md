# Plan: request capture

## Goal

Let the people who build on kaiak see what their apps did with the models: for every
chat request of a group that opted in, the gateway records what the client sent and
what it got back, and exports it as an OpenTelemetry span with the GenAI content
attributes, to a tool such as Langfuse. An app gets this without adding an
observability SDK of its own; captures also serve as material to try a new model
against an existing one on a task. Each group decides whether it is captured — set
by the app that manages the config.

The facts this plan rests on, read from source, are in `RESEARCH.md` beside this file.

## Scope

- **Protocol 8:** a required `capture` boolean on every group.
- **A capture stage** in the request pipeline for `/v1/chat/completions`,
  `/v1/messages` and `/v1/responses`: the client's request and the answer it
  received, streams assembled, in the conventions' one message format.
- **Sessions:** the optional `Kaiak-Session-Id` request header, recorded as
  `gen_ai.conversation.id`.
- **Trace export:** OTLP/HTTP JSON spans on the existing telemetry connection, with
  their own memory budget, best effort.
- **Both halves:** the specs, schemas, fixtures, the gateway, `kaiak-control`, the
  sample, the live-test kit and the docs.
- **A live run** against real backends with a self-hosted Langfuse.

## Out of scope

- **Distributed tracing:** no `traceparent` read or sent, no link to client or
  backend traces; each capture is a trace of its own. The backlog entry stays.
- **Destinations per group**, vendor attributes (`langfuse.*`), and any translation
  for a tool: the gateway speaks the conventions; mapping groups and keys to access in
  the tool is the app's job.
- **Other endpoints** (`/v1/completions`, embeddings, rerank, images, audio) —
  backlog.
- **An end-user ID** (`user.id`) — backlog.
- **Truncating large captures** — a capture over the export limit is dropped; backlog.
- **gzip on OTLP export** — backlog.
- **Requests refused before their body is parsed** (auth, size, malformed JSON): no
  content to capture; they stay in the logs and metrics.

## Decisions

Settled with the user (2026-10-12/13):

1. **The purpose:** users see what their apps did with the models, without an SDK in
   each app; secondly, captures serve to validate a new model against an existing one
   for a task. Users decide what is captured.
2. **Chat endpoints first:** `/v1/chat/completions`, `/v1/messages`, `/v1/responses`.
3. **OpenTelemetry, the GenAI conventions only.** Langfuse is the example consumer,
   not the target: if a tool does not read the conventions, translating happens
   outside the gateway.
4. **One destination per gateway.** Access in the tool by group or key is the app's
   job.
5. **Best effort:** a dropped capture is acceptable, and counted.
6. **`capture` is required on every group in the protocol** — a privacy setting the
   app states for each group, so that any logic for passing it down the tree lives in
   the app alone. Not inherited, not in `child_defaults`.
7. **The key's own group decides:** only the flag of the group the key names counts;
   its ancestors' flags do not.
8. **One message format across the three APIs** — the conventions' message structure,
   which Langfuse displays.
9. **Sessions:** the optional `Kaiak-Session-Id` request header (no standard header
   exists across gateways), recorded as `gen_ai.conversation.id`; never forwarded;
   body fields (`prompt_cache_key`, `metadata.user_id`) are not read for it.
10. **Not distributed tracing** (out of scope above).
11. **The content rule is amended:** prompt and response content leaves the gateway
    only as a capture — only for groups whose `capture` is on, only to the trace
    destination; never in logs, metrics or usage records.

Made while planning, all confirmed by the user (2026-10-13; 14, 18, 20 and 21
discussed and changed first):

12. **No trace SDK.** Each capture is one span built at the request's end from what
    the gateway holds: no propagation, no sampling (the group flag decides), no span
    tree. A span is a small struct with random IDs; the encoder and the exporter are
    ours anyway, as for logs and metrics. The SDK would add about 1.85 MB and 11
    modules for a span type re-encoded by our exporter, and report through a global
    logger (`RESEARCH.md`). Rejected: the SDK core (allowed by the dependency rule,
    2026-10-08) — nothing here uses what it brings.
13. **One span per client request**, kind `CLIENT`, named `chat {model}`, from the
    request's arrival to the end of its response; a trace of its own (random trace
    and span IDs, no parent). Attempts are not spans; `kaiak.attempts` counts them.
14. **Content as JSON strings** (settled 2026-10-13) on `gen_ai.input.messages`, `gen_ai.output.messages`,
    `gen_ai.system_instructions` and `gen_ai.tool.definitions`. The conventions allow
    a JSON string on spans and prefer structured values; Langfuse turns a structured
    (`kvlistValue`) attribute into its raw OTLP wrapper. A deviation named in the
    spec.
15. **The conventions' version:** `semantic-conventions-genai` `main` at `5293eff`
    (2026-10-12), named in the spec: `gen_ai.usage.cache_write.input_tokens`;
    `gen_ai.response.finish_reasons` on the span, no `finish_reason` on output
    messages.
16. **What is captured:** the request as the client sent it (`rq.body`, before the
    gateway's edits) and the answer as the client received it (the bytes written to
    it), in the client's API.
17. **Which requests:** those whose key's group has `capture: true`, on the three
    endpoints, once the body has parsed — refusals after that (model access, endpoint
    list, limits, queue) and failures included, with `error.type` and no output; a
    stream cut short keeps its partial output.
18. **Capture never gets in the way of serving** (settled 2026-10-13, the user's
    rule: captures, logs and metrics must not get in the way of the main
    functionality): it never refuses, delays or alters a request. Its own failures —
    the budget, the size, a malformed session ID — drop or reduce the capture, never
    the request.
19. **The mapping into messages:**
    - Chat Completions: every message, `system` and `developer` included, in the input
      messages with its role (they are chat history); `tools` → the tool definitions;
      each choice → one output message.
    - Messages: `system` → the system instructions; `messages` → the input; the
      answer's content blocks → one output message.
    - Responses: `instructions` → the system instructions; `input` (a string: one user
      message) → the input, function calls and their outputs as tool parts; the
      response's output items → one output message.
    - Parts: text → `text`; reasoning (Chat's `reasoning_content`/`reasoning`,
      Messages' `thinking`, Responses' reasoning summaries) → `reasoning`; tool calls
      → `tool_call` (arguments parsed when they are valid JSON, else the string);
      tool results → `tool_call_response`; inline media (`data:` URLs, base64 sources)
      → `blob` with the modality and MIME type; media by URL → `uri`; by file ID →
      `file`; **anything else → a generic part keeping its original JSON**, as
      passthrough preserves what it does not understand.
20. **Attributes:** the conventions' — `gen_ai.operation.name` (`chat`),
    `gen_ai.provider.name` (settled 2026-10-13: the cloud types' well-known values —
    `openai`, `azure.ai.openai`, `anthropic` — and every other type its kaiak type
    name as a custom value — `vllm`, `llama-server`, `vllm-omni`,
    `openai-compatible` — as the pinned text allows: "If one of them applies, then
    the respective value MUST be used; otherwise, a custom value MAY be used"; **the
    request line and the usage metrics change to match**, amending the 2026-10-06
    rule that left it out on self-hosted types — one vocabulary; a custom value names
    the provider family, not the instance, which `kaiak.backend.id` names; `kaiak` is
    rejected: it relays, it does not provide), `gen_ai.request.model`, the request
    parameters the client set (`max_tokens` from whichever field its API uses,
    `temperature`, `top_p`, `top_k`, `frequency_penalty`, `presence_penalty`,
    `stop_sequences`, `seed`, `choice.count` when not 1, `stream` when streaming),
    `gen_ai.response.id`, `gen_ai.response.model`, `gen_ai.response.finish_reasons`,
    the usage tokens as on the request line, `gen_ai.conversation.id`,
    `server.address` and `server.port` (the answering backend), `error.type` — and
    kaiak's, named as on the request line and the metrics: `kaiak.request.id`, `kaiak.key.id`,
    `kaiak.key.group`, `kaiak.key.root_group`, `kaiak.backend.id`,
    `kaiak.backend.type`, `kaiak.deployment.model`, `kaiak.attempts`,
    `kaiak.time_to_first_token`, `kaiak.relay_end`, `kaiak.usage.cost_usd`,
    `kaiak.usage.estimated`, `kaiak.usage.partial`. Span
    status `ERROR` with `error.type` as its message when the request ended in an
    error. No per-modality usage keys (Langfuse sums them into its total).
21. **The session header is optional, and a malformed one is ignored** (settled
    2026-10-13, with the user): a valid value is 1–256 characters of visible ASCII
    (`!`–`~`); any other value is treated as no session — the capture has no
    `gen_ai.conversation.id`, and both the request line and the span carry
    `kaiak.session_id.ignored` with the reason (`too_long`, `invalid_characters`,
    `empty`), so the user can see in the tool why a capture has no session. Read
    only when the request is captured. Rejected: refusing the request (`400`) — most
    tools send no session, and a header for observability must not break a request
    (decision 18). The value itself is never logged: it is client text.
22. **Memory:** `KAIAK_CAPTURE_MEMORY_BYTES`, default 128 MiB, bounds every byte a
    capture holds — the copy of the request, the answer as it is received, the
    encoded span until it is exported. A capture that would pass it is dropped
    (`memory`); the request goes on. Exported or dropped, its bytes are given back.
23. **The export limit:** an export request is at most 16 MiB (under the Collector's
    20 MiB default); a span larger than that is dropped (`too_large`).
24. **Work on the request's path stays a copy:** the stage copies the body; the relay
    appends the bytes it writes; the parsing, assembling and encoding run in a
    background builder, after the client's response has completed. *Amended
    2026-10-13 at step 6:* finishers run inside the handler, before net/http ends the
    response (the last chunk, HTTP/2's END_STREAM), so building in the finisher would
    delay every captured response's end by the build — against decision 18. The
    finisher only hands the copied bytes (still in the budget) and the summary to the
    builder; the drain waits for the builder before the trace export's final flush.
25. **Trace export needs an explicit switch:** `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`,
    or `OTEL_TRACES_EXPORTER=otlp` (with the general endpoint, `/v1/traces` appended).
    The general `OTEL_EXPORTER_OTLP_ENDPOINT` alone, which turns logs and metrics on,
    does not turn traces on: an operator who sends logs to a collector does not send
    prompts there without saying so. A deviation from the specification's default
    (`otlp`), named in the spec. The other trace variables as for logs: headers,
    timeout, protocol (`http/json` only).
26. **Groups capture, export off:** a config that turns capture on for a group while
    this gateway has no trace export is applied, with a warning on the config-applied
    line naming how many groups capture. Nothing else — captures are not built.
27. **The request line** gains `kaiak.capture` when the key's group captures:
    `queued` (handed to the builder), or why not — `memory` (the copy or the answer
    did not fit while the request ran), `export_off` — and `kaiak.session_id.ignored`
    when a malformed session ID was dropped (decision 21). *Amended 2026-10-13 at
    step 6 (decision 24):* the line is written before the build, so drops in the
    build — `too_large`, and `memory` when the encoded span does not fit — are in the
    counts (decision 28) only. *Amended 2026-10-14 at step 11:* a request body over
    the export limit drops the capture at the copy — nothing copied, held or built —
    so the line says `too_large` for it; a span built over the limit is still in the
    counts alone.
28. **Counts:** `otel.sdk.processor.span.processed` and `otel.sdk.exporter.span.exported`
    as the log exporter's twins (component types `capture_span_processor`,
    `otlp_http_json_span_exporter`); drops before the exporter (`memory`,
    `too_large`) are `otel.sdk.processor.span.processed` with `error.type`.
29. **The exporter is generic:** `internal/telemetry/otlptrace` encodes a span model
    (no kaiak imports) and queues encoded spans with a release callback; the budget
    bounds its queue, so it drops nothing for a full queue. The capture code is
    `internal/capture`: the decision, the budget, the conversion of each API, the
    stream assembly.
30. **No change to usage records:** a capture carries `kaiak.request.id`, which the
    usage record and the request line carry too.

## Constraints

- **One pipeline, no side doors:** capture is a stage, registered like the others.
- **Passthrough** is untouched: the request sent to the backend is the same with
  capture on or off.
- **Nothing sensitive in logs, metrics or usage records:** content, the session ID and
  tool names never reach them (decision 11).
- **The gateway writes nothing to disk:** captures live in memory until exported.
- **The telemetry tree imports nothing of kaiak's.**
- **No new dependency** (decision 12).
- **Protocol changes land on both halves at once.**

## Risks

- **Content leaving the gateway** — the reason this feature exists, and its danger.
  - Mitigation: the required per-group flag set by the app; the explicit export switch
    (decision 25); content never in logs; the guide and the deployment notes say who
    decides what.
- **Memory:** a copy of every captured request and answer.
  - Mitigation: the capture budget (decision 22), drops never refusals; the
    deployment notes size `GOMEMLIMIT` above every budget.
- **Latency:** capture work on the request path.
  - Mitigation: copies only on the path; conversion after the last byte (decision 24).
- **The conventions move** (`Development`, unreleased repository).
  - Mitigation: one pinned commit in the spec; Langfuse checked at a pinned commit;
    the live run shows what Langfuse displays.
- **Stream assembly diverging from what the client saw** (deltas, tool-call
  fragments, reasoning).
  - Mitigation: the assembler reads the bytes the client received; tests compare each
    assembled stream with the same answer unstreamed; the live kit compares them on
    real servers.
- **The protocol 8 churn:** every config fixture, example, seed and test config gains
  `capture`.
  - Mitigation: one conversion pass over the fixtures (a throwaway script, not kept);
    each half's test helpers build groups centrally.

## Tag

`v0.15.1`, the release of the Foundry key fix (2026-10-13): the plan starts from it,
with no anchor of its own (settled with the user) — only the plan's own documents
land on `main` between it and step 1.

## Branch and worktree

Branch `capture`, worktree `.claude/worktrees/capture`. It rebases onto `main` before
the ff merge.

Steps are implemented by subagents (the same model as the main session), one step per
brief. The main session reviews each against its acceptance criteria and commits at
the step boundary. The release after the merge is the user's call.

## Phases and steps

- **Phase 1 — Protocol 8** (steps 1–3). Green at the end: both halves at protocol 8,
  every group stating `capture`.
  1. `STEP-1-contract.md` — the specs for the whole feature; protocol 8's schemas and
     fixtures.
  2. `STEP-2-kaiak-control-protocol-8.md` — the version, the types, the sample, the
     guide.
  3. `STEP-3-gateway-protocol-8.md` — the version, the schema check, the group's flag.
- **Phase 2 — Trace export** (steps 4–5). Green at the end.
  4. `STEP-4-trace-exporter.md` — the traces signal, `otlptrace`, the fake collector's
     spans.
  5. `STEP-5-trace-wiring.md` — settings, start and exit, counts, the config warning.
- **Phase 3 — Capture** (steps 6–9). Green at the end.
  6. `STEP-6-capture-stage.md` — the stage, the session header, the budget, the
     client-bytes tee, the span with every attribute but the content.
  7. `STEP-7-chat-completions-content.md` — Chat Completions: the request, the answer,
     the stream.
  8. `STEP-8-messages-responses-content.md` — Messages and Responses.
  9. `STEP-9-e2e.md` — gateway e2e and cross-half e2e.
- **Phase 4 — Live kit, docs, review, live run** (steps 10–12). Green at the end.
  10. `STEP-10-live-kit-and-docs.md`
  11. `STEP-11-review-and-green.md`
  12. `STEP-12-live-run.md` — real backends, a self-hosted Langfuse.

Expected reds inside phase 1:
- After step 1, both halves fail the moved fixtures and the version checks.
- Step 2 clears `kaiak-control`'s; step 3 the gateway's.
- The cross-half e2e is red from step 2 until step 3.

Phases 2 and 3 stay green step by step: nothing reads the flag before step 6, and
steps 7–8 fill in content the step 6 span already has a place for.

## Verification

- **Component tests:**
  - the config rule on both halves (`capture` required, a boolean, per group, not
    inherited — the resolved fixtures pin it);
  - the traces settings (the explicit switch, malformed values failing the start);
  - the span encoding against the OTLP JSON rules (hex IDs, integer enums, decimal
    nanoseconds), the export limit, the release callback;
  - the stage: who is captured, the session header, the budget's drops, the request
    line's `kaiak.capture`;
  - each API's conversion over a corpus of requests and answers (text, multi-part,
    images by data and by URL, tool calls and results, reasoning, unknown items kept
    as generic parts), and each stream assembled equal to the same answer
    unstreamed.
- **Gateway e2e** against the fake backend and `fakeotlp`: each endpoint streamed and
  not, a capturing and a non-capturing group, refusals, a cut stream, the session
  header, the budget, the drain flushing captures at exit.
- **Cross-half e2e:** the sample control plane's config turns a group's capture on; the
  gateway's captures follow it.
- **`scripts/check-all.sh`** green at every phase end, and 3× in a row at the end.
- **Live (step 12):** real backends through kaiak into a self-hosted Langfuse; the
  traces show the conversation, the tools, the reasoning, the usage, the session; the
  live kit's captures equal the answers it received.
