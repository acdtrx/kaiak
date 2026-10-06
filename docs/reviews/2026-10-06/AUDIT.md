# Pre-merge review — 2026-10-06 (`messages-responses` at `f653939`)

Scope: everything since `v0.10.1` on the `messages-responses` branch. That covers
Anthropic Messages and OpenAI Responses passthrough, the `anthropic` and
`azure-anthropic` backend types, and the removal of model `defaults`
(`docs/plans/messages-responses/`).

Four read-only reviewers worked on a detached worktree at `f653939`, one per area:

- **[P]** the request pipeline and inbound handling;
- **[S]** response handling and accounting;
- **[A]** the Anthropic types and security;
- **[C]** the contract across both halves, and the docs.

An independent review of a plain copy (Codex, no git history) joins as **[B]** once
it has run (pending).

Every finding is tagged by frequency under legitimate use: `daily` / `occasional` /
`rare` / `adversarial`. Findings that two reviewers reached separately name both.

## Verdict

**The contract holds and the happy path is right.**
- Schemas and fixtures agree across the halves.
- Every error code in the spec is emitted, and every code emitted is in the spec.
- The endpoint table matches the modules.
- Model rewriting never leaks a backend name over any of the 16 recorded streams.
- Completeness reads every recorded stream right, and a stream cut in half reads
  incomplete.
- No client header reaches a backend, and credentials never leave.

**Three problems need fixing before the merge:**
- A cut-off Messages stream bills almost no output.
- Nested bodies make the input estimate quadratic.
- OpenAI's `shell` tool can still run in a hosted container.

**Several occasional edges** sit in how new failure kinds meet the circuit breaker and
the limits.

## Findings

### High

- **H1 — A cut-off Messages stream records output ≈ 0, not flagged as estimated** [S]
  `daily`
  - `accounting/messages_usage.go:82-85,134-139`, `accounting/meter.go:204-206`.
  - `message_start` already carries usage (`output_tokens` 0 or 1), so `reported()`
    is non-nil from the first event. `Settle` then never falls back to the content
    estimate.
  - Any stream that ends before `message_delta` settles at the opening count, with
    `Estimated: false`. That includes a client pressing stop, a stall, the drain's
    cut, a backend death, or a mid-stream `overloaded_error`.
  - Confirmed by [S]: 16 KB of `text_delta` settles as `tokens_out: 1`. Reading
    confirms it.
  - Fix: the output count of `message_start` is provisional. Without a
    `message_delta` usage, keep the reported input and cache fields, set
    `tokens_out = max(reported, EstimateTokens(contentBytes))`, and flag the record
    estimated. Refine the spec's "the backend's report when one arrived" for
    Messages.
- **H2 — The input estimate is quadratic in nesting depth** [P][S] `adversarial`
  (any valid key)
  - `accounting/estimate.go:239` (`messagesSource`), `:270` (`responsesParts`).
  - Each nested `source` or `content` part is decoded, decoded again for its
    `type`, and rescanned. The rescan meets the next level and repeats.
  - This runs in the inbound stage, before limits.
  - Measured: a 224 KB body took 4.5 s, and a 1.1 MB body 3.6 s. Extrapolated to the
    4 MiB cap and about 9,000 levels, that is about a minute of CPU per request,
    and 16 cores for one key at the per-key concurrency default.
  - Fix: one pass. Read the part with the outer decoder, keep its span, and swap in
    `InlineMediaTokens` once its `type` is known to be media. A `source` counts as
    media only directly under an image or document block, which also fixes [S] L1
    below.
- **H3 — OpenAI's `shell` tool with a hosted `environment` passes the hosted-tool
  allowlist** [A] `adversarial` / `occasional`
  - `server/inbound_responses.go:66`, `server/inbound.go:138-163`. Only `type` is
    judged.
  - `{"type": "shell", "environment": {"type": "container_auto"}}` runs in an
    OpenAI-hosted container (OpenAI and Azure docs). That is container time outside
    the price table, against the settled "hosted tools are refused". The same goes
    for `tool_choice` / `allowed_tools`.
  - An agent SDK configured for hosted shell does this with no ill intent.
  - Fix: `shell` passes only without `environment`, or with `environment.type`
    `"local"`. Allowlisted types are judged by their members, not their name alone.
    Spec: Hosted tools.

