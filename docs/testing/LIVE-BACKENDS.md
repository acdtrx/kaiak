# Live backend checks

> The runbook for `scripts/live/`: run the built `kaiak` against a real vLLM,
> llama-server, Azure OpenAI or OpenAI backend and check it end to end. Opt-in —
> never needed for a green suite (`docs/TECH-STACK.md`, Testing). The kit's own
> self-test, against the fake backend, runs in `scripts/check-gateway.sh`.

## What it does

One command per backend kind. The runner:

1. builds `kaiak` from this checkout (or runs the one given with `-kaiak`);
2. generates a config for the backend — a fresh random client key and its hash, one
   backend of the type `-kind` names (two with `-base-url-2`: see Two vLLM
   processes, one model; one more with `-embeddings-base-url`: see Embeddings on a
   server of their own), four public models on it (below), one group holding the
   key;
3. starts `kaiak` on free loopback ports with a temporary data directory and JSON logs;
4. runs the checks, printing `PASS`/`FAIL`/`SKIP` per check and a summary;
5. sends SIGTERM, requires a clean drain and exit 0, and deletes everything it made
   (`-keep` keeps the config and the gateway log, and prints where).

Exit code 0 only when every check passed. Ctrl-C stops the gateway and cleans up.

Public models in the generated config, the chat ones all routed to the one backend
model:

| Name | Purpose |
|---|---|
| `live-chat` | chat checks; output limit `-max-output` (default 1024) |
| `live-capped` | output-limit ceiling check; ceiling `-ceiling` (default 16) |
| `live-rpm` | limit check; 1 request per minute for the key's group |
| `live-embed` | embeddings, only with `-embeddings-model` — whatever the model's name on the backend (a path-style id too) |

Chat models are priced at `-price-in` / `-price-out` USD per million tokens (default
1 / 2 — placeholders, only so the cost path is exercised; `0` and `0` leave them
unpriced and skip the cost checks).

## Prerequisites

- Go (the version in `gateway/go.mod`) and this repository checked out; run from the
  repository root (the runner finds `gateway/` from the working directory).
- The backend reachable from this machine: `curl <base-url>/models` (vLLM,
  llama-server, OpenAI) or
  `curl https://<resource>.openai.azure.com/openai/v1/models -H "api-key: $AZURE_OPENAI_API_KEY"`
  answers.
- The backend key in an environment variable (vLLM and llama-server only if they run
  with `--api-key`). The runner passes the variable's **name** into the config
  (`api_key_env`), never the value; the value never appears in its output or in the
  gateway log.
- Each run sends about eight small chat requests (one asks for a long story, cut at
  16 tokens) and one embeddings request — a few thousand tokens on a paid API.

## Commands

### vLLM

```sh
go -C scripts/live run . -kind vllm \
  -base-url http://vllm-host:8000/v1 \
  -model Qwen/Qwen3-32B \
  -embeddings-model BAAI/bge-m3          # optional: a pooling model on the same URL
# vLLM started with --api-key:  export VLLM_API_KEY=...  and add  -api-key-env VLLM_API_KEY
# thinking model, answers are all reasoning:  -chat-defaults '{"chat_template_kwargs":{"enable_thinking":false}}'
#                                        or:  -max-output 4096
```

- `base-url` is what an OpenAI client would use: it **includes `/v1`**.
- `model` is the name vLLM serves (`--served-model-name`, else the model path) —
  `curl http://vllm-host:8000/v1/models` lists it.
- No key by default: vLLM runs unauthenticated unless started with `--api-key`.
- Embeddings usually live on a separate vLLM instance (a pooling model): add
  `-embeddings-base-url` (see Embeddings on a server of their own).

### llama-server

```sh
go -C scripts/live run . -kind llama-server \
  -base-url http://llama-host:8080/v1 \
  -model /models/qwen3-8b-q4_k_m.gguf
# llama-server started with --api-key:  export LLAMA_API_KEY=...  and add  -api-key-env LLAMA_API_KEY
```

- `base-url` includes `/v1`, as for vLLM.
- `model` is the id llama-server lists — the model file's path unless it was started
  with `--alias <name>` — `curl http://llama-host:8080/v1/models` lists it.
