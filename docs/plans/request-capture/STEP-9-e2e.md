# Step 9 — end to end

**Status:** done

## Intent

Prove the feature through the whole gateway and across both halves.

## Files likely touched

- `gateway/e2e/capture_test.go` (new), the gateway against the fake backend and
  `fakeotlp` receiving traces:
  - each endpoint, streamed and not: one span per request, its attributes and content
    as the spec's examples show;
  - a capturing and a non-capturing group side by side; a child group capturing under
    a parent that does not, and the reverse;
  - refusals after the stage (model access, limits, queue): spans with `error.type`
    and no output; a refusal before it (bad JSON): no span;
  - a client that leaves mid-stream: the partial output, `kaiak.relay_end`;
  - an upstream error answered `upstream_error`;
  - the session header: kept; malformed — the request served, the span without a
    session, `kaiak.session_id.ignored` on the span and the line; absent;
  - the budget: a tiny `KAIAK_CAPTURE_MEMORY_BYTES` drops captures and the requests
    still succeed; the counts and `kaiak.capture` say why;
  - traces off while the group captures: the warning, `export_off`, no span;
  - the drain: captures of requests finished before SIGTERM reach the collector;
  - the backend receives the same request bytes with capture on and off.
- `gateway/e2e/` cross-half (`crosshalf` tag): the sample control plane serves a config
  whose group captures; a gateway's capture follows a publish that turns it off.
- `gateway/internal/fakebackend/` if a shape the tests need is missing (reasoning
  deltas, tool-call fragments, Messages thinking, Responses items).

## Acceptance criteria

- The tests above pass; `scripts/check-all.sh` green (phase 3 ends here).

## Result

### What changed

- `gateway/e2e/capture_test.go` (new) — request capture through the built binary,
  traces sent to `fakeotlp` by `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`. One scenario
  config (`captureScenario`): model `cap` on a `vllm` backend (the three chat APIs,
  priced, output limit 64/128), `other` (not allowed to `cap-child`), `chat-only`
  (chat completions alone), `held` (a backend with one slot, queue size 0); groups
  `team` (off) ⊃ `cap-child` (on), `cap-root` (on) ⊃ `plain-child` (off), `limited`
  (on, 1 request a minute). `expectSpan` checks each span against its request line:
  every attribute the two share (keys, model, provider, backend, attempts, the five
  usage counts, cost, flags, `error.type` — else `kaiak.relay_end` — time to first
  token, `kaiak.session_id.ignored`) has the line's value, the span's key set is
  exactly those plus the expected span-only ones, the name, kind `CLIENT`, ID lengths,
  a duration within the line's, and the status (`ERROR` with `error.type`, else
  none). Each subtest sends its requests and waits once for their spans (the trace
  export sends on a one-second ticker).
- `gateway/e2e/capture_sample_test.go` (new, `crosshalf`) — `TestAcrossHalvesCapture`.
- `gateway/internal/fakebackend/` — `Reply.ToolCalls` (`ToolCall{ID, Name,
  Arguments}`, the arguments in pieces): a chat completion's `tool_calls` (finish
  reason `tool_calls`; streamed: an opening fragment with the ID, type and name, then
  one fragment per piece), Messages `tool_use` blocks (stop reason `tool_use`;
  streamed: `content_block_start` with an empty input, an `input_json_delta` per
  piece), Responses `function_call` items (streamed: `output_item.added`, a
  `function_call_arguments.delta` per piece, `.done`, `output_item.done`) — each
  after the text. Nothing changes when unset. The chat chunk builders now share
  `chatChunk` / `streamChunk`.
- No gateway code changed: no bug found. No dependency added.

### Cases and where each is tested

- **Each endpoint, streamed and not** (`TestCaptureEndToEnd`, first subtest): the
  spec's three examples (the Responses one with its tool declared), each answered with
  text (reasoning included on Chat and Responses) and with a tool call (thinking
  included on Messages) — 12 requests, one span each, its input, system instructions,
  tools, parameters (the client's `max_tokens` 1024 while the backend got the ceiling
  128; no `max_tokens` on Chat though the gateway injected 64), output messages,
  finish reasons, response ID and model; a streamed answer's output equal to the
  whole one's.
- **Groups side by side, parents and children** (second subtest): `team` and
  `plain-child` make no span and no `kaiak.capture`; `cap-child` (under a parent that
  does not capture) and `cap-root` (whose child does not) do, with their root group.
- **Refusals** (third subtest): after the stage — model access (`model_not_found`),
  the endpoint list (`endpoint_not_served`), the limit (`rate_limit_exceeded`), the
  queue (`queue_full`, the slot held by a non-capturing stream) — spans with the
  input, `error.type`, status `ERROR`, no output and no routed attributes, the line
  `queued`; before it, a body that is not JSON (`invalid_json`): no `kaiak.capture`,
  no span.
- **An upstream error** (`upstream_error`, fourth subtest): routed, no output.
- **A client that leaves mid-stream** (fifth subtest): the paced stream read two
  events and closed — `kaiak.relay_end` and `error.type` `client_closed`, the output
  `Hello from`, no finish reason.
