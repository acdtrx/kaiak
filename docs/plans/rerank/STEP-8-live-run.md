# Step 8 — live run

**Status:** done — reviewed and committed 2026-10-09
gateway or the kit reads; the DGX is restored.

## Intent

The real servers confirm what steps 1–6 built from their sources:
- rerank answers and usage;
- the wrong-endpoint answers;
- the oversize-document refusal;
- the `DEPLOYMENT.md` setup.

Then the branch merges.

## Files likely touched

- This file: the commands run, server versions, model files and the kit's output (no
  keys, no prompt text).
- Fixes for anything the run shows, each with its test, and the spec where a rule
  changes.
- `docs/plans/rerank/OVERVIEW.md`: verification status.

## Decisions made during planning

- **Models:**
  - Qwen3-Reranker-8B on vLLM, with the `DEPLOYMENT.md` flags;
  - a Qwen3-Reranker GGUF on llama-server: converted with llama.cpp's converter,
    which knows the model, or a published conversion.
- **Each server runs with the documented settings.** llama-server runs a build at or
  after b11223, where an oversize pair should answer `400` whatever `-ub` is
  (decision 15).
- **The user's DGX model is restored afterwards** (kaiak working style).

## Acceptance criteria

- The kit passes on `-kind vllm` and on `-kind llama-server`, each with the reranker
  and the wrong-endpoint checks.
- Each server's real answer matches the fake backend's shape. Otherwise the fake is
  fixed first.
- `scripts/check-all.sh` green after any fix.
- The branch rebases onto `main` and merges ff.

## Result

### Starting state on the DGX (2026-10-09 12:07 UTC, read-only)

- `cria status --json` (exit 0): one server, `qwen38-flash` (llama, build
  `~/opt/llama.cpp-mtp/build-spark`), picks `cache=q8 cache_ram=0 context=512k mtp=3
  slots=2 vision=on`, `0.0.0.0:11434`, PID 747416, running since
  2026-10-08T16:22:20Z, health 200; both slots idle (`/slots`). No broken entries, no
  router.
- `~/spark-setup/vllm.sh status`: `stopped` (exit 1).
- `nvidia-smi`: GB10, driver 595.91.07; one compute process, the Flash
  `llama-server` (96,208 MiB).
- `free -h`: 121 GiB total, 108 GiB used, 13 GiB available; swap 951 MiB of 8 GiB used.
- Listening: 22 (ssh), 53 (resolver, loopback), 11434 (`llama-server`, PID 747416).
- Restore target: `cria start qwen38-flash cache=q8 context=512k mtp=3 vision=on
  slots=2 cache_ram=0 --wait`.

The Flash server was idle (both slots); the user had said the DGX was free. It was
stopped through cria (`cria stop qwen38-flash`) for the run and restored at the end.

### Versions and models

- **vLLM 0.30.0**, the existing venv `~/spark-setup/vllm/.venv` (PyTorch
  2.13.0+cu130, transformers 5.17.0), unchanged.
- **llama.cpp, new build:** `ggml-org/llama.cpp` `master` at
  `609290be6b15db02f9d73443435403cbff6e7802` (2026-10-09 13:54 +0200), **build 11525**
  (`llama-server --version`: `0.6.0-dev (build 11525, commit 609290be6)`), a fresh
  full-history clone (`git clone --reference ~/opt/llama.cpp --dissociate`, no
  alternates left) in `~/opt/llama.cpp-kaiak-rerank`. Built with the documented flags
  into `build-spark/` — Release, Ninja, `GGML_CUDA=ON`, arch 121,
  `/usr/local/cuda-13.3` compiler and toolkit root, tests/examples/UI off, prebuilt UI
  on (its download failed for b11525: no matter for the API) — target `llama-server`
  only, `-j 10`, a few minutes.
- **llama.cpp, old build** (optional check): the official `~/opt/llama.cpp/build-spark`,
  build 10970 (`bfdc32183`), unchanged.
- **Reranker:** `Qwen/Qwen3-Reranker-8B`, revision
  `77d193c791ed757ca307ee72715aa132723da912`, downloaded with `hf download` into the
  Hugging Face cache (16 GB). This revision also ships sentence-transformers files
  (`modules.json`, `1_LogitScore/`, `config_sentence_transformers.json`) and a
  `chat_template.jinja` with the score prompt; `config.json` still says
  `Qwen3ForCausalLM`, so the `--hf_overrides` stay needed.
