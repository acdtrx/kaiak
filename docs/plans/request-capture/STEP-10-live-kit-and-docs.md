# Step 10 — the live-test kit and the docs

**Status:** done

## Intent

Let the live-test kit check captures against real servers, and finish the
operator- and app-facing docs.

## Files likely touched

- `scripts/live/`:
  - a small OTLP/HTTP receiver of its own (the kit is a separate module and cannot
    import the gateway's `fakeotlp`), started with the gateway, which gets
    `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` pointing at it;
  - one test group with `capture: true`;
  - checks per chat endpoint the kind serves, streamed and not: a span arrives; its
    input holds the prompt sent; its output equals the answer the kit received
    (text, tool calls, reasoning when the model gives it); the usage matches the
    usage record;
  - its own self-test (`check-gateway.sh` runs it) extended.
- `docs/DEPLOYMENT.md`:
  - **Request capture:** what it is, who decides (the app, per group), the trace
    variables and the explicit switch, `KAIAK_CAPTURE_MEMORY_BYTES` and `GOMEMLIMIT`
    — beside the budget, the builder's short-lived garbage: about 10× the capture
    being built (measured at step 7: a 4 MiB request with a 3 MiB inline image, ~18 ms
    and ~42 MB), one build at a time, so up to ~160 MB at the 16 MiB span limit —
    what is dropped and how to see it (`kaiak.capture`, the counts);
  - **Langfuse:** the endpoint, the Basic auth header (percent-encoded in
    `OTEL_EXPORTER_OTLP_TRACES_HEADERS`), `x-langfuse-ingestion-version: 4`; what it
    shows (session from `Kaiak-Session-Id`, metadata from the `kaiak.*` attributes;
    time to first token not shown);
  - **sending the session ID:** a header per request; the pi extension example
    (`before_provider_headers`); SDKs' per-request headers;
  - **upgrade notes, the next release** (protocol 8): every group needs `capture`;
    gateways and control plane upgrade together; apps decide each group's value;
    `gen_ai.provider.name` now on every type — the self-hosted types' usage metrics
    gain a `gen_ai_provider_name` label (`vllm`, `llama-server`, …) and the request
    line the attribute, so dashboards and alerts that matched its absence change;
    a sample config copied from the example before this change (the README's
    `data/sample-config.json`) is refused — copy `examples/local-config.json` again
    and re-add the key entry.
- `docs/testing/LIVE-BACKENDS.md` — the capture checks.
- `README.md` — the protocol version and a line on capture if the README lists
  features.
- `docs/architecture/*.html` — the version, and the capture stage where the pipeline
  is drawn.

## Acceptance criteria

- The kit's self-test passes; its capture checks pass against the fake backend.
- The docs name every variable, the explicit switch, the drops and the header.
- `scripts/check-all.sh` green.

## Result

### The live-test kit

- `scripts/live/traces.go` (new) — **the live-test kit's own OTLP/HTTP JSON receiver of
  traces**, on a free loopback port, `POST /v1/traces` only: it decodes OTLP JSON
  spans (attributes as plain values; an attribute twice, a value of no kind or of
  two, an `intValue` that is no decimal integer refuse the export `400`), refuses
  what is not `application/json` or over 32 MiB, and answers `{}`. With a forward
  URL it posts every export it took, as received, to that second endpoint with the
  forward headers — in the background, counted, `close` waiting for them.
  `parseForwardHeaders` reads `key=value,…` with percent-decoded values
  (`OTEL_EXPORTER_OTLP_TRACES_HEADERS`' format), naming an entry by its position,
  never its text.
- `scripts/live/capture.go` (new) — **the capture checks**:
  - `runKind` starts the receiver before the gateway and gives the gateway
    `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` pointing at it; `startGateway` now drops the
    shell's `OTEL_` variables as it drops `KAIAK_`, so a shell's OpenTelemetry
    settings cannot turn the export off (`OTEL_TRACES_EXPORTER=none`), fail the
    start (`…_PROTOCOL=http/protobuf`) or send the run's telemetry elsewhere.
  - The config gains one group, `live-capture`, `capture: true`, with its own key;
    `live`, `live-metered` and `live-media` stay `false`. The run has one
    `Kaiak-Session-Id`, `live-<label>-<8 hex>`, printed on its header line.
  - `sendCaptures` (in `checks`, after the Responses checks): per chat API the kind
    serves, four requests from the capture key — a system prompt and a question
    (`capture/<api>`, `-stream`), and a question for the weather with `get_weather`
    offered (`-tool`, `-tool-stream`); `<api>` is `chat`, `msg`, `resp`. Twelve on
    vllm and llama-server, eight on openai and azure-openai, four on the Anthropic
    kinds. The answer each got is read **by the live-test kit itself** into the GenAI
    conventions' output messages: chat choices by index (reasoning —
    `reasoning_content` else `reasoning` —, text, refusal, function calls; a stream
    gathered by choice and tool-call index up to `[DONE]`), Messages blocks (a
    stream from `message_start` to `message_stop`, blocks by index, tool input its
    fragments or its start's), the Responses response object (a stream's terminal
    event, so vLLM's other `call_id` on its item events does not matter).
  - **How the checks wait for spans: they do not.** `checkCaptures`,
    `checkCapturedOnly` and `checkForward` run in `runKind` after `gw.stop()`: the
    exit's trace flush has sent every capture the gateway built (`TestCaptureDrain`
    holds it), and the receiver stores an export before it answers it, so by the
    gateway's exit every span is in — no wait on the one-second send, no sleep. A
    run's capture lines print after `metrics`.
  - `capture/<api>…` per request: the request line says `kaiak.capture=queued`;
    exactly one span carries its `kaiak.request.id`; name `chat live-chat`, kind 3,
    no parent, no error status; `gen_ai.operation.name`, `gen_ai.request.model`,
    the key, group and root group, `gen_ai.conversation.id` the session, no
    `kaiak.session_id.ignored`; `gen_ai.request.stream` exactly when streamed; no
    `gen_ai.request.max_tokens` though the gateway filled the output limit (the
    client's own when a `-…-params` sets one); `server.address`/`server.port` the
    answering backend's base URL (`live` or `live-2`); provider, backend, type,
    deployment model, attempts, the five usage counts, cost, `estimated`,
    `partial`, time to first token and `kaiak.relay_end` equal to the request
    line's (numbers by value), `error.type` the line's; the response ID, model and
    finish reasons the answer's; `gen_ai.system_instructions` (Messages,
    Responses), `gen_ai.input.messages` and `gen_ai.tool.definitions` exactly what
    was sent; `gen_ai.output.messages` equal to the live-test kit's reading of the answer. A
    mismatch names the first differing path
    (`[0].parts[2].id is "fc_fake_1", want "call_fake_1"`) and shows both JSONs
    clipped.
  - `capture/groups`: after the exit, no span but the capture checks' — none for
    the requests of the groups that do not capture (48–78 per kind in the
    self-test), whose request lines carry no `kaiak.capture`; no export refused; no
    `trace export failing` line.
  - `capture/forward` (only with `-traces-forward`): every export forwarded and
    answered `2xx`.
- **A second destination** (for step 12's Langfuse): a gateway has one trace
  destination, the live-test kit's receiver; `-traces-forward <URL>` (env
  `LIVE_TRACES_FORWARD`) makes the receiver forward every export, as received, to
  a second traces endpoint, with the headers from the variable
  `-traces-forward-headers-env NAME` names. For Langfuse: `-traces-forward
  http://<langfuse>:3000/api/public/otel/v1/traces -traces-forward-headers-env
  LANGFUSE_OTLP_HEADERS` with
  `LANGFUSE_OTLP_HEADERS="Authorization=Basic%20<base64 pk:sk>,x-langfuse-ingestion-version=4"`
  (`LIVE-BACKENDS.md` → Request capture has the command). The live-test kit's checks and
  Langfuse then see the same spans; the session on the header line finds the run.
- **The fake backend** (`gateway/internal/fakebackend`): `Reply.ToolCallsWhenOffered`
  — the `ToolCalls` only for a request whose `tools` list is not empty, as a model
  calls only tools it was offered; the process gains `-reasoning <text>` (one stream
  event per word, vLLM's shape on the three APIs) and `-tool-call <name>` (call
  `call_fake_1`, arguments `{"city": "Oslo"}` in two pieces, only when tools are
  offered). The self-test's chat fakes, for every kind and the two-backend run, run
  with both, so every capture check holds reasoning and each tool check a tool
  call, whole and streamed; the other checks pass unchanged with the reasoning
  (their text is still there).
- **The self-test** forwards each run's traces to a second receiver of the live-test kit's,
  which refuses (`401`) an export without the forward headers, given
  percent-encoded (`Authorization=Basic%20…,x-langfuse-ingestion-version=4`):
  `capture/forward` proves they arrive decoded.
- `config.go`'s comment named protocol 7: now 8.

### Mutation checks (each reverted)

- The gateway's streamed chat reasoning dropped (`chatChoice.message`):
  `capture/chat-stream` and `capture/chat-tool-stream` fail (`gen_ai.output.messages
  differs from the answer received`).
- The capture stage capturing every group (`startCapture` ignoring the flag):
  `capture/groups` fails, naming `live-messages (group live)`, ….
- A Responses function call's ID read from `id` instead of `call_id`:
  `capture/resp-tool` and `-tool-stream` fail with `[0].parts[2].id is "fc_fake_1",
  want "call_fake_1"`.
- The receiver forwarding without its headers: `capture/forward` fails, `answered
  401`.

### Docs

- `docs/DEPLOYMENT.md`:
  - **Request capture** (new section, after Observability): what it is and the
    contract's place; the two switches and owners (the app per group, the operator
    per gateway) and the warning with export off; captures hold content — treat
    the destination so, access by the `kaiak.key.*` attributes; the explicit switch
    (`OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`, or `OTEL_TRACES_EXPORTER=otlp` beside the
    general endpoint, never the general endpoint alone; `none`,
    `OTEL_SDK_DISABLED`); never in the way of a request (copies on the path, the
    builder after the response, a send every second or at 16 MiB); memory
    (`KAIAK_CAPTURE_MEMORY_BYTES`, a destination down fills it); what is not
    captured and how drops show (`kaiak.capture`, `kaiak.session_id.ignored`, the
    two `otel_sdk_*_span_*` counters with their `error_type`s, `trace export
    failing`, `capture build failed`); at exit.
  - **Langfuse**: the endpoint, the Basic credential percent-encoded in
    `OTEL_EXPORTER_OTLP_TRACES_HEADERS` (with the `base64 | tr -d '\n'` that keeps
    Linux's wrapping out), `x-langfuse-ingestion-version: 4`, a Kubernetes env
    snippet, one project per gateway, no redirects; what Langfuse shows (one
    generation, the conversation with system instructions first, tools, the public
    model, parameters, usage with cache taken out of input, the session, `kaiak.*`
    as metadata, errors at level ERROR, inline media to its media store) and what
    it does not (time to first token, an end user).
  - **Sending the session ID**: the header's rule in short (pointing to the spec);
    curl, OpenAI's SDKs (per request and per conversation), Anthropic's SDKs,
    Claude Code (`ANTHROPIC_CUSTOM_HEADERS`), and pi's `before_provider_headers`
    extension (read from pi 1.1.0's own types: handlers mutate `event.headers`;
    `ctx.sessionManager.getSessionId()`, a UUID; the hook since pi 0.80.4),
    limited to the provider named `kaiak` in pi's `models.json`.
  - **Resources**: with trace export on, add the capture budget and the builder's
    short-lived garbage — about 10× the capture being built, one build at a time:
    about 40 MB for a 4 MiB request with an inline image, up to about 160 MB at the
    16 MiB export limit — so limit = 2 × body + upload + capture budget + 672 MiB
    (2080 MiB at the defaults, `GOMEMLIMIT=1870MiB` literal); the hard bounds
    include the capture budget.
  - **Environment**: `KAIAK_CAPTURE_MEMORY_BYTES`, `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`,
    `OTEL_TRACES_EXPORTER`, and the traces variants in the headers, timeout and
    protocol rows; the general endpoint "never trace export"; `OTEL_SDK_DISABLED`
    and `OTEL_SERVICE_NAME` over every export; "the live-test kit's `LIVE_*`".
  - Topology (a dotted edge to the trace tool), Draining (the last captures built
    and sent at exit), Observability (logs never hold content — captures only; the
    general endpoint sends no captures), the starter alert **Captures lost**.
  - **Upgrades → Next release (protocol 8)**: protocol 8, both halves together;
    every group states `capture`, the app decides each value (`false` changes
    nothing); nothing captured until the operator switches trace export on, and the
    memory to add then; `gen_ai.provider.name` on every type — self-hosted usage
    series gain `gen_ai_provider_name`, request lines the attribute, dashboards and
    alerts matching its absence change; the new header, variables, attributes,
    metrics, log lines and alert; a quick-start `data/sample-config.json` copied
    before is refused — copy `examples/local-config.json` again and re-add the key
    entry; host apps on `kaiak-control` (`PROTOCOL_VERSION` 8, `Group.capture`
    required). The 0.12.0 note saying the provider label is absent for self-hosted
    types now says until protocol 8 (it was the only such passage).
- `docs/testing/LIVE-BACKENDS.md`: the runner's steps (the capture group, the
  receiver, the gateway's environment without `OTEL_`, spans read after the exit);
  `live-chat`'s row; the request count; tool calling for vLLM
  (`--enable-auto-tool-choice --tool-call-parser`) and llama-server (`--jinja`); a
  **Request capture** section (the checks, tool calling, forwarding to a second
  destination with the Langfuse command); the self-test's reasoning, tool calls and
  forward target; three rows in the checks table (`capture/<api>…`,
  `capture/groups`, `capture/forward`), placed before `gateway-exit`; six readings
  of capture failures; the tester note on what the output holds.
- `README.md`: protocol version 8; the intro names capture; a **Captures** concept.
- `docs/architecture/gateway.html`: the pipeline diagram's Inbound box shows
  `capture`, the finishers box its hand-off to the builder (the figure 20 px
  taller), its label and caption; a **Capture** entry in What each stage does; the
  packages table gains `capture` and `telemetry/otlptrace`; the extension
  diagram's future "prompt / response log" is now the built "request capture"
  (a seam in code, its edge solid), its caption, and the stage seam's list
  (`capture` in, "prompt/response logging" out of the next plugs); Observability
  gains Request captures; the footer names protocol 8. Text measured in a browser:
  every new label inside its box.
- `docs/architecture/control-plane.html`: `Kaiak-Protocol: 8` (the callout and the
  stream diagram's label); a **Captures** row in Who owns what; the footer.
- `docs/ARCHITECTURE.md`: the live-test kit entry names its receiver and forward.

### Grep checklist

- **Protocol 7 as current**: none. The remaining mentions are the 0.15.0 and
  0.15.1 upgrade notes, the new entry's "a protocol-7 config document is refused",
  the two architecture footers' dated history, and
  `gateway/internal/control/remotetext_test.go`'s mismatching version `7` (a test of
  a mismatch). `Kaiak-Protocol: 7` appears nowhere outside plans and reviews.
- **Capture variables in `DEPLOYMENT.md`**: every variable the gateway reads for
  capture (`grep` of `cmd/kaiak` and `telemetry/otlp`:
  `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`, `OTEL_TRACES_EXPORTER`,
  `OTEL_EXPORTER_OTLP_TRACES_HEADERS`, `…_TIMEOUT`, `…_PROTOCOL`,
  `KAIAK_CAPTURE_MEMORY_BYTES`) is named, each 2–5 times; `Kaiak-Session-Id` 9.
- **"the kit"**: no line this step added says it; the new text says "live-test
  kit". The runbook and the live-test kit's own comments already used "the kit" for
  it before this step (about 40 places in `LIVE-BACKENDS.md`, about 20 comments in
  `scripts/live`), left as they were — a mechanical rename is for the main session
  to call.

### Suite

- The live-test kit's self-test: green for every kind, the capture checks included
  — vllm 89 passed, 0 skipped; llama-server 62 passed, 1 skipped; openai 32 and
  azure-openai 31 passed, 1 skipped each; anthropic and azure-anthropic 22 passed,
  3 skipped each; vllm with two backends 49 passed, 3 skipped (the skips as before:
  the checks whose servers the self-test does not give). It stays about as fast as
  before: no capture check waits.
- `scripts/check-all.sh`: green. It ran `scripts/check-gateway.sh` first — gofmt,
  vet, staticcheck and the telemetry boundary clean; `go test -race -count=1 ./...`
  passes every package (`kaiak/e2e` 142.9 s); the live-test kit's gofmt, vet and
  staticcheck clean and its self-test passes. Control `npm test`: 679 tests, 678
  pass, 0 fail, 1 skipped (as before); `npm run lint` clean. The cross-half e2e
  passes. Final lines:

  ```
  ==> npm run lint (control)
  > tsc -p . && node scripts/check-boundaries.ts
  boundaries ok
  ==> cross-half e2e (sample control plane + two gateways; two cores over one store)
  ok  	kaiak/e2e	82.879s
  all checks passed
  ```

- After the run, comments in `scripts/live` were re-wrapped (no code change): the
  live-test kit's gofmt, vet, staticcheck and self-test were run again, green.

### For steps 11 and 12

- **Step 12, the live run**: the `-tool` capture checks need tool calling on the
  self-hosted servers (vLLM `--enable-auto-tool-choice --tool-call-parser hermes`
  for Qwen3; llama-server `--jinja`), or they fail with the server's `400`. With
  Langfuse: `-traces-forward http://<langfuse>:3000/api/public/otel/v1/traces
  -traces-forward-headers-env LANGFUSE_OTLP_HEADERS` (`LIVE-BACKENDS.md` →
  Request capture); each run's captures form one session, named on its header
  line. The kit's reading of real answers follows the spec's rules for what real
  servers send (text, reasoning, function calls, unknown items generic); a
  difference on a real server is a finding for that API's mapping, named by path.
- **Step 12, Langfuse**: the DEPLOYMENT notes on what Langfuse shows come from
  `RESEARCH.md` (Langfuse read at 4.58) — the live run confirms or corrects them;
  `docs/DEPLOYMENT.md` → Request capture: Langfuse is the place for a gap found.
- **Step 11, review**: the fake backend's `ToolCallsWhenOffered` is a library
  change (test tooling) no gateway test uses yet; the pi example was read from pi
  1.1.0's installed types, not run; Claude Code's `ANTHROPIC_CUSTOM_HEADERS` is
  from its documentation.
