# Recorded backend answers

Real answers on the Messages and Responses endpoints, recorded on 2026-10-06 for the
fake backend and the format tests (`docs/plans/messages-responses/`). Bodies are
exactly as the server sent them; the prompts were test prompts only.

- `vllm/`: vLLM 0.30.0 serving `unsloth/Qwen3.8-27B-NVFP4` (a reasoning model, so
  thinking blocks and reasoning items appear).
  - `messages*`, `responses*`, `count-tokens.json`: answers.
  - `*-model-missing.json`: the `404` for a model the server does not have.
  - `*-max-tokens.sse`, `*-incomplete.sse`: streams cut by the output limit.
- `llama-server/`: llama.cpp build b10802 (commit d1a92352c) serving
  `unsloth/Qwen3.8-27B-GGUF:UD-Q4_K_XL` (a reasoning model).
  - `messages*`, `responses*`, `count-tokens.json`, `input-tokens.json`: answers.
  - `*-max-tokens.sse`, `*-incomplete.sse`: streams cut by the output limit.
  - `*-unknown-model-name.json`: a request naming a model the server does not
    have. A single-model llama-server ignores the name and answers `200` with its
    own model: it has no missing-model `404`.
  - `path-missing.json`: its `404` for a path it does not have (`File Not Found`).
  - Unlike vLLM, its Messages content blocks interleave (a block opens before the
    previous one closes), its `tool_use` start has no `input`, its thinking
    signatures are empty, and its `message_delta` carries `output_tokens` only; its
    Responses `response.created` and `response.in_progress` carry no `model`, and a
    length-capped Responses stream ends with `response.completed`, `status:
    "completed"`.

When a server version changes these shapes, record again and say so here.