- No key by default: llama-server runs unauthenticated unless started with
  `--api-key`.
- Embeddings run on a llama-server of their own (started with `--embeddings`): add
  `-embeddings-base-url` (see Embeddings on a server of their own).

### OpenAI

```sh
export OPENAI_API_KEY=sk-...
go -C scripts/live run . -kind openai \
  -model gpt-4.1-mini \
  -embeddings-model text-embedding-3-small
```

- `base-url` defaults to `https://api.openai.com/v1`.
- The key goes in `Authorization: Bearer`; another variable name: `-api-key-env NAME`.

### Azure OpenAI

```sh
export AZURE_OPENAI_API_KEY=...
go -C scripts/live run . -kind azure-openai \
  -base-url https://<resource>.openai.azure.com \
  -model <chat-deployment-name> \
  -embeddings-model <embeddings-deployment-name>
```

- `base-url` is the **resource endpoint** — no path. The gateway appends `/openai/v1/`
  and the endpoint path (a URL ending in `/openai/v1` is trimmed, with a note).
- `model` is the **deployment name** you created in the Azure portal, not the model
  name (unless you named the deployment after the model).
- The key goes in the `api-key` header; another variable name: `-api-key-env NAME`.

### Self-test (no backend needed)

```sh
go -C scripts/live run . -self-test          # all four kinds
go -C scripts/live run . -self-test -kind azure-openai
```

Runs every check against the fake backend (`gateway/internal/fakebackend/cmd/fakebackend`)
standing in for each kind: vLLM and llama-server layout without a key, OpenAI
layout with `Authorization: Bearer`, Azure layout (`/openai/v1/`) with `api-key` — the
fake answers `401` to a missing or wrong credential and `404` to a wrong path, so a
config the kit generates wrongly fails here first.

The self-test ends with a two-backend run: two fakes serving one model, each capped
at one request (`-max-in-flight 1`), with the failover check — the runner stops the
second fake and starts it again on its address itself — and a third fake as the
embeddings server (with a bearer key, `-embeddings-api-key-env`), listing a path-style
model id (`/models/fake-embed-q8_0.gguf`) the way llama-server does.

Other flags: `-v` echoes the gateway log; `-keep` keeps the temporary directory;
`-request-timeout` (default 2m) bounds each request and is each of the backend's
upstream timeouts (first event, whole response, stall); `-kaiak PATH` runs a given binary; `-context-length` sets the declared
context length. `go -C scripts/live run . -h` lists them all.

## The checks

| Check | Proves |
|---|---|
| `auth-reject` | a request without a key gets `401` (the config loaded, the pipeline runs) |
| `models` | `/v1/models` lists exactly the configured public models for the key |
| `chat` | non-streamed chat reaches the backend and returns content under the **public** model name, with `usage` |
| `usage-log/chat` | its log line carries the backend's own token counts (`estimated=false`) and a cost |
| `chat-stream` | a stream without client `include_usage` relays chunks, every one named `live-chat`, ends with `[DONE]`, and **no usage chunk reaches the client** (the gateway asked for it and withholds it) |
| `usage-log/stream` | the gateway still counted that stream exactly: the backend honored the injected `stream_options.include_usage` |
| `chat-stream-usage` | with client `include_usage: true`, exactly one usage chunk arrives, last before `[DONE]` |
| `usage-log/stream-usg` | that stream counted exactly too |
| `embeddings` | embeddings reach the backend; the answer names `live-embed`, has a vector and `prompt_tokens`; with `-embeddings-base-url`, its log line names backend `live-embeddings` |
| `usage-log/embeddings` | embeddings usage counted exactly (input tokens, no output) |
| `output-ceiling` | a request for the model's whole context length in output tokens (`-context-length`; more is refused, `400 invalid_value`) is lowered to the ceiling: `completion_tokens` ≤ ceiling, `finish_reason: "length"`. vLLM and llama-server are asked through `max_tokens` (the gateway lowers the client's own key), OpenAI and Azure through `max_completion_tokens` (their reasoning models refuse `max_tokens`) |
| `rate-limit` | the second `live-rpm` request in a minute gets `429 rate_limit_exceeded` with `Retry-After` and `x-ratelimit-*-requests` headers — before reaching the backend |
| `metrics` | the admin `/metrics` shows the chat requests, their `tokens_out` and cost, and the rate-limit refusal |
| `spread` | two backends: six requests one after another are served by both (`backend` on each log line) — tied deployments take turns |
| `capacity` | two backends with `-max-in-flight N`: 2N+2 requests at once are all answered — those over the cap wait in the gateway's queue (the count queued is reported, not required) — and `kaiak_backend_max_in_flight` shows N for each |
| `failover` | two backends with `-check-failover`: see the failover procedure below |
| `gateway-exit` | (reported only on failure) SIGTERM drained the gateway and it exited 0 |