- **vLLM score template:** `examples/pooling/score/template/qwen3_reranker.jinja` at tag
  `v0.30.0` (sha256 `e1ee98e69aab7b2da366edf1c50efcef37e34b4a0c50fb816336213e68d9047a`).
- **GGUF:** the 8B, converted by the new checkout's `convert_hf_to_gguf.py` straight
  from the Hugging Face snapshot, `--outtype q8_0` →
  `~/opt/llama.cpp-kaiak-rerank/kaiak-models/qwen3-reranker-8b-q8_0.gguf`
  (8,047,134,368 bytes; about 1 minute). Converter venv
  `~/opt/llama.cpp-kaiak-rerank/.venv-convert` (uv 0.12.18, CPython 3.13.15, the
  checkout's `requirements-convert_hf_to_gguf.txt`: torch 2.11.0+cpu, transformers
  4.57.6; `gguf-py` 0.19.0 from the checkout). The converter recognised the model by
  its README heading (`# Qwen3-Reranker-8B`; the snapshot directory is a hash).
  `gguf-dump` shows `qwen3.pooling_type = 4` (rank), `qwen3.classifier.output_labels
  = ['yes', 'no']`, `cls.output.weight` {4096, 2}, `tokenizer.chat_template.rerank`
  (the Qwen3 prompt with the default instruction) beside the model's chat template;
  `general.name` is the snapshot hash (cosmetic).
- **Chat models:** vLLM — cria's `qwen35-2b-a` (`Qwen/Qwen3.5-2B`, revision
  `15852e8c…`, served as `qwen3.5-2b`, already cached); llama-server — the cached
  `unsloth/Qwen3.8-27B-GGUF` `UD-Q4_K_XL` (revision `4ca72078…`) on the **new** build,
  with cria's `qwen38-27b` flags minus MTP and vision. Both servers of the
  llama-server run were b11525.

### Commands

On the DGX (test servers on `0.0.0.0`, ports 8001, 8004–8007; none on 11434):

```sh
# vLLM chat (cria entry, port 8001)
~/.local/bin/cria start qwen35-2b-a --wait
# vLLM reranker (port 8004) — the DEPLOYMENT.md command
vllm serve Qwen/Qwen3-Reranker-8B --host 0.0.0.0 --port 8004 --gpu-memory-utilization 0.25 \
  --runner pooling \
  --hf_overrides '{"architectures": ["Qwen3ForSequenceClassification"],"classifier_from_token": ["no", "yes"],"is_original_qwen3_reranker": true}' \
  --chat-template qwen3_reranker.jinja
# llama-server reranker, b11525 (port 8005) — the DEPLOYMENT.md command
llama-server -m qwen3-reranker-8b-q8_0.gguf --host 0.0.0.0 --port 8005 \
  --embedding --pooling rank --reranking -c 32768 -np 4
# llama-server chat, b11525 (port 8006)
llama-server -m <snapshot>/Qwen3.8-27B-UD-Q4_K_XL.gguf --alias qwen38-27b --host 0.0.0.0 --port 8006 \
  --gpu-layers all --load-mode dio --parallel 1 --flash-attn on --fit off --jinja \
  --temp 1.0 --top-p 0.95 --top-k 20 --min-p 0.0 --repeat-penalty 1.0 --presence-penalty 0.0 \
  --chat-template-kwargs '{"reasoning_effort":"medium"}' --cache-type-k q8_0 --cache-type-v q8_0 --ctx-size 65536
# optional: the same reranker command on the old build b10970 (port 8007)
```

vLLM's reranker loaded in 14.11 GiB (105 s), KV cache 14.13 GiB, `max_model_len` 40960
(the model's default); its route list has the pooling routes (`/v1/rerank`, `/rerank`,
`/v2/rerank`, `/v1/score`, `/classify`, `/pooling`…) and no chat, completions,
Messages or Responses route. llama-server's reranker: 4 slots of 8192 tokens; it
forces `n_batch = n_ubatch = 512` for an embeddings-mode server (its own warning).

On the Mac, from the worktree:

```sh
go -C scripts/live run . -kind vllm -base-url http://dgx.local:8001/v1 -model qwen3.5-2b -max-output 4096 \
  -rerank-base-url http://dgx.local:8004/v1 -rerank-model Qwen/Qwen3-Reranker-8B
go -C scripts/live run . -kind llama-server -base-url http://dgx.local:8006/v1 -model qwen38-27b -max-output 4096 \
  -rerank-base-url http://dgx.local:8005/v1 \
  -rerank-model /home/acdtrx/opt/llama.cpp-kaiak-rerank/kaiak-models/qwen3-reranker-8b-q8_0.gguf
```

### Kit output — vLLM: 34 passed, 0 failed, 3 skipped

```
== vllm: http://dgx.local:8001/v1, model qwen3.5-2b, reranker Qwen/Qwen3-Reranker-8B on http://dgx.local:8004/v1
  PASS  auth-reject          no key → 401
  PASS  models               lists live-capped, live-chat, live-rerank, live-rpm; endpoints chat_completions, completions, embeddings, messages, messages_count_tokens, rerank, responses
  PASS  chat                 "A lighthouse is for guiding ships with their lights toward s…" (finish stop, 24+14 tokens)
  PASS  usage-log/chat       in 24 (cache read 0, cache write 0), out 14 (reasoning 0), cost 5.2e-05
  PASS  chat-stream          no usage chunk, 18 chunks, [DONE], "Lighthouses are structures built to guide ships safe out of …"
  PASS  usage-log/stream     in 24 (cache read 0, cache write 0), out 17 (reasoning 0), cost 5.8e-05
  PASS  chat-stream-usage    usage chunk relayed, 32 chunks, [DONE], "A lighthouse is a beacon used to guide ships and boats safel…"
  PASS  usage-log/stream-usg in 24 (cache read 0, cache write 0), out 30 (reasoning 0), cost 8.4e-05
  SKIP  embeddings           no -embeddings-model
  SKIP  usage-log/embeddings no -embeddings-model
  PASS  rerank               2 results under live-rerank, 198 prompt tokens
  PASS  rerank-relevance     relevant document 0.9972, irrelevant 7.132e-06
  PASS  usage-log/rerank     in 198 (cache read 0, cache write 0), out 0 (reasoning 0), cost 0.000198
  PASS  rerank-cap           5 documents over max_rerank_documents 4 → 400 invalid_value on documents, never sent
  PASS  rerank-oversize      a 1.0 MB document → 400 BadRequestError from the server: "This model's maximum context length is 40960 tokens. However…"
  PASS  chat-to-reranker     chat → 502 upstream_endpoint_missing, warned for live-reranker; a rerank right after → 200
  PASS  rerank-to-chat       rerank → 502 upstream_endpoint_missing, warned for live; a chat right after → 200
  PASS  messages             "A lighthouse serves as a navigational beacon to guide ships …" (stop end_turn, 24+18 tokens), max_tokens set by the gateway
  PASS  usage-log/messages   in 24 (cache read 0, cache write 0), out 18 (reasoning 0), cost 6e-05
  PASS  messages-stream      29 events, message_start … message_stop, "Lighthouses serve as navigational beacons that guide ships s…"
  PASS  usage-log/msg-stream in 24 (cache read 0, cache write 0), out 25 (reasoning 0), cost 7.4e-05
  SKIP  messages-cache       the server reported no cache read: its prefix cache is off or does not report it
  PASS  messages-count       24 input tokens, no usage record
  PASS  messages-models      Anthropic's shape: live-capped, live-chat, live-rerank, live-rpm
  PASS  messages-errors      wrong key → 401 authentication_error, unknown model → 404 not_found_error
  PASS  messages-hosted-tool web_search_20250305 → 400 hosted_tool_unsupported, never sent
  PASS  price-options        speed "fast" passed through: vllm prices nothing
  PASS  responses            "A lighthouse serves as a beacon of guidance that enables shi…" (status completed, 24+30 tokens)
  PASS  usage-log/responses  in 24 (cache read 0, cache write 0), out 30 (reasoning 0), cost 8.4e-05
  PASS  responses-stream     28 events, response.created … response.completed, "A lighthouse serves as a navigational beacon to illuminate t…"
  PASS  usage-log/resp-stream in 24 (cache read 0, cache write 0), out 21 (reasoning 0), cost 6.6e-05
  PASS  responses-stateful   previous_response_id → 400 stateful_responses_unsupported, never sent
  PASS  responses-hosted-tool web_search → 400 hosted_tool_unsupported, never sent
  PASS  endpoint-not-served  /v1/responses/input_tokens → 400 endpoint_not_served
  PASS  output-ceiling       max_tokens 32768 → 16 tokens, finish length
  PASS  rate-limit           second request → 429, Retry-After 60, reset 1m0s
  PASS  metrics              live-chat: 12 usage records, 229 output tokens, 0.020572 USD; 1 rate-limited; 16 successful upstream attempts
  34 passed, 0 failed, 3 skipped
```

