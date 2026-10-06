# Recorded backend answers

Real answers on the Messages and Responses endpoints, recorded on 2026-10-06 for the
fake backend and the format tests (`docs/plans/messages-responses/`). Bodies are
exactly as the server sent them; the prompts were test prompts only.

- `vllm/`: vLLM 0.30.0 serving `unsloth/Qwen3.8-27B-NVFP4` (a reasoning model, so
  thinking blocks and reasoning items appear).
  - `messages*`, `responses*`, `count-tokens.json`: answers.
  - `*-model-missing.json`: the `404` for a model the server does not have.
  - `*-max-tokens.sse`, `*-incomplete.sse`: streams cut by the output limit.
- `llama-server/`: llama.cpp build b9917 serving
  `/models/qwen3-embedding-0.6b-q8_0.gguf`. It is an embedding model, so the text is
  noise; the shapes are what matter.

When a server version changes these shapes, record again and say so here.