## Two vLLM processes, one model

Two copies of one model behind one public name: the gateway balances between them,
retries on the other copy, takes a stopped copy out of rotation and probes it back
in (`docs/specs/GATEWAY.md`, Routing and reliability).

**Run the two vLLM processes.** Same model, same `--served-model-name`, different
ports. On one GPU, give each a bit under half the memory; start the second once the
first has finished loading (each profiles free memory at start):

```sh
vllm serve Qwen/Qwen3-8B --served-model-name qwen3-8b --port 8001 --gpu-memory-utilization 0.45
vllm serve Qwen/Qwen3-8B --served-model-name qwen3-8b --port 8002 --gpu-memory-utilization 0.45
# two GPUs: CUDA_VISIBLE_DEVICES=0 for the first, =1 for the second, default memory
# a smaller KV cache if the second does not fit:  --max-model-len 8192
```

`curl http://gpu-host:8001/v1/models` and `…:8002/v1/models` both list `qwen3-8b`.

**Run the kit:**

```sh
go -C scripts/live run . -kind vllm \
  -base-url http://gpu-host:8001/v1 \
  -base-url-2 http://gpu-host:8002/v1 \
  -model qwen3-8b \
  -max-in-flight 4 \
  -check-failover
# Qwen3 thinking on:  -chat-defaults '{"chat_template_kwargs":{"enable_thinking":false}}'
```

- `-base-url-2` (env `LIVE_BASE_URL_2`): the second backend, same kind, serving the
  same `-model`. The generated config has backends `live` and `live-2`, and every
  chat model deploys on both (embeddings stay on `live`, or go to their own server
  with `-embeddings-base-url`). The circuit breaker is set
  to open after 2 consecutive failures and to probe every second, so the failover
  check runs in seconds (the defaults are 5 failures and a probe every 10 s).
- `-max-in-flight N`: each backend's `max_in_flight` — keep it at or below vLLM's
  `--max-num-seqs`. Adds the `capacity` check.
- `-check-failover`: adds the `failover` check, which needs you (below).
  `-failover-wait` (default 10m) bounds each of its waits — long enough for vLLM to
  load the model again.

Everything else runs as for one backend; `spread`, `capacity` and `failover` run
after `rate-limit`.

**The failover procedure.** When the kit prints

```
  >>>>  Stop the second backend (http://gpu-host:8002/v1) now, …
```

stop the vLLM process on port 8002 (Ctrl-C in its terminal, or `kill` its PID). The
kit sends a short chat every second the whole time; each must answer `200`. It then:

1. waits for `live-2`'s circuit to open — requests routed to it while it is down are
   retried on `live` (connect refused), and the second failure takes it out of
   rotation;
2. checks `kaiak_circuit_open{backend="live-2",…}` is 1 and that two more requests go
   straight to `live` in one attempt;
3. prints `>>>>  Start the second backend (…) again`: run the same `vllm serve` command
   again. vLLM opens its port only once the model is loaded; the gateway's probe
   (`GET /v1/models`, every second) then makes the circuit half-open;
4. sends up to four requests until one is served by `live-2` again — the trial,
   whose success closes the circuit.

`PASS failover` reports how many requests were served while `live-2` was down (and how
many of them were retried), how long the circuit took to open, and how long after the
restart request the probe closed it. What each step proves: no client saw the outage
(retries before the first byte), a dead copy stops costing a failed attempt per
request (circuit open), and a recovered copy comes back without a config change
(probe).

