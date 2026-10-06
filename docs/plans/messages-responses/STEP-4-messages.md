# Step 4 — Messages

**Status:** done (2026-10-06)

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

**What changed**

- **Captures:** step 1's llama-server shapes (b9917, an embedding model) are replaced
  by the llama.cpp b10802 recordings of Qwen3.8-27B Q4: Messages and Responses,
  streamed and not, with thinking/reasoning and tool calls, counting answers, the
  wrong-path `404`, and the unknown-model-name answers. The Responses files wait for
  step 5. `captures/README.md` names the build, the model, the missing-model wording
  (a single-model llama-server has none) and its differences from vLLM.
- **Provider** (`gateway/internal/provider/`):
  - `stream_end.go`: a stream's end per format behind `streamEnd`.
    - OpenAI: `[DONE]` or every choice's `finish_reason`, moved unchanged.
    - Messages: complete at `message_stop`; `event: error` is the backend abandoning
      the stream. Events are read by their data's `type`, so interleaved content
      blocks change nothing.
  - **Error events:**
    - Mid-stream, the error event is relayed and the next read ends the stream
      `ErrIncomplete` (`kaiak.relay_end=upstream_incomplete`).
    - As the first event, `sendWire` returns the new `CodeErrorEvent`.
  - **Nested model rewriting:** `model.go`'s rewriter can also rewrite the `model`
    of one top-level member's object. Messages streams name `message`, so
    `message_start.message.model` is rewritten; bodies keep the top-level rule.