### Medium

- **M1 — A client-chosen string in a 404 can open a deployment's circuit** [A]
  `adversarial`
  - `provider/wire.go:128-148` (`modelMissing` → `namesWord`).
  - A 404 whose message names the backend model as a whole word counts as
    `upstream_model_missing`: a circuit failure, retried elsewhere.
  - The new endpoints let a client make the backend echo an ID it chose:
    - Responses `item_reference` → OpenAI's "Item with id '<x>' not found";
    - Anthropic `file_id` sources.
  - Five requests can open every deployment of a model. On `anthropic` the name
    match is the only signal.
  - Confirmed by [A] on recorded error shapes.
  - Fix: on cloud types, judge a missing model by structured fields only:
    - OpenAI and Azure: `code: model_not_found` or `param: model`;
    - Anthropic: `not_found_error` whose message starts `model:`.

  Self-hosted types keep the name match (vLLM's message is the only signal). M5
  below removes most echo vectors too.
- **M2 — 1-hour cache writes in nested blocks escape the price refusal** [A][P]
  `occasional`
  - `provider/anthropic_price.go:55-77` walks only the top level, `system[]`,
    `messages[].content[]` and `tools[]`.
  - It misses `cache_control` in `tool_result.content[]`, in
    `document.source.content[]` and in `search_result.content[]`. Agents put
    breakpoints on tool results.
  - Repeated `cache_control` or `ttl` members read last-wins ([A] L1).
  - Confirmed by [A] and [P] tests.
  - The scan also copies every block on every attempt ([A] L6).
  - Fix: one recursive token-stream scan with a depth bound, once per request,
    refusing any `cache_control.ttl: "1h"` and any repeated `cache_control` / `ttl`.
    It reports the path. Spec: the places list becomes "anywhere".
- **M3 — An old server missing an endpoint keeps drawing that endpoint's traffic**
  [P] `occasional` (mixed versions during upgrades)
  - `server/upstream.go` (`classifyAttempt`, `avoidAfter`), `routing.choose`.
  - The 404 is instant, so that deployment has the fewest in flight and is chosen
    first for every Messages or Responses request.
  - The avoid list is per request and the circuit is neutral. Every request wastes an
    attempt, and past about 1 request/s the retry budget refuses retries. Clients
    then get `502 upstream_endpoint_missing` while a working deployment sits idle.
  - This is the "fast 404 draws traffic" effect the spec rejected for missing models.
  - Fix: remember (backend, endpoint) as missing for the probe interval, and exclude
    it in `servingDeployments`. Log one warning per interval instead of per request.
- **M4 — Overloads and caller faults in stream error events count as backend
  failures** [P][A] `occasional`
  - `provider/stream_end.go:140`, `server/upstream.go` (`classifyAttempt`).
  - Anthropic's `529 overloaded_error`, as a status or as an error event, counts as a
    circuit failure ([A] L5). An Anthropic-wide overload opens every circuit, and
    clients get `503` instead of a retryable overload.
  - Responses `response.failed` / `error` events with caller codes count as backend
    failures too ([P] M2). Examples: `invalid_prompt`, `invalid_image_url`,
    `failed_to_download_image`, `*_policy_violation`. So does `rate_limit_exceeded`.
    A few failed image downloads open the circuit.
  - Fix: classify by the error's code or type, like the matching HTTP status:
    - overload and rate limit → neutral with the cooldown (as `429`);
    - caller codes and Anthropic `invalid_request_error` → neutral, not retried;
    - `server_error`, `api_error`, unknown or missing → failure.
- **M5 — Responses and Messages references to server-stored objects pass** (decided 2026-10-06: refuse) [A][P]
  `rare` / `adversarial`
  - `server/inbound_responses.go:42`.
  - Responses:
    - `prompt` (stored templates, which can carry hosted tools);
    - `item_reference` input items;
    - `input_file` / `input_image` with `file_id`.
  - Anthropic: `file_id` sources (Files API, GA).
  - These read objects stored in the operator's project or workspace by other apps.
    With `store: false`, kaiak's own clients never create any.
  - IDs are not guessable. The impact is a hosted-tool path (`prompt`) and the echo
    vector of M1.
  - Fix (needs a decision): refuse them as `stateful_responses_unsupported` (a
    stored-object refusal on Messages), in the same pass as H2.
- **M6 — `max_tokens` lowered to the ceiling breaks thinking requests** [P]
  `daily` where a ceiling sits below client requests
  - `server/params.go:53`.
  - Anthropic requires `thinking.budget_tokens < max_tokens`. Claude Code sends
    `max_tokens: 32000` with a budget of 31999. A ceiling below that lowers
    `max_tokens`, and the backend answers `400` about a value the client never sent.
  - The decision to leave this to the backend lives only in the step file.
  - Fix: read `thinking.budget_tokens` (read only). When the lowered value would be
    ≤ the budget, answer `400 invalid_value` naming `max_tokens` and the model's
    ceiling, so the operator sees that the ceiling is the problem. Record the rule
    in the spec, and the operator advice in DEPLOYMENT.
- **M7 — DEPLOYMENT.md's data-file format table is stale** [C] `occasional`
  (every upgrade with a data directory)
  - `docs/DEPLOYMENT.md:963-966` says 1/2/1/1. The code and GATEWAY.md say
    spool 3, last-known-good 6, totals 2, limits 2.
  - Fix: point to GATEWAY.md, or correct the table.
- **M8 — Log attributes missing from the spec's field tables** [C] `occasional`
  - `kaiak.endpoint` (`server/upstream.go:229`) is in no table.
  - `kaiak.request.id` and `kaiak.backend.type` on the new operational lines
    (endpoint missing; `model check not available`) are undocumented there.
  - Fix: add the rows.

### Low

Fixes are cheap unless noted.

- **L1 — Token-counting requests are refused by token and USD limits** [P]
  `occasional`. A 0-token reservation still runs every counter (`limits.go:31`). A
  full token window, a spent budget or `budget_unavailable` refuses a request that
  costs nothing. Confirmed by [P]. Fix: only requests-per-minute counters apply.
- **L2 — `endpoint_not_served` comes after limits** [P] `occasional`. It spends an
  rpm slot, and with a full limit it answers `429` instead of the useful `400`. Fix:
  check in `model_access` / `model_params`, before limits.
- **L3 — A Responses tool with no `type` passes the allowlist** [C] `rare`. The
  backend refuses it; there is no bypass. Fix: refuse a missing type on Responses,
  matching the spec.
- **L4 — Mid-stream error events reach the client verbatim** [S] `rare`. Their text
  can name the backend model or engine internals, unlike the `5xx` rule. Fix: replace
  the error's message with gateway text and keep its type and code. Alternatively,
  record it as an exception.
- **L5 — `anthropic` probe needs the exact listed id** [A] `occasional`. A deployment
  using an alias the list doesn't return stays open after its circuit opens. The
  config-time check warns. Fix: docs ("use the ids the models list returns").
- **L6 — `azure-anthropic` verify says ok without contacting anything** (decided 2026-10-06: not checkable) [A]
  `occasional`. The always-pass probe is as decided; the verify result reads as
  "verified". Fix: report it as not checkable (an `ok: false` code or a distinct
  status). Decide in review.
- **L7 — Only the first block is checked for a retryable error event** [S] `rare`.
  A comment block first makes an error event unretried. No current server does
  this. Record only.
- **L8 — A future vLLM could double-count cache reads on Messages** [S] none today.
  Record. Add a live-kit check that `input_tokens + cache_read` stays near
  `count_tokens`.
- **L9 — Docs and spec wording** [C], docs only:
  - The output-limit row misses `max_output_tokens`.
  - The duplicate-member row is narrower than the code.
  - The `upstream_endpoint_missing` row says "beyond OpenAI's three".
  - `service_tier` is overstated for `azure-anthropic`.
  - The retries metric help text misses `endpoint_missing`.
  - The cardinality numbers say 6/12 (8/14 in code).
  - BACKEND-VERIFY's `model-not-listed` row and its Requests bullet.
  - "Strict everywhere" is overstated, and history narration appears in two specs.
  - "The gateway sets only the output limit" is too broad (GUIDE, CONTROL-PROTOCOL,
    kaiak.md, AGENTS.md's owned-edits list).
  - The llama-server router-mode nuance on the new endpoints.
  - No format-5 upgrade step in DEPLOYMENT.
  - kaiak.md's opening still says OpenAI-only; TECH-STACK too.
  - AGENTS.md's `-kind` list.
  - gateway.html's Clients box.
  - LIVE-BACKENDS points to `examples/local-config.json` for client checks, and says
    every run sends embeddings.
  - The spec cites llama.cpp b9917 where the captures are b10802.
  - Codex's `web_search` default is stated as fact for custom providers.
- **L10 — Dead `FiniteNumbers`** [C] (`schemacheck.go:337`), left by the `defaults`
  removal. Delete it.

## Checked and sound

- **Contract:** the schema copies are byte-identical. The fixtures pass in both
  halves. Every error code, endpoint-support row, metric label, relay end, attempt
  outcome (14) and retry reason (8) agrees with the spec [C].
- **`defaults`** is gone everywhere; only L10 remains [C].
- **Versions:** protocol 5, config format 5 and last-known-good 6 are consistent;
  the spool is unchanged [C]. The one exception is M7.
- **Completeness:** all 16 recordings read complete, halves read incomplete, and
  vLLM's length-capped `completed` / `incomplete` is handled. Overlapping blocks and
  items don't matter. OpenAI's logic is unchanged since `v0.10.1` [S].
- **Error events:**
  - As the first event: retried, a circuit failure, no units.
  - Later: relayed, then the stream ends incomplete and settles partial.
  - Classification is the subject of M4 [S][P].
- **Model rewriting:** never leaks over any recording; nested, escaped and
  non-object cases are left alone [S].
- **Body edits:**
  - `store: false` on Responses only.
  - `include_usage` on OpenAI streams only.
  - `service_tier`: `default` on OpenAI and Azure Responses, `standard_only` on
    `anthropic` Messages, untouched elsewhere [S].
- **Units:** per format and per server, the cache fields and cumulative
  `message_delta` are handled right, and the Responses clamping is right [S]. The
  exception is H1.
- **Headers:** a fixed allowlist both ways. No client `Authorization`, `x-api-key`,
  `anthropic-*` reaches a backend, and no backend request id or rate-limit header
  reaches the client [A][P].
- **Credentials:** required for the cloud types, `KAIAK_` / `OTEL_` refused twice,
  never in errors or logs [A].
- **Inbound auth:** Bearer wins; an empty `x-api-key` is a missing key; a malformed
  `Authorization` is never bypassed by `x-api-key` [A][P].
- **Allowlists:** dated client-tool versions only; `computer_toolset_*`, server
  tools, `mcp_servers` and `container` are refused; duplicate `type` is refused
  [A][P]. The exception is H3.
- **Statelessness:** `previous_response_id`, `conversation` and `background: true`
  are refused, and `store: false` is forced on every backend [A][P]. The exception is
  M5.
- **Refusals:** never retried, circuit-neutral, no usage record [A][P].
- **Token-counting path:** zero reservation, no record [P]. The exception is L1.
- **Output limits** per format: negative values refused, values above the context
  refused, the ceiling lowered, the default fitted to the prompt [P]. The exception
  is M6.
- **The unknown-format panic** is unreachable [P].
- **OpenAI chat, completions and embeddings:** unchanged apart from defaults [P][S].
- **`backend-verify`** for `anthropic`: GET only, manual redirects, the credential
  never echoed [A].

## Triage proposal

**Fix before the merge** (as a review-fix step on the branch):
- H1–H3;
- M1–M4 and M6–M8;
- M5, once decided;
- L1–L4, L9, L10;
- L6, once decided.

**Record only:** L5 (docs line), L7, L8.

**[B]** adds its own findings when it has run; they are verified by re-running its
reproduction tests before they join this list.
