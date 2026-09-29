# Plan: backend verification helper (`verifyBackend`)

## Goal

Stop hand-typing context lengths and capabilities for 20+ models, and catch wrong
values when a backend or model is added. `kaiak-control` gets a helper, `verifyBackend`,
that the app calls on purpose, when the operator adds a backend or a model:
1. it checks that the backend answers and accepts the credential;
2. it reads what the backend reports about its models;
3. it returns that to the app.

The app decides what goes into config and can show the information to its users. The
gateway and the config contract don't change: metadata stays declared.

## Scope

- `verifyBackend` in `kaiak-control`, a new subsystem (`src/backend-verify/`):
  - **Input:** backend type and `base_url` exactly as config spells them, an optional
    credential (the value, passed by the app), an optional backend-side model name.
  - **Output:** a report:
    - whether the backend answered, and why not (unreachable, credential refused, not
      a models list);
    - which server it is, as far as its answer says;
    - the models it lists, with what each reports: `context_length` and, for
      llama-server, capabilities and a reasoning hint;
    - where each value came from.
  - Sources:
    - **vLLM:** `max_model_len` on `/v1/models`.
    - **llama-server** (`owned_by: "llamacpp"`): `GET <root>/props`, where `<root>` is
      `base_url` without its trailing `/v1`:
      - `default_generation_settings.n_ctx` — the context one request gets, once slots
        split the cache;
      - `modalities.vision`;
      - `chat_template_caps.supports_tools`;
      - `chat_template_caps.supports_reasoning_effort` as the reasoning hint.
    - **OpenAI / Azure:** reachability and credential only.
- **Sample:** a `verify` CLI next to `keygen` that prints the report and a
  ready-to-paste `metadata` block.
- **Docs:**
  - a new spec, `docs/specs/BACKEND-VERIFY.md`;
  - the `AGENTS.md` rule on who talks to backends;
  - a `GUIDE.md` section;
  - the "discovery is deferred" lines in `GATEWAY.md` and `kaiak.md`.

## Out of scope

Each of these is a backlog entry:
- **Backend credentials in config** instead of gateway env: the user is thinking
  about it.
- **Gateway-side discovery or drift warnings:** the gateway does no extra backend
  work.
- **Reasoning efforts:** discovery, template heuristics, numeric efforts.
- **Active probes** (tiny real requests to observe vision, tools, reasoning): the
  helper stays passive (settled 2026-09-29). vLLM 0.30.0 reports only
  `max_model_len`: `/v1/models`, `/tokenize`, `/version` and `/metrics` read the same
  with vision on and off (checked live on the DGX); only an image request told them
  apart.
- llama-server router mode (`/props?model=`), and default sampling parameters.

## Decisions (settled 2026-09-29 with the user)

1. **Discovery lives in `kaiak-control`, not the gateway.**
   - The gateway does no extra work against backends.
   - The information reaches the app, which can show it.
   - The values become declared config, so the config contract, the protocol and the
     client API stay as they are.
   - Rejected: the gateway-side plan (optional metadata filled from the gateway's
     model check). It moved `null`s into the client API and a discovered-values store
     into the gateway.
2. **The helper only reports.** It never writes config, never adds a backend or a
   model, and keeps nothing. It runs when the app calls it, never on a schedule.
3. **The credential is a parameter.** The app passes the value it has. This works the
   same whether keys stay in gateway env or later move into config.
4. **llama-server context comes from `/props`,** not `meta.n_ctx`: the server can
   split its context across slots, and `/props` reports what one request gets.

Decisions made while planning (confirm in review):

5. **Missing is reported as missing.**
   - A field the backend doesn't report is absent from the report, never guessed.
   - A `base_url` that doesn't end in `/v1` skips `/props` and says so in the report.
   - No fallback to `meta.n_ctx` (CODING-RULES §4: visible-and-absent over
     silent-and-plausible).
