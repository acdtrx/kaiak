# Step 6 — live-test kit

**Status:** not started

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

(filled in when the step is done)