### Kit output — llama-server (b11525): 34 passed, 0 failed, 4 skipped, exit 0

```
== llama-server: http://dgx.local:8006/v1, model qwen38-27b, reranker /home/acdtrx/opt/llama.cpp-kaiak-rerank/kaiak-models/qwen3-reranker-8b-q8_0.gguf on http://dgx.local:8005/v1
  PASS  auth-reject          no key → 401
  PASS  models               lists live-capped, live-chat, live-rerank, live-rpm; endpoints chat_completions, completions, embeddings, messages, messages_count_tokens, rerank, responses, responses_input_tokens
  PASS  chat                 "A lighthouse is a tower that emits a powerful light to warn …" (finish stop, 22+78 tokens)
  PASS  usage-log/chat       in 22 (cache read 0, cache write 0), out 78 (reasoning 0), cost 0.000178
  PASS  chat-stream          no usage chunk, 81 chunks, [DONE], "A lighthouse is a tower with a light that warns ships of coa…"
  PASS  usage-log/stream     in 22 (cache read 18, cache write 0), out 82 (reasoning 0), cost 0.000186
  PASS  chat-stream-usage    usage chunk relayed, 80 chunks, [DONE], "A lighthouse is a tower that projects a powerful light to wa…"
  PASS  usage-log/stream-usg in 22 (cache read 18, cache write 0), out 80 (reasoning 0), cost 0.000182
  SKIP  embeddings           no -embeddings-model
  SKIP  usage-log/embeddings no -embeddings-model
  PASS  rerank               2 results under live-rerank, 198 prompt tokens
  PASS  rerank-relevance     relevant document 0.9972, irrelevant 4.891e-06
  PASS  usage-log/rerank     in 198 (cache read 0, cache write 0), out 0 (reasoning 0), cost 0.000198
  PASS  rerank-cap           5 documents over max_rerank_documents 4 → 400 invalid_value on documents, never sent
  PASS  rerank-oversize      a 1.0 MB document → 400 exceed_context_size_error from the server: "request (216080 tokens) exceeds the available context size (…"
  SKIP  chat-to-reranker     llama-server registers every route whatever its model: no endpoint missing to check
  PASS  rerank-to-chat       rerank → 502 upstream_endpoint_missing, warned for live; a chat right after → 200
  PASS  messages             "A lighthouse uses a powerful light to warn ships of dangerou…" (stop end_turn, 4+78 tokens), max_tokens set by the gateway
  PASS  usage-log/messages   in 22 (cache read 18, cache write 0), out 78 (reasoning 0), cost 0.000178
  PASS  messages-stream      87 events, message_start … message_stop, "A lighthouse is a tower that emits a bright light to guide s…"
  PASS  usage-log/msg-stream in 22 (cache read 18, cache write 0), out 82 (reasoning 0), cost 0.000186
  PASS  messages-cache       first request wrote 0, second read 9943 of 9947 input tokens
  PASS  messages-count       22 input tokens, no usage record
  PASS  messages-models      Anthropic's shape: live-capped, live-chat, live-rerank, live-rpm
  PASS  messages-errors      wrong key → 401 authentication_error, unknown model → 404 not_found_error
  PASS  messages-hosted-tool web_search_20250305 → 400 hosted_tool_unsupported, never sent
  PASS  price-options        speed "fast" passed through: llama-server prices nothing
  PASS  responses            "A lighthouse is a tower that emits light to guide ships safe…" (status completed, 22+77 tokens)
  PASS  usage-log/responses  in 22 (cache read 18, cache write 0), out 77 (reasoning 0), cost 0.000176
  PASS  responses-stream     79 events, response.created … response.completed, "A lighthouse emits a powerful light to warn ships of dangero…"
  PASS  usage-log/resp-stream in 22 (cache read 18, cache write 0), out 72 (reasoning 0), cost 0.000166
  PASS  responses-count      22 input tokens, no usage record
  PASS  responses-stateful   previous_response_id → 400 stateful_responses_unsupported, never sent
  PASS  responses-hosted-tool web_search → 400 hosted_tool_unsupported, never sent
  SKIP  endpoint-not-served  llama-server serves every endpoint the kit checks
  PASS  output-ceiling       max_tokens 32768 → 16 tokens, finish length
  PASS  rate-limit           second request → 429, Retry-After 59, reset 59s
  PASS  metrics              live-chat: 12 usage records, 668 output tokens, 0.021428 USD; 1 rate-limited; 16 successful upstream attempts
  34 passed, 0 failed, 4 skipped
```

