# Step 6 — live-test kit

**Status:** done (2026-10-06)

## Intent

Extend the live-test kit so a single command checks a real backend on the new APIs,
and a tester with access to Anthropic, Foundry, OpenAI or Azure can run it and send
back output that holds no secrets. Live runs stay manual. Only `-self-test` (against
the fakes) runs in `scripts/check-gateway.sh`, as today.

## Files likely touched

- `scripts/live/`:
  - **kinds:** `anthropic`, `azure-anthropic`; their config generation (types, base
    URLs, `api_key_env`)
  - **Messages checks** for the kinds that serve it: plain and streamed answers;
    usage and cache units in the log line; the public model name in the answer
    (`message_start` included); count_tokens; a bad key and a bad model in
    Anthropic's shape; a hosted tool refused before the backend; on Anthropic kinds a
    price option refused and `standard_only` accepted by the backend
  - **Responses checks** for the kinds that serve it: plain and streamed answers;
    usage and reasoning units; the model name; input_tokens; `store` forced (the
    answer's `store` is `false`); a stateful field refused; a hosted tool refused; on
    `openai` / `azure-openai` the service tier
  - the self-test, covering every new check against the fake backend per kind
- `docs/testing/LIVE-BACKENDS.md`:
  - the new kinds and checks
  - a **"For a tester with access"** section: what each kind needs (key, resource
    name, a cheap model or deployment), the one command per kind, that the output
    holds no keys or prompt text, the expected cost (cents)
  - a **client checks** section: point Claude Code (`ANTHROPIC_BASE_URL`,
    `ANTHROPIC_AUTH_TOKEN`) and Codex (a custom provider with `wire_api =
    "responses"`) at a local kaiak, run one task each, and send back the gateway's log
    lines
  - the known client settings: Codex `web_search = "disabled"`, and Claude Code's
    1-hour cache if the live run shows it

## Decisions made during planning

- **The kit stays a module of its own**, standard library only.
- **Checks that need a capability** a cheap model lacks (thinking, cache writes need
  a long prompt) say SKIP with the reason, rather than FAIL.

## Acceptance criteria

- `go -C scripts/live run . -self-test` passes, covering every new check for every
  kind.
- `scripts/check-gateway.sh` runs only the lint and the self-test of the kit, as
  before.
- The runbook lets someone with no kaiak background run a kind and send back the
  result.
- Live run against the DGX vLLM and llama-server (Messages and Responses), recorded
  here. The user's DGX model is restored afterwards.
- Suite run and recorded. Expected red: none.

## Result

**What changed**

- `scripts/live/`:
  - `apis.go` (new): the endpoints each kind's backend type serves, mirroring
    `docs/specs/GATEWAY.md` (Providers → Endpoint support). The kit runs each API's
    checks only where it is served.
  - `main.go`: kinds `anthropic` and `azure-anthropic` (keys `ANTHROPIC_API_KEY`
    and `ANTHROPIC_FOUNDRY_API_KEY`, both required; Anthropic's URL is the default
    for `anthropic`; a Foundry URL ending in `/anthropic/v1` or `/anthropic` is
    trimmed with a note). New flags `-messages-params` and `-responses-params`, the
    `-chat-params` of the other two APIs. `-base-url-2` is refused for kinds without
    chat (the two-backend checks are chat checks); `-embeddings-model` needs
    `-embeddings-base-url` on them.
  - `messages.go` (new): `messages`, `messages-stream`, `messages-cache`,
    `messages-count`, `messages-models`, `messages-errors`, `messages-hosted-tool`,
    `price-options`, and the Messages variants of `output-ceiling` and `rate-limit`
    for kinds without chat.
  - `responses.go` (new): `responses`, `responses-stream`, `responses-count` (where
    served), `responses-stateful`, `responses-hosted-tool`, `service-tier` (OpenAI
    and Azure), and `endpoint-not-served` across all kinds.
  - `events.go` (new): typed-event stream reading and the error-shape helpers.
  - `checks.go`: the check sequence by served API; `auth-reject` through Messages
    for kinds without chat; `models` also checks `live-chat`'s `endpoints`;
    `metrics` also requires a request on each served `messages`/`responses`
    endpoint.
  - `process.go`: the self-test covers all six kinds (Anthropic layout with
    `x-api-key`, Foundry layout with `api-key`); its fakes report 3 of 7 prompt
    tokens as cache reads (`-cached-tokens`).
- `gateway/internal/fakebackend/`: the command gains `-auth x-api-key` and
  `-cached-tokens N` (`Reply.CachedTokens`); a Responses answer now reports back the
  request's `store` (OpenAI's default `true` when absent) and `service_tier`, as
  OpenAI's response object does — so the kit's `store` and tier checks have
  something real to read on the fake. No gateway test depended on the old
  hard-coded `store: false`.
