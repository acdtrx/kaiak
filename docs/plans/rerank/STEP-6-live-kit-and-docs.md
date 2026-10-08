# Step 6 — live kit and docs

**Status:** done — reviewed and committed 2026-10-09

## Intent

- **One kit run checks a real reranker:** on vLLM or llama-server, beside the run's
  chat model.
- **The kit checks the wrong-endpoint answers live.**
- **An operator can serve Qwen3-Reranker behind kaiak from `DEPLOYMENT.md` alone.**

Live runs stay manual. Only `-self-test` runs in `scripts/check-gateway.sh`.

## Files likely touched

- `scripts/live/`:
  - **endpoints:** `apis.go` adds `rerank` for `vllm` and `llama-server`, and the
    other kinds check `endpoint_not_served`;
  - **reranker server:** `-rerank-base-url` and `-rerank-model` (env alike) add a
    reranker backend of the run's kind and a public model `live-rerank`;
  - **rerank checks:**
    - an answer with results, and the public model name;
    - a relevant document above an irrelevant one;
    - the usage log line exact, not estimated;
    - the documents cap refused before the backend;
    - an oversize document refused with a `400` the caller can read;
  - **the wrong-endpoint check:**
    - chat to `live-rerank` answers `upstream_endpoint_missing`, and rerank works
      right after;
    - on llama-server, rerank to the chat model, whose server is not started with
      `--reranking`;
  - **existing checks:** the wrong-path check updated for `vllm` (step 5);
  - **the self-test:** every new check against the fakes.
- `docs/testing/LIVE-BACKENDS.md`:
  - the reranker flags and checks;
  - a "Reranker on a server of its own" section, like the embeddings one, with both
    servers' start commands.
- `docs/DEPLOYMENT.md` (OVERVIEW decision 15):
  - serving Qwen3-Reranker on vLLM: `--runner pooling`, the `--hf_overrides` for the
    original checkpoint, the score template;
  - serving it on llama-server: `--embedding --pooling rank --reranking`, and `-ub`
    and `-b` at least a slot's context;
  - one backend per reranker server;
  - `max_rerank_documents`;
  - the upgrade note: upgrade gateways before setting `max_rerank_documents`;
  - the two places that still say a missing endpoint means an older server
    ("upgrade it"), now that it also means a model without the endpoint;
  - the wrong-`base_url` signal on `vllm` now being warnings.
- `README.md`, `docs/ARCHITECTURE.md`, `docs/architecture/gateway.html`: the endpoint
  lists.
- `scripts/live/config.go`, if it writes a config the new field belongs in.

## Decisions made during planning

- **The reranker backend takes the run's kind,** since only `vllm` and `llama-server`
  serve rerank. Other kinds skip the reranker flags, with the reason.
- **The relevance check is coarse.** A clearly relevant document must outrank a
  clearly irrelevant one. No score thresholds: they vary by model and template.
- **The kit's embeddings backend stays `openai-compatible`.** Typing it by kind is a
  separate change, made only if step 8 shows a need.

## Acceptance criteria

- `go -C scripts/live run . -self-test` passes, covering every new check.
- `scripts/check-gateway.sh` runs only the kit's lint and self-test, as before.
- Someone with no kaiak background can run a reranker check from
  `LIVE-BACKENDS.md`.
- **Phase 3 so far:** `scripts/check-all.sh` green and recorded.

## Result

### What changed