The skips are by design: no `-embeddings-model`; vLLM reports no prefix-cache reads
(as in earlier runs); `chat-to-reranker` and `endpoint-not-served` do not apply to
llama-server.

### Real answers, sent with curl straight to the servers

The kit's query and documents (the bread-dough document first, the lighthouse-keeper
one second).

**vLLM 0.30.0, rerank** (`200`):

```json
{"id":"score-931b1b6de08d4e56","model":"Qwen/Qwen3-Reranker-8B","usage":{"prompt_tokens":198,"total_tokens":198},
 "results":[{"index":1,"document":{"text":"A lighthouse keeper tends the lamp, …","multi_modal":null},"relevance_score":0.9972110390663147},
            {"index":0,"document":{"text":"Knead the dough for ten minutes, …","multi_modal":null},"relevance_score":7.131840902729891e-06}]}
```

With `top_n: 1`: the same, one result.

**vLLM, a 1 MB document** (`400`):

```json
{"error":{"message":"This model's maximum context length is 40960 tokens. However, you requested 0 output tokens and your prompt contains at least 40961 input tokens, for a total of at least 40961 tokens. Please reduce the length of the input prompt or the number of requested output tokens. (parameter=input_tokens, value=40961)","type":"BadRequestError","param":"input_tokens","code":400}}
```

**vLLM, route missing:** chat to the reranker (`:8004/v1/chat/completions`) and rerank
to the chat model (`:8001/v1/rerank`) both answer `404` `{"detail":"Not Found"}`
(`content-type: application/json`, `server: uvicorn`); `GET :8004/v1/rerank` answers
`405` `{"detail":"Method Not Allowed"}`.

**llama-server b11525, rerank** (`200`):

```json
{"model":"/home/acdtrx/opt/llama.cpp-kaiak-rerank/kaiak-models/qwen3-reranker-8b-q8_0.gguf","object":"list",
 "usage":{"prompt_tokens":198,"total_tokens":198},
 "results":[{"index":1,"relevance_score":0.9972056746482849},{"index":0,"relevance_score":4.890599484497216e-06}]}
```

With `top_n: 1`: the same, one result. A 1,880-token pair (over `-ub` 512, under the
slot's 8192) is scored, `200`, `prompt_tokens` 1880: the chunked prefill works.

**llama-server b11525, a 1 MB document** (`400`):

```json
{"error":{"code":400,"message":"request (216080 tokens) exceeds the available context size (8192 tokens), try increasing it","type":"exceed_context_size_error","n_prompt_tokens":216080,"n_ctx":8192}}
```

**llama-server b11525, rerank to the chat server** (not started with `--reranking`;
`501`):

```json
{"error":{"code":501,"message":"This server does not support reranking. Start it with `--reranking`","type":"not_supported_error"}}
```

**llama-server b10970 (old build), same GGUF and flags:** rerank of the two short
documents `200` (the same scores); the 1,880-token pair and the 1 MB document both
`500`:

```json
{"error":{"code":500,"message":"input (1880 tokens) is too large to process. increase the physical batch size (current batch size: 512)","type":"server_error"}}
```

— the build requirement in `DEPLOYMENT.md` (decision 15) confirmed both ways.

### Against the fake backend and the spec

- **llama-server:** the fake's shapes match exactly — `object: "list"`, no `id`, no
  `document`, `usage` `{prompt_tokens, total_tokens}`, results sorted by score, `top_n`
  cutting; `exceed_context_size_error` with `n_prompt_tokens`/`n_ctx` and the same
  message wording; `501 not_supported_error` for rerank without `--reranking`.
- **vLLM:** every field the gateway or the kit reads matches (`model`, `usage`
  `{prompt_tokens, total_tokens}`, `results[].index` / `relevance_score`, sorted,
  `top_n`; the error's `type` `BadRequestError`, `param` `input_tokens`, `code` 400;
  the route-missing `404 {"detail":"Not Found"}`). Three cosmetic differences, nothing
  reads them, no fix made:
  - each result's `document` carries `"multi_modal": null` beside `text` (the fake
    sends only `text`);
  - the `id` is `score-<hex>` (the fake's `rerank-fake-1`);
  - the oversize message says "at least 40961 input tokens … (parameter=input_tokens,
    value=40961)" — vLLM stops counting one token past the limit — where the fake
    prints the exact count without the suffix.