## Embeddings on a server of their own

Embedding models usually run apart from the chat model — a vLLM pooling instance or a
llama-server. `-embeddings-base-url` (env `LIVE_EMBEDDINGS_BASE_URL`) names that
server; the generated config gets a third backend, `live-embeddings`
(openai-compatible whatever `-kind` is), and `live-embed` deploys there.

- `-embeddings-model` is the model's name **on that server** — `curl
  <embeddings-base-url>/models` lists it. It may be path-style (llama-server lists
  the model file, e.g. `/models/qwen3-embedding-0.6b-q8_0.gguf`, unless started with
  `--alias`): backend-side names accept the backend's own naming; the public name
  stays `live-embed`.
- No key by default; `-embeddings-api-key-env NAME` if the server wants one.
- Without `-embeddings-base-url`, `-embeddings-model` is served by `-base-url`, the
  chat model's backend.

**A full run** — two vLLM chat copies (Two vLLM processes, one model) plus an
embeddings server, one kit run, all on one GPU host. Either a vLLM embeddings
instance (port 8003, served as `qwen3-embedding-0.6b`):

```sh
go -C scripts/live run . -kind vllm \
  -base-url http://gpu-host:8001/v1 \
  -base-url-2 http://gpu-host:8002/v1 \
  -model qwen3.5-2b \
  -max-in-flight 4 \
  -check-failover \
  -embeddings-base-url http://gpu-host:8003/v1 \
  -embeddings-model qwen3-embedding-0.6b
```

or the llama-server embeddings instance, in place of the last two lines:

```sh
  -embeddings-base-url http://embed-host:11435/v1 \
  -embeddings-model /models/qwen3-embedding-0.6b-q8_0.gguf