- `docs/testing/LIVE-BACKENDS.md`: new kinds and commands, Thinking models (the
  per-API params), the self-test's six kinds, every new check in the checks table,
  **For a tester with access** (what each kind needs, one command per kind, the
  output holds no keys or prompt text, cost), **Client checks** (Claude Code via
  `ANTHROPIC_BASE_URL`/`ANTHROPIC_AUTH_TOKEN`, Codex with a `wire_api = "responses"`
  provider) with the **known client settings** (Codex `web_search = "disabled"`;
  Claude Code's 1-hour cache refused on Anthropic types; Claude Code's WebSearch
  refused), new failure readings, the vLLM reasoning-count note brought up to
  0.30.0, and "Anthropic and Foundry: assumptions made without access".

**Decisions made during the step**

- `-messages-params` / `-responses-params` beside `-chat-params`, rather than one
  flag for all three: the reasoning switches differ per API and per server.
- The cache check fails on the Anthropic kinds and skips on the self-hosted ones
  when no cache read is reported: Anthropic caches every marked prefix this long;
  vLLM and llama-server report cache reads only with their prefix cache on.
- `price-options` on the self-hosted kinds checks the option passes through (they
  price nothing), so the check runs on every Messages kind.
- `service-tier` skips when the answer reports no tier (live only; the fake echoes
  it).

**Self-test** — `go -C scripts/live run . -self-test`, every kind green:

| Kind | Passed | Failed | Skipped |
|---|---|---|---|
| vllm | 30 | 0 | 0 |
| llama-server | 30 | 0 | 1 (`endpoint-not-served`: it serves every endpoint the kit checks) |
| openai | 22 | 0 | 0 |
| azure-openai | 21 | 0 | 0 |
| anthropic | 16 | 0 | 2 (embeddings: Claude serves none) |
| azure-anthropic | 16 | 0 | 2 (same) |
| vllm with two backends | 33 | 0 | 0 |

**Live — vLLM 0.30.0, `unsloth/Qwen3.8-27B-NVFP4` on dgx.local:11434** (the
server already running; ordinary requests only):

```sh
go -C scripts/live run . -kind vllm -base-url http://dgx.local:11434/v1 \
  -model unsloth/Qwen3.8-27B-NVFP4 -max-output 4096 \
  -chat-params '{"chat_template_kwargs":{"enable_thinking":false}}' \
  -messages-params '{"chat_template_kwargs":{"enable_thinking":false}}' \
  -responses-params '{"chat_template_kwargs":{"enable_thinking":false}}'
```

27 passed, 0 failed, 3 skipped:

| Check | Result |
|---|---|
| auth-reject | PASS — no key → 401 |
| models | PASS — endpoints chat_completions, completions, embeddings, messages, messages_count_tokens, responses |
| chat, chat-stream, chat-stream-usage + usage logs | PASS |
| embeddings, usage-log/embeddings | SKIP — no `-embeddings-model` |
| messages | PASS — end_turn, 24+21 tokens, `max_tokens` set by the gateway |
| usage-log/messages | PASS — in 24, out 21 |
| messages-stream | PASS — 14 events, message_start … message_stop |
| usage-log/msg-stream | PASS |
| messages-cache | SKIP — vLLM reported no cache read on the repeated 8k-token prompt |
| messages-count | PASS — 24 input tokens, no usage record |
| messages-models | PASS — Anthropic's shape |
| messages-errors | PASS — 401 authentication_error, 404 not_found_error |
| messages-hosted-tool | PASS — 400 hosted_tool_unsupported, never sent |
| price-options | PASS — `speed: "fast"` passed through |
| responses | PASS — completed, 24+25 tokens (vLLM's answer carries no `store`) |
| usage-log/responses | PASS |
| responses-stream | PASS — 23 events, response.created … response.completed |
| usage-log/resp-stream | PASS |
| responses-stateful | PASS |
| responses-hosted-tool | PASS |
| endpoint-not-served | PASS — /v1/responses/input_tokens → 400 |
| output-ceiling | PASS — max_tokens 32768 → 16, finish length |
| rate-limit | PASS |
| metrics | PASS — 10 usage records, 1 rate-limited, 13 successful attempts |

A second run with thinking left on (no `-*-params`, `-max-output 4096`) also passed
27/0/3: chat and Responses reported reasoning tokens (chat 44 of 62, Responses 41 of
67 and 47 of 64 streamed); Messages reported none, as expected.

**Live findings**

- vLLM 0.30.0 now reports `completion_tokens_details.reasoning_tokens` on chat (the
  runbook said it usually did not; updated).
- vLLM's Messages answer reported no cache read for an identical ~8k-token prefix
  sent twice (`cache_read_input_tokens` absent or 0). Whether its prefix cache is
  off on this server or its Messages layer does not report it was not settled —
  the check skips by design; nothing in the spec depends on it.
- No finding contradicts the spec or step 1's recordings.

**Live — llama-server build b10802, `unsloth/Qwen3.8-27B-GGUF:UD-Q4_K_XL` (alias
`qwen38-27b`) on dgx.local:11434** (ctx 65536, `--jinja`, reasoning on with
`reasoning_effort` medium, `--parallel 1`; the main session swapped the DGX, the kit
sent ordinary requests only):

```sh
go -C scripts/live run . -kind llama-server -base-url http://dgx.local:11434/v1 \
  -model qwen38-27b -max-output 4096
```

28 passed, 0 failed, 3 skipped:

| Check | Result |
|---|---|
| auth-reject | PASS |
| models | PASS — endpoints chat_completions, completions, embeddings, messages, messages_count_tokens, responses, responses_input_tokens |
| chat, chat-stream, chat-stream-usage + usage logs | PASS |
| embeddings, usage-log/embeddings | SKIP — no `-embeddings-model` |
| messages | PASS — end_turn; the answer's `input_tokens` 4 beside 18 cache reads |
| usage-log/messages | PASS — in 22 (cache read 18), out 75 |
| messages-stream | PASS — 79 events, message_start … message_stop |
| usage-log/msg-stream | PASS — in 22 (cache read 18), out 74 |
| messages-cache | PASS — second request read 9943 of 9947 input tokens (the first wrote 0: llama-server reports no cache writes) |
| messages-count | PASS — 22 input tokens, no usage record |
| messages-models | PASS |
| messages-errors | PASS |
| messages-hosted-tool | PASS |
| price-options | PASS — passed through |
| responses | PASS — completed, 22+95 tokens |
| usage-log/responses | PASS — in 22 (cache read 18), out 95 |
| responses-stream | PASS — 82 events, response.created … response.completed |
| usage-log/resp-stream | PASS |
| responses-count | PASS — 22 input tokens, no usage record |
| responses-stateful | PASS |
| responses-hosted-tool | PASS |
| endpoint-not-served | SKIP — llama-server serves every endpoint the kit checks |
| output-ceiling | PASS — max_tokens 32768 → 16, finish length |
| rate-limit | PASS |
| metrics | PASS — 10 usage records, 1 rate-limited, 14 successful attempts |

Consistent with step 1's recordings: Messages' `input_tokens` excludes the cache
reads and the gateway adds them back (log line in 22 = 4 + 18), Responses'
`input_tokens` includes them, and llama-server reports no reasoning count (the
log's reasoning is 0, with reasoning on). Nothing contradicts the spec.

**Suite** — `scripts/check-all.sh`: all checks passed (gateway gofmt, vet,
staticcheck, race tests including the e2e; the live-test kit's lint and self-test,
every kind green; control `npm test` and lint; the cross-half e2e). Expected red:
none. `scripts/check-gateway.sh` still runs only the kit's lint and `-self-test`.

**Embeddings, live (2026-10-06, run by the main session after step 7):** the vllm
kind against vLLM 0.30.0 (`unsloth/Qwen3.8-27B-NVFP4`, `-max-output 4096`) with
`-embeddings-base-url`, twice:
- vLLM's `qwen3-embed` entry (`qwen3-embedding-0.6b`, `dgx.local:8003`, started for
  the run and stopped after): 29 passed, 0 failed, 1 skipped (`messages-cache`);
  `embeddings` 1024 dimensions, 6 tokens; `usage-log/embeddings` in 6, out 0.
- llama-server on `llama-embed.local:11435` (`/models/qwen3-embedding-0.6b-q8_0.gguf`):
  29 passed, 0 failed, 1 skipped; the same embeddings figures.
