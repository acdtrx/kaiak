# Step 12 — the live run

**Status:** done 2026-10-14 — the live-test kit's capture checks pass on vLLM and
llama-server (28 of 28), and Langfuse shows each endpoint's capture as intended; no
gateway gap found; the DGX restored and verified. Langfuse is left running on
dev.local until the user has looked at it (Cleanup, below).

## Intent

Send real traffic through kaiak with capture on, into a self-hosted Langfuse, and
confirm that what Langfuse shows is what the apps sent and received: the
conversation, system prompts, tools, reasoning, usage, sessions.

## How

- **Ask the user** where Langfuse runs (its Docker Compose setup: web, worker,
  Postgres, ClickHouse, Redis, MinIO) and which backends to use:
  - the DGX when it is free — a vLLM chat model with reasoning and tool calling, and
    llama-server; the DGX rules as before: record `cria status --json` and
    `~/spark-setup/vllm.sh status` first, stop the user's model only through cria,
    restore and verify exactly that state, no sudo, no edits to cria or vLLM configs
    or existing venvs, keep weights and builds; or requests to the user's running
    model as it is, if the user allows it;
  - cloud kinds (OpenAI, Anthropic) if the user provides keys.
- **The live-test kit** per backend with capture on, sending its spans to Langfuse
  (and to its own receiver for its checks).
- **In Langfuse:** each endpoint's trace shows input and output as a conversation;
  system instructions; tool definitions and calls; reasoning; token usage (cache
  tokens subtracted as the convention says); the session from `Kaiak-Session-Id`;
  errors as error level. Screenshots of what each looks like, kept outside the repo
  and deleted at the end; findings written here.
- **Anything Langfuse does not show** is weighed against the conventions: a gateway
  bug is fixed; a Langfuse gap is recorded in the deployment notes, not worked
  around in the gateway (OVERVIEW decision 3).
- **Cleanup:** Langfuse's containers and volumes removed unless the user wants them
  kept.

## Acceptance criteria

- The kit's capture checks pass on each backend (counts recorded).
- What Langfuse displays per endpoint recorded, with any gap and its cause.
- The DGX restored and verified if it was used.
- `scripts/check-all.sh` green after any fix, 3× in a row if code changed.

## Result

### Langfuse

- **Langfuse 4.58.0** (`/api/public/health`), its Docker Compose file at tag
  `v4.58.0`, on `dev.local` through the `dev` Docker context: random secrets,
  telemetry off, headless init (organization `kaiak`, project `kaiak-live`, its keys,
  one user), the media endpoint `http://dev.local:9090`. Its env file is in the
  session's scratch folder, outside the repository.
- dev.local had 6.5 GB free; the images took 3.7 GB (1.2–1.4 GB left during the
  run). Its build cache (15 GB reclaimable) was left alone.
- Langfuse 4 runs in "events_only" mode: the v1 read endpoints (`/traces`,
  `/observations`, `/sessions`) answer 404; `/api/public/v2/observations` reads what
  was ingested. That touches only reading Langfuse, not the gateway's export.

### Starting state on the DGX (read-only)

- 08:42:07 UTC, `cria status --json` (exit 0): one server, `qwen38-flash` (llama,
  `unsloth/Qwen3.8-Flash-Next-GGUF` `UD-Q4_K_XL`, `~/opt/llama.cpp-slotpick`), picks
  `cache=q8 cache_ram=0 context=512k mtp=3 slots=2 vision=on`, `0.0.0.0:11434`, PID
  868181, launched 2026-10-12T10:56:22Z, health 200; no broken entries, no router.
  Both slots idle.
- `~/spark-setup/vllm.sh status`: `stopped` (exit 1). `nvidia-smi`: the Flash
  `llama-server` alone (96,465 MiB). Listening: 22, 53 (loopback), 11434.
- Re-read 08:43:27 (unchanged, idle); stopped through cria (`cria stop qwen38-flash`)
  at 08:43:36.

### Servers

- **vLLM 0.30.0**, the kept venv `~/opt/vllm-omni-kaiak/.venv`, plain `vllm serve`:
  `Qwen/Qwen3.5-2B` (revision `15852e8c…`, from the cache) served as `qwen3.5-2b` on
  8001, with cria's `qwen35-2b-a` flags (tool calls `qwen3_coder`, 32k context, 0.15
  of the GPU, language model only) plus `--reasoning-parser qwen3` and
  `--default-chat-template-kwargs '{"enable_thinking": true}'` — the model's
  template thinks only when asked, and cria's entry has no reasoning parser.
- **llama.cpp build 11525** (`~/opt/llama.cpp-kaiak-rerank`): `Qwen3.8-27B`
  `UD-Q4_K_XL` as `qwen38-27b` on 8008, the rerank run's command (`--jinja`,
  `reasoning_effort` medium, 64k context).
- Both healthy at 08:44:53, a minute after the start; `EngineCore` 16,566 MiB, the
  27B 18,671 MiB.

