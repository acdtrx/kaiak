# Live backend checks

> The runbook for `scripts/live/`: run the built `kaiak` against a real vLLM,
> llama-server, OpenAI, Azure OpenAI, Anthropic or Claude in Microsoft Foundry
> backend and check it end to end, through every client API the backend serves (chat
> completions, Anthropic Messages, OpenAI Responses, rerank, speech-to-text) — and,
> with vLLM-Omni servers beside it, speech, image generation and image edits. Opt-in —
> never needed for a green suite (`docs/TECH-STACK.md`, Testing). The kit's own
> self-test, against the fake backend, runs in `scripts/check-gateway.sh`. Someone
> running it for the kaiak team without knowing kaiak: start at **For a tester with
> access**.

## What it does

One command per backend kind. The runner:

1. builds `kaiak` from this checkout (or runs the one given with `-kaiak`);
2. generates a config for the backend — a fresh random client key and its hash, one
   backend of the type `-kind` names (two with `-base-url-2`: see Two vLLM
   processes, one model; one more with `-embeddings-base-url`: see Embeddings on a
   server of their own; one more with `-rerank-base-url`: see Reranker on a server of
   its own; one more with `-asr-base-url`: see Speech-to-text on a server of its own;
   one each with `-speech-base-url`, `-image-base-url` and `-edit-base-url`: see
   Speech, images and image edits on vLLM-Omni; one or two whose `base_url` is wrong
   on purpose: see A path the server does not have), the public models below, every
   model with its `endpoints` list, one group holding the key;
3. starts `kaiak` on free loopback ports with JSON logs;
4. runs the checks — through each client API the backend type serves (the table in
   `docs/specs/GATEWAY.md`, Providers → Endpoint support) — printing
   `PASS`/`FAIL`/`SKIP` per check and a summary;
5. sends SIGTERM, requires a clean drain and exit 0, and deletes everything it made
   (`-keep` keeps the config and the gateway log, and prints where).

Exit code 0 only when every check passed. Ctrl-C stops the gateway and cleans up.

Public models in the generated config, the chat ones all routed to the one backend
model:

| Name | Purpose |
|---|---|
| `live-chat` | chat, Messages and Responses checks; output limit `-max-output` (default 1024) |
| `live-capped` | output-limit ceiling check; ceiling `-ceiling` (default 16) |
| `live-rpm` | limit check, sent with a second key whose group (`live-metered`) allows 1 request per minute: a limit counts every request of its group |
| `live-embed` | embeddings, only with `-embeddings-model` — whatever the model's name on the backend (a path-style id too) |
| `live-rerank` | rerank, only with `-rerank-base-url` on `vllm` or `llama-server` |
| `live-asr` | speech-to-text, only with `-asr-base-url` on `vllm` or `llama-server`; `endpoints` `audio_transcriptions` |
| `live-tts` | speech and the speech batch, only with `-speech-base-url`; output limit 2048, ceiling 4096 (`max_new_tokens`); `endpoints` `audio_speech`, `audio_speech_batch` |
| `live-image` | image generation, only with `-image-base-url`; `endpoints` `images_generations` |
| `live-edit` | image edits, only with `-edit-base-url`; `endpoints` `images_edits` |
| `live-path-missing` | misconfigured on purpose (A path the server does not have): on `vllm` the chat model with `endpoints` `embeddings`, which its server lacks; on `llama-server` the chat model on a backend whose `base_url` has a wrong path |
| `live-omni-path-missing` | with a vLLM-Omni server: its model again, on a backend whose `base_url` has a wrong path |

Every model is priced at `-price-in` / `-price-out` USD per million tokens (default
1 / 2 — placeholders, only so the cost path is exercised; `0` and `0` leave them
unpriced and skip the cost checks); images and seconds of audio are never priced,
and the misconfigured models are not priced at all. With a reranker, the config also
sets `global.max_rerank_documents` to 4, for the `rerank-cap` check. With a vLLM-Omni
server it sets `global.max_speech_batch_items` to 3 and
`global.max_images_per_request` to 2, for the cap checks. With a vLLM-Omni or
speech-to-text server it adds a third key whose group (`live-media`) allows 1 image,
1 second of audio generated and 1 second of audio processed per minute, for the
media limit checks.

## For a tester with access

For someone with an account on one of the cloud APIs, running the kit for the kaiak
team. You need no kaiak background: the kit builds and runs the gateway itself.

**What you need**

- A Mac or Linux machine with Go (the version in `gateway/go.mod`) and git.
- This repository: `git clone <the URL you were given> && cd kaiak`, on the branch or
  tag you were asked to test.
- For each kind you can run:

  | Kind | What you need | Environment variable |
  |---|---|---|
  | `anthropic` | an Anthropic API key with access to a cheap model (Claude Haiku 4.5) | `ANTHROPIC_API_KEY` |
  | `azure-anthropic` | a Foundry resource name, a Claude deployment in it (Haiku 4.5 if you can) and its key | `ANTHROPIC_FOUNDRY_API_KEY` |
  | `openai` | an OpenAI API key with access to a cheap model (`gpt-4.1-mini` or similar) | `OPENAI_API_KEY` |
  | `azure-openai` | an Azure OpenAI resource endpoint, a chat deployment in it that supports the Responses API, and its key | `AZURE_OPENAI_API_KEY` |

**One command per kind**, from the repository root (`export` the key first):

```sh
go -C scripts/live run . -kind anthropic -model claude-haiku-4-5
go -C scripts/live run . -kind azure-anthropic -base-url https://<resource>.services.ai.azure.com -model <deployment>
go -C scripts/live run . -kind openai -model gpt-4.1-mini
go -C scripts/live run . -kind azure-openai -base-url https://<resource>.openai.azure.com -model <deployment>
```

Each run takes a minute or two, ends with `N passed, N failed, N skipped`, and costs
cents (Prerequisites). **Send back the whole output.** It holds no keys — the kit
passes only the variable's *name* to the gateway — and no prompt text; the
answers' first few words appear, from fixed questions about lighthouses. A `FAIL`
line says what was expected and what came back; nothing else is needed from you. If
a run cannot start (a `live:` error before the first check), the message says which
flag or variable is missing.

## Client checks

The kit checks the gateway's own behavior; these check that real clients work
through it. Start a gateway in front of a backend that serves the client's API —
any config with a key and a model on such a backend works (`README.md`;
`examples/config.json` has vLLM models — `examples/local-config.json`'s
`openai-compatible` backend serves neither Messages nor Responses),
or run a kit kind with `-keep` and reuse the config and key it generated — then run
one small task in each client and send back the gateway's log lines for it (they
hold no prompt or response content).