- **Accounting** (`gateway/internal/accounting/`):
  - The meter reads through a per-format `usageReader`:
    - `openai_usage.go` holds the existing OpenAI reading, moved unchanged.
    - `messages_usage.go` reads `message_start` then `message_delta`, the latest
      value of each field winning. Mapping: `input_tokens` → `tokens_in`, cache read
      → `tokens_cached`, cache creation → `tokens_cache_write`, `output_tokens` →
      `tokens_out`, reasoning 0.
    - Estimated Messages output counts text, thinking and tool-call input (stream
      deltas, or the body's `content` list).
  - The input estimate reads a Messages `source` whole:
    - `base64`, `url` or `file` is one media item, whatever its member order.
    - A `text` or `content` source is text, its own blocks scanned by the same rules.
- **Server** (`gateway/internal/server/`):
  - **Endpoints:** `/v1/messages` and `/v1/messages/count_tokens` are body endpoints
    (metric names `messages`, `messages_count_tokens`; `gen_ai.operation.name` `chat`
    for Messages, none for counting).
  - **Inbound** (`inbound_messages.go`):
    - Fields read: `model`; `stream` and `max_tokens` on `/v1/messages`.
    - The hosted-tool allowlist on both endpoints: `mcp_servers` and `container`
      refused; a `tools` entry's `type` must be absent, `custom`, or start `bash_`,
      `text_editor_`, `computer_` or `memory_`.
    - A tool naming `type` twice is `duplicate_member`; wrong shapes are
      `invalid_type`.
  - **Output limit:** Messages' key is `max_tokens`; counting has none.
  - **Token counting** reserves no tokens (`counts()`), so only request limits bite,
    and settles no record.
  - **Error shape:** `writeError` writes OpenAI's or Anthropic's shape
    (`errorShapeOf`). Anthropic's covers the Messages endpoints, their `405`s, and the
    model list and entry carrying `anthropic-version`, with the type following the
    status and kaiak's `code` inside `error`.
  - **Anthropic-shaped `/v1/models`:** with `anthropic-version`, the list and entry
    cover the models the key may use that serve Messages. A model without Messages
    answers `404 model_not_found` (`auth.ModelNotFound`, the same refusal as an
    unknown model).
  - **`CodeErrorEvent`:**
    - answered `502 upstream_error`
    - retried as `server_error`
    - a circuit failure, attempt outcome `server_error`
    - the meter treats it as refused (no units)
- **Fake backend:**
  - Messages and count_tokens under `/v1/` and `/anthropic/v1/` (the
    `azure-anthropic` layout), streamed and not.
  - Anthropic's error shape on those paths.
  - The existing fault modes apply to Messages streams; `ErrorEvent` /
    `ErrorEventAfter` add the error event.
  - The stream pacing moved into a `streamWriter` shared by both formats.
- **Spec** (`docs/specs/GATEWAY.md`):
  - An error-event-first stream is in the retry, outcome and attempt-metric rows.
  - A `tools` entry naming its `type` twice is `duplicate_member`.
  - The estimate's Messages source rule is stated in full.
- **Tests:**
  - **Provider:** every recorded Messages stream and body from both servers relays
    whole. Only the model name changes, byte count checked. Each stream cut before
    `message_stop` is incomplete. Error events mid-stream and first. Nested
    rewriting cases, split at every byte.
  - **Accounting:** usage from the recorded streams of both servers, the
    latest-value merge with cache read and write, body usage, estimated output
    (interleaved blocks, signatures not counted), and the media-source estimate (5
    cases, plus `source` outside Messages being text).
  - **Server:**
    - every gateway answer in Anthropic's shape: auth, unknown model, not served,
      not JSON, `max_tokens` above the context, 405, 413, a backend 5xx with its
      Retry-After
    - limit refusals with headers; token counting under a 1-token budget
    - the hosted-tool refusals on both endpoints, and client tools passing
    - passthrough edits, default and ceiling
    - usage mapping, streamed and not; error events; count_tokens with no record
    - the Anthropic-shaped list and entry
  - **e2e** (`gateway/e2e/messages_test.go`):
    - vllm, anthropic and azure-anthropic, streamed and not: path, credential,
      `anthropic-version` only on Anthropic types, `standard_only` only on
      `anthropic`, the client's key never forwarded, public model name, usage on the
      log line
    - count_tokens settles nothing
    - not served, hosted tool, price option and token limit refused in Anthropic's
      shape
    - an old vLLM's `404` on `/v1/messages`, 8 times, fails over with the circuit
      still closed and the warning logged
    - the Anthropic-shaped list

**Decisions made in this step** (please review)

1. **A `tools` entry naming `type` twice is refused** (`duplicate_member`,
   `tools[i].type`). The duplicate-member rule otherwise covers only the top level
   and `stream_options`, but the hosted-tool check owns `type` and must read the
   one the backend reads. Recorded in the spec.
2. **An error event as the first event is not a new client-visible code:**
   - It answers `502 upstream_error`, as the spec says ("answered like a 5xx").
   - Internally it is `provider.CodeErrorEvent`, retried as `server_error` and a
     circuit failure.
   - The meter counts it as refused (no units): the backend gave up before
     generating.
3. **The `computer_` allowlist prefix** is the spec's, but it also admits a type like
   `computer_toolset_20260801` if Anthropic's newer toolsets use that name and run
   server-side. Flagged for review; not changed against the spec.
4. **Errors on unknown paths stay OpenAI-shaped,** even with `anthropic-version`.
   The spec gives Anthropic's shape only to the Messages endpoints and the model
   list; a `405` on a Messages path does take it.
5. **The fake backend's Messages streams are synthetic** (a single text block). The
   recorded streams drive the provider and accounting tests directly, so their
   quirks (interleaved blocks, empty signatures, delta-only output usage) are tested
   on the real bytes.

**Suite** (2026-10-06):
- `scripts/check-gateway.sh` passed: gofmt, vet, staticcheck, `go test -race` with
  the e2e test, and the live kit's lint and self-test (vllm, llama-server, openai,
  azure-openai, vllm with two backends).
- `scripts/check-all.sh` passed: the gateway checks, control `npm test` (pass 576,
  fail 0), `npm run lint` (boundaries ok), and the cross-half e2e (`ok kaiak/e2e
  48.888s`), ending "all checks passed".
- The new server tests ran 5× and the Messages e2e 3× under `-race`, all green.
- **Phase 2 ends green.**