- **Usage:** both servers report the same `prompt_tokens` for the same pair (198, the
  template included), recorded as `tokens_in` with no output and not estimated
  (`usage-log/rerank`). The two servers' scores agree to four digits.
- **`DEPLOYMENT.md`'s vLLM template claim, checked:** the same vLLM command without
  `--chat-template` logs `Serving an original Qwen3 reranker (Qwen/Qwen3-Reranker-8B)
  without a --chat-template … may produce inaccurate relevance scores` and does not
  use the repository's own `chat_template.jinja`: `prompt_tokens` 56 instead of 198,
  scores 0.921 (relevant) and 0.697 (irrelevant) instead of 0.9972 and 7e-6. The flag
  stays needed; the 142-token difference over two pairs also bears out "about 70
  tokens per pair" for the template.
- **Observation, not a mismatch:** a chat request to the Qwen3-Reranker GGUF on
  llama-server answers `200` with meaningless text (`"GMT      …"`, 8 tokens, usage
  reported), and embeddings answer `200` with `null` values in the vector. No `5xx`, so
  nothing for the circuit; it bears out the kit's reason for skipping
  `chat-to-reranker` on llama-server. `DEPLOYMENT.md` covers only the encoder case
  (chat answering `500`); whether to mention the causal case is the main session's
  call.

### Left on the DGX

- `~/opt/llama.cpp-kaiak-rerank` — 9.2 GB in all: the clone's `.git` 437 MB,
  `build-spark/` 266 MB (b11525 `llama-server`), `.venv-convert/` 809 MB (the
  converter's venv), `kaiak-models/qwen3-reranker-8b-q8_0.gguf` 7.5 GiB. Not used by
  cria or the `~/.local/bin` symlinks.
- `~/.cache/huggingface/hub/models--Qwen--Qwen3-Reranker-8B` — 16 GB (revision
  `77d193c…`).
- vLLM's compile cache for the reranker, `~/.cache/vllm/torch_compile_cache/fd07b6becb`
  (932 KB, beside the existing entries; it speeds a re-run).
- Removed: the scratch folder `~/kaiak-step8` (server logs, the score template, the PID
  file, the uv download cache). Nothing else was created; cria's config,
  `~/spark-setup/vllm/config.json` and the existing builds are unchanged.

### Restoration (2026-10-09 12:29 UTC)

- All test servers stopped (vLLM reranker twice — with and without the template —
  cria's `qwen35-2b-a`, the three llama-servers); the GPU held no process and ports
  were back to 22/53 before the restore.
- `~/.local/bin/cria start qwen38-flash cache=q8 context=512k mtp=3 vision=on slots=2
  cache_ram=0 --wait` → running after 18 s, PID 763935, the same command line as the
  starting state.
- Checked: `cria status --json` — `qwen38-flash`, the same picks, `0.0.0.0:11434`,
  running, health 200, no broken entries, no router; `/health` `{"status":"ok"}`;
  `/v1/models` lists `unsloth/Qwen3.8-Flash-Next-GGUF:UD-Q4_K_XL` (262144 per slot);
  `/slots` two idle slots of 262144; a chat "What is 17*19?" → `323` (finish `stop`);
  `nvidia-smi` one compute process, this `llama-server` (96,055 MiB); `vllm.sh status`
  `stopped`; listening 22, 53, 11434 only. No other vLLM, llama-server, converter,
  build or download process on the DGX, and no kit, gateway or fake process left on
  the Mac.