- **Claude Code** (Messages; a model on `vllm`, `llama-server`, `anthropic` or
  `azure-anthropic`):

  ```sh
  ANTHROPIC_BASE_URL=http://localhost:8080 ANTHROPIC_AUTH_TOKEN=<kaiak key> \
    ANTHROPIC_MODEL=<public model name> claude
  ```

  `ANTHROPIC_BASE_URL` has no `/v1` (the client adds it). Claude Code sends the key
  as `Authorization: Bearer`; the gateway takes `x-api-key` too.
- **Codex** (Responses; a model on `vllm`, `llama-server`, `openai` or
  `azure-openai`), in `~/.codex/config.toml`:

  ```toml
  model = "<public model name>"
  model_provider = "kaiak"
  web_search = "disabled"

  [model_providers.kaiak]
  name = "kaiak"
  base_url = "http://localhost:8080/v1"
  env_key = "KAIAK_API_KEY"
  wire_api = "responses"
  ```

  then `KAIAK_API_KEY=<kaiak key> codex`.

**Known client settings.** The gateway refuses a few things clients may send by
default; each refusal names the parameter, so the fix is one setting:

- **Codex may attach its hosted `web_search` tool**: its default is `"cached"`, and
  whether it sends the tool to a custom provider is not yet checked live (the client
  checks above say). If it does, kaiak answers `400 hosted_tool_unsupported` naming
  `web_search`; set `web_search = "disabled"`. Search runs on OpenAI's side, which
  kaiak does not serve.
- **Claude Code with a 1-hour prompt cache** on Anthropic or Foundry: a
  `cache_control` with `ttl: "1h"` is refused, `400 price_option_unsupported` — the
  1-hour cache write is priced above the standard one. Leave Claude Code's cache at
  its default (5 minutes). Self-hosted backends pass it through.
- **Claude Code's web search** (its WebSearch tool) sends Anthropic's server
  `web_search` tool and is refused the same way; every other Claude Code tool runs
  on your machine and works.

## Prerequisites

- Go (the version in `gateway/go.mod`) and this repository checked out; run from the
  repository root (the runner finds `gateway/` from the working directory).
- The backend reachable from this machine: `curl <base-url>/models` (vLLM,
  llama-server, OpenAI),
  `curl https://<resource>.openai.azure.com/openai/v1/models -H "api-key: $AZURE_OPENAI_API_KEY"`
  (Azure OpenAI) or
  `curl https://api.anthropic.com/v1/models -H "x-api-key: $ANTHROPIC_API_KEY" -H "anthropic-version: 2023-06-01"`
  (Anthropic) answers. Foundry has no models list; its first check is the kit's
  `messages`.
- The backend key in an environment variable (vLLM and llama-server only if they run
  with `--api-key`). The runner passes the variable's **name** into the config
  (`api_key_env`), never the value; the value never appears in its output or in the
  gateway log.
- Each run sends a few small requests per API the backend serves (one asks for a long
  story, cut at 16 tokens) and, with `-embeddings-model`, one embeddings request; a
  Messages backend also gets
  the cache check's two requests of about 8,000 input tokens each. With
  `-rerank-base-url`, a few small rerank requests and one carrying a 1 MB document,
  which the reranker server refuses. With `-asr-base-url`, about five transcriptions
  of 3.6 seconds of audio. With `-speech-base-url`, about ten clips of a sentence each;
  with `-image-base-url`, three 512×512 images at 10 steps; with `-edit-base-url`,
  one 512×512 edit at 10 steps. On a paid API a run costs cents (Claude Haiku 4.5 at
  $1 / $5 per million tokens: under $0.05).

## Commands

### vLLM

```sh
go -C scripts/live run . -kind vllm \
  -base-url http://vllm-host:8000/v1 \
  -model Qwen/Qwen3-32B \
  -embeddings-model BAAI/bge-m3          # optional: a pooling model on the same URL
# vLLM started with --api-key:  export VLLM_API_KEY=...  and add  -api-key-env VLLM_API_KEY
# thinking model, answers are all reasoning:  -chat-params '{"chat_template_kwargs":{"enable_thinking":false}}'
#                                        or:  -max-output 4096
```

- `base-url` is what an OpenAI client would use: it **includes `/v1`**.
- `model` is the name vLLM serves (`--served-model-name`, else the model path) —
  `curl http://vllm-host:8000/v1/models` lists it.
- No key by default: vLLM runs unauthenticated unless started with `--api-key`.
- Embeddings usually live on a separate vLLM instance (a pooling model): add
  `-embeddings-base-url` (see Embeddings on a server of their own).
- A reranker always does — one vLLM process serves one model: add
  `-rerank-base-url` and `-rerank-model` (see Reranker on a server of its own).
- So does a speech recognition model: add `-asr-base-url` and `-asr-model` (see
  Speech-to-text on a server of its own).
- Speech, image and image edit models run on vLLM-Omni, one model per server: add
  `-speech-base-url`, `-image-base-url`, `-edit-base-url` and their models (see
  Speech, images and image edits on vLLM-Omni), beside a run of any kind.
- `-embeddings-model` served by `-base-url` itself skips `path-missing`: that check
  needs the chat server without an embeddings route (A path the server does not
  have).

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
- So does a reranker (started with `--reranking`): add `-rerank-base-url` and
  `-rerank-model` (see Reranker on a server of its own).
- And a speech recognition model, with its audio projector (`--mmproj`): add
  `-asr-base-url` and `-asr-model` (see Speech-to-text on a server of its own).

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

### Anthropic

```sh
export ANTHROPIC_API_KEY=sk-ant-...
go -C scripts/live run . -kind anthropic \
  -model claude-haiku-4-5
```

- `base-url` defaults to `https://api.anthropic.com/v1`.
- The key goes in `x-api-key`; another variable name: `-api-key-env NAME`.
- Claude serves only Messages: the chat, Responses, embeddings and rerank checks do
  not run, and `endpoint-not-served` checks the gateway refuses them.

### Claude in Microsoft Foundry

```sh
export ANTHROPIC_FOUNDRY_API_KEY=...
go -C scripts/live run . -kind azure-anthropic \
  -base-url https://<resource>.services.ai.azure.com \
  -model <claude-deployment-name>
```

- `base-url` is the **resource endpoint** — no path. The gateway appends
  `/anthropic/v1/` (a URL ending in `/anthropic/v1` or `/anthropic` is trimmed, with
  a note).
- `model` is the **deployment name** (Foundry portal → Build → Models → your Claude
  deployment); by default it is the model ID, e.g. `claude-haiku-4-5`.
