# Step 12 — the live run

**Status:** not started

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