- `gateway/internal/fakebackend/` (test-only additions):
  - `SetRerankContext(words)`: a rerank request holding a pair (query plus one
    document) of more words than the context is refused `400` in its shape's error —
    vLLM's validation error (`{"error": {"message": "This model's maximum context
    length is …", "type": "BadRequestError", "param": "input_tokens", "code": 400}}`),
    llama-server's `{"error": {"code": 400, "message": "request (N tokens) exceeds
    the available context size (M tokens), try increasing it", "type":
    "exceed_context_size_error", "n_prompt_tokens", "n_ctx"}}`. 0 (the default)
    keeps every existing test as it was.
  - `SetNotSupported(endpoints...)`: those endpoints answer llama-server's `501
    not_supported_error`, after the credential check.
  - `cmd/fakebackend`: `-rerank-shape vllm|llama-server`, `-rerank-context N`,
    `-no-route LIST` (vLLM's `404 {"detail":"Not Found"}`, through `SetNoRoute`),
    `-not-supported LIST`.
- `scripts/live/`:
  - **Flags** (`main.go`): `-rerank-base-url` (env `LIVE_RERANK_BASE_URL`),
    `-rerank-model` (env `LIVE_RERANK_MODEL`), `-rerank-api-key-env`. The first two go
    together; the key flag needs the URL and a set variable. `options.reranker()`:
    a URL given and the kind serves rerank.
  - **Config** (`config.go`): with a reranker, backend `live-reranker` typed as the
    run's kind, model `live-rerank` on it (priced like the others), and
    `global.max_rerank_documents` 4.
  - **New checks** (`rerank.go`), run on `vllm` and `llama-server` after the
    embeddings checks:
    - `rerank`: `200`, `model` the public name, a result with `index` and
      `relevance_score` per document, `usage.prompt_tokens`;
    - `rerank-relevance`: the lighthouse-keeper document scores above the bread-dough
      one, which is sent first — no thresholds;
    - `usage-log/rerank`: `gen_ai.operation.name` `rerank`, then `checkUsageLog`
      (not estimated, not partial, input tokens, cost);
    - `rerank-cap`: 5 documents → `400 invalid_value` on `documents`, never sent
      (`notSent`);
    - `rerank-oversize`: one 1.03 MB document (over 200,000 tokens; a quarter of the
      default body cap) → `400` with an error message, from `live-reranker` (the log
      line's backend); a `5xx` fails with the hint to DEPLOYMENT → Rerankers;
    - `chat-to-reranker` (vllm): chat to `live-rerank` → `502
      upstream_endpoint_missing`, the endpoint-missing warning for the request naming
      `chat_completions` and `live-reranker`, then a rerank → `200`. Skipped on
      llama-server, with the reason;
    - `rerank-to-chat` (vllm and llama-server, with or without a reranker): rerank to
      `live-chat` → `502 upstream_endpoint_missing`, the warning naming `rerank` and
      a chat backend, then a chat → `200`.
    - Without `-rerank-base-url`: one `SKIP rerank` line; a kind serving no rerank
      given the flags: `SKIP rerank  <kind> serves no rerank: only vllm and
      llama-server do`.
  - **Existing checks**: `endpoint-not-served` refuses `/v1/rerank` (OpenAI's shape)
    on the kinds without it; `models` and `messages-models` expect `live-rerank`
    with a reranker (its backend's type serves Messages, so it is in the
    Anthropic-shaped list).
  - **Self-test** (`process.go`): `startFake` takes the models as a slice plus extra
    flags; `fakeFlags(kind, reranker)` plays each server: vLLM's chat fake has no
    rerank route, vLLM's reranker fake no route for the six generating and embedding
    endpoints, llama-server's chat fake answers rerank `501`, both reranker fakes a
    context of 32,768 words. For vllm and llama-server a reranker fake with a bearer
    key (`LIVE_SELF_TEST_RERANK_KEY`, covering `-rerank-api-key-env`). The
    two-backend fakes have no rerank route either (their run has `rerank-to-chat`).
    The self-test clears the rerank flags first, so `LIVE_RERANK_*` in the
    environment cannot point it at a real server.
- `docs/testing/LIVE-BACKENDS.md`: rerank in the scope line; the config's extra
  backend, `live-rerank` in the models table, every model priced, the cap; reranker
  bullets under vLLM and llama-server; the cost note; the self-test's reranker fakes;
  seven rows in the checks table and rerank in `endpoint-not-served`'s; new section
  **Reranker on a server of its own** (flags, both servers' start commands, the run
  command, which PASS lines matter); Reading failures: `upstream_path_missing` no
  longer names vLLM, `upstream_endpoint_missing` gives the three causes and vLLM's
  wrong base URL, and notes for `rerank-relevance`, a `500` on `rerank-oversize`, and
  `rerank-to-chat` answering `200`.
- `docs/DEPLOYMENT.md`:
  - Clients: four client APIs, rerank on `vllm` and `llama-server` only; the "older
    server version" bullet rewritten as a server that does not serve an endpoint its
    type serves — version, loaded model, flags — and a wrong `base_url` on `vllm`
    showing as the two warnings, not an open circuit.
  - Config for many hosts: `openai-compatible` loses rerank too;
    `global.max_rerank_documents` in the slot paragraph and the batch-caps bullet.
  - New section **Rerankers** (after Config for many hosts): the endpoint and types,
    a backend of its own on vLLM (router mode on llama-server), usage and price, no
    per-document cap, serving Qwen3-Reranker on vLLM and on llama-server, the live kit
    before go-live.
  - Starter alerts: the Wrong model, path or credential alert's meaning names the
    causes of `endpoint_missing`, `vllm`'s wrong `base_url` included.
  - Upgrades: **Next release** (unreleased; protocol 5, format 5): rerank and its
    operation; `max_rerank_documents` — upgrade gateways before setting it; the
    endpoint-missing reading (`vllm`'s route-missing answers, llama-server's `501`,
    `vllm`'s wrong `base_url` no longer opening the circuit, memory per deployment);
    the warning's new text.
- `docs/specs/GATEWAY.md` → Limits (batch caps, no per-document cap): the llama-server
  sentence corrected from the source (below).
- `README.md`: Client APIs gains rerank; Quick start step 3 rewrapped.
- `docs/ARCHITECTURE.md`: rerank in the mermaid client box, the request-path bullet
  and `accounting`'s list; the `fakebackend` paragraph (context refusal, endpoints
  lacking, the command's new flags).
- `docs/architecture/gateway.html`: four client APIs on the listener; Inbound's
  formats; the seams table's v1 formats; Rerank in the plug-points figure's client
  box; a Rerank row in the client API × backend figure (passthrough on vLLM ·
  llama-server, "not served" elsewhere) with its caption.
- Endpoint enumerations checked with `git grep` (`responses/input_tokens`,
  `/v1/embeddings`, "three client APIs"): `docs/kaiak.md` and the spec already list
  rerank; no other list of the gateway's endpoints.

### Design choices

- **`rerank-to-chat` runs on every vllm and llama-server run**, reranker or not: it
  needs only the chat server, and it is the one live check of each server's
  wrong-endpoint answer that works on both (vLLM's chat server has no rerank route;
  llama-server without `--reranking` answers `501`).
- **`chat-to-reranker` is skipped on llama-server.** llama-server registers every
  route whatever its flags and has no guard on chat in embedding mode
  (`handle_completions_impl`, master), so a chat to a reranker is not an endpoint
  missing (spec: "llama-server registers every route…"). The brief's "on llama-server,
  rerank to the chat model … too" is the check that runs there.
- **The warning check accepts any of the chat backends**: with two backends both
  deployments are tried and warned; the first line found names one of them, and the
  PASS line names it.
- **Cap 4**: above every rerank request the checks send (2 documents), so the cap
  touches nothing else; only set with a reranker.
- **The fake's context is in words**, the unit its scoring already uses; the kit's
  oversize document (198,000 words) is far above the self-test's 32,768.
- **`SetNotSupported` writes llama-server's body itself** (a generic message naming
  the endpoint): the `501` is llama-server's alone and the gateway reads only status
  and `type`. `SetNoRoute` keeps taking its body, since `404` bodies vary by
  framework.
- **The kit's embeddings backend stays `openai-compatible`** (planning decision).

### Deviations and findings (please review)

1. **llama-server and long pairs — decision 15 does not hold as written.**
   - `tools/server/server-context.cpp` checks a pooling task that cannot be split
     against `n_ubatch` first (`500 server_error`, "input (N tokens) is too large to
     process. increase the physical batch size") and only then against the slot's
     context (`400 exceed_context_size_error`).
   - So with `-ub` equal to a slot's context, a pair longer than the context still
     answers `500`.
   - Since build **b11223** (commit `4da6337`, PR #28876, "allow RANK pooling batch
     splitting for causal LLM rerankers (ie. Qwen3 and Qwen3-VL)"), `can_split()`
     is true for rank pooling on a causal model. A Qwen3-Reranker pair is then
     prefilled in chunks whatever `-ub`, and a pair over the slot's context answers
     `400` — the default `-ub` 512 is fine.
   - The `-ub` advice holds only for builds before b11223 and for encoder rerankers
     (BERT-style, no KV memory), where a pair longer than `-ub` answers `500` on any
     setting.
   - Written that way in DEPLOYMENT → Rerankers ("Build b11223 or later"), in
     LIVE-BACKENDS (the llama-server command has no `-ub`), and in `GATEWAY.md`'s
     no-per-document-cap paragraph, whose sentence said the opposite.
   - OVERVIEW decision 15 and the Risks line still say "`-ub` and `-b` at least a
     slot's context" — left for the main session.
   - Step 8's planned second run "with its default `-ub`" should now show `400` on a
     b11223+ build.
2. **`--reranking` alone sets `embedding = true` and pooling `rank`**
   (`common/arg.cpp`); the three flags are kept as decision 15 and the README write
   them, and the doc says `--reranking` turns the other two on.
3. **The GGUF carries the rerank prompt.** `conversion/qwen.py` (`Qwen3Model`)
   recognises Qwen3-Reranker by its model card ("# Qwen3-Reranker"), its name, or a
   `*SequenceClassification` architecture, and writes:
   - pooling `RANK`;
   - classifier labels `yes`/`no`, and a `cls_out` head from the `yes`/`no` rows of
     the LM head;
   - a named chat template `rerank` (`tokenizer.chat_template.rerank`) with
     `{query}`/`{document}` and the fixed default instruction.

   `format_prompt_rerank` (`server-common.cpp`) uses `llama_model_chat_template(model,
   "rerank")` when present, else joins the query and the document with BOS/EOS/SEP.
   So the operator does nothing for a GGUF from llama.cpp's converter. llama-server
   reads no `instruction`, so the task is fixed. A GGUF from elsewhere may lack the
   template: DEPLOYMENT says to check with `gguf-dump`.
4. **Without `--chat-template` vLLM joins query and document** (`prompt_1 +
   prompt_2`, no separator, for an LLM-as-reranker) and logs a warning at start
   ("Serving an original Qwen3 reranker … without a --chat-template", in 0.30.0 and
   main).
5. Step 5 noted that the kit had no wrong-path check to update. `rerank-to-chat`
   (vLLM's `{"detail":"Not Found"}` on an endpoint) is now the kit's live check of
   that reading.
6. **Pre-existing, not changed:** the self-test copies `-embeddings-base-url` (or
   `LIVE_EMBEDDINGS_BASE_URL`) from the environment into its single-kind runs. A set
   variable would point the self-test's `live-embed` at a real server. The rerank
   flags are cleared; the embeddings one is left as it was (out of scope).

### Sources verified (2026-10-09)

- **vLLM** — `main` at `240785b82c299c6c08bc5239e997cf62d67cf7bb` (2026-10-08), with
  tags `v0.30.0` and `v0.31.0` (latest release, 2026-10-05) for the example, the
  template and the warning:
  - `docs/models/pooling_models/scoring.md`: the `Qwen3ForSequenceClassification`
    row, the original-checkpoint `--hf_overrides` note, the score template section;
  - `examples/pooling/score/qwen3_reranker_online.py`: the serve commands with
    `--runner pooling` and `--chat-template`, and for `*-seq-cls` no overrides — the
    same in all three refs;
  - `examples/pooling/score/template/qwen3_reranker.jinja`: blob `558e9b9` in all
    three; it reads `instruction`, then `instruct`, then a system message, else the
    web-search default;
  - `vllm/entrypoints/pooling/scoring/protocol.py`: `instruction` folded into
    `chat_template_kwargs` (0.30.0, 0.31.0, main); `RerankResult.document` always
    present; `max_tokens_per_doc`;
  - `vllm/entrypoints/pooling/scoring/io_processor.py`: the missing-template warning
    and the join;
  - `vllm/renderers/params.py`: the context checks (`param` `input_text` /
    `input_tokens`);
  - `vllm/entrypoints/serve/exception_handling/error_response.py` and
    `serve/engine/protocol.py`: `VLLMValidationError` → `400`, `{"error":
    {"message", "type": "BadRequestError", "param", "code": 400}}`;
  - `vllm/engine/arg_utils.py` (`--runner`, `--hf-overrides`) and
    `vllm/utils/argparse_utils.py` (underscores accepted).
- **llama.cpp** — `master` at `de7fa0a3c6a2e1b4cd9f22eb8d6bf5b12dbdb63b` (build
  **b11514**, 2026-10-08), plus b11513, b11146 and b9917 for `can_split`, and
  `compare` to place `4da6337` at b11223:
  - `tools/server/README.md`: `--rerank, --reranking`, `--pooling … rank`,
    `--embedding`, `-b`/`-ub` defaults 2048/512, `-np`, `-c`; the rerank endpoint
    "requires … `--embedding --pooling rank`";
  - `common/arg.cpp`: what `--reranking` sets;
  - `tools/server/server.cpp`: `n_batch` lowered to `n_ubatch` with embeddings;
  - `tools/server/server-context.cpp`: `can_split`, the `n_ubatch` / slot-context
    checks, `post_rerank`, `handle_completions_impl`;
  - `tools/server/server-common.cpp`: `format_error_response`,
    `format_prompt_rerank`;
  - `tools/server/server-task.cpp`: `server_task_result_error::to_json`;
  - `src/llama-context.cpp`: `n_batch` capped by `n_ctx` for causal models;
  - `src/llama-model.cpp` and `src/llama-arch.cpp`: the named template key;
  - `conversion/qwen.py`;
  - `gguf-py` (`add_chat_template`, the `gguf-dump` script).

### Mutation checks (each reverted)

- vLLM chat fake serving rerank, vLLM reranker fake with a chat route and no context,
  `relevantIndex`/`irrelevantIndex` swapped, no `max_rerank_documents`:
  - `rerank-relevance`, `rerank-cap`, `rerank-oversize`, `chat-to-reranker` and
    `rerank-to-chat` fail, the last in the two-backend run too ("self-test failed
    for vllm, vllm with two backends").
- The fake answering the oversize pair `500 server_error`, llama-server's chat fake
  serving rerank, and the gateway's rerank row naming operation `embeddings`:
  - `rerank-oversize` fails with the DEPLOYMENT hint, and `rerank-to-chat` and
    `usage-log/rerank` fail.
- Live mode against a hand-started fake:
  - `-kind openai -rerank-base-url …` prints `SKIP rerank  openai serves no rerank:
    only vllm and llama-server do`, and `endpoint-not-served` covers `/v1/rerank`;
  - the three flag errors read as intended.

### Suite (2026-10-09)

- `go -C scripts/live run . -self-test`: `self-test passed for vllm, llama-server,
  openai, azure-openai, anthropic, azure-anthropic, vllm with two backends` — vllm
  37 passed / 0 skipped, llama-server 36 / 2 skipped (`chat-to-reranker`,
  `endpoint-not-served`), openai 22, azure-openai 21, the Anthropic kinds 16 / 2
  skipped, two backends 34 / 1 skipped (`rerank`).
- `scripts/check-gateway.sh` (2 m 06 s): gofmt, vet, staticcheck 2026.2.1, telemetry
  boundary; `go test -race -count=1` every package `ok` (`e2e` 120.0 s,
  `internal/server` 15.0 s); the kit's lint and self-test as above; `gateway checks
  passed`.
- `scripts/check-all.sh`, run 1 (204 s): gateway stages as above (`e2e` 117.3 s),
  `gateway checks passed`; control `npm test`: tests 630, suites 45, pass 629, fail
  0, skipped 1; `npm run lint`: `boundaries ok`; cross-half `ok kaiak/e2e 70.417s`;
  `all checks passed`.
- `scripts/check-all.sh`, run 2 (212 s, after this file was written): every gateway
  package `ok` (`e2e` 125.1 s, `internal/server` 15.5 s), self-test passed for the
  same kinds, `gateway checks passed`; control tests 630, pass 629, fail 0, skipped
  1; `boundaries ok`; cross-half `ok kaiak/e2e 70.332s`; `all checks passed`.