- The key (the deployment's Details tab) goes in the `api-key` header; another
  variable name: `-api-key-env NAME`. Entra ID tokens are not supported.

### Thinking models

A model that reasons first can spend the whole output limit on reasoning, and the
checks then say `only reasoning came back`. Raise `-max-output` (e.g. 4096), or
switch reasoning off per API — each flag adds its JSON object to every request of
that API the kit sends:

```sh
  -chat-params      '{"chat_template_kwargs":{"enable_thinking":false}}'   # vLLM, llama-server
  -messages-params  '{"chat_template_kwargs":{"enable_thinking":false}}'   # vLLM's Messages takes it too
  -responses-params '{"chat_template_kwargs":{"enable_thinking":false}}'   # and its Responses
  -responses-params '{"reasoning":{"effort":"low"}}'                       # OpenAI, Azure
```

### Self-test (no backend needed)

```sh
go -C scripts/live run . -self-test          # all six kinds
go -C scripts/live run . -self-test -kind azure-anthropic
```

Runs every check against the fake backend (`gateway/internal/fakebackend/cmd/fakebackend`)
standing in for each kind: vLLM and llama-server layout without a key, OpenAI
layout with `Authorization: Bearer`, Azure OpenAI layout (`/openai/v1/`) with
`api-key`, Anthropic layout with `x-api-key`, Foundry layout (`/anthropic/v1/`) with
`api-key` — the fake answers `401` to a missing or wrong credential and `404` to a
wrong path, so a config the kit generates wrongly fails here first. The fakes report
3 of every 7 prompt tokens read from the cache, so the cache check passes there.

For vLLM and llama-server more fakes play the servers beside the chat server, each
with a bearer key (`-rerank-api-key-env`, `-asr-api-key-env`):

- the reranker server: it answers rerank in that server's shape and refuses a pair
  over 32,768 words with that server's `400`;
- the speech-to-text server: it answers in that server's shape — vLLM's seconds, as
  if it heard every upload 2.5 s long (3 s rounded up, where the kit's 3.6-second
  uploads measure 4 s, so a record carrying the gateway's own measurement instead of
  the server's seconds fails), llama-server's tokens.

Each fake lacks endpoints the way its server does: vLLM's reranker has no chat route
and vLLM's chat model no rerank or embeddings route (`404 {"detail":"Not Found"}`),
so the vLLM run's embeddings model has a server of its own (`-embeddings-base-url`),
as on a real vLLM, one model per process; llama-server's chat server answers rerank
`501 not_supported_error`, as one started without `--reranking` does. A path a fake
has no route for (a wrong `base_url`) is answered as its server answers one: FastAPI's
`404` on vLLM and vLLM-Omni, llama-server's `File Not Found`.

The vLLM run has three more fakes playing vLLM-Omni, each with a bearer key
(`-speech-api-key-env`, `-image-api-key-env`, `-edit-api-key-env`): a speech server
whose model answers an image request as vLLM-Omni's speech models do (`503`, no
diffusion stage), an image server whose model answers speech with vLLM-Omni's `400`
(`TTS model did not produce audio output.`), should a request the models'
`endpoints` lists refuse reach them, and an image edit server comparing a request's
model with its own. The fake's audio is 2.5 s of silence per clip and per batch
item, so a record of two items counts 5 s — rounded once — where rounding each item
would count 6. The `image` and `edit` checks ask the fakes for two images of two
layers each, as a layered model answers, so `usage-log/image` and `usage-log/edit`
check a count of four, more than `n`.

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
| `models` | `/v1/models` lists exactly the configured public models for the key, each with the endpoints its `endpoints` list names |
| `chat` | non-streamed chat reaches the backend and returns content under the **public** model name, with `usage` |
| `usage-log/chat` | its log line carries the backend's own token counts (`kaiak.usage.estimated=false`) and a cost |
| `chat-stream` | a stream without client `include_usage` relays chunks, every one named `live-chat`, ends with `[DONE]`, and **no usage chunk reaches the client** (the gateway asked for it and withholds it) |
| `usage-log/stream` | the gateway still counted that stream exactly: the backend honored the injected `stream_options.include_usage` |
| `chat-stream-usage` | with client `include_usage: true`, exactly one usage chunk arrives, last before `[DONE]` |
| `usage-log/stream-usg` | that stream counted exactly too |
| `embeddings` | embeddings reach the backend; the answer names `live-embed`, has a vector and `prompt_tokens`; with `-embeddings-base-url`, its log line names backend `live-embeddings` |
| `usage-log/embeddings` | embeddings usage counted exactly (input tokens, no output) |
| `rerank` | vLLM and llama-server, with `-rerank-base-url`: a rerank request reaches the reranker server and returns a result per document — its `index` and `relevance_score` — under the **public** model name, with `usage` |
| `rerank-relevance` | in that answer, the document about a lighthouse keeper scores above the one about bread dough, sent first — coarse: no score thresholds, which vary by model and template |
| `usage-log/rerank` | its log line names the operation `rerank` and carries the server's own token count (not estimated, no output) and a cost |
| `rerank-cap` | one document more than `global.max_rerank_documents` (4 here) is refused, `400 invalid_value` on `documents`, before routing |
| `rerank-oversize` | a 1 MB document, longer than any reranker's context, is refused by the server with a `400` the gateway relays to the caller (vLLM: `BadRequestError`; llama-server: `exceed_context_size_error`) — not a `5xx`, which would count toward the circuit |
| `chat-to-reranker` | vLLM and llama-server, with a reranker: a chat request to `live-rerank`, whose `endpoints` list names `rerank` alone, is the gateway's own `400 endpoint_not_served` with the list's message, never sent — a reranker's vLLM has no chat route, and llama-server's would answer from a model that does not chat |
| `rerank-to-chat` | vLLM and llama-server, with or without a reranker: a rerank request to `live-chat`, whose list leaves `rerank` out, is the gateway's own `400 endpoint_not_served`, never sent — vLLM's chat model has no rerank route, and llama-server started without `--reranking` answers `501` |
| `messages` | a Messages request **without `max_tokens`** (the API requires it; the gateway sets the output-limit default) returns text under the public model name, with `usage` |
| `usage-log/messages` | its log line carries the backend's own token counts and a cost |
| `messages-stream` | a Messages stream opens with `message_start` naming `live-chat`, streams text deltas and ends with `message_stop` — no error event, no `[DONE]` |
| `usage-log/msg-stream` | that stream counted exactly |
| `messages-cache` | the same ~8,000-token system prompt, marked for the prompt cache, sent twice: the second request's log line reports input read from the cache. Anthropic caches it always (a miss fails); a self-hosted server reports cache reads only with its prefix cache on (a miss skips) |
| `messages-count` | `/v1/messages/count_tokens` answers `input_tokens` and settles no usage record |
| `messages-models` | `/v1/models` asked the way Anthropic's SDKs ask (`x-api-key`, `anthropic-version`) answers in Anthropic's shape, listing the models served through Messages |
| `messages-errors` | a wrong key (`401 authentication_error`) and an unknown model (`404 not_found_error`) come back in Anthropic's error shape, with kaiak's code |
| `messages-hosted-tool` | a tool the backend would run (`web_search_20250305`) is refused, `400 hosted_tool_unsupported`, before routing |
| `price-options` | Anthropic and Foundry: `speed: "fast"` is refused, `400 price_option_unsupported`, before it is sent (and on Anthropic the `messages` check passing shows the backend took `service_tier: "standard_only"`); self-hosted: it passes through, as they price nothing |
| `responses` | a Responses request asking for `store: true` and without `max_output_tokens` returns text under the public model name, with `usage`; OpenAI and Azure report back `store: false`, what the gateway sent (the self-hosted servers' answers carry no `store`) |
| `usage-log/responses` | its log line carries the backend's own token counts (reasoning too, where reported) and a cost |
| `responses-stream` | a Responses stream opens with `response.created`, streams text deltas and ends with `response.completed` naming `live-chat` — no `error`, `response.failed` or `response.incomplete` event |
| `usage-log/resp-stream` | that stream counted exactly |
| `responses-count` | where served (llama-server, OpenAI): `/v1/responses/input_tokens` answers `input_tokens` and settles no usage record |
| `responses-stateful` | a request naming `previous_response_id` is refused, `400 stateful_responses_unsupported`, before routing — the gateway serves Responses stateless |
| `responses-hosted-tool` | a hosted tool (`web_search`) is refused, `400 hosted_tool_unsupported`, before routing |
| `service-tier` | OpenAI and Azure: a request asking for `service_tier: "priority"` runs on `default` (skipped when the answer does not report its tier) |
| `speech-wav` | with `-speech-base-url`: the whole clip as WAV (`audio/wav`), decoded here — its rate, channels and length from the header and the data bytes — and none of vLLM-Omni's `x-vllm-omni-*` usage headers reaching the client |
| `usage-log/speech-wav` | its log line: operation `generate_content`, output type `speech`, the server's token counts (not estimated, read from those headers) and `kaiak.usage.audio.output.duration` equal to the length decoded here, rounded up to a whole second |
| `speech-pcm` | the whole clip as PCM with `sample_rate` 24000 (`audio/pcm`), its length from the byte count |
| `usage-log/speech-pcm` | as for WAV |
| `speech-raw` | the raw-audio stream (`stream_format: "audio"`) in WAV: a streamed WAV whose size fields are placeholders, measured from the bytes received |
| `usage-log/speech-raw` | its tokens **estimated and flagged** (vLLM-Omni reports none for this mode), `gen_ai.request.stream` true, the seconds as measured here |
| `speech-sse` | the SSE stream (`stream: true`) in WAV: `speech.audio.delta` events whose base64 audio, joined, is a WAV stream, then `speech.audio.done` with usage, last |
| `usage-log/speech-sse` | the tokens `done` reported, exactly, and the seconds as measured here |
| `speech-refused` | `mp3` (`400 response_format_unsupported`), a `ref_audio` URL (`400 external_reference_unsupported`) and `pcm` without `sample_rate` (`400 missing_required_parameter`) are refused before routing |
| `speech-batch` | a batch of a WAV and a PCM item: two successful results, each item's audio measured by its `media_type` |
| `usage-log/batch` | the items' usage summed, exactly, and their lengths summed before the one rounding |
| `speech-batch-cap` | one item more than `global.max_speech_batch_items` (3 here) is refused, `400 invalid_value` on `items`, before routing |
| `image` | with `-image-base-url`: one 512×512 image, `b64_json` decoding to a PNG, JPEG or WebP |
| `usage-log/image` | `kaiak.usage.images` equal to the answer's `data[]` count, output type `image`, the input tokens estimated and flagged (vLLM-Omni reports no image usage) |
| `image-cap` | `n` one above `global.max_images_per_request` (2 here) is refused, `400 invalid_value` on `n`, before routing |
| `image-refused` | `response_format` `url` and `file` (`400 response_format_unsupported`), a `lora` and `extra_params` (`400 external_reference_unsupported`) are refused before routing |
| `chat-to-image` | a chat request to `live-image` is the gateway's own `400 endpoint_not_served`: `vllm-omni` serves the media endpoints alone |
| `edit` | with `-edit-base-url`: a 512×512 PNG the kit draws, uploaded with a prompt as OpenAI's multipart edit form; `b64_json` data, every image decoding to a PNG, JPEG or WebP |
| `usage-log/edit` | `kaiak.usage.images` equal to the answer's `data[]` count, output type `image`, the input tokens estimated and flagged (vLLM-Omni reports no image usage) |
| `edit-refused` | `stream=true` (`400 invalid_value`), an input image by `https` URL and a `lora` (`400 external_reference_unsupported`) are refused before routing |
| `generation-to-edit` | an image generation to `live-edit`, whose list names edits alone, is the gateway's own `400 endpoint_not_served`, never sent — where vLLM-Omni answers a generation on an edit model `500`, a backend failure |
| `transcribe-wav`, `transcribe-flac`, `transcribe-mp3` | with `-asr-base-url`: 3.6 s of audio the kit writes — a tone as 16-bit 16 kHz WAV and as FLAC (verbatim frames with their CRCs, the MD5 in its header), silence as MPEG-1 Layer III (one unbroken run of frames, nothing else) — each uploaded as OpenAI's transcription form; a text comes back, and on vLLM `usage.seconds` 4 — the upload's length rounded up, as vLLM decoded it — on llama-server its token usage |
| `usage-log/transcribe-*` | operation `generate_content`, output type `text`, `kaiak.usage.audio.input.duration`: on vLLM the seconds the server reported, the tokens estimated and flagged (vLLM reports none); on llama-server 4, the gateway's own measurement of the upload, and the server's tokens exactly |
| `transcribe-verbose` | vLLM: `verbose_json` answers the decoded `duration` (3.6 s) and no usage; its log line counts that duration rounded up. Skipped only on vLLM's refusal `Currently do not support verbose_json for <model>` (`400`, relayed): only models with segment timestamps (Whisper, FunASR, FireRedASR2) answer it; any other answer fails the check |
| `transcribe-refused` | `stream=true` (`400 invalid_value`), and on llama-server an Ogg upload the gateway cannot measure (`400 audio_format_unsupported` on `file`), are refused before routing |
| `audio-limit` | with the `live-media` key: a first clip is served and fills the 1-second window — audio generated settles after the request — and the next is refused `429 audio_seconds` with `Retry-After`, no `x-ratelimit-*` header, its log line naming `audio_seconds_out_per_minute` |
| `image-limit` | the same for images: a request reserves its `n`, so the second image in the minute is refused `429 images`, naming `images_per_minute` |
| `transcribe-limit` | the same for audio processed: it settles after the request, so a first transcription fills the 1-second window and the next is refused `429 audio_seconds`, naming `audio_seconds_in_per_minute` |
| `image-to-speech` | an image request to `live-tts`, whose `endpoints` list leaves image generation out, is the gateway's own `400 endpoint_not_served`, never sent to the server |
| `speech-to-image` | a speech request to `live-image`, whose list leaves speech out, is the gateway's own `400 endpoint_not_served`, never sent — where vLLM-Omni would answer `400` only after a whole image generation |
| `media-metrics` | `/metrics` counts the seconds of audio generated (`kaiak_usage_audio_output_duration_seconds_total`, `gen_ai_output_type="speech"`) and processed (`kaiak_usage_audio_input_duration_seconds_total`, `gen_ai_output_type="text"`), the images and the edited ones (`kaiak_usage_images_total`, `gen_ai_output_type="image"`) and each media limit's refusal (`kaiak_limit_rejections_total` by `kaiak_limit_type`) |
| `endpoint-not-served` | every API the chat model does not serve — chat completions on Claude, Messages on OpenAI, `responses/input_tokens` on vLLM and Azure, rerank on the cloud APIs, speech, the speech batch, image generation and image edits on every kind, speech-to-text on every kind (on vLLM and llama-server the chat model's list leaves it out), … — is refused `400 endpoint_not_served`, in that API's error shape; the upload endpoints are sent uploads, since any other body is refused before the model is known |
| `path-missing` | vLLM: `live-path-missing`'s embeddings requests reach its chat server, which has no embeddings route; llama-server: its chat requests reach a `base_url` with a wrong path (A path the server does not have). Each answers `502 upstream_path_missing` with the gateway's message, in one attempt — not retried, with two backends too — and a record of zero usage; after one request more than the circuit's threshold, no circuit of the model's deployments opened (`kaiak_circuit_state` closed), the attempts are counted `path_missing`, and for a wrong `base_url` the config-apply warning `the backend has no models list at its base_url` named the backend |
| `omni-path-missing` | with a vLLM-Omni server: the same for its model behind a `base_url` with a wrong path — the speech server's, else the image or the edit server's |
| `output-ceiling` | a request for the model's whole context length in output tokens (`-context-length`; more is refused, `400 invalid_value`) is lowered to the ceiling: `completion_tokens` ≤ ceiling, `finish_reason: "length"`. vLLM and llama-server are asked through `max_tokens` (the gateway lowers the client's own key), OpenAI and Azure through `max_completion_tokens` (their reasoning models refuse `max_tokens`); Anthropic and Foundry through Messages' `max_tokens`, ending `stop_reason: "max_tokens"` |
| `rate-limit` | the second `live-rpm` request in a minute (with the metered key) gets `429 rate_limit_exceeded` with `Retry-After` and `x-ratelimit-*-requests` headers — before reaching the backend (through Messages on Anthropic and Foundry, in Anthropic's shape: `rate_limit_error`) |
| `metrics` | the admin `/metrics` shows the `live-chat` usage records, their output tokens (`gen_ai_client_inference_usage_output_tokens_total`) and cost, the rate-limit refusal, successful upstream attempts with their durations, no backend label on a usage series, and a request on each of the `messages` and `responses` routes the backend serves (`http_server_request_duration_seconds`, by `http_route`) |
| `spread` | two backends: six requests one after another are served by both (`kaiak.backend.id` on each log line) — tied deployments take turns |
| `capacity` | two backends with `-max-in-flight N`: 2N+2 requests at once are all answered — those over the cap wait in the gateway's queue (the count queued is reported, not required) — and `kaiak_backend_active_requests_limit` shows N for each |
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
# Qwen3 thinking on:  -chat-params '{"chat_template_kwargs":{"enable_thinking":false}}'
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
2. checks `kaiak_circuit_state{kaiak_backend_id="live-2",…,kaiak_circuit_state="open"}`
   is 1 and that two more requests go straight to `live` in one attempt;
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

## Reranker on a server of its own

A reranker runs apart from the chat model: a vLLM process serves one model, and
llama-server serves rerank only when started for it. `-rerank-base-url` (env
`LIVE_RERANK_BASE_URL`) names that server and `-rerank-model` (env
`LIVE_RERANK_MODEL`) the model's name on it; the generated config gets one more
backend, `live-reranker`, of the run's `-kind`, and `live-rerank` deploys there. Only
`vllm` and `llama-server` serve rerank: another kind given the flags says `SKIP
rerank`.

- `-rerank-model` is the name `curl <rerank-base-url>/models` lists.
- No key by default; `-rerank-api-key-env NAME` if the server wants one.

**Start the reranker** — Qwen3-Reranker; `docs/DEPLOYMENT.md` → Rerankers says why
each flag is there. On vLLM (port 8004, sharing the GPU with the chat model):

```sh
# the score template, from the tag of the vLLM you run (here v0.31.0)
curl -LO https://raw.githubusercontent.com/vllm-project/vllm/v0.31.0/examples/pooling/score/template/qwen3_reranker.jinja
vllm serve Qwen/Qwen3-Reranker-8B --port 8004 --gpu-memory-utilization 0.25 \
  --runner pooling \
  --hf_overrides '{"architectures": ["Qwen3ForSequenceClassification"],"classifier_from_token": ["no", "yes"],"is_original_qwen3_reranker": true}' \
  --chat-template qwen3_reranker.jinja
# -rerank-model Qwen/Qwen3-Reranker-8B
```

On llama-server (port 8005), from a GGUF made by llama.cpp's converter, which writes
the model's rerank prompt into the file:

```sh
python convert_hf_to_gguf.py <dir>/Qwen3-Reranker-0.6B --outfile qwen3-reranker-0.6b-f16.gguf
llama-server -m qwen3-reranker-0.6b-f16.gguf --port 8005 --embedding --pooling rank --reranking -c 32768 -np 4
# -rerank-model: the id curl …:8005/v1/models lists — the model file's path unless started with --alias
```

**Run the kit** with the chat server and the reranker:

```sh
go -C scripts/live run . -kind vllm \
  -base-url http://gpu-host:8001/v1 \
  -model qwen3.5-2b \
  -rerank-base-url http://gpu-host:8004/v1 \
  -rerank-model Qwen/Qwen3-Reranker-8B
```

For llama-server: `-kind llama-server`, its chat server's `-base-url` and `-model`,
and the reranker's URL and model id.

`PASS rerank`, `rerank-relevance`, `usage-log/rerank` (exact input tokens from the
server) and `rerank-oversize` (the server refuses a pair longer than its context with a
`400`) are the checks that matter for the reranker. `chat-to-reranker` and
`rerank-to-chat` check the other side: each model's `endpoints` list refuses the
other's requests before any server.

## Speech-to-text on a server of its own

A speech recognition model runs apart from the chat model: a vLLM process serves one
model, and llama-server hears audio only with the model's audio projector.
`-asr-base-url` (env `LIVE_ASR_BASE_URL`) names that server and `-asr-model` (env
`LIVE_ASR_MODEL`) the model's name on it; the generated config gets one more backend,
`live-transcriber`, of the run's `-kind`, and `live-asr` deploys there. Only `vllm` and
`llama-server` serve speech-to-text: another kind given the flags says `SKIP
transcribe`.

- `-asr-model` is the name `curl <asr-base-url>/models` lists.
- No key by default; `-asr-api-key-env NAME` if the server wants one.
- The kit writes its own audio, 3.6 seconds long: a 440 Hz tone as 16-bit 16 kHz
  mono WAV and as FLAC, and silence as MP3 (32 kHz frames of zeros, which every
  decoder plays as silence: one unbroken run of frames and nothing else, which the
  gateway measures for llama-server). Any text comes back; the checks are about the
  seconds counted, not the transcript.

**Start the server.** On vLLM (port 8006), any model it transcribes with — Whisper,
Voxtral, Qwen3-ASR, Granite Speech, …; Whisper also answers `verbose_json`:

```sh
vllm serve openai/whisper-large-v3-turbo --port 8006 --gpu-memory-utilization 0.2
# -asr-model openai/whisper-large-v3-turbo
```

On llama-server (port 8007), a GGUF of a model with an audio encoder (Voxtral,
Qwen2.5-Omni, Qwen3-ASR, Ultravox, …) and its audio projector:

```sh
llama-server -m <model>.gguf --mmproj <audio-projector>.gguf --port 8007
# -asr-model: the id curl …:8007/v1/models lists — the model file's path unless started with --alias
```

**Run the kit** with the chat server and the speech-to-text server:

```sh
go -C scripts/live run . -kind vllm \
  -base-url http://gpu-host:8001/v1 \
  -model qwen3.5-2b \
  -asr-base-url http://gpu-host:8006/v1 \
  -asr-model openai/whisper-large-v3-turbo
```

For llama-server: `-kind llama-server`, its chat server's `-base-url` and `-model`,
and the speech-to-text server's URL and model id.

The checks that matter: `transcribe-wav`, `-flac`, `-mp3` and their `usage-log/*`
lines. On vLLM the record's seconds must be the ones the server reported, and the
check holds those to the upload's length: vLLM decoded the file as the kit wrote it
(the gateway's own measurement, the fallback, gives the same 4 s here, so the
self-test is what shows the record follows the server's count). On llama-server
they are the gateway's own measurement of the upload, held to the same length — run
them on every new speech recognition model and every llama.cpp upgrade.
`transcribe-refused` shows an upload llama-server cannot decode refused before it is
sent.

## Speech, images and image edits on vLLM-Omni

Speech, image generation and image edits run on vLLM-Omni, one model per server, and
the gateway serves them on `vllm-omni` backends alone. `-speech-base-url` (env
`LIVE_SPEECH_BASE_URL`) and `-speech-model` (env `LIVE_SPEECH_MODEL`) name a speech
server and its model, `-image-base-url` (env `LIVE_IMAGE_BASE_URL`) and `-image-model`
(env `LIVE_IMAGE_MODEL`) an image server and its model, `-edit-base-url` (env
`LIVE_EDIT_BASE_URL`) and `-edit-model` (env `LIVE_EDIT_MODEL`) an image edit server
and its model; the generated config gets a `vllm-omni` backend for each,
`live-speech`, `live-images` and `live-edits`, with `live-tts`, `live-image` and
`live-edit` deployed there. They join a run of any kind — the chat checks run on the
kind's server as without them — and any one can come alone.

- `-speech-model`, `-image-model` and `-edit-model` are the names `curl
  <base-url>/models` lists: the model path unless the server was started with
  `--served-model-name` — its first one, when it has several: image generation and
  edits answer any other `400 Model mismatch`.
- No key by default; `-speech-api-key-env NAME`, `-image-api-key-env NAME` and
  `-edit-api-key-env NAME` if the servers want one.
- The kit sends no `voice`: the model's default speaks (Qwen3-TTS CustomVoice:
  `vivian`). The PCM checks ask for `sample_rate` 24000, which only Qwen3-TTS and
  Breeze-TTS-2 take.

**Start the servers** — vLLM-Omni in a virtual environment of its own, with the vLLM
release it is built for; `docs/DEPLOYMENT.md` → Speech and images says why:

```sh
uv venv --python 3.12 --seed ~/vllm-omni && source ~/vllm-omni/bin/activate
uv pip install vllm==0.30.0 --torch-backend=auto
uv pip install vllm-omni==0.30.0

# speech: Qwen3-TTS CustomVoice (port 8091)
vllm serve Qwen/Qwen3-TTS-12Hz-1.7B-CustomVoice --omni --port 8091 \
  --trust-remote-code --enforce-eager
# sharing a GPU: its two stages take 0.3 of GPU 0's memory each; lower them with
#   --stage-overrides '{"0":{"gpu_memory_utilization":0.1},"1":{"gpu_memory_utilization":0.1}}'
# images: Qwen-Image (port 8092)
vllm serve Qwen/Qwen-Image --omni --port 8092
# image edits: Qwen-Image-Edit-2511 (port 8093), about 60 GB with its text encoder
vllm serve Qwen/Qwen-Image-Edit-2511 --omni --port 8093
```

**Run the kit** with the chat server and all three:

```sh
go -C scripts/live run . -kind vllm \
  -base-url http://gpu-host:8001/v1 \
  -model qwen3.5-2b \
  -speech-base-url http://gpu-host:8091/v1 \
  -speech-model Qwen/Qwen3-TTS-12Hz-1.7B-CustomVoice \
  -image-base-url http://gpu-host:8092/v1 \
  -image-model Qwen/Qwen-Image \
  -edit-base-url http://gpu-host:8093/v1 \
  -edit-model Qwen/Qwen-Image-Edit-2511
```

Each image check asks for one 512×512 image at 10 steps, the edit check for one edit
of a 512×512 PNG the kit draws, at 10 steps; a slow GPU, or a server still loading,
may need a longer `-request-timeout` (default 2m).

The checks that matter for the servers:

- `speech-wav`, `speech-pcm`, `speech-raw`, `speech-sse`, `speech-batch` and their
  `usage-log/*` lines: each mode answers audio, the tokens are the server's where it
  reports them (estimated on the raw-audio stream, which reports none), and the
  seconds the gateway counted equal the audio decoded on this machine, rounded up —
  run them on every vLLM-Omni upgrade and every new speech model: they are the check
  that the gateway measures that model's audio right.
- `image` and `usage-log/image`: an image answered and counted.
- `edit` and `usage-log/edit`: an edit answered and counted as images.
- `audio-limit` and `image-limit`: the media limits refuse.
- `image-to-speech`, `speech-to-image` and `generation-to-edit`: each model's
  `endpoints` list keeps the other kinds of request off its server. What vLLM-Omni
  itself answers — `503` for an image request to a speech model, `400` for speech to
  an image model after a whole image generation (about a minute of the GPU), read on
  the live run of 2026-10-09 (`GATEWAY.md` → Model metadata: Endpoints per model),
  and `500` for a generation to an edit model, read from its source — the kit sends
  to no server.
- `omni-path-missing`: the server's answer to a path it does not have, read as
  `upstream_path_missing` (A path the server does not have).

## A path the server does not have

Every model lists its endpoints, so a client's request for an endpoint the model's
server lacks stops at the gateway (`endpoint_not_served`) and never shows how the
gateway reads a real server's answer to a path it does not have. The kit makes that
happen on purpose, with a model misconfigured in one of the two ways the answer
means (`GATEWAY.md` → Providers: A path the server does not have):

- **`vllm`: a list naming an endpoint the server does not serve.** `live-path-missing`
  is the chat model again, on the same deployments, with `endpoints` `embeddings`:
  vLLM creates no embeddings route for a model that generates, so it answers its
  route-missing `404 {"detail":"Not Found"}`. This needs the `-base-url` server
  without an embeddings model: with `-embeddings-model` and no
  `-embeddings-base-url` the check is skipped.
- **`llama-server`, and every vLLM-Omni server given: a `base_url` with a wrong
  path.** These servers register every route whatever their model, so only a wrong
  path gets the answer: `live-path-missing` (llama-server) and
  `live-omni-path-missing` (the speech server's model, else the image or edit
  server's) deploy on a second backend at the same server whose `base_url` ends in
  `/wrong-path` — llama-server answers `404 File Not Found`, vLLM-Omni FastAPI's `404`.

Each sends one request more than the circuit's failure threshold (5; 2 with
`-base-url-2`) times the model's deployments, and passes when every one answers `502
upstream_path_missing` with the gateway's message, in one attempt (not retried, with
two backends too), with a record of zero usage; when no circuit of those deployments
opened and `/metrics` counts the attempts `path_missing`; and, for a wrong
`base_url`, when the gateway warned at config apply that the backend has no models
list there — the operator's sign of it. On vLLM the deployments are `live-chat`'s, so
the chat checks that follow (`output-ceiling`, `rate-limit`, with two backends
`spread` and `failover`) show them still serving. The cloud kinds are not checked
this way: their answers to a wrong path are taken from their documentation.

## Reading failures

- A failed request prints the status, the body and the gateway's view from its log
  line: `error.type` and `kaiak.upstream.error.message` (which names the backend
  address, never a credential):
  - `502 upstream_unavailable` — the gateway could not connect: wrong host or port,
    DNS, TLS, a proxy in the way (`HTTPS_PROXY` is honored).
  - `502 upstream_auth_failed` — the backend answered `401`/`403` to the gateway's
    credential: wrong key, wrong variable (`-api-key-env`), or for Azure a key from
    another resource.
  - `504 upstream_timeout` — nothing within `-request-timeout`; a cold vLLM loading
    the model can take longer — raise it.
  - `502 upstream_path_missing` — the backend answered the way its server answers a
    path it does not have, not retried: a wrong `-base-url` (OpenAI without `/v1`; for
    Azure, a path after the resource endpoint), and the gateway's log warns `the
    backend has no models list at its base_url` at startup; or, on vLLM, an endpoint
    the server does not serve — the model it loaded leaves it out (a chat model has
    no rerank or embeddings route, a reranker no chat route, a model that does not
    transcribe no speech-to-text route: check that `-base-url`, `-rerank-base-url`
    and `-asr-base-url` are not swapped), or its version predates it (vLLM before its
    Messages or Responses support). Expected on `path-missing` and
    `omni-path-missing` alone.
  - `502 upstream_model_missing` — the backend does not serve `-model` (for Azure,
    no deployment of that name).
  - `404` relayed from the backend — a `404` the gateway does not read as the
    deployment's: the backend's text says what is missing.
  - `400` relayed from the backend — a parameter the backend refuses; the body says
    which. `-chat-params` values are the first suspect.
- `400 endpoint_not_served` on a check that should have run: the backend type does
  not serve that API (Providers → Endpoint support) — the kit runs each API's
  checks only on the kinds that serve it, so this means the kit and the gateway
  disagree on the table.
- `rerank-relevance`: the server scored the irrelevant document higher — on vLLM the
  score template (`--chat-template`) or the `--hf_overrides` is missing, on
  llama-server the GGUF carries no rerank prompt (`docs/DEPLOYMENT.md` → Rerankers).
- `rerank-oversize` with status `500` (llama-server): the server could not fit the
  pair in one batch and failed instead of refusing it — a build before b11223, or a
  reranker that is not a causal model; `docs/DEPLOYMENT.md` → Rerankers.
- `speech-pcm` with a `400` relayed from the server: the model takes no
  `sample_rate` — only Qwen3-TTS and Breeze-TTS-2 do, and PCM through kaiak needs
  one. Use WAV with that model (`docs/DEPLOYMENT.md` → Speech and images).
- `usage-log/speech-*` or `usage-log/batch` counting other seconds than measured
  here: the gateway read the audio otherwise than this machine did — for example
  PCM that is not 16-bit mono at the request's rate. Rerun with `-v -keep` and keep
  the log; that model's audio is not measured right.
- `usage-log/speech-wav` or `-pcm` with `kaiak.usage.estimated=true`: the answer
  carried no `x-vllm-omni-*` usage headers (a speech server in diffusion mode sends
  none), or audio the gateway could not measure (its seconds 0).
- `image` or `edit` with `502 upstream_model_missing`: `-image-model` or
  `-edit-model` is not that server's first served name — vLLM-Omni compares the
  request's model with it and answers `400 Model mismatch`.
- `chat-to-reranker`, `rerank-to-chat`, `chat-to-image`, `image-to-speech`,
  `speech-to-image` or `generation-to-edit` with the request sent to a server: the
  model's `endpoints` list did not reach the gateway — the kit and the gateway
  disagree on the config (`docs/specs/GATEWAY.md`, Model metadata → Endpoints per
  model).
- `transcribe-*` with `502 upstream_path_missing` (vLLM) or `501` (llama-server): the
  `-asr-model` server's model does not transcribe — vLLM mounts no speech-to-text
  route for it; llama-server has no audio encoder for it (start it with the model's
  `--mmproj`).
- `transcribe-*` on vLLM with other seconds than 4: vLLM decoded the upload otherwise
  than the kit wrote it — the record carries vLLM's count, so that is what clients
  are billed. Rerun with `-v -keep` and keep the output.
- `usage-log/transcribe-*` on vLLM with other seconds than the server reported: the
  gateway did not take the answer's seconds — a metering bug. Rerun with `-v -keep`
  and keep the log.
- `usage-log/transcribe-*` on llama-server with other seconds than 4, or
  `kaiak.usage.estimated=true`: the gateway measured the upload otherwise than the
  kit wrote it — a measuring bug. Rerun with `-v -keep` and keep the log.
- `transcribe-verbose` skipped: the model has no segment timestamps — expected for
  every model but Whisper, FunASR and FireRedASR2. Failing with a `400`: vLLM refused
  the request for another reason — its message is in the output.
- `path-missing` answering `200` (vLLM): the `-base-url` server serves embeddings for
  the chat model — an embedding-capable model, or a proxy routing by model in front
  of it; point the run at a plain chat server.
- `path-missing` or `omni-path-missing` answering another status or code: the
  server's answer to a wrong path is not the one the gateway knows for that server
  (a proxy in front of it, or a new version's `404` body) — send the run's output:
  the gateway's reading for that server needs a look. A retried request or an opened
  circuit there is the gateway's own regression.
- `400 hosted_tool_unsupported`, `stateful_responses_unsupported`,
  `price_option_unsupported` on a check other than the one testing it: a
  `-messages-params` or `-responses-params` value asks for something the gateway
  refuses — the message names the parameter.
- `kaiak.usage.estimated=true` on a `usage-log/*` check: the backend sent no usage, so the gateway
  fell back to its 4-bytes-per-token estimate. On `usage-log/stream` it means the
  backend ignored `stream_options.include_usage` (older vLLM, or a proxy in front of it
  that strips it).
- `only reasoning came back`: a thinking model spent the whole output limit on
  reasoning. Switch thinking off with `-chat-params`, `-messages-params` or
  `-responses-params`, or raise `-max-output` (Thinking models).
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
   `prompt_tokens_details.cached_tokens`, `prompt_tokens_details.cache_write_tokens`,
   `completion_tokens_details.reasoning_tokens`. Cache reads and writes only show on
   prompts over 1024 tokens, so the kit's short prompts report 0 — to check the cache
   paths, send a fresh long prompt twice: the first log line carries
   `gen_ai.usage.cache_write.input_tokens` (gpt-5.6 and later) and a cost at the
   write price, the second `gen_ai.usage.cache_read.input_tokens`.
6. **Streaming usage**: `stream_options.include_usage` is honored and yields the
   usage-only final chunk (`choices: []`). `usage-log/stream` and `chat-stream-usage`
   check it. Azure may also send chunks with empty `choices` for content-filter
   results (`prompt_filter_results`) before the first token; they carry no usage and
   are relayed as they are — watch `chat-stream-usage` for a miscount there.
7. **Error shapes**: Azure answers errors in the OpenAI shape (`{"error": {...}}`),
   relayed as they are; content-filter refusals are `400` with
   `code: content_filter`. A backend `429` (quota) is relayed with its `Retry-After`
   and counted in
   `kaiak_errors_total{kaiak_error_class="upstream_rate_limited"}`.
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
- **`chat_template_kwargs` passthrough** — `-chat-params
  '{"chat_template_kwargs":{"enable_thinking":false}}'` on a Qwen3 model should make
  answers come back without reasoning; with thinking on they carry it.
- **Reasoning parser fields** — with vLLM's `--reasoning-parser`, reasoning arrives in
  `reasoning_content` (older) or `reasoning` (newer) beside `content`, streamed as
  deltas. vLLM 0.30.0 reports `completion_tokens_details.reasoning_tokens` on chat
  and `output_tokens_details.reasoning_tokens` on Responses, and the log line's
  `gen_ai.usage.reasoning.output_tokens` (the record's `tokens_reasoning`) shows
  them; Messages reports no reasoning count, so it stays 0 there. Reasoning is
  always inside `gen_ai.usage.output_tokens` (the record's `tokens_out`), which is
  what is priced.
- **Unknown sampling parameters pass through** — `top_k`, `min_p` sent with
  `-chat-params` reach vLLM untouched (the backend log shows each request's parameters when request
  logging is enabled on the vLLM server).

## Anthropic and Foundry: assumptions made without access

The `anthropic` and `azure-anthropic` providers and these kinds were written from
Anthropic's and Microsoft's documentation, without an account. What was assumed —
check these first when a tester runs them:

1. **Paths**: Anthropic `https://api.anthropic.com/v1/messages` (and
   `/messages/count_tokens`, `/models?limit=1000`); Foundry
   `https://<resource>.services.ai.azure.com/anthropic/v1/messages` and
   `/messages/count_tokens`, with no models list. `messages` and `messages-count`
   passing proves them.
2. **Auth**: `x-api-key` (Anthropic) and `api-key` (Foundry) with
   `anthropic-version: 2023-06-01`, which the gateway sends itself.
3. **`service_tier: "standard_only"`** on every Anthropic request is accepted
   (`messages` passing); Foundry gets none (it has no Priority Tier).
4. **Usage**: `input_tokens` excludes cache reads and writes, reported as
   `cache_read_input_tokens` and `cache_creation_input_tokens`; streams report them
   in `message_start` and the output in `message_delta`. `usage-log/messages` and
   `messages-cache` read them.
5. **Errors**: `{"type": "error", "error": {"type", "message"}}`; `529
   overloaded_error` counts as a `5xx`; a missing model is a `404 not_found_error`
   naming it (`upstream_model_missing` if the deployment's model is wrong — check
   by running once with a wrong `-model`).
6. **Prompt cache minimum**: the cache check's ~8,000-token prompt is above every
   current model's minimum cacheable length (up to 4,096 tokens).

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

and check `curl -s localhost:9090/metrics | grep -E 'kaiak_usage|gen_ai_client_inference_usage'`
afterwards.