- **The session header** (sixth subtest): absent; kept (`gen_ai.conversation.id`);
  malformed — spaces (`invalid_characters`), 257 characters (`too_long`), empty
  (`empty`) — all served, the reason on the line and the span, no session on the
  span; no value in the log; never sent to the backend.
- **The same request to the backend** (seventh subtest): four requests (Chat streamed
  with `<`/`&`, Chat with an unknown member, Messages, Responses streamed), each sent
  by a capturing and a non-capturing key under one request ID with a session header:
  the backend got the same path, headers and body.
- At the end of `TestCaptureEndToEnd`: the processor's handed count and the
  exporter's accepted count equal the captured requests, every drop count 0; after the
  stop the collector holds exactly the captured requests' spans, one each, every
  export at `/v1/traces` under one resource (`service.name` `kaiak`, the instance ID,
  the version) and the scope `kaiak`; the log holds none of the content.
- **The drain** (`TestCaptureDrain`): three requests finished before SIGTERM, a
  stream the drain lets finish, one it cuts at its timeout (`kaiak.relay_end`
  `shutdown`, output `Hello`) — all five spans at the collector once the process has
  exited; the trace exports before the final metric export, which counts five handed
  and five exported, none dropped. The drain timeout (500 ms) ends the drain before
  the trace export's first periodic send, so every one of the five is sent by the
  exit's flush.
- **The budget** (`TestCaptureBudget`, `KAIAK_CAPTURE_MEMORY_BYTES=2048`): a body the
  budget cannot copy and an answer it cannot hold (its buffer starts at 4 KiB) — both
  answered whole, the lines `memory`, the processor's `memory` count 2 and handed 0;
  a non-capturing key's request unaffected; nothing sent to the collector.
- **Trace export off** (`TestCaptureTraceExportOff`): with nothing set, and with only
  `OTEL_EXPORTER_OTLP_ENDPOINT` — the warning (WARN, `kaiak.config.capture_groups`
  3), `export_off` with the session header not read, no trace counts, no trace
  endpoint on `kaiak starting`; with the general endpoint the collector got logs and
  metrics and nothing at `/v1/traces`. Its counterpart: `OTEL_TRACES_EXPORTER=otlp`
  beside the general endpoint sends the span to its `/v1/traces`, no warning.
- **Cross-half** (`TestAcrossHalvesCapture`, `crosshalf`): the sample serves
  `testConfig` with `eval` capturing; a gateway it configures captures `eval`'s
  request (span checked against its line) and not `ann`'s; the config file edited to
  turn `eval`'s capture off, the sample publishes, the gateway applies it, and the
  next `eval` request has no `kaiak.capture`; after the stop, the collector holds the
  first request's span alone. `check-all.sh`'s `^TestAcrossHalves` runs it.
- Covered elsewhere, not repeated end to end: `too_large`, a build fault (`_OTHER`),
  a failing collector and the final `trace export failing` line (`internal/capture`,
  `internal/server`, `cmd/kaiak` tests).

### Checks

- Mutation checks (each reverted): a child group capturing by its parent fails the
  groups subtest; the capture stage moved after model access fails the refusals
  subtest; no trace flush at exit fails the drain test (no span at the collector).
  The flush run before the builder's last builds is not caught end to end — the
  builder has built the last capture before the exit path reaches the flush — and is
  caught by `cmd/kaiak`'s exit test with the builder held still.
- `go test -race -tags crosshalf -run 'TestCapture|TestAcrossHalvesCapture'
  -count=3 ./e2e`: green, three runs (`TestCaptureEndToEnd` about 8 s each — seven
  waits on the one-second send — the others 1–3.5 s).

### Suite

- `scripts/check-all.sh`: green — phase 3 ends here. It ran `scripts/check-gateway.sh`
  first: gofmt, vet, staticcheck and the telemetry boundary clean; `go test -race
  -count=1 ./...` passes every package (`kaiak/e2e` 141.6 s); the live-test kit's lint
  is clean and its self-test passes for vllm, llama-server, openai, azure-openai,
  anthropic, azure-anthropic, and vllm with two backends. Control `npm test`: 679
  tests, 678 pass, 0 fail, 1 skipped (as before); `npm run lint` clean. The
  cross-half e2e, `TestAcrossHalvesCapture` included, passes. Final lines:

  ```
  ==> npm run lint (control)
  > tsc -p . && node scripts/check-boundaries.ts
  boundaries ok
  ==> cross-half e2e (sample control plane + two gateways; two cores over one store)
  ok  	kaiak/e2e	82.886s
  all checks passed
  ```

### For step 10

- `fakebackend.Reply` answers reasoning (`Reasoning`) and tool calls (`ToolCalls`) on
  the three chat APIs, but the fake backend process
  (`internal/fakebackend/cmd/fakebackend`) has no flag for either: the kit's
  self-test needs one (or a profile) to check captured tool calls and reasoning
  against the fake.
- The trace export sends on a one-second ticker from its start: a receiver in the kit
  waits up to a second per span, or stops the gateway — the final flush delivers
  everything (`TestCaptureDrain`).
- The docs' explicit switch, as the e2e shows it: `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`,
  or `OTEL_TRACES_EXPORTER=otlp` beside `OTEL_EXPORTER_OTLP_ENDPOINT` (to its
  `/v1/traces`); the general endpoint alone sends logs and metrics, never spans.