### The live-test kit (branch `capture` at `8f98635`)

```sh
export LANGFUSE_OTLP_HEADERS="Authorization=Basic%20<base64 of public:secret>,x-langfuse-ingestion-version=4"
go -C scripts/live run . -kind vllm -base-url http://dgx.local:8001/v1 -model qwen3.5-2b -max-output 4096 \
  -request-timeout 5m -traces-forward http://dev.local:3000/api/public/otel/v1/traces \
  -traces-forward-headers-env LANGFUSE_OTLP_HEADERS
go -C scripts/live run . -kind llama-server -base-url http://dgx.local:8008/v1 -model qwen38-27b -max-output 4096 \
  -request-timeout 5m -traces-forward http://dev.local:3000/api/public/otel/v1/traces \
  -traces-forward-headers-env LANGFUSE_OTLP_HEADERS
```

- **vLLM: 40 passed, 3 failed, 6 skipped.** The three failures (`chat-stream`,
  `responses`, `responses-stream`: "only reasoning came back") are the setup: the 2B
  model with thinking on reasons through the whole 4096-token output limit, at about
  47 tokens a second. Every capture check passes: the 12 `capture/*` checks with
  reasoning in each answer and a `get_weather` call in each tool check;
  `capture/groups` (12 spans, none for the 33 requests of the other groups);
  `capture/forward` (12 spans in 12 exports, each answered 2xx).
- **llama-server: 45 passed, 0 failed, 5 skipped**, the 14 capture checks among them.

### What Langfuse shows (24 observations, read through its API)

- One `GENERATION` per capture, named `chat live-chat`, its model the public name,
  its session the run's `Kaiak-Session-Id` (`live-vllm-…`, `live-llama-server-…`).
- **Input** as a conversation: the system instructions first — Chat's system message
  as sent, Messages' `system` and Responses' `instructions` as Langfuse's system
  message — then the user's message, and the tools offered beside them.
- **Output**: the assistant's reasoning and text, or reasoning and the tool call with
  its ID and arguments — whole and streamed alike.
- **Usage**: Langfuse takes the cache reads out of the input (llama-server: 290 →
  4 + 286 cached) and the reasoning out of the output (vLLM: 1,552 → 23 + 1,529
  reasoning). The second was not in the deployment notes: added.
- **Metadata**: every `kaiak.*` attribute and the `gen_ai.*` ones; the request
  parameters set (`stream`, `max_tokens`, `temperature`) as model parameters.
- **Not shown**, as the deployment notes say: the time to first token
  (`completionStartTime` empty; `kaiak.time_to_first_token` in the metadata) and a
  cost (Langfuse has no price for the model; kaiak's is in the metadata).

### Straight from the gateway, no receiver between

The gateway built from the branch, in file mode against llama-server, its trace
export set as an operator would: `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` to Langfuse and
`OTEL_EXPORTER_OTLP_TRACES_HEADERS` percent-encoded. Eleven requests:

- **Two turns of tool use on each API under one session**: turn 2's input shows the
  call and its result — Chat's `tool` message, Messages' `tool_result`, Responses'
  `function_call_output` with the reasoning item sent back.
- **Errors at level ERROR with `error.type`**: a model the key may not use
  (`model_not_found`, 404) and a backend error (`upstream_error`, 500), neither with
  output; a stream the client cut (`client_closed`; the request line's
  `kaiak.relay_end=client_closed`), keeping the reasoning that had arrived.
- **A malformed session ID**: no session; `kaiak.session_id.ignored=invalid_characters`
  in the metadata and on the request line.
- **A group that does not capture**: no `kaiak.capture` on its line, nothing in
  Langfuse.
- The gateway log: 20 lines, no warning or error, no prompt or answer text; drained
  and stopped on SIGTERM.

No screenshots: the browser login needs the Langfuse user's password, which this
session does not read; what Langfuse holds was read through its API, and the UI is
the user's to look at.

### Gaps

None on the gateway's side. One line added to the deployment notes (the reasoning
tokens).

### The DGX restored

- Test servers stopped (09:07:19): no vLLM or llama-server process, the GPU empty,
  listening 22 and 53.
- `~/.local/bin/cria start qwen38-flash cache=q8 cache_ram=0 context=512k mtp=3
  vision=on slots=2 --wait` (09:07:45 → running at 09:07:57).
- Checked: `cria status --json` — the same entry, repository, quant, picks, host,
  port and command, phase running, health 200, no broken entries, no router;
  `vllm.sh status` `stopped` (exit 1); `nvidia-smi` the Flash `llama-server` (96,025
  MiB); listening 22, 53, 11434; two slots of 262,144. The scratch folder
  `/tmp/kaiak-live-capture` removed.

### Cleanup

Langfuse (containers, volumes, images) stays on dev.local until the user has looked at
the UI; then `docker --context dev compose -p langfuse-kaiak down -v --rmi all`.