```

`PASS embeddings … served by live-embeddings` and `PASS usage-log/embeddings` (exact
input tokens from the server) are the checks that matter here; the chat and failover
checks run as without it.

## Reading failures

- A failed request prints the status, the body and the gateway's view from its log
  line: `error_code` and `upstream_error` (which names the backend address, never a
  credential):
  - `502 upstream_unavailable` — the gateway could not connect: wrong host or port,
    DNS, TLS, a proxy in the way (`HTTPS_PROXY` is honored).
  - `502 upstream_auth_failed` — the backend answered `401`/`403` to the gateway's
    credential: wrong key, wrong variable (`-api-key-env`), or for Azure a key from
    another resource.
  - `504 upstream_timeout` — nothing within `-request-timeout`; a cold vLLM loading
    the model can take longer — raise it.
  - `502 upstream_path_missing` — the backend answered `404` for a path it does not
    have: a wrong `-base-url` (vLLM or OpenAI without `/v1`; for Azure, a path after
    the resource endpoint). The gateway's log also warns `the backend has no models
    list at its base_url` at startup.
  - `502 upstream_model_missing` — the backend does not serve `-model` (for Azure,
    no deployment of that name).
  - `404` relayed from the backend — a `404` the gateway does not read as the
    deployment's: the backend's text says what is missing.
  - `400` relayed from the backend — a parameter the backend refuses; the body says
    which. `-chat-defaults` values are the first suspect.
- `estimated=true` on a `usage-log/*` check: the backend sent no usage, so the gateway
  fell back to its 4-bytes-per-token estimate. On `usage-log/stream` it means the
  backend ignored `stream_options.include_usage` (older vLLM, or a proxy in front of it
  that strips it).
- `only reasoning came back`: a thinking model spent the whole output limit on
  reasoning. Switch thinking off with `-chat-defaults` or raise `-max-output`.
- `output-ceiling` with `finish_reason` other than `length`: the model stopped on its
  own before 16 tokens (unlikely with the prompt used) — or the backend ignored the
  output limit; compare `completion_tokens` with the ceiling.
- Rerun with `-v -keep` to see the whole gateway log and keep the generated config.

## Azure: assumptions made without access

The azure-openai provider and this kit were written without an Azure subscription.
What was assumed, from Microsoft's documentation of the v1 API — check these first
when access arrives, in this order:

1. **The v1 API path**: requests go to
   `https://<resource>.openai.azure.com/openai/v1/chat/completions` (and
   `/embeddings`), with **no `api-version`** query parameter. If the resource answers
   `404` or asks for an `api-version`, the resource may predate the v1 API or need it
   enabled — the classic deployment-in-URL API is deliberately not supported
   (`docs/specs/GATEWAY.md`, Providers).
2. **`api-key` header auth** with the resource key (Keys and Endpoint in the portal).
   Microsoft Entra ID tokens are not supported (see `docs/BACKLOG.md`, Cloud workload
   identity).
3. **The deployment name is the model**: the request's `model` field carries the
   deployment name, as the OpenAI v1 surface expects. Check that `chat` passes with
   the deployment name and fails (`404`) with the underlying model name when the two
   differ.
4. **Responses name a dated model version** (e.g. `gpt-4.1-2025-04-14`), which the
   gateway replaces with the public name — `chat` and `chat-stream` check it.
5. **Usage fields** are OpenAI's: `usage.prompt_tokens`, `completion_tokens`,
   `prompt_tokens_details.cached_tokens`, `completion_tokens_details.reasoning_tokens`.
   Cached tokens only show on prompts over 1024 tokens, so the kit's short prompts
   report 0 — to check the cached path, send a long prompt twice and look at
   `tokens_cached` on the second log line.
6. **Streaming usage**: `stream_options.include_usage` is honored and yields the
   usage-only final chunk (`choices: []`). `usage-log/stream` and `chat-stream-usage`
   check it. Azure may also send chunks with empty `choices` for content-filter
   results (`prompt_filter_results`) before the first token; they carry no usage and
   are relayed as they are — watch `chat-stream-usage` for a miscount there.
7. **Error shapes**: Azure answers errors in the OpenAI shape (`{"error": {...}}`),
   relayed as they are; content-filter refusals are `400` with
   `code: content_filter`. A backend `429` (quota) is relayed with its `Retry-After`
   and counted as `upstream_rate_limited` in the metrics.
8. **Output-limit field**: `max_completion_tokens` is accepted by every current Azure
   chat model; reasoning deployments (o-series) refuse `max_tokens`.

## vLLM: worth checking by hand

The kit covers the main path; these vLLM behaviors matter to the gateway and are
worth a manual look on the real host (run with `-v -keep`, or send requests to a
gateway started from `examples/config.json`):

- **`stream_options.include_usage`** — supported by current vLLM releases;
  `usage-log/stream` passing proves it on this host.
- **`max_completion_tokens` honored** — the gateway writes its output-limit default
  there. Send a chat without any limit to `live-capped` and check
  `completion_tokens` ≤ the ceiling.
- **`chat_template_kwargs` passthrough** — `-chat-defaults
  '{"chat_template_kwargs":{"enable_thinking":false}}'` on a Qwen3 model should make
  answers come back without reasoning; with thinking on they carry it.
- **Reasoning parser fields** — with vLLM's `--reasoning-parser`, reasoning arrives in
  `reasoning_content` (older) or `reasoning` (newer) beside `content`, streamed as
  deltas. Check that `tokens_reasoning` stays 0 unless vLLM reports
  `completion_tokens_details.reasoning_tokens` (it usually does not — reasoning is
  still inside `tokens_out`, which is what is priced).
- **Unknown sampling parameters pass through** — `top_k`, `min_p` as defaults reach
  vLLM untouched (the backend log shows each request's parameters when request
  logging is enabled on the vLLM server).

## Manual: an OpenAI client library

The plan's manual check also streams through an OpenAI client library. Start a
gateway from your own config (see `README.md`), then:

```python
from openai import OpenAI
client = OpenAI(base_url="http://localhost:8080/v1", api_key="<your kaiak key>")
for chunk in client.chat.completions.create(model="qwen3-32b", stream=True,
        messages=[{"role": "user", "content": "Say hello."}]):
    print(chunk.choices[0].delta.content or "", end="", flush=True)
```

and check `curl -s localhost:9090/metrics | grep kaiak_usage` afterwards.