6. **Reasoning is a hint, labelled as one.** It comes from the structured
   `supports_reasoning_effort` field only; the chat template text is not scanned
   (backlog). `tools` from the template caps is labelled a hint too: the embedding
   host reports `supports_tools: true`.
7. **Backend answers are validated** with kaiak-control's one validation mechanism (ajv,
   TECH-STACK), using lenient schemas for the fields read. An answer that doesn't
   match is reported as such.
8. **Bounded:** a timeout per request (default a few seconds, set by the caller), an
   `AbortSignal` from the caller, and a cap on the bytes read.
9. **Admin-only.** The helper fetches a URL the caller supplies, which is usually
   private. The GUIDE says to expose it only to admins; the helper does not block
   private addresses (backends live there).
10. **The sample's CLI never takes a secret as an argument** (CODING-RULES §9).
    `--api-key-env <NAME>` reads the credential from the environment.

## Phases and steps

- **Phase 1 — the helper** (steps 1–3). Green at the end.
  1. `STEP-1-contract.md` — `BACKEND-VERIFY.md` spec, the `AGENTS.md` rule, the
     backlog.
  2. `STEP-2-kaiak-control.md` — `src/backend-verify/`, exported from the package entry; tests
     against in-process fake servers.
  3. `STEP-3-sample-and-proof.md` — `verify` CLI, `GUIDE.md` section, `GATEWAY.md` /
     `kaiak.md` wording, live check against the llama-servers, check-all 3×.

## Risks

- **llama-server `/props` fields move between builds.** The live check pins them to
  the builds in use (b9917 on both hosts, 2026-09-29). A missing field is reported
  missing.
- **A stale declaration:** a backend restarted with another `--ctx-size` after
  verification leaves config wrong until someone re-runs verify. This is accepted;
  the drift warning is in the backlog.

## Verification

- **kaiak-control tests** against fake vLLM, llama-server, OpenAI-style and Azure-style servers:
  - each source read;
  - `401`/`403`, timeout, non-JSON and a missing field;
  - a `base_url` without `/v1`.
- **Live**, read-only, with the sample CLI:
  - DGX llama-server `dgx.local:11434` (qwen3.8-27b): context 262144, vision, tools,
    reasoning hint true;
  - embed host `llama-embed.local:11435`: context 32768, no reasoning hint.
  - A live vLLM too, if one is up that day; otherwise the fake one, and the step says
    so.
- `scripts/check-all.sh` green 3×.

**Verification status:** done (2026-09-29), with one live check not run:

- [x] kaiak-control tests against fake vLLM, llama-server (chat and embedding),
  OpenAI-style and Azure-style servers: each source, `401`/`403`, timeout, non-JSON,
  missing fields, `base_url` without `/v1` (STEP-2, 22 tests).
- [x] Sample `verify` command: arguments, env, errors, exit status, output streams
  (STEP-3, 8 tests against an in-process fake llama-server).
- [x] Live DGX llama-server `dgx.local:11434` (qwen3.8-27b, picks cache=q8
  context=256k mtp=3 vision=on): server `llama-server`, context 262144 from
  `default_generation_settings.n_ctx`, vision true, tools and reasoning true as hints,
  `metadata` `{"context_length":262144,"capabilities":{"vision":true}}`, exit 0
  (STEP-3; run after step 3 by swapping out the vLLM, which was restored).
- [x] Live embedding llama-server `llama-embed.local:11435`: context 32768, vision
  false, tools true as a hint kept out of `metadata`, no reasoning (STEP-3).
- [x] Live vLLM 0.30.0 on the DGX (the one running that day): context 262144 from
  `max_model_len`, no capabilities; `model-not-listed` with exit 1 (STEP-3).
- [x] `scripts/check-all.sh` green 3× in a row (STEP-3).

## Git

- Anchor: the release tag `v0.7.4` (a4a191b); only docs commits (the naming
  change and this plan) sit between it and the start of step 1.
- Branch `backend-verify`, worktree `.claude/worktrees/backend-verify`. Commit per
  step. Rebase onto `main` and fast-forward merge after phase 1.
