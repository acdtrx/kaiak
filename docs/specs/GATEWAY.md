# Gateway

> The data plane: client API, request pipeline, routing, limits, accounting, lifecycle.
> Principles in `docs/kaiak.md`; the control-plane side in `CONTROL-PROTOCOL.md`.
> Decisions below are settled 2026-09-24 unless dated otherwise. Config field names and
> shapes are the config document's (`CONTROL-PROTOCOL.md`, Config); this file records
> only how the gateway acts on them.

## Client API

- **Endpoints**: `POST /v1/chat/completions`, `POST /v1/completions`,
  `POST /v1/embeddings`, `POST /v1/messages`, `POST /v1/messages/count_tokens`,
  `POST /v1/responses`, `POST /v1/responses/input_tokens`, `GET /v1/models`,
  `GET /v1/models/{id}`, `GET /v1/models/{id}/props`. Streaming responses are SSE,
  exactly as the API's own server streams them. Messages and Responses settled
  2026-10-06.
- **Client APIs** (settled 2026-10-06): three formats, each endpoint in one of them —
  **OpenAI** (chat completions, completions, embeddings, the model endpoints),
  **Anthropic Messages** (`/v1/messages`, `/v1/messages/count_tokens`, and the
  Anthropic-shaped model list below) and **OpenAI Responses** (`/v1/responses`,
  `/v1/responses/input_tokens`). Every request is **passed through** to a backend
  that serves its endpoint natively; the gateway never translates between formats.
  So a model is reachable through the endpoints its deployments' backends serve
  (Providers → Endpoint support): a Claude model on `anthropic` backends through
  Messages only, an OpenAI model through the OpenAI and Responses endpoints, a
  vLLM or llama-server model through all three. Rejected: translating through
  Chat Completions — it loses each format's own fields (thinking signatures, cache
  markers, typed output items) and the backends serving self-hosted models already
  speak all three; translation stays in `docs/BACKLOG.md` for a client that must
  reach a model its backends cannot serve in its format.
- **Responses is stateless** (settled 2026-10-06): the gateway keeps no conversation
  and lets no backend keep one. Every Responses request is sent with `store: false`
  (a client's `true` is replaced, not refused: clients default to it and work
  without it); `previous_response_id`, `conversation`, `prompt` (a stored prompt
  template, which can carry tools of its own) and `background: true` are refused
  (`400 stateful_responses_unsupported`, `param` naming the field); there are no
  `/v1/responses/{id}` routes (retrieve, delete, cancel, input items: `404
  unknown_url`) and no Conversations API. A `null` value counts as absent, as for
  every owned field.
  **Stored objects** (settled 2026-10-06, the pre-merge review's M5 and [B] M1): a
  reference to an object stored at the backend is refused the same way — an
  `item_reference` input item, an input item with neither `type` nor `role` (the
  reference's short form, `{"id": "msg_…"}`), a content part or a tool call's output
  part naming a `file_id`; on Messages, a block whose `source` is of type `file`, or
  a block naming a `file_id` (a container upload), in a message's, a tool result's or
  a content source's content, answer `400 stored_object_unsupported`. The gateway
  stores nothing and has no upload endpoint, so such an ID can only name another
  application's object in the shared provider account; complete inline items that
  carry their own IDs pass. Rejected: allowing file IDs — a leaked ID would read
  another application's file, and its content would escape the input estimate. Rejected: a conversation store in the gateway — it would hold
  prompt content and state, against principles 1 and 9; routing follow-ups back to
  the backend that stored the response — no failover, per-replica stores on
  vLLM, and every translated or replaced deployment loses the conversation.
- **Hosted tools are refused** (settled 2026-10-06): a tool the backend would run
  itself (web search, web fetch, code execution, file search, remote MCP,
  computer-use toolsets run server-side, image generation) is refused before
  routing, `400 hosted_tool_unsupported`, `param` naming the tool's position
  (`tools[2].type`), on every backend type — on local backends there are none to
  run, and on cloud ones they would bill units kaiak does not price. An allowlist
  of tool types the client runs itself, so a new server tool is refused until it is
  judged:
  - Messages: a tool with no `type`, or `custom`, or one of Anthropic's client-run
    tools — `bash_`, `text_editor_`, `computer_` or `memory_` followed directly by
    its version date (`computer_20250124`; settled 2026-10-06, the step 4 review: a
    type that merely shares the prefix, `computer_toolset_20260801`, is another
    tool, refused until judged); `mcp_servers` and `container` are refused the same
    way (`param` naming them).
  - Responses: `function`, `custom`, `local_shell`, `shell`, `apply_patch` — every
    Responses tool names its type (a tool without one is refused, `invalid_type`);
    a `shell` only when it runs on the client: no `environment`, or one of type
    `local` (settled 2026-10-06, the pre-merge review's H3 and [B] H2: a
    `container_auto` or `container_reference` environment runs the shell in the
    backend's container, refused as a hosted tool). A `tool_choice` naming any other
    type (`{"type": "web_search"}`, …) is refused too. A `tool_choice` of type
    `allowed_tools` names no tool of its own — it narrows the `tools` list — so it
    passes, its own `tools` list held to the same rule (settled 2026-10-06). Tools an
    input item brings in — an `additional_tools` item's or a `tool_search_output`
    item's `tools` — are held to it too, on both Responses endpoints (settled
    2026-10-06, [B] H3).

  **Every member the gateway reads to enforce these rules is read once** (settled
  2026-10-06, [B] L1): a tools entry, a `tool_choice`, a shell's `environment`, an
  input item, a content part, a Messages block or source naming any member twice is
  refused (`400 duplicate_member`, `param` its path) — the gateway and the backend
  could read different occurrences, one of them unchecked.

  Clients that attach a hosted tool by default need it turned off (Codex:
  `web_search = "disabled"`; `docs/testing/LIVE-BACKENDS.md`).
- **Token-counting endpoints** (settled 2026-10-06): `/v1/messages/count_tokens` and
  `/v1/responses/input_tokens` run the whole pipeline — auth, model access, limits,
  routing, the backend — and meet **only requests-per-minute limits**: they reserve
  no tokens, no token or USD limit refuses them, nor does an unknown budget
  (`budget_unavailable`), and they produce **no usage record** — nothing is
  generated or billed (settled 2026-10-06, the pre-merge review's L1). They
  are logged and counted in the ops metrics like any request, and routed only to
  deployments whose backend has them (Providers → Endpoint support).
- **Auth**: `Authorization: Bearer <key>` (scheme case-insensitive), or `x-api-key:
  <key>` when the request has no `Authorization` header (settled 2026-10-06: the
  Anthropic SDKs send it, on every endpoint; with both headers `Authorization` is the
  one read). Keys carry a
  recognizable prefix; the gateway stores and compares SHA-256 hashes only (keys are
  long and random, so a slow hash buys nothing). Everywhere past authentication, a key
  is its **key ID**. A key is valid through its `expires_at` instant and expired after
  it. Messages for disabled and expired keys say so: only a holder of the real key can
  see them.
- **Model access** (settled 2026-09-24): one check, in the auth package, run once the
  requested model is known. An unknown model and a model the key's group path does
  not allow (`CONTROL-PROTOCOL.md`, Config → The group tree: allowed models intersect
  down the path) get the same `404` `model_not_found`, built only from the name the client sent, so a
  key never learns which other models exist. The model endpoints (`/v1/models/{id}`,
  `…/props`) apply the same check to the model in the path. Model names may contain
  `/`, so `{id}` is the rest of the path; a trailing `/props` always selects the props
  endpoint, which is why config refuses public model names ending in `/props`
  (`CONTROL-PROTOCOL.md`, Config).
- **Errors** use the OpenAI error shape (`{"error": {"message", "type", "param",
  "code"}}`; `param` is always present, `null` unless one parameter is at fault) so
  client libraries handle them natively. Limit rejections are `429` with `Retry-After`
  and OpenAI-style `x-ratelimit-*` headers; a queue refusal is `429` (type
  `server_error`, below), no healthy deployment is `503`. This deviates from CODING-RULES §5's `{ error, detail? }` on purpose — the
  API being implemented dictates the shape. **On the Messages endpoints** every
  answer the gateway gives itself uses Anthropic's shape instead (settled
  2026-10-06): `{"type": "error", "error": {"type", "message", "code"}}` — `code` is
  kaiak's stable code from the table, a member Anthropic's SDKs ignore — with the
  same status and headers (`Retry-After` and the `x-ratelimit-*` headers included).
  Its `type` follows the status, as Anthropic's do: 400 `invalid_request_error`,
  401 `authentication_error`, 403 `permission_error`, 404 `not_found_error`, 413
  `request_too_large`, 429 `rate_limit_error`, 503 and 529 `overloaded_error`, any
  other 5xx `api_error`. The Responses endpoints keep the OpenAI shape, which is theirs.
  Rejected: carrying kaiak's code in a header only — clients log the body. Codes the
  gateway answers with so far (settled 2026-09-24, later rows as dated where they
  are specified):

  | Situation | Status | `type` | `code` |
  |---|---|---|---|
  | No `Authorization` or `x-api-key` header | 401 | `invalid_request_error` | `missing_api_key` |
  | Not `Bearer <key>`; unknown, disabled or expired key | 401 | `invalid_request_error` | `invalid_api_key` |
  | Unknown model, or model not allowed for the key | 404 | `invalid_request_error` | `model_not_found` |
  | Body is not a JSON object | 400 | `invalid_request_error` | `invalid_json` |
  | The top-level object, or `stream_options`, names a member twice — or any member the gateway reads to enforce its rules: a tools entry, a `tool_choice`, a shell's `environment`, a Responses input item or part, a Messages block or source, `thinking`, a `cache_control` or its `ttl` on Anthropic types (`param` names it; Request pipeline → duplicate members; Client API → Hosted tools are refused) | 400 | `invalid_request_error` | `duplicate_member` |
  | Body could not be read | 400 | `invalid_request_error` | `invalid_body` |
  | `model` missing or empty (`param: "model"`) | 400 | `invalid_request_error` | `missing_required_parameter` |
  | An owned field has the wrong type (`param` names it) | 400 | `invalid_request_error` | `invalid_type` |
  | `n` or `best_of` above `global.max_n` (`param` names it) | 400 | `invalid_request_error` | `n_too_large` |
  | `max_tokens`, `max_completion_tokens` or `max_output_tokens` below 0 or above the model's `context_length` (`param` names it; Limits → Output limit out of range); or a Messages output limit the gateway would set — the model's ceiling or default — at or below `thinking.budget_tokens` (Limits → Output limit) | 400 | `invalid_request_error` | `invalid_value` |
  | More generated sequences than `global.max_sequences_per_request` (`param`: `prompt`, `n` or `best_of`), or more embeddings inputs than `global.max_embedding_inputs` (`param: "input"`; Limits → Output multiplicity) | 400 | `invalid_request_error` | `invalid_value` |
  | Body over `max_request_body_bytes`, or over the whole body budget (`KAIAK_BODY_MEMORY_BYTES`) when that is smaller | 413 | `invalid_request_error` | `request_too_large` |
  | A Responses request sets `previous_response_id`, `conversation`, `prompt` or `background: true`, or refers to an item or file stored at the backend (`param` names it; Client API → Responses is stateless) | 400 | `invalid_request_error` | `stateful_responses_unsupported` |
  | A Messages request refers to a file stored at the backend (`param` names it; Client API → Responses is stateless: stored objects) | 400 | `invalid_request_error` | `stored_object_unsupported` |
  | A tool type the backend would run, a shell running in the backend's container, `mcp_servers`, `container`, or a Responses `tool_choice` or input item naming such a tool (`param` names it; Client API → Hosted tools are refused) | 400 | `invalid_request_error` | `hosted_tool_unsupported` |
  | A Messages request to an `anthropic` or `azure-anthropic` deployment asks for a price option the gateway does not price (`param` names it; Providers → Standard price on Anthropic types) | 400 | `invalid_request_error` | `price_option_unsupported` |
  | No deployment of the model is on a backend that serves the endpoint (Providers → Endpoint support) | 400 | `invalid_request_error` | `endpoint_not_served` |
  | Path matches no endpoint | 404 | `invalid_request_error` | `unknown_url` |
  | Known path, wrong method (`Allow` header set) | 405 | `invalid_request_error` | `method_not_allowed` |
  | Backend unreachable: connect refused or timed out, DNS, TLS, connection lost before the first event | 502 | `server_error` | `upstream_unavailable` |
  | Backend refused the gateway's own credential (backend `401`/`403`) | 502 | `server_error` | `upstream_auth_failed` |
  | Backend answered `404` saying the deployment's model does not exist there (Providers: wrong model on a host) | 502 | `server_error` | `upstream_model_missing` |
  | Backend answered `404` the way its server answers a path it does not have — the backend's `base_url` is wrong (Providers: wrong path to a host) | 502 | `server_error` | `upstream_path_missing` |
  | The same answer on an endpoint beyond the type's core ones: the server version lacks an endpoint its type serves (Providers → An endpoint missing from a server) | 502 | `server_error` | `upstream_endpoint_missing` |
  | A stream got no first event within the backend's first-event timeout, or a non-stream response did not arrive within its response timeout | 504 | `server_error` | `upstream_timeout` |
  | Backend answered a `5xx` (its error code and type logged, its text neither logged nor relayed; its `Retry-After` kept) | the backend's `5xx` | `server_error` | `upstream_error` |
  | Backend answered `529` (Anthropic's `overloaded_error`), or a successful stream's first event was an error event naming the backend busy (settled 2026-10-06; Providers: error events) | `529`, or `503` for the event | `server_error` | `upstream_overloaded` |
  | A successful stream's first event was an error event naming the request's own fault; the message names the backend's code when it is a plain identifier (settled 2026-10-06; Providers: error events) | 400 | `invalid_request_error` | `upstream_refused` |
  | Gateway fault building the upstream request | 500 | `server_error` | `internal_error` |
  | Client left before any answer (never sent: the request line carries no status, the request metric counts it as `4xx`) | 499 | `invalid_request_error` | `client_closed` |
  | A requests-per-minute limit is full | 429 | `requests` | `rate_limit_exceeded` |
  | The key already has `global.max_concurrent_requests_per_key` requests in flight on this gateway (`Retry-After: 1`; Limits → Per-key concurrency) | 429 | `requests` | `concurrency_limit_exceeded` |
  | A token limit has no room for the reservation | 429 | `tokens` | `rate_limit_exceeded` |
  | A USD limit is spent | 429 | `budget` | `budget_exceeded` |
  | No eligible deployment has a free slot and the model's queue is full or has size 0 (`Retry-After: 1`) | 429 | `server_error` | `queue_full` |
  | Waited the model's queue timeout without a slot | 429 | `server_error` | `queue_timeout` |
  | Every deployment of the model has its circuit open | 503 | `server_error` | `no_healthy_deployment` |
  | The body does not fit in what the requests in flight leave of the body budget (`Retry-After: 1`; Request pipeline → request bodies) | 503 | `server_error` | `server_busy` |
  | The gateway is draining and refuses new requests; or a request the drain cut off before its response started | 503 | `server_error` | `server_shutting_down` |
  | No config in force (the admission stage's guard; the binary binds its listeners only once a config is in force, in both modes) | 503 | `server_error` | `config_not_loaded` |
  | A USD limit covers the request's priced model and the control plane has been out of reach past `control_outage_grace_ms` (no stream bytes, or usage waiting that long for an answer or to be shown counted), or no totals have arrived since the start (control-plane mode) | 503 | `server_error` | `budget_unavailable` |

  Upstream error messages never name the backend or its address; the log line does
  (settled 2026-09-24). A client string an error message echoes (the method, the
  path, the model name) is clipped to its first 256 bytes, then `…` (settled
  2026-09-25, L3).
  A `401` carries `WWW-Authenticate: Bearer`. **Owned fields**, per format, read by
  exact key; a `null` value counts as absent:
  - OpenAI: `model`, `stream`, `max_tokens`, `max_completion_tokens`,
    `stream_options.include_usage`, `n` and `best_of` (chat and completions),
    `prompt` (completions; only how many prompts it holds), `input` (embeddings;
    only how many inputs it holds).
  - Messages (settled 2026-10-06): `model`, `stream`, `max_tokens`,
    `thinking.budget_tokens` (read only), each `tools[].type`, `mcp_servers`,
    `container`, the content blocks' `source` type and `file_id`; on Anthropic types also
    `service_tier` (on `anthropic`), `speed`, `inference_geo` and every `cache_control.ttl` (Providers
    → Standard price on Anthropic types).
  - Responses (settled 2026-10-06): `model`, `stream`, `max_output_tokens`, `store`,
    `previous_response_id`, `conversation`, `prompt`, `background`, each
    `tools[]`'s `type` and a shell's `environment`, `tool_choice`, the input items'
    `type`, `role` and `tools`, and their parts' `file_id`, and `service_tier` on
    `openai` and `azure-openai`.
  - The token-counting endpoints own `model` and the tool fields of their format,
    and `responses/input_tokens` the stateful fields too, refused as on
    `/v1/responses` (settled 2026-10-06: a stored response or conversation the
    gateway never lets a backend keep could only be another client's); they take no
    output limit and no `stream`.
- **Request IDs**: `x-request-id` is accepted (or generated), forwarded to the backend,
  returned to the client, and stamped on the log line and the usage record. A client
  ID is kept when it is 1–128 characters of `[A-Za-z0-9._:-]`; any other value is
  replaced by a generated one (32 hex characters, random) rather than refused.
- **`/v1/models`** lists only the models the calling key may use, sorted by name:
  `{"object": "list", "data": [<entry>, …]}`. **`/v1/models/{id}`** answers one entry.
  An entry keeps the OpenAI fields and adds kaiak's; OpenAI clients ignore unknown
  fields (settled 2026-09-24):

  ```json
  {"id": "qwen3-32b", "object": "model", "created": 0, "owned_by": "kaiak",
   "context_length": 32768,
   "capabilities": {"streaming": true, "tools": true, "vision": false, "reasoning": true},
   "reasoning_efforts": ["low", "high"],
   "endpoints": ["chat_completions", "completions", "embeddings", "messages",
                 "messages_count_tokens", "responses", "responses_input_tokens"]}
  ```

  `id` is the public model name; `created` is always `0` — models are declared, not
  created at a moment the gateway knows, and a fixed value keeps listings identical
  across reloads and replicas; `owned_by` is always `"kaiak"`; `reasoning_efforts` is
  `[]` when none are declared. `endpoints` (settled 2026-10-06) lists the body
  endpoints some deployment of the model serves (Providers → Endpoint support), by
  their metric names, sorted — how a client sees which API reaches the model. It is
  information, derived from the config; it says nothing about health.
- **Anthropic-shaped model list** (settled 2026-10-06): `GET /v1/models` and
  `GET /v1/models/{id}` with an `anthropic-version` request header (Anthropic's SDKs
  always send it; OpenAI's never do) answer in Anthropic's shape, over the models
  the key may use **whose `endpoints` include `messages`**:
  `{"data": [<entry>, …], "has_more": false, "first_id", "last_id"}` (the ids `null`
  for an empty list), an entry `{"type": "model", "id", "display_name", "created_at",
  …kaiak's fields}` with `display_name` the public name and `created_at`
  `"1970-01-01T00:00:00Z"` (the fixed `created` above). One page, always:
  `limit`, `before_id` and `after_id` are ignored. A model without Messages answers
  `404 model_not_found` here, as an unknown one does. Errors take the Messages shape.
- **`/v1/models/{id}/props`** is the entry plus `output_limit` — `{"default": n,
  "ceiling": n}`, or `null` when the model declares none — what the gateway applies
  to a request's output (settled 2026-09-24; Model metadata → no model defaults).

## Model metadata

- **Declared** in config; the gateway does no discovery from backends and serves what
  config says. Filling the declaration in is the control plane's job:
  `kaiak-control`'s `verifyBackend` (`docs/specs/BACKEND-VERIFY.md`) reads what a
  backend reports when an operator adds it or a model (settled 2026-09-29).
  Gateway-side discovery is not planned.
- **Metadata is information** (settled 2026-10-06): `context_length`,
  `capabilities` and `reasoning_efforts` tell clients what a model is; the gateway
  acts on `context_length` alone — an output limit above it is refused and the
  injected default is fitted under it (Limits). The output limit (`output_limit`,
  default and ceiling) is the one request parameter a model's config sets.
- **No model defaults** (settled 2026-10-06): the gateway sets no other request
  parameter. Each backend applies its own defaults — vLLM from the model's
  generation config, llama-server from its flags, cloud APIs their own — and the
  config has no `defaults` field. Rejected: keeping declared defaults — they are
  parameter names of one client API (`reasoning_effort` in chat, `reasoning.effort`
  in Responses, `output_config.effort` or `thinking` in Messages), so a model served
  in three formats would need three declarations or a mapping between them, which
  is translation; and the backends already own defaults. What an app wants to show
  as suggested settings belongs to the app. The cost: no gateway-side lever for a
  cloud model's settings (a lower reasoning effort, say) — the client sets them.

## Request pipeline

Every request passes the same ordered stages — the seam where caching, guardrails and
prompt logging later plug in:

0. **Admission** — once a drain refuses new requests (Lifecycle), they end here with
   `503 server_shutting_down`, and with no config in force with `503
   config_not_loaded` (a guard: the binary serves only once a config is in force),
   before anything is read (settled 2026-09-24).
1. **Auth** — resolve key → key ID, group and its path. Runs on headers alone, before any body is
   read, so an unauthenticated client never makes the gateway buffer a body (settled
   2026-09-24). Then the key's **concurrency limit** (Limits → Per-key concurrency,
   settled 2026-09-25): the request counts against its key from here, still before
   the body is read, to its end.
2. **Inbound format** — parse the request in its endpoint's format (Client API →
   Client APIs: OpenAI, Messages or Responses; settled 2026-10-06, each format one
   plug): read the body up to the cap (request bodies, below), parse only the fields
   that format's owned fields name, keep the raw bytes for passthrough, refuse what
   the gateway does not serve (stateful Responses fields, stored objects, hosted
   tools). Then the model-access check (Client API) — the second half of auth, which
   needs the model — and whether any of the model's deployments is on a backend that
   serves the endpoint (Providers → Endpoint support; none: `400
   endpoint_not_served`, settled 2026-10-06 here, before limits — the pre-merge
   review's L2: a request no deployment can answer spends no request slot and gets
   the answer that says what is wrong), and the model's output limit (Model
   metadata, Limits), which fixes the request's effective output limit before limits
   reserve it.
3. **Limits** — check every applicable scope; reserve the output limit (below). The
   reservation settles in a request finisher that runs after accounting's settlement
   and reads its usage record (settled 2026-09-24).
4. **Routing** — resolve alias → deployment, among the deployments whose backend
   serves the endpoint (Providers → Endpoint support), less those whose server was
   found lacking it within the probe interval (Providers → An endpoint missing from a
   server); queue for a concurrency slot.
5. **Provider** — send upstream. Every request passes through in its own format
   (principle 4); translating providers (Bedrock) are later plugs.
6. **Accounting** — settle usage, compute cost, emit metrics and a usage record (from
   which limits settle their reservation). The stage sits between routing and the provider — it opens the
   request's meter, which the relay feeds — and settles in a request finisher, so it
   runs however the request ends (settled 2026-09-24).

Stages 4–6 run as one **attempt loop** (settled 2026-09-24): each attempt takes a slot
(routing, the queue included), opens its meter (accounting) and sends upstream
(provider); an attempt that failed before anything reached the client is retried
through all three again (Routing and reliability: retries). Stages 0–3 run once per
client request — one limits reservation, however many attempts.

**Request bodies** (settled 2026-09-25; the audit's M2). A body is capped by
`global.max_request_body_bytes` (default 4 MiB: chat and completion bodies are KiB
to hundreds of KiB; 4 MiB still carries long contexts and base64 images, and a
larger default only raises what one request can pin). Every body also takes its
size from one gateway-wide **body budget**, `KAIAK_BODY_MEMORY_BYTES` (default
512 MiB — 128 bodies at the default cap, thousands of ordinary requests): taken
step by step as the body arrives — the buffer doubles from 4 KiB, each step taken
before it is allocated, capped at the declared `Content-Length` when there is one
(settled 2026-09-25, the follow-up audit's N-S1: taking the declared length up
front let 128 connections of one valid key declaring 4 MiB and sending nothing
hold the whole default budget for the body-read deadline, every other client
answered `503 server_busy`; now such a connection holds at most 4 KiB, and the
per-key concurrency limit — Limits — bounds how many one key opens) — and given
back when the body is dropped: as soon as a
response starts relaying (no retry can use the body after that; the input
estimate was taken when the attempts opened), or when the request ends. A body
that does not fit in what is left is refused at once, `503 server_busy` with
`Retry-After: 1`, its body unread (the connection closes after the answer).
Rejected: waiting for room — a wait bounded by the body-read deadline holds a
connection and adds latency exactly when the gateway is overloaded, and refusing
at once is simpler to reason about and to retry. A body larger than the whole
budget could never be held: the effective cap is the smaller of the two, and a
config whose cap exceeds the budget is applied with a warning in the log (a
control-plane config cannot fail the gateway's start, so the check is not a
startup error). The provider's edited copy of the body is dropped once the first
event is in; the budget counts the client's body only, so peak body memory is
bounded by about twice the budget (the edited copy while an attempt is sent;
parsing makes short-lived garbage too). Outside the budget, every non-stream answer
in flight keeps its `choices` (up to 4 MiB each) to estimate output when usage is
missing — bounded by those answers in flight, not by the budget. Size `GOMEMLIMIT`
above both.

**Duplicate members** (settled 2026-09-25; the independent audit's finding 2). A
body whose top-level object names a member twice — or whose `stream_options`, the
one owned object the provider edits, does; or a Messages `tools` entry naming its
`type` twice (settled 2026-10-06: the hosted-tool check and the backend could read
different types) — is refused, `400 duplicate_member`,
`param` naming the member (clipped like any echoed client string), before any
rewrite and before the backend sees anything. Names compare after decoding
(`"model"` and `"mod\u0065l"` are one). JSON leaves a repeat's meaning open: the
gateway and the backend could read different occurrences, and editing every
occurrence let 10 000 short `"model"` members become 10 000 long deployment names
(43.6× the body, outside the body budget). With one member per name the provider's
edited copy is at most the body plus one member per owned field. Repeats deeper in
the body (inside `messages`, say) are the backend's business and pass untouched.
Rejected: collapsing repeats to one member — it rewrites bytes the gateway does not
own, and a client sending repeats is broken either way.

## Providers

- **Backend types**, one per server (OpenAI streams get `stream_options.include_usage`
  set so the final chunk reports tokens):
  - **openai** — OpenAI's API.
  - **azure-openai** — Azure's OpenAI-compatible `/openai/v1/` API: the same wire
    format and model naming, `api-key` header auth, no `api-version`. The classic
    deployment-in-URL API is not supported.
  - **vllm** — vLLM.
  - **llama-server** — llama.cpp's server.
  - **anthropic** — Anthropic's API (settled 2026-10-06).
  - **azure-anthropic** — Claude in Microsoft Foundry: Anthropic's Messages API on
    an Azure resource, `https://{resource}.services.ai.azure.com/anthropic/v1/*`,
    with the Foundry deployment name as the model (settled 2026-10-06). Claude
    models only; OpenAI models on Azure are `azure-openai`.
  - **openai-compatible** — the generic type: any other server speaking the OpenAI
    format (SGLang, …). It carries no server's own rules and serves OpenAI's three
    endpoints only: a server that serves Messages or Responses gets a type of its
    own (A type per server, below).
- **Endpoint support** (settled 2026-10-06) is fixed per backend type, by its module
  — no config field. What each type serves, checked against the servers on
  2026-10-06 (vLLM 0.30.0's route list; llama.cpp builds b9917 and b10802, each route
  called;
  OpenAI's and Anthropic's API references; Microsoft Foundry's Claude page;
  `docs/plans/messages-responses/STEP-1-contract.md` records the sources):

  | Type | chat, completions, embeddings | messages | messages_count_tokens | responses | responses_input_tokens |
  |---|---|---|---|---|---|
  | `openai` | yes | — | — | yes | yes |
  | `azure-openai` | yes | — | — | yes | — (Azure's v1 API answers `404` for it) |
  | `vllm` | yes | yes | yes | yes | — (not in 0.30.0) |
  | `llama-server` | yes | yes | yes | yes | yes |
  | `anthropic` | — | yes | yes | — | — |
  | `azure-anthropic` | — | yes | yes | — | — |
  | `openai-compatible` | yes | — | — | — | — |

  Routing takes only the deployments whose backend's type serves the request's
  endpoint (Request pipeline, stage 4); a type gains an endpoint when its server
  does, in its module. Rejected: a per-backend `endpoints` config field — the
  operator would declare what the type already knows, and `openai-compatible`
  servers that serve more deserve their own type.
- **A module per backend type over one OpenAI wire core** (settled 2026-09-29): each
  backend type is a module that is the provider for that type — its URL layout,
  credential, body edits, what its models list says, which error code means a
  missing model. The modules build on a shared core of helpers for what the OpenAI
  format has in common: body editing, sending, streaming, usage, timeouts, error
  mapping, reading a models list. The core is not a provider, knows no backend type
  and reads no per-type flags; the registry's choice of module is the one place a
  type is named. Rejected: one provider reading a per-type description — the shared
  code still branched on flags that were one type's behavior; a full provider per
  type — it would copy the core, where nearly all the complexity is.
- **A type per server** (settled 2026-09-30): each server kind the gateway knows has
  its own type and module, even while its module behaves as `openai-compatible`
  does; `openai-compatible` is left for servers without one. A module is
  self-contained over the wire core — no module embeds another or reads another's
  type — so a fix for one server never reaches another. Replaces the 2026-09-29 rule
  that a new type is added only when a real difference needs one: with vLLM,
  llama-server and OpenAI under one type, a rule meant for one server landed on all
  (OpenAI's service tier reached every self-hosted server), and a fix for one had
  nowhere to go — the 2026-09-30 review found three (llama-server's usage and error
  quirks, `docs/BACKLOG.md`). Rejected: keeping one type until a difference needs
  its own — the differences were already there, and a fix made inside the generic
  module reaches every server under it. Adding a type changes neither the config
  format nor the protocol version (`CONTROL-PROTOCOL.md`, Config → Backend types).
- **Base URLs** (settled 2026-09-24): an `openai-compatible`, `openai`, `vllm` or
  `llama-server` `base_url` is what an OpenAI client would use, API version path
  included (`http://vllm:8000/v1`, `https://api.openai.com/v1` — no type has a default
  URL); the gateway appends the endpoint path (`/chat/completions`, …). An
  azure-openai `base_url` is the resource endpoint
  (`https://<resource>.openai.azure.com`); the gateway appends `/openai/v1/` and the
  endpoint path. An `anthropic` `base_url` is what an Anthropic client would use with
  the version path, `https://api.anthropic.com/v1`; an `azure-anthropic` `base_url` is
  the resource endpoint (`https://<resource>.services.ai.azure.com`) and the gateway
  appends `/anthropic/v1/` (settled 2026-10-06, the two Azure types alike). The
  endpoint paths are `chat/completions`, `completions`, `embeddings`, `messages`,
  `messages/count_tokens`, `responses`, `responses/input_tokens`, `models`.
- Credentials are referenced by environment-variable name in config, never inline:
  azure-openai and azure-anthropic send `api-key: <value>`, anthropic `x-api-key:
  <value>`, every other type `Authorization: Bearer <value>` (nothing when the backend
  has no `api_key_env`). `openai`, `azure-openai`, `anthropic` and `azure-anthropic`
  require `api_key_env` (schema, both halves): none answers without a key. The two
  Anthropic types also send `anthropic-version: 2023-06-01`, the API's one version
  (settled 2026-10-06). An `api_key_env` starting with `KAIAK_` is refused by the schema, both halves
  (settled 2026-09-25, N-S2): those variables hold the gateway's own settings and
  tokens (`KAIAK_CONTROL_TOKEN`, `KAIAK_METRICS_TOKEN`), and a backend credential is
  sent to the backend's URL — which the config author chooses, and the background
  model check calls with no client request. The provider refuses such a name again
  where it reads the credential (settled 2026-09-25, the independent audit's finding
  1: a config repeating `backends` hid one from the schema): a backend naming a
  `KAIAK_` variable gets no credential at all, so the value never leaves. **`OTEL_`
  is reserved the same way** (settled 2026-10-05, the 2026-10-05 review's H1) — by
  the schema, both halves, and again by the provider: the log export's headers
  (`OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_EXPORTER_OTLP_LOGS_HEADERS`) carry the
  collector's credentials, and a models probe alone would have sent them to the
  author's URL. The whole prefix, since endpoints can carry credentials too
  (`CONTROL-PROTOCOL.md`, Config → `api_key_env`).
- **Backend model names** (settled 2026-09-25, E12): a deployment's `model` is the
  model name in the backend's own naming (`backend_model_name`: 1 to 512 printable
  ASCII characters, no spaces), so llama-server's path-style ids
  (`/models/qwen3-embedding-0.6b-q8_0.gguf`) work as they are listed; public model
  names keep their stricter shape. The alternative on llama-server is `--alias
  <name>`, which makes the models list and requests use that name. A name outside the
  rule (spaces, non-ASCII) needs an alias. The probe matches the name whole against
  the models list; the `404` check below finds it as a whole word (path characters
  included).
- The provider interface must fit a translating provider (Bedrock: different request
  shape, binary stream framing, request signing) without changing the pipeline. It
  does (settled 2026-09-24): the pipeline hands a provider the client's request as
  received plus the routing decision, and gets back status, headers and a sequence of
  events already in the client's format; relaying and observing those events is the
  pipeline's job, the same for every provider. A provider can also refuse a request
  as the caller's mistake before sending it (settled 2026-10-06: Standard price on
  Anthropic types, below) — an answer the pipeline gives as a `400`, never retried,
  never counted toward the circuit.
- **Passthrough edits** (settled 2026-09-24; per format 2026-10-06) are the only
  changes to the client's body: `model` becomes the deployment's model name; the
  output limit is set as the Limits section says; on `openai` and `azure-openai`,
  `service_tier` becomes `"default"` (Service tier, below); an OpenAI stream whose
  client did not set `stream_options.include_usage: true` gets it set (other
  `stream_options` members kept) — Messages and Responses always report usage, so
  they get no such edit; a Responses request gets `store: false` (Client API →
  Responses is stateless); a Messages request to an `anthropic` backend gets
  `service_tier: "standard_only"` (Standard price on Anthropic types). Edits splice the owned values into the raw bytes — every
  other byte (unknown fields, their values, key order, whitespace) reaches the backend
  unchanged. Each owned key is edited once: a body repeating a top-level member never
  gets here (Request pipeline → duplicate members), and the editor refuses a repeated
  key it edits as a gateway fault (settled 2026-09-25).
- **Service tier** (settled 2026-09-29; by type 2026-09-30): on `openai` and
  `azure-openai`, every request runs on the backend's standard tier. Prices are
  standard-tier rates, and OpenAI and Azure bill priority processing at about twice
  that (flex at half), so a client choosing its tier would spend budgets at a rate
  its records do not show. A `service_tier` the client sent becomes `"default"`, on
  every generating endpoint, and every chat completions and Responses request
  carries `"service_tier": "default"` even when the client sent none (Responses
  settled 2026-10-06, the same reasoning): an absent tier (`auto`) means the
  deployment's own setting on Azure (which may be priority) and the project's on
  OpenAI. Completions and embeddings requests without one are left without one:
  OpenAI refuses a parameter an endpoint does not define. `responses/input_tokens`
  is left as the client sent it — nothing is generated or billed there.
  `openai-compatible`, `vllm` and `llama-server` pass the client's `service_tier`
  untouched and add none (settled 2026-09-30): no tier is billed there, and a strict
  server that refuses unknown fields would refuse one it did not ask for. An OpenAI
  backend configured as `openai-compatible` therefore runs on the tier the client
  asks for, or the project's — OpenAI takes the `openai` type. Rejected: forcing the tier on `openai-compatible` too, for
  OpenAI backends left under the generic type — the generic type would carry one
  server's rule to every other; removing the client's field (`auto` again); adding it
  on every endpoint (OpenAI's completions and embeddings would refuse it); Azure's
  `x-ms-service-tier` header (one body edit covers both backends; client headers are
  never forwarded, so a client cannot send it either); pricing the tier the response
  reports — per-tier price tables and a rule for who may ask for priority, not built
  until a deployment needs priority or flex.
- **Standard price on Anthropic types** (settled 2026-10-06): the `anthropic` and
  `azure-anthropic` modules refuse, before sending, every Messages option that bills
  above the standard rates the price table holds — `400 price_option_unsupported`,
  `param` naming it (a caller's mistake: never retried, never a circuit failure):
  - `speed` other than `"standard"` (fast mode: about 2× the standard price; Claude
    API only);
  - `inference_geo` other than `"global"` (`"us"`: 1.1× on Claude 4.6 and later; the
    Claude API only — Foundry has no such parameter, its US Data Zone is a deployment
    type and its price a deployment matter, `docs/BACKLOG.md` → prices per deployment
    or region);
  - a `cache_control` with `ttl: "1h"` at the top level or anywhere under `system`,
    `messages` or `tools`, at any depth — a tool result's content, a document's
    content source, a search result's content (settled 2026-10-06, the pre-merge
    review's M2) — `param` its path (`messages[0].content[0].content[0].cache_control.ttl`).
    A 1-hour cache write costs 2× input, a 5-minute one 1.25×, and
    `tokens_cache_write` is one unit. A `cache_control` or `ttl` named twice in one
    object is refused as `400 duplicate_member`: the gateway and the backend could
    read different ones. One streaming pass over the body, nothing copied.

  `messages/count_tokens` is neither refused nor edited: nothing is generated or
  billed there (settled 2026-10-06, as `responses/input_tokens` keeps its tier).
  The `anthropic` module also sets `service_tier: "standard_only"` on every Messages
  request, the client's value replaced: `auto`, the API's default, draws on Priority
  Tier capacity where the organization has a commitment, billed outside the price
  table. `azure-anthropic` adds none and passes the client's untouched: Foundry has
  no Priority Tier. Self-hosted types pass all of these untouched — they price
  nothing and accept the fields (vLLM 0.30.0, llama-server b9917 and b10802, checked
  2026-10-06). Rejected: refusing them on every backend in the inbound stage — a
  client like Claude Code that asks for 1-hour caching would then fail on vLLM, where
  the option means nothing; pricing them — new units and per-request multipliers,
  not built until a client needs one (`docs/BACKLOG.md`). An operator workspace whose
  `default_inference_geo` is `"us"` bills 1.1× without any request asking: that is
  the deployment's price, set in the price table.
- **Usage chunk** (settled 2026-09-24; OpenAI streams only): when the gateway set
  `include_usage` for a client that did not ask, the usage-only chunk (`choices: []`, non-null `usage`) is
  withheld from the client; accounting still sees it. Other chunks may carry
  `"usage": null` (OpenAI adds it once `include_usage` is set); they pass as they are.
- **Headers** (settled 2026-09-24), both ways an allowlist:
  - to the backend: `Content-Type: application/json`, `Accept`, `User-Agent: kaiak`,
    `X-Request-Id`, the backend credential, and on Anthropic types
    `anthropic-version`. No client header is forwarded — the client's
    `Authorization` or `x-api-key` (its kaiak key), cookies and vendor headers never
    reach a backend. That includes Anthropic's `anthropic-beta` and the client's
    `anthropic-version` (settled 2026-10-06): a beta can change what a request does
    and costs on an Anthropic backend, and which ones to let through is an open
    decision (`docs/BACKLOG.md`).
  - to the client: the backend's `Content-Type`, `Retry-After` and `Retry-After-Ms`
    (Azure's, in milliseconds; settled 2026-09-25, L1 — the rest of Azure's
    rate-limit headers stay behind, the gateway's own `x-ratelimit-*` are its
    limits'), plus the gateway's
    own `X-Request-Id`, and on streams `X-Accel-Buffering: no` so nginx-style proxies
    in front of the gateway pass events through instead of buffering them. Backend request IDs, cookies, server and rate-limit headers
    stay behind (the gateway's own rate-limit headers are its limits').
- **Relaying** (settled 2026-09-24): a response is a stream when its content type is
  `text/event-stream`; its events are parsed (WHATWG SSE framing), forwarded byte for
  byte and flushed one at a time. Other bodies are relayed as they are read.
- **Response model name** (settled 2026-09-24): responses carry the public model name
  the client asked for, never the backend's (a deployment name, or Azure's dated model
  version): the top-level `model` string of a JSON body and of every stream chunk is
  replaced; every other byte passes as the backend sent it. JSON bodies are edited
  while they stream through (never collected whole — embeddings answers can be
  large); a nested `model`, a non-string value and non-JSON bodies pass unchanged.
  Observers (accounting) read stream payloads as the backend sent them. Where a
  format's stream carries the model one level down (settled 2026-10-06), that one is
  replaced as well: `message.model` in a Messages `message_start` event, and
  `response.model` in a Responses lifecycle event (`response.created`,
  `response.queued`, `response.in_progress`, `response.completed`,
  `response.incomplete`, `response.failed`) — only there, by the event's `type`
  (settled 2026-10-06, the pre-merge review's [B] L3): an unknown or extension
  event's nested members pass untouched. A Messages or Responses JSON body carries it
  at the top level, as OpenAI's does.
- **The first event of a stream is its first data event** (settled 2026-10-06, the
  pre-merge review's [B] M3): comment and keep-alive blocks the backend sends ahead
  of it (up to 64) are held and relayed just before it, so the first-event window —
  the first-event timeout, a retry of an error event, the client's headers held
  back — lasts until the backend has said something. Rejected: taking a comment as
  the first event — a backend pinging before an error event made the error
  unretryable, and its silence afterwards a stall rather than a first-event timeout.
- **Upstream failures** (settled 2026-09-24; timeouts and completeness 2026-09-25):
  before the first event, failures are gateway errors (Client API table) — a
  stream's first-event timeout runs until its first event (not just response
  headers), a non-stream response's response timeout until its body ends (Routing and
  reliability: timeouts). A backend `4xx` is relayed as is, body included
  (backends answer in their API's error shape, mostly — vLLM 0.30.0 answers a
  Messages request that fails validation in OpenAI's; relayed as it came, settled
  2026-10-06), except `401`/`403`: the client
  authenticated to the gateway, so a refused backend credential is the gateway's
  fault (`502 upstream_auth_failed`). **Backend error bodies** (settled 2026-09-25,
  L5): a `4xx` is the caller's actionable error (context too long, a bad parameter)
  and passes untouched — its text may name backend internals (the server's own
  limits, a parameter it rejects, its version); that is accepted, as rewriting it
  would take the fix away from the caller. A `5xx` is the backend's fault and
  nothing the caller can act on, while its text can name hosts, devices or stack
  traces: the gateway answers its own `upstream_error` under the backend's status
  and `Retry-After`/`Retry-After-Ms`, and logs only the backend's error `code` and
  `type` when its body names them (`kaiak.upstream.error.code`, `kaiak.upstream.error.type`:
  identifiers of letters, digits and `_.:-` or an integer code, at most 64 bytes;
  at most 4 KiB of the body is read). A relayed `4xx` logs the same two fields,
  read from its first 4 KiB as they pass (settled 2026-09-25, D6: `400
  context_length_exceeded` and a backend's `429` are daily questions, and the
  line held only the status). Rejected: relaying `5xx` bodies (they leak
  internals to every caller); mapping the status to `502` (the backend's `503` plus
  `Retry-After` is a meaningful answer); logging the start of the backend's text
  (settled 2026-09-25 — a backend can echo request content in its message, and logs
  never hold prompt or response content). **An error answer broken before its
  body** (settled 2026-09-25, the independent audit's finding 3): a `4xx`/`5xx`
  whose body breaks before its first byte — the connection lost, or a first-event
  or response timeout after the headers — is still the backend's answer, because
  the status came with the headers: accounting settles it as answered (no units —
  not the estimated input of a request left unanswered), retries and the circuit go
  by its status (a `429` or a `5xx` fails over to another deployment — a `429`
  also starts the deployment's cooldown — a caller's `4xx` is not retried), and the client gets the gateway's
  `upstream_error` under the backend's status and `Retry-After`/`Retry-After-Ms`
  (nothing of the body can be relayed). A `2xx` broken before its first event stays
  a connection lost (`upstream_unavailable`). After the first event, a backend failure cuts
  the client connection rather than ending the body cleanly, so a truncated response
  never looks complete; the log line records `kaiak.relay_end`: `upstream_failed` (the
  connection broke), `upstream_stalled` (a stream silent for the stall timeout),
  `upstream_incomplete` (a successful response ended before it was complete, below),
  `upstream_timeout` (a non-stream body still arriving at its response timeout).
- **Wrong model on a host** (settled 2026-09-25; the audit's H8): a backend `404`
  whose error says the deployment's model does not exist is the deployment's
  failure, not the caller's — the host serves another model than the config says
  (a restart with other flags, a mistyped name). It answers `502
  upstream_model_missing` (the backend's text is not relayed: it names the
  backend-side model), is retried on another deployment (failover, as every retry),
  counts toward the circuit and produces no usage (nothing was
  processed). Matched conservatively, over the error shapes servers use
  (`{"error": {"message", "code"}}` — OpenAI, vLLM; `{"message", "code"}` — older
  vLLM; `{"error": "<text>"}` — Ollama; Anthropic's `{"type": "error", "error":
  {"type", "message"}}`), by each module's rule:
  - the self-hosted types (`openai-compatible`, `vllm`, `llama-server`): the message
    names the deployment's backend-side model as a whole word (vLLM: ``The model `…`
    does not exist.``, in either format — vLLM's Messages endpoints answer it in
    Anthropic's shape, type `NotFoundError`, run on 0.30.0), or the code is
    `model_not_found`;
  - the cloud types, by structured fields only (settled 2026-10-06, the pre-merge
    review's M1): `openai`, code `model_not_found`; `azure-openai` (OpenAI's shape),
    `DeploymentNotFound` or `model_not_found`; `anthropic`, a `not_found_error` whose
    message begins `model:`; `azure-anthropic`, that or `DeploymentNotFound` — the
    Anthropic two from the documentation, not verified live. Their messages are not
    read: a request can make these APIs echo an ID it chose in a `404` (a Responses
    `item_reference`, a `file_id` — refused anyway, Client API → stored objects), and a
    client naming the backend-side model there would open the deployment's circuit.
    Rejected: the whole-word match on every type — five such requests could open
    every deployment of a model.

  Any other
  `404` stays the caller's and is relayed as it came. The probe checks the model too
  (Routing and reliability → Circuit mechanics), and every applied config is checked
  once in the background: each backend's models list is fetched (up to 8 backends at
  once, off the request path, never delaying or refusing the config) and each
  deployment whose model is not listed gets a warning
  (`the backend does not list the deployment's model`: backend, deployment model);
  one whose models list answers `404` gets a warning (Wrong path to a host, below).
  A backend that does not answer at all (DNS failure, connection refused, timeout,
  any other failure) gets a warning too, `model check skipped: the backend did not
  answer` (backend, error — never a credential) (settled 2026-10-01; the 2026-09-30
  review's O4): config apply is infrequent, and a backend out of reach then — often
  a mistyped host — is what the operator needs to see. Rejected: info — the line
  read as routine noise. A server that ignores the request's
  model name (llama-server) must still be configured with a name it lists.
  Rejected: relaying the `404` — the client would read its own model name as wrong,
  and a fast `404` made the wrong host the least loaded one, so it drew traffic.
- **Wrong path to a host** (settled 2026-10-01; the 2026-09-30 review's O1): a
  backend `404` that the backend's module recognizes as its server's answer to a
  path it does not have is the deployment's failure, as a missing model is — the
  backend's `base_url` is wrong (typically the API version path left out). It answers
  `502 upstream_path_missing` (the backend's text is not relayed), is retried on
  another deployment, counts toward the circuit and produces no usage, with its own
  retry reason and attempt outcome, `path_missing`. Like a refused credential it
  belongs to the backend, not the deployment: every deployment of the model on that
  backend shares its `base_url` and would hit the same wrong path, so all of them are
  refused for the request's retries (a missing model stays per deployment). A missing
  model is read first.
  The signatures, per module:
  - `vllm`: `{"detail": "Not Found"}` — FastAPI's answer to a route it does not
    have. vLLM's own errors take the OpenAI shape; its handler is registered for
    FastAPI's `HTTPException`, which the router's Starlette `404` does not match, so
    the framework's default answer stands (vLLM's source, `main` on 2026-10-01; run
    on vLLM serving Qwen3.8-27B-NVFP4, 2026-10-01).
  - `llama-server`: `{"error": {"message": "File Not Found", "type":
    "not_found_error", "code": 404}}` (run on build 11146). Its HTTP layer gives
    every `404` that body, so in router mode a model the server does not have reads
    as a wrong path too — the deployment's failure on the core endpoints; on
    Messages and Responses the same body reads as an endpoint missing (below),
    neutral for the circuit. llama-server serves
    chat completions and its models list without `/v1` as well, so a `base_url`
    missing it gives no `404` there (`docs/BACKLOG.md`, llama-server quirks).
  - `openai`: an `invalid_request_error` whose message starts `Invalid URL` (`Invalid
    URL (POST /chat/completions)`) — OpenAI's known answer, not verified live.
  - `azure-openai`: `{"error": {"code": "404", "message": "Resource not found"}}` —
    the resource's answer to a path it does not have, not verified live.
  - `anthropic` (settled 2026-10-06): a `not_found_error` in Anthropic's shape whose
    message is `Not Found` — the API's answer to a route it does not have, from the
    documentation, not verified live.
  - `azure-anthropic` (settled 2026-10-06): `{"error": {"code": "404", "message":
    "Resource not found"}}`, the Azure resource's answer as for `azure-openai`, not
    verified live.
  - `openai-compatible`, whose server is unknown: a `404` whose body is not an
    OpenAI-shaped error (`{"error": {...}}` or `{"error": "<text>"}`) — plain text,
    HTML, other JSON (older vLLM's top-level `{"message": …}` included), an empty
    body. A server speaking the OpenAI format answers in that shape; anything else
    came from below the API — a web framework, a proxy.

  Any other `404` stays the caller's, relayed as it came. The check at config apply
  (Wrong model on a host) logs a models list answering `404` as a warning, `the
  backend has no models list at its base_url` (backend, `base_url`, `hint`): the
  hint says `base_url` should end in the API version path (e.g. `/v1`), or, for
  `azure-openai`, that it is the resource endpoint with no path (the gateway adds
  `/openai/v1`). Rejected: folding it into `upstream_model_missing` and its label —
  one alert either way, but the operator could not tell a wrong URL from a wrong
  model; reading every `openai-compatible` `404` as the deployment's — a caller's
  `404` (an unknown adapter) would fail over and open the circuit.
- **An endpoint missing from a server** (settled 2026-10-06): each type has core
  endpoints — OpenAI's three for the OpenAI-format types, `messages` for the
  Anthropic types — whose wrong-path answer means a wrong `base_url` (above). On any
  other endpoint the type serves (Messages and Responses on vLLM and llama-server,
  Responses on OpenAI and Azure, the token-counting endpoints), the same answer
  means the server's version predates the endpoint: an older vLLM without
  `/v1/messages` answers `{"detail": "Not Found"}` there while chat works. It
  answers `502 upstream_endpoint_missing` (the backend's text not relayed), is
  retried on deployments of other backends (every deployment on that backend runs
  the same server and is refused for the request's retries), produces no usage and
  is **not a circuit failure** — the deployment keeps serving its other endpoints.
  **Remembered for a probe interval** (settled 2026-10-06, the pre-merge review's
  M3): the backend is left out of routing for that endpoint until the interval
  passes — unless every deployment of the model is on such a backend, then they are
  tried again (a server may have been upgraded) — so its instant `404`, which made it
  look least loaded, no longer draws the endpoint's traffic and spends the retry
  budget while a working deployment sits idle. Logged at warning level with the
  backend and the endpoint once per interval, attempt outcome `endpoint_missing`. vLLM's `405 {"detail": "Method Not Allowed"}` on these
  endpoints reads the same way: its `POST /v1/responses/input_tokens` lands on the
  `GET /v1/responses/{id}` route (0.30.0), which is why `vllm` does not claim that
  endpoint. Rejected: a circuit failure — chat on a healthy server would stop for
  want of an endpoint it never had; a wrong-path answer — the operator would look
  for a URL mistake that is not there.
- **Complete responses** (settled 2026-09-25; the audit's M13): HTTP framing ending
  cleanly does not make an answer whole — a backend whose generator dies can end its
  response on an event boundary. A successful (`2xx`) stream is complete once it
  carried `[DONE]`, or once every choice it carried (by `index`) has a non-null
  `finish_reason` — servers that end without `[DONE]` still end whole; a usage-only
  chunk after that changes nothing, and `[DONE]` alone suffices (OpenAI-compatible
  servers send it last). A successful JSON body is complete once its top-level value
  closed. A successful answer that ends before it is complete is a backend failure:
  `kaiak.relay_end=upstream_incomplete`, a circuit failure, usage settled partial, never
  retried once bytes reached the client; one that ends before its first event (an
  empty stream or body) is a connection lost before the first event
  (`502 upstream_unavailable`, retried). A body cut short of its `Content-Length`, or
  of its chunked framing, already breaks off at the connection (`upstream_failed`).
  Error answers (`4xx` relayed, `5xx` answered by the gateway) are not checked for
  completeness.
  Rejected: counting `content_filter` or other finish reasons differently — any
  finish reason is the backend saying the choice ended.
  **Per format** (settled 2026-10-06; the end events checked on vLLM 0.30.0 and
  llama-server b9917 and b10802 — the recorded streams in
  `gateway/internal/fakebackend/captures/` are b10802's): a Messages stream is complete once it carried
  `message_stop`; a Responses stream once it carried `response.completed` or
  `response.incomplete` — vLLM ends a stream cut by `max_output_tokens` with
  `response.completed` whose `status` is `incomplete`, OpenAI with
  `response.incomplete`; both are whole. Neither format sends `[DONE]`. A
  stream that carries an **error event** — Messages `event: error` (Anthropic's
  `overloaded_error` mid-stream), a Responses `error` or `response.failed` event —
  ends there: once relayed, the stream ends as an incomplete one does
  (`kaiak.relay_end=upstream_incomplete`, usage settled partial). Rejected: relaying
  an error event and ending cleanly — the client would see a finished answer from a
  stream the backend abandoned.
- **Error events** (settled 2026-10-06, the pre-merge review's M4 and [S] L4): an
  error event is classified by what it names, as the HTTP status it matches would be:
  - **busy** — Anthropic `overloaded_error` and `rate_limit_error`, Responses
    `rate_limit_exceeded`: as a `429`;
  - **the caller's** — Anthropic `invalid_request_error`, `request_too_large`,
    `not_found_error`; the Responses codes naming the request's own fault (an
    invalid prompt or image, a policy): `invalid_prompt`, `bio_policy`,
    `misalignment_policy_violation`, `invalid_image`, `invalid_image_format`,
    `invalid_base64_image`, `invalid_image_url`, `image_too_large`,
    `image_too_small`, `image_parse_error`, `image_content_policy_violation`,
    `invalid_image_mode`, `image_file_too_large`, `unsupported_image_media_type`,
    `empty_image_file`, `failed_to_download_image`, `image_file_not_found` (OpenAI's
    `ResponseError` codes, openai-python `src/openai/types/responses/response_error.py`,
    `main` on 2026-10-06): as a `4xx`;
  - **a failure** — anything else (`api_error`, `server_error`,
    `vector_store_timeout`, `data_residency_mismatch`, a type or code unknown or
    missing): as a `5xx`.

  As the **first event**, before anything reached the client, the gateway answers as
  that status: a failure `502 upstream_error`, retried (`server_error`), a circuit
  failure; busy `503 upstream_overloaded`, retried as `rate_limited`, the
  deployment cooling down (429 cooldown, its default wait), neutral; the caller's
  `400 upstream_refused` naming the backend's code when it is a plain identifier
  (never its message), not retried, neutral. None bills anything: nothing was
  generated. **After the first event** the event is relayed and the stream ends
  incomplete; the circuit and the metrics follow its kind — a failure is `broke_off`,
  a failure for the circuit; busy is `rate_limited` and the caller's `client_error`,
  both neutral. A relayed error event keeps its type and code, but **every message
  it carries becomes the gateway's** (`The model backend ended the response with an
  error.`): like a `5xx` body, its text can name hosts, engine internals or the
  backend-side model. Rejected: counting every error event as a failure — one
  Anthropic-wide overload opened every circuit, and a few undownloadable images
  opened a deployment's; relaying the backend's text.
- **Probe and model check for the new types** (settled 2026-10-06): `anthropic`
  lists its models at `GET <base_url>/models?limit=1000` with its credential and
  `anthropic-version` (the list is paged, 20 by default; `data[].id`).
  `azure-anthropic` has no models list (Foundry offers no Models API): its probe
  always succeeds, so an open circuit turns half-open after each probe interval and
  the half-open trial decides (Routing and reliability → Circuit mechanics), and the
  config-time model check skips its backends with an info line, `model check not
  available for this backend type`. A better check is in `docs/BACKLOG.md`.
- **Connections** (settled 2026-09-24): one connection pool per backend, kept across
  config reloads (a new pool only when the connect timeout changes), up to 256 idle
  connections per host so many concurrent streams reuse connections; HTTP/2 when the
  backend offers it; proxy from the standard environment variables; no response
  compression, so stream bytes flow undecoded; redirects are not followed.

## Routing and reliability

- A model maps to one or more deployments — identical copies of that model (e.g. the
  same model on several vLLM hosts); load balancing across the healthy ones with
  capacity. **No fallback** to another model or provider (settled 2026-09-24): a
  request for a model is served by that model's deployments or not at all.
- **Load balancing** (settled 2026-09-24): the deployment with the fewest requests in
  flight wins; deployments tied on that count take turns (round-robin per model). A
  request counts as in flight from the moment it is routed until its response has
  been relayed or has broken off (success, backend error, client disconnect alike).
  Counts belong to the process: a deployment is its backend ID plus the model name on
  that backend, so a config reload that keeps a deployment keeps its count. No
  weights — they wait for a real need.
- **Retries** (settled 2026-09-24) on connect errors, stream first-event timeouts,
  backend 5xx and backend 429 — only before anything reached the client; after that,
  failures surface to the client as they are. At most 3 attempts in total (config:
  global, per-model override). Caller-caused 4xx is never retried. **Retries are
  failover only** (settled 2026-09-25, D4): a retry goes to a deployment of the same
  model the request has not tried — never the same deployment again; with none left
  the attempt's answer goes to the client, so a single-deployment model answers its
  backend's error at once. The client's own retries (OpenAI SDKs retry `429`, `5xx`
  and connection errors, twice by default) cover the same deployment; the gateway
  retrying it too multiplied the backend hits per call (up to 9) and made a burst of
  failures on a single-deployment model open its circuit within a couple of calls.
  Rejected: same-deployment retries with a pause before them (they multiply with
  the SDK's all the same).
- **Retry mechanics** (settled 2026-09-24):
  - The retry decision is one function, on an attempt that has written nothing to the
    client (a provider returns only once the first event arrived, and a backend error
    status is held, not relayed, until the decision):

    | Attempt ended with | Retried |
    |---|---|
    | Connect error, connection lost before the first event (`upstream_unavailable`) | yes — reason `unavailable` |
    | A stream's first-event timeout (`upstream_timeout`) | yes — `timeout` |
    | A non-stream response timeout (`upstream_timeout`) | no — the backend was working on a long answer; another would take as long (D2, 2026-09-25) |
    | Backend `5xx` (but `529`), or a successful stream whose first event is an error event naming a failure (answered `502 upstream_error`, settled 2026-10-06) | yes — `server_error` |
    | Backend `429`, Anthropic's `529` (`overloaded_error`), or a first error event naming the backend busy (answered `503 upstream_overloaded`) — settled 2026-10-06 for the last two | yes — `rate_limited`; the deployment cools down (429 cooldown, below) |
    | A first error event naming the caller's fault (`400 upstream_refused`, settled 2026-10-06) | no |
    | Backend `401`/`403` (`upstream_auth_failed`) | yes — `auth_failed`; every deployment of the model on that backend is refused for the request |
    | Backend `404` naming the deployment's model (`upstream_model_missing`) | yes — `model_missing` |
    | Backend `404` at a path its server does not have (`upstream_path_missing`) | yes — `path_missing`; every deployment of the model on that backend is refused for the request |
    | The same on an endpoint beyond the type's core ones (`upstream_endpoint_missing`, settled 2026-10-06) | yes — `endpoint_missing`; every deployment of the model on that backend is refused for the request |
    | A response relayed (`2xx`, a caller's `4xx`), anything after the first event | no |
    | Client gone, the drain's cut, a gateway fault | no |

    `401`/`403`: the credential is per backend — another backend has its own and may
    serve; the same backend would refuse it again, so a single-backend model answers
    `upstream_auth_failed` at once. Rejected: never retrying it — one backend's bad
    credential would fail requests another deployment can serve.
  - **Choice across attempts** (settled 2026-09-25, D4): a retry goes to an eligible
    deployment (circuit closed) the request has not tried — and, after a `401`/`403`,
    not on the backend that refused the credential; never to one it tried. Which ones
    qualify is decided anew each time a slot is handed out, as circuits open and
    close — an attempt's failure may itself open its deployment's circuit. None left:
    the request ends with its last attempt's answer (`kaiak.retry_refused:
    "no_deployment_left"`).
  - **Queue on retry**: a retry takes a slot as a new request does — at once when a
    deployment it may use has a free slot, else at the back of the model's queue, the
    same size and timeout, its wait counted. It never waits for a deployment it may
    not use: with none left it stops at once. A queued retry that may not use a freed
    slot lets the requests behind it that can use it go first.
  - **A retry that gets no slot** (queue full, queue timeout, no deployment left):
    the request ends with its last attempt's answer — the held backend error (a
    `4xx`/`429` relayed as is, a `5xx` answered `upstream_error`), or the gateway's
    error for it; `Retry-After` is the backend's, never the
    queue's. The log line says why (`kaiak.retry_refused`).
  - **Retry budget** (settled 2026-09-25; the audit's L1): per model, the retries
    sent in the last 10 s may reach 20% of the attempts sent in that window, and at
    least 10 — so a model with little traffic still retries, while a failure across
    every deployment does not multiply the load on a struggling fleet by the attempt
    count. A retry past the budget is not sent: the request ends with its attempt's
    answer, as a retry that got no slot, `kaiak.retry_refused: "retry_budget"`. Counted
    in-process per gateway. A retry counts when it is **sent**, not when the budget
    approves it (settled 2026-09-25, N-P8): one approved and then never sent (no
    slot, the client gone) spends nothing; retries approved at the same moment may
    pass the budget by the few in between. Rejected: a budget per deployment — the retry leaves the
    failing deployment by design; what must be bounded is the model's extra load.
  - **Client gone or drain cut between attempts** (or while a retry waits in the
    queue): no further attempt; the request ends as `client_closed` or
    `server_shutting_down`.
  - Each attempt holds its own slot and in-flight count, and tells the circuit
    breaker its own outcome as it ends (a retried attempt before the next is sent):
    a failing deployment reaches its threshold through retries too.
- **429 cooldown** (settled 2026-09-25, D3; the independent audit's finding 3): a
  deployment that answers `429` cools down, per gateway, for what the answer asks —
  `Retry-After-Ms` (Azure's, finer) when readable, else `Retry-After` (seconds or an
  HTTP date) — at most 60 s, and 5 s when it carries neither; `0` (or a date past)
  starts none. A later `429` moves the end only later, never earlier (settled
  2026-09-25): the cooldown ends at the later of the two — a short `Retry-After` right
  after a long one is not the quota refilling sooner. While it cools down the deployment
  takes **no request** — first attempts, queued requests and retries alike — as
  long as another deployment of the model the request may use is eligible (even a
  full one: the request waits in the queue for it rather than take a certain `429`).
  With none — every deployment cooling down, the others open, or a single
  deployment — it is used, and the client gets the backend's `429` honestly. A
  cooldown's end hands the deployment's free slots to waiting requests at once; a
  deployment dropped by a reload drops its cooldown. The circuit stays neutral for
  `429` (busy, not broken). Why: least-in-flight prefers the deployment that answers
  `429` at once (it never has requests in flight), so a throttled Azure resource
  took nearly every first attempt and the retry budget ran out on the spill-over —
  in the reviewers' probe, 76 of 100 requests failed while another resource had
  room. Rejected: a longer ceiling — a resource whose quota refills should be back
  within a minute, and a new `429` costs one attempt; counting `429` toward the
  circuit — quota is not a fault, and probes cannot tell when it refills. Shown as
  `kaiak_deployment_cooling_down`; not in status (no protocol change).
- **Usage across attempts** (settled 2026-09-24; D1 2026-09-25): one client request
  counts once toward request limits. An attempt answered `5xx` or `429`, or that
  failed to connect or to write its request in full, produces no usage (the backend
  refused it, or never had the prompt). An attempt whose request reached the backend
  in full and got no answer — the first-event timeout, a connection lost after
  sending — had its prompt taken upstream and may have been billed there: it is
  recorded with estimated input tokens and no output, flagged `estimated` and
  `partial` (Accounting: billed from the moment the request was sent). The attempt
  that answers is recorded as before. Made concrete: every record carries the
  client request's ID and its own attempt's deployment; a request's records are one
  per retried attempt that was sent in full and unanswered, plus the last attempt's
  — always exactly one, so every routed request still settles. Limits settle the
  request's one reservation to the **sum** of its records; the usage-metrics sink
  gets each record.
- **Health**: one mechanism — failures on real traffic open a deployment's **circuit
  breaker**; while open, the gateway probes it on an interval and closes the circuit
  when a probe succeeds. No always-on active checks (deferred). Settled 2026-09-24:
  backend failures count (connect errors, stream first-event and stall timeouts,
  5xx, a refused credential, a missing model, a wrong path, broken-off responses — the full list
  is the outcome-class table below); backend 429 (busy, not broken) and
  caller-caused 4xx do not. The circuit opens after 5 consecutive failures (config).
  The probe is a cheap `GET …/models` on the backend every 10 s (config) — no tokens,
  works for vLLM, OpenAI and Azure; the first success makes the circuit half-open,
  and one real request (the trial) decides: its first data event (or first body
  bytes) closes the circuit, its failure before that opens it again. (A per-backend completion probe is in `docs/BACKLOG.md`.)
- **Outcome classes** (settled 2026-09-24) — one classification, applied to every
  routed request when it is over:

  | Outcome of the request on the deployment | Class |
  |---|---|
  | Connect error (refused, DNS, connect timeout, TLS), connection lost before the first event (`upstream_unavailable`) | failure |
  | A stream's first-event timeout (`upstream_timeout`) | failure |
  | Backend `5xx` but `529` (answered `upstream_error`), or a successful stream opening with an error event naming a failure (settled 2026-10-06) | failure |
  | Backend `401`/`403` (`upstream_auth_failed`) | failure |
  | Backend `404` naming the deployment's model (`upstream_model_missing`) | failure |
  | Backend `404` at a path its server does not have (`upstream_path_missing`) | failure |
  | The same on an endpoint beyond the type's core ones (`upstream_endpoint_missing`, settled 2026-10-06) | neutral — the deployment serves its other endpoints |
  | A provider's refusal before sending (`price_option_unsupported`) | neutral — nothing was sent |
  | Response broken off upstream after the first event (`kaiak.relay_end=upstream_failed`) | failure |
  | Stream silent for the stall timeout after the first event (`kaiak.relay_end=upstream_stalled`) | failure |
  | Successful response ended before it was complete (`kaiak.relay_end=upstream_incomplete`), an error event naming a failure among the ways | failure |
  | The same ended by an error event naming the backend busy, or the caller's fault (settled 2026-10-06; Providers: error events) | neutral |
  | Non-stream response timeout before the first bytes (`upstream_timeout`) | neutral — but a failure for a half-open trial, and from the 3rd in a row on the deployment with no success between |
  | Non-stream response timeout after the first bytes (`kaiak.relay_end=upstream_timeout`) | neutral |
  | Backend `429`, Anthropic's `529` (settled 2026-10-06), a first error event naming the backend busy | neutral |
  | A first error event naming the caller's fault (`upstream_refused`, settled 2026-10-06) | neutral |
  | Other backend `4xx` (the caller's) | neutral |
  | Client gone or the drain's cut, before the first event | neutral |
  | Gateway fault building the upstream request | neutral |
  | Response relayed to its end, status below 400 | success |
  | Client gone or the drain's cut after the first event, status below 400 | success |

  A failure adds one to the deployment's consecutive-failure count; a success resets
  it; a neutral outcome leaves it as it is. `401`/`403` count: the gateway's own
  credential is refused, every request will fail the same way, and an open circuit
  stops hammering the backend and says so in status and metrics. A stream broken off
  upstream, stalled or ended incomplete counts: the backend broke, even if it
  answered first. A non-stream response timeout does not (D2, 2026-09-25): a long
  generation is the backend working, and counting it would open the circuit of a
  healthy host serving long answers. **Except a hung backend** (settled 2026-09-25,
  N-C2): an engine hung behind a live models list answers no non-stream request,
  and response timeouts alone would never open its circuit — nor decide its
  half-open trial, which would hand the deployment on, trial after trial. So a
  response timeout before the first bytes is a failure when it is the half-open
  trial's, and from the **3rd in a row** on one deployment (no success between;
  other outcomes do not break the run) each counts as a failure, toward the usual
  threshold: one or two are long generations, three with nothing succeeding is a
  hung engine. The number is fixed, not config — revisit if real long-generation
  traffic trips it. A timeout after the first bytes stays neutral (the backend was
  sending). A client that left
  before the first event counts neither way — the request says nothing about the
  backend; one that left after it (or the drain's cut) leaves the backend's status to
  speak: a response under way is a success — the backend proved it serves (settled
  2026-09-24) — a backend `5xx` stays a failure and another `4xx` neutral. Every
  attempt of a retried request is classified on its own.
- **Circuit mechanics** (settled 2026-09-24):
  - State is per deployment, with the in-flight counts' identity and life: it
    outlives reloads that keep the deployment and is dropped with it. Outcomes on
    deployments the applied config does not have (requests still running under an
    older snapshot) are not counted.
  - The `failure_threshold`-th consecutive failure opens the circuit. While open,
    request outcomes change nothing (requests already in flight may still report);
    only a probe moves it on.
  - **Half-open** (settled 2026-09-25; the audit's L12): a successful probe makes the
    circuit half-open — the models list answering does not prove completions work.
    A half-open deployment is eligible for exactly one request at a time, the trial,
    handed out like any slot (to a waiting request first). **The trial is decided
    at its first data event** (settled 2026-09-25, E4; the audit's N-C1/N-S3): a
    stream's first event carrying data, or a non-stream response's first body bytes,
    under a status below
    `400` closes the circuit at once (failure count 0) — the deployment serves other
    requests while the trial runs on, and the trial's own outcome when it ends
    counts like any request's against the closed circuit (a break-off is one
    ordinary failure). A trial that fails before its first event (a failure class,
    its response timeout included) opens the circuit again at once, whatever the
    threshold; a neutral outcome (backend `429`, a caller `4xx`, the client gone
    before the first event) or a trial released without a verdict leaves it
    half-open for the next request. Until the trial's first event the deployment is
    not eligible, so a model whose other deployments are open answers
    `no_healthy_deployment`. Rejected: deciding at the relay's end — a long stream
    or a slow reader as the trial kept a recovered deployment out for minutes.
    Other requests' outcomes (ones still running from before the circuit opened)
    change nothing. Only a data event decides (settled 2026-09-25, consistent with
    E8's stall timer): a comment block (`: ping`) proves the connection, not that
    the backend generates, so a trial whose backend pings and then fails is a
    failed trial. The first-event timeout still counts any block. **Half-open is
    its own state in status and metrics** (settled 2026-09-25): status reports
    `half_open` with the circuit's original opening time; `kaiak_circuit_open` reads
    0 for it and `kaiak_circuit_half_open` 1 — an idle recovered deployment, which
    no trial closes, must not keep an open-circuit alert firing. Rejected: reporting
    half-open as open, which paged for deployments that had recovered.
  - An open deployment is not eligible: routing skips it, the dispatcher gives none
    of its slots to waiting requests. When every deployment of a model is open, a new
    request is refused at once with `503 no_healthy_deployment` (never queued; no
    usage record — never routed). Requests already waiting in the model's queue keep
    waiting until their timeout: a probe may close a circuit first, and the
    dispatcher then serves them. Rejected: failing them at once — the probe interval
    is usually far below the queue timeout.
  - Probes are per backend, as the probe is: one prober per backend with open
    circuits, started when its first circuit opens, probing every
    `probe_interval_ms` (the value in force when each wait starts). A success makes
    every circuit of that backend that was open when the probe started half-open.
    **The prober keeps probing while circuits are half-open** (settled 2026-09-25,
    N-C2): a success leaves them half-open, a failure opens them again (their trial,
    if one runs, no longer decides anything) — so a half-open circuit with no
    traffic, which no trial can close, still follows the backend: it stays
    half-open while the models list answers, and opens again when it stops. The prober stops once no circuit of the backend is open or half-open
    (trials closed them, a reload dropped them), and at shutdown (after the drain,
    so probes can still half-open circuits for queued requests).
  - The probe: `GET` on the models list (`<base_url>/openai/v1/models` for
    azure-openai, `<base_url>/models` for every other type) with the backend's
    credential, over the backend's connection pool, bounded by its connect timeout
    plus 5 s. Success is a `2xx` carrying an OpenAI models list (settled 2026-09-25,
    H8); a circuit goes half-open only when the list has its deployment's backend-side
    model among `data[*].id` — one whose model is missing stays open while the
    backend's others go half-open, its prober keeps probing, and the first such probe
    logs `circuit kept open: the backend does not list the deployment's model` (warn).
    An azure-openai list names models, not the deployment names requests carry, so it
    is not checked there.
  - A probe is an invocable mechanism (`ProbeNow`, with the trigger that invoked it);
    the prober's timer is its trigger `interval`. A probe cut short by shutdown is
    neither counted nor logged.
  - Log lines: `circuit opened` (warn: backend, deployment model, failures — or
    `kaiak.circuit.trial: true` when a half-open trial failed, or the probe's
    `kaiak.trigger` when a failed probe re-opened a half-open circuit —,
    `kaiak.circuit.last_error` — the failure that opened it), `circuit half-open`
    (info: trigger, `kaiak.circuit.open_duration`), `circuit closed` (info: trigger
    `trial`, `kaiak.circuit.open_duration`), `probe succeeded` (info when it made circuits
    half-open, debug for the repeats while they wait for a trial: trigger, duration,
    circuits made half-open),
    `probe failed` (info for the first failure after the circuits opened, debug for
    the repeats: trigger, duration, error). Opening and closing send a status report
    (control-plane mode) — at once, or at the end of the status minimum gap
    (Configuration sources → Control-plane mode → Status minimum gap).
- **Concurrency cap per backend**: max in-flight requests, a bounded queue with a queue
  timeout; full queue or timeout → `429`. For llama-server the cap equals its slot count.
  Settled 2026-09-24: a backend is one endpoint (URL — one process and port), so the
  cap is per process, shared by every deployment on it; optional (no cap by default;
  cloud backends usually have none). Routing prefers a deployment whose backend has a
  free slot; a request queues only when every eligible deployment is full. The
  **gateway** holds the queue — the waiting client request itself, until a slot frees
  — so waiting is visible (metrics, status) and bounded. **One queue per model**
  (settled 2026-09-24): a waiting request is served by whichever of its model's
  backends frees a slot first; when a freed slot could serve several models' queues,
  the longest-waiting request wins. Size and timeout are a global default with a
  per-model override (defaults 100 and 30 s); full → `429 queue_full` at once,
  timeout → `429 queue_timeout`. A stream holds its slot until it ends.
- **Caps across gateways** (settled 2026-09-25, D3; the audit's M1): `max_in_flight`
  is what the backend can take, whatever the number of gateways in front of it. In
  control-plane mode each gateway enforces its share, `ceil(max_in_flight ÷ live
  gateways)`, the live-gateway count taken from the latest totals (0 or none yet
  counts as 1; the share is at least 1), recomputed whenever totals arrive: a lower
  share applies to new admissions, a higher one hands its new slots to waiting
  requests at once. File mode enforces the whole cap (one gateway per config file).
  Rounding up keeps a small cap usable on every gateway at the cost of overshooting
  by at most one request per gateway. Status reports the configured cap; the metric
  `kaiak_backend_max_in_flight` shows the share enforced. Known limit: a gateway
  whose requests run longer queues while other gateways' shares sit idle — the
  demand-weighted split is in `docs/BACKLOG.md`. Rejected: leaving the cap per
  gateway (N gateways admitted N × the cap onto one host).
- **Queue mechanics** (settled 2026-09-24):
  - A slot is held from routing until the response is over — the same span as the
    in-flight count, so every relay end (success, backend error, client gone,
    upstream cut, drain cut) frees it. A backend's slots are its in-flight requests
    over all its deployments, counted by backend ID, so they outlive reloads that
    keep the backend. The cap in force is the latest applied config's: a lowered cap
    applies to new admissions (requests above it finish as they are), a raised one
    lets waiting requests in at once. A backend a reload dropped keeps the cap of the
    snapshot its requests run under.
  - One dispatcher hands out slots: whenever a slot may have freed (a release, a
    config applied) it gives each free slot to the longest-waiting request that can
    use it, choosing the deployment as for a new request. A model's queue is served
    in order; only a retry, which may not use some deployments, can let the requests
    behind it pass for a slot it may not take. A waiting request is woken only with a slot already taken for it — no
    polling, no herd of waiters racing for one slot. Queues are per public model
    name, so requests under an older snapshot wait in the same queue.
  - A new request queues only when no eligible deployment has a free slot; it never
    jumps waiting requests (a slot a waiter could use is never left free).
  - A queued request already holds its limits reservation (limits run before
    routing). A request that leaves the queue — full, timeout, its client gone (out of
    the queue at once), the drain's cut — never routed: it has no usage record, and
    the limits finisher releases its token reservation as for any request without one.
  - Errors: `queue_full` and `queue_timeout` are `429` with type `server_error` — the
    platform is at capacity, the caller did nothing wrong, so not OpenAI's
    `requests`/`tokens` rate-limit types, which name the caller's own limits; `429`
    still makes OpenAI clients back off and retry. `queue_full` carries
    `Retry-After: 1` (a slot may free any moment; the smallest whole value);
    `queue_timeout` carries none — the gateway has no estimate better than the wait
    that just ran out. The `x-ratelimit-*` headers of the admitted reservation stay.
  - The request log line carries `kaiak.queue.wait_duration` for every request that entered its
    model's queue, whatever the outcome.
- **Reliability settings** (settled 2026-09-24; schema: `config.schema.json`):
  - backend `max_in_flight` — integer ≥ 1, for the backend as a whole (split among
    the live gateways in control-plane mode); omitted = no cap;
  - `global.queue` `{ size, timeout_ms }` — defaults 100 and 30000; a model's `queue`
    overrides it field by field (a field left out keeps the global value). `size` 0
    is allowed and means never queue: a request finding no free slot is refused at
    once with `queue_full` — for clients that would rather retry elsewhere than wait.
    `timeout_ms` ≥ 1;
  - `global.retries.max_attempts` — attempts in total, the first included: default
    3, 1 (no retries) to 10; a model's `retries: { max_attempts }` replaces it;
  - `global.circuit` `{ failure_threshold, probe_interval_ms }` — defaults 5 and
    10000; threshold ≥ 1, interval ≥ 100 ms (probes must not hammer a backend that
    is already failing). Global only: a per-model threshold waits for a real need.
- **Timeouts** (settled 2026-09-25, D2; the audit's H1 and H7), per backend, integer
  milliseconds ≥ 1:
  - `connect_timeout_ms` (default 5 s) — TCP connect and the TLS handshake; running
    out of it is a connect failure (`502 upstream_unavailable`), not a `504`;
  - `first_event_timeout_ms` (default 60 s) — a **streaming** request, from sending
    to the stream's first event (prefill included). Running out: `504
    upstream_timeout`, retried (`timeout`), a circuit failure;
  - `response_timeout_ms` (default 30 min) — a **non-streaming** request, from
    sending to the body's end: a non-streaming answer arrives whole, so this bounds a
    full generation. Running out before the first bytes: `504 upstream_timeout`,
    **not retried and not a circuit failure** — the backend was working on a long
    answer, another deployment would take as long, and failing it would open the
    circuit of a healthy host (except a half-open trial's, and from the 3rd in a
    row: Outcome classes); billed as sent (Accounting: estimated input once the
    request was sent). After the first bytes (a large body still arriving): the
    connection is cut, `kaiak.relay_end=upstream_timeout`, neutral, usage partial;
  - `stall_timeout_ms` (default 120 s) — a **streaming** request, the longest silence
    between data events once the first event arrived. It runs only while the gateway
    waits on the backend (time spent writing to a slow client is not the backend's
    silence). **Only data events are progress** (settled 2026-09-25, E8): comment
    blocks (`: ping`) and blank keep-alives are relayed but do not reset it — a
    backend pinging while it produces nothing has stalled; backends that reason
    silently raise their `stall_timeout_ms` instead. Running out ends
    the relay as a backend failure: the connection is cut, `kaiak.relay_end=upstream_stalled`,
    a circuit failure, never retried (part of the answer reached the client), usage
    settled partial (the backend's report if one arrived, else the estimate).
  No timeout bounds a running stream's total length. Which timer applies follows the
  client's `stream` flag. Rejected: one first-byte timeout for both kinds of request —
  a non-stream generation needs a bound in minutes, a stream's first event one in
  seconds, and a timeout that fits one mis-serves the other (a long generation timed
  out, retried on two more hosts and counted against their circuits); and no stall
  bound — a hung engine (TCP alive) held its slot until the client left and never
  reported a failure.

## Limits

- **Scopes** (settled 2026-09-27): global and every group on the key's path — its
  group and each ancestor (`CONTROL-PROTOCOL.md`, Config → The group tree). Keys have
  no limits of their own; a key's usage counts toward every scope on its path. A
  request passes every limit of every scope on its path. A group's limits are its
  effective ones, its parent's `child_defaults` merged in.
- **Types**: requests/min, tokens/min, tokens/hour, USD/month (extensible), at most
  one of each per scope. A limit counts every model its scope uses. Per-minute
  windows are **sliding**; hourly windows are fixed UTC hours; USD months are
  calendar months, UTC.
- **Where each window is enforced** (principle 6):
  - **Per-minute windows** — locally in each gateway, on a **share**: limit ÷ the
    live-gateway count the control plane pushes (Control-plane mode, below). Drift:
    bounded by skew in traffic across replicas within the minute. Known limitation: HTTP keep-alive pins a client to one
    replica, so a scope driven by a single client gets about its share, not the full
    limit (demand-weighted shares are in `docs/BACKLOG.md`).
  - **Hourly and longer windows** — tracked by the control plane from usage records; the
    gateway enforces the pushed totals plus its own usage not yet counted against the
    limit (`CONTROL-PROTOCOL.md`, Budgets), each pushed window matched to the count of
    its scope and type (`CONTROL-PROTOCOL.md`, Messages → Matching totals to limits).
    Drift: in-flight requests plus the other gateways' unreported usage, about one
    batch interval each.
- **File mode** (single instance, local and development use): the same local window
  counter enforces every window, at the full limit; a restart starts every window
  empty (File mode keeps nothing across a restart, below). The gateway only enforces limits —
  authoritative totals are the control plane's job, never computed twice.
- **Tokens before the request runs**: input tokens are estimated from the request body
  (about 4 bytes of text per token — no tokenizer, zero dependencies); the check
  reserves that estimate plus the output limit, and settlement replaces both with
  actual usage.
- **The input estimate** (settled 2026-09-25, D1/D2; the daily-operations review's
  findings 1 and 2): taken once per request from the body as received, and used
  everywhere an input estimate is — the limits reservation, the injected default
  output limit, and estimated usage records (no usage report, or an attempt sent and
  left unanswered). Text counts by bytes (÷ 4, rounded up); these count otherwise,
  their bytes left out of the text:
  - **each media item counts a flat 1000 tokens** (a constant, not configurable): any
    string value that is a data URL (`data:` then a header of at most 256 bytes with
    no whitespace, naming a media type or `;base64`, then a comma — in any field, so
    `file.file_data` and new part types are covered), an `image_url` part's URL in
    any form (`image_url.url` or a bare `image_url` string — a remote image is
    fetched and billed by the backend all the same), and an `input_audio` part's
    `data` (raw base64, no data URL). An image's encoded size says nothing about its
    cost (backends bill by dimensions and detail, per model), so base64 counted as
    text overstated a 1 MB photo as ~350k tokens — refused by token limits, or its
    injected default output cut to 256. Rejected: a model-aware image cost from
    dimensions and detail — it needs decoding images and per-model rules for an
    estimate that settlement replaces anyway.
  - **each token ID** in a completion's `prompt` or an embeddings `input` counts one
    token.
  - **Messages and Responses** (settled 2026-10-06): the same rules over their
    bodies — text by bytes wherever it is (`system`, `instructions`, content
    blocks, tool definitions, tool results, function-call outputs), and a flat 1000
    tokens per media item: a Messages `image` or `document` block whose `source` is
    `base64` (its raw `data`), `url` or `file` — the whole source counts as the item,
    whatever order its members come in; a `text` or `content` source is text, a
    `content` source's own blocks read by the same rules; a Responses `input_image` or
    `input_file` part (its data URL, URL or file ID). **Only at the format's
    documented content paths** (settled 2026-10-06, the pre-merge review's [S] L1
    and [B] M2): Messages blocks in `messages[].content`, `system`, a tool result's
    or search result's `content` and a content source's `content`; Responses parts in
    an input item's `content`, and in a function or custom tool call's `output` list
    (a multimodal tool result). Exact keys, as the inbound stage reads them. Anywhere
    else — a tool's arguments (`tool_use.input.source`), a function's JSON Schema that
    holds a `content` list — such an object is text. Rejected: matching member names
    anywhere — tool data that looked like a part swapped kilobytes of text for one
    media figure, and media in a tool call's output went uncounted.
  - **One pass** (settled 2026-10-06, the pre-merge review's H2): every byte of the
    body is read once, however deep sources and content parts nest — a part's own
    count is held apart until its object closes, then kept or swapped for the media
    figure. Rejected: reading each source or part whole and rescanning it — a body
    nesting them thousands deep made the estimate quadratic, about a minute of CPU
    for one request at the body cap, before limits run.
  - The estimate has two figures: the **total** (every prompt of a completion batch —
    what limits reserve and estimated records bill) and the **input one sequence
    sees** (the request less every prompt of a batch but the largest — what the
    injected default is fitted to: the context belongs to each generated sequence,
    not to the batch). Chat has one prompt, so the two are equal.
- **Output limit**: each model has an output-limit **default** (applied when the request
  sets none) and **ceiling** (requests above it are lowered). While a request runs, its
  output limit counts as used against its scopes' local allowance; the unused part is
  released at settlement. **Thinking budgets** (settled 2026-10-06, the pre-merge
  review's M6): Anthropic requires `thinking.budget_tokens` below `max_tokens`, so a
  Messages request with thinking enabled whose `max_tokens` the gateway would set to
  the model's ceiling or default at or below its budget is refused, `400
  invalid_value` naming `max_tokens` and saying which of the model's limits is in
  the way — the backend would refuse it over a value the client never sent. The
  gateway reads `thinking.budget_tokens` and never edits it. Operators serving
  clients that think with large budgets (Claude Code sends `max_tokens` 32000 with a
  budget of 31999) set the ceiling above them.
- **Output-limit keys** (settled 2026-09-24): chat reads `max_completion_tokens` and
  `max_tokens`; completions reads `max_tokens` only (`max_completion_tokens` is not a
  completions parameter and passes untouched); embeddings has no output limit. A
  request setting none of its endpoint's keys gets the default under
  `max_completion_tokens` (chat) or `max_tokens` (completions).
  `max_completion_tokens` is OpenAI's current chat field; vLLM, SGLang, llama-server
  (an alias of `n_predict`), OpenAI and Azure all read it, and OpenAI's and Azure's
  reasoning models refuse `max_tokens` — so it is the one the gateway writes. Each key
  the client set is checked on its own: above the ceiling, it is lowered to the
  ceiling (negative or above the context: refused, below). The request's **effective
  output limit** is the largest of its keys after these edits (none when the model
  declares no limit and the client sent none). **Messages** reads and writes
  `max_tokens`, **Responses** `max_output_tokens` (settled 2026-10-06), each its
  format's one key, under the same rules: out of range refused, above the ceiling
  lowered, absent filled with the default fitted to the context. Messages requires
  the field, so a model with an `output_limit` always sends one; on a model without
  one, a request that omits it reaches the backend without it, which answers as it
  does. The token-counting endpoints have no output limit.
- **Output limit out of range** — above the context (settled 2026-09-25, E9; the
  follow-up audit's N-M1): a key the client set (chat: either key, each checked on its own;
  completions: `max_tokens`) above the model's `context_length` is refused — `400`,
  type `invalid_request_error`, code `invalid_value`, `param` naming the key — before
  limits and routing: prompt and output share the context, so the value can never be
  honored, and on a model with no `output_limit` it would otherwise reserve an absurd
  amount. The code and message wording are OpenAI's for the same refusal
  (`max_tokens is too large: …`), so client libraries treat it as they do there.
  A value within the context but above the ceiling is still lowered to the ceiling.
  Every model declares `context_length` (config), so every client output limit is
  bounded. Rejected: a gateway-specific code (`max_tokens_too_large`) — OpenAI's
  existing answer fits. **Below 0** (settled 2026-09-25): refused the same way on
  every model — `400 invalid_value`, `param` naming the key, OpenAI's "integer below
  minimum value" wording — because llama-server reads `-1` as unlimited: on a model
  with no `output_limit` it would generate past the reservation (which counts no
  output), and lowering it to the ceiling only covered models that declare one.
  `0` passes (the backend judges it). Rejected: lowering negatives to the ceiling —
  it left the models without one unbounded.
- **Injected default fits the context** (settled 2026-09-25; the audit's L11): the
  default the gateway sets is lowered to `context_length − input estimate` (the input
  one sequence sees — a batch's largest prompt; The input estimate, above), but not
  below 256 — the estimate is rough, and a tiny limit
  would cut answers short. A default larger than the room a long prompt leaves made
  backends refuse the request (vLLM answers `400` when prompt + `max_tokens` exceed
  the context). A value the client set is never lowered for this: only the ceiling
  applies to it. The lowered value is the request's effective output limit (what
  the reservation counts).
- **Output multiplicity** (settled 2026-09-25, H10): a backend's output limit bounds
  each generated sequence, not the request, so the reservation counts the effective
  output limit × the request's sequences: `n` per prompt, or `best_of` when it is
  larger (the backend generates `best_of` and returns the best `n` — vLLM's and
  OpenAI's semantics), times a completion's prompts
  (`prompt` as a list of strings or of token-ID lists holds one prompt per element; a
  string or one token-ID list is one prompt). `n` and `best_of` are each at most
  `global.max_n` (default 8), else `400 n_too_large`; a value below 1 counts as 1
  (the backend refuses it). `best_of` is read on chat as well as completions
  (settled 2026-09-25, N-S4): OpenAI has it on completions only, but vLLM versions
  accept it on chat, so an unowned `best_of` would multiply generation past the
  reservation. The input estimate reserved is the total (The input estimate). The arithmetic
  saturates, and the reservation is capped at 2^53 − 1 (the largest amount the
  protocol carries), so an absurd product is "request too large", never a wrapped
  small number. Rejected: a per-model
  `max_n` — one global ceiling is enough until a model needs its own.
  **Batch caps** (settled 2026-09-25; the independent audit's finding 4): the
  request's sequences (the product above) are at most
  `global.max_sequences_per_request` (default 16: `n` at the default `max_n` on two
  prompts, or 16 single prompts), and an embeddings request's inputs (`input` counted
  as `prompt` is: one per element of a list of strings or of token-ID lists) at most
  `global.max_embedding_inputs` (default 2048, OpenAI's own limit); above either,
  `400 invalid_value`, `param` the prompt list (`prompt`) when there is more than one
  prompt, else `n` or `best_of` (whichever is larger), and `input` for embeddings.
  10 000 prompts under `max_n: 8` had been one request. **Backend slots stay per
  HTTP request**: `max_in_flight` counts requests, and one request's sequences
  share its slot — `docs/DEPLOYMENT.md` says to size `max_in_flight` and the
  backends' own batch limits (vLLM's `--max-num-seqs`, llama-server's `--parallel`)
  accordingly. Rejected: weighting slots by sequences — fair queueing of weighted
  requests is a design of its own, and the caps bound the difference.
- **Per-key concurrency** (settled 2026-09-25, E10; the follow-up audit's N-S1): a
  key has at most `global.max_concurrent_requests_per_key` (default 16) requests in
  flight on one gateway. A request counts from authentication — before its body is
  read — until it ends, however it ends (success, any error, a queue refusal, the
  client leaving, a drain cut, a broken-off stream): its slot is given back by a
  request finisher. Every authenticated request counts, the model endpoints too —
  simplest, and they are over in microseconds. Past the limit the request is
  refused at once, `429`, type `requests`, code `concurrency_limit_exceeded`,
  `Retry-After: 1`, its body unread (the connection closes after the answer). Why
  before the body: the other limits run after it, so idle connections declaring
  bodies cost a key nothing; this bounds them per key (with the body budget taken as
  bytes arrive — Request pipeline → request bodies). Enforced **per gateway**: N
  replicas allow up to N × the limit (the fleet-wide split is in
  `docs/BACKLOG.md`). Error class `rate_limited` — the caller's own doing, like its
  rate limits; the log line's `error.type` tells the two apart. No metric is
  labelled by key for it (series per key would be unbounded; the per-key usage
  metrics already exist). Rejected: `requests_per_minute` before the body — it
  bounds arrivals, not requests held open; one counter per key is simpler.
- **Local counters** (settled 2026-09-24; by scope 2026-10-07): an hour and a month
  counter for every scope (Every scope is counted, below) and one per-minute counter
  for each per-minute limit, counting requests, tokens or nano-USD (accounting's cost
  unit) against an effective limit — the configured value in file mode, the pushed
  share for per-minute windows in control-plane mode. Sliding minutes count one-second
  buckets over the last 60 s, and only per-minute counters hold them; hours and months
  reset at their UTC boundary. A config allocates at most 50 000 counters
  (`CONTROL-PROTOCOL.md`, Config → The group tree: Counters are bounded). Every limit of a scope applies to every
  request in it, whatever its model (settled 2026-10-06: limits have no model sets,
  `CONTROL-PROTOCOL.md`, Config → The group tree); the scopes checked are global and
  every group on the key's path, a
  path of any length up to 8 (each group's limits already merged with its parent's
  `child_defaults`). Only the body endpoints are limited — the model endpoints
  use no model capacity.
- **Check and reserve** (settled 2026-09-24), before routing, across every applicable
  counter under one lock — all-or-nothing, so two concurrent requests never both take
  the last unit and a refused request reserves nothing anywhere:
  - requests: +1; admitted while the window holds fewer than the limit;
  - tokens: the input estimate's total (The input estimate) plus the effective
    output limit once per sequence the request asks for (Output multiplicity); a
    model with no output limit reserves the input estimate only; admitted while the
    reservation fits in what remains — checked as `need ≤ limit − used`, and every
    count saturates, so no amount wraps a counter negative (settled 2026-09-25, N-M1:
    a wrapped sum admitted a huge reservation and left the counter negative, admitting
    everything after it);
  - USD: nothing is reserved (cost is known only afterwards); refused once the window
    has reached its value.
- **Unpriced models** (settled 2026-09-25, D6): a USD limit applies to a request only
  while its model has a price in force (a `prices` entry effective now). A model
  without one costs nothing, so no budget refuses it — not a spent USD limit, not the
  outage refusals — and none is spent by it. A free vLLM model under a
  global `usd_per_month` keeps serving when the budget is spent or unknown; a priced
  model under the same limit is refused. Rejected: refusing every model a USD limit
  names — that turned a control-plane outage into an outage of free local models.
  *Edge case — a reload during a request* (settled 2026-09-25, the independent
  audit's finding 6): "priced" is read from the request's own config snapshot at its
  arrival — the entry its usage is priced from — not from the config in force when
  it reaches the limits stage. A request that took a priced snapshot and then a
  reload made the model free is still checked against (and spends) its USD limits;
  one that took a free snapshot is not held by them after a reload priced the model.
  The limits themselves are the live config's, as everywhere.
- **Settle** (settled 2026-09-24), once the request is over, on the counters it was
  checked against: token reservations are replaced by the actual tokens processed —
  `tokens_in + tokens_cache_write + tokens_out` (written tokens settled 2026-10-02,
  since leaving them out would take them out of the token limits; reasoning is
  inside `tokens_out`). **Input read from the cache does not count** (settled
  2026-10-05): a token limit measures backend load, and a prefix-cache hit costs the
  backend almost nothing — an agent resending a long context every turn spent its
  whole per-minute limit on input the backend barely touched. Cache reads stay in
  the record and are priced; the reservation still holds the full input estimate,
  since what will be cached is unknown until the backend answers. Rejected: counting
  every token the backend handled (OpenAI's rule for its per-minute limits). The
  record's cost is added to
  USD counters; the request stays counted, whatever its outcome — a request that
  failed upstream or was dropped still took a slot. A request with zero units (no
  backend answer, backend error status) releases its token reservation. A reservation
  counts in the bucket or window where it was made and is released only if that
  bucket or window is still current; the actual amount counts at settlement time, so
  every token counts in exactly one window.
- **Refusal** (settled 2026-09-24): `429` in the OpenAI shape (table above). The
  message names the kind of scope — `group limit` or `global limit` (settled
  2026-09-27) — its value and use, never a group ID or a group's `labels`: which group refused is
  the operator's to read in the log line (Observability → Logs: `kaiak.limit.scope`,
  `kaiak.limit.group`), and a client need not learn the tree's names. A token request larger
  than the limit's full value says so.
  `Retry-After` (whole seconds, rounded up) is the time until every refusing limit
  has room: a sliding minute frees room as its oldest buckets expire; an hour or
  month at its end. OpenAI clients retry `429` on their own; for a spent budget they
  get the month end in `Retry-After`.
  **A token limit blocked only by requests still running** (settled 2026-10-05,
  the 2026-10-05 review's M4) — one that would admit the request were its
  in-flight reservations gone, minute and hour windows alike — counts a short fixed
  **2 s** toward `Retry-After` instead of its slot expiry or window end, and its
  `x-ratelimit-reset-tokens` on that refusal is `2s`; a limit blocked by settled
  usage keeps its time. Minute windows keep their in-flight reservations apart from
  settled usage, as hour windows do, to tell the two. Why: a reservation holds the
  full input estimate plus the output limit, and with cache reads out of the count a
  request settles at a few percent of it, freeing the room within seconds — a
  refusal answered `Retry-After: 59` was admitted 3 s later, an hour limit's
  `55m0s` 5 s later, while OpenAI's SDKs sleep any `Retry-After` up to 60 s. The
  2 s is a short guess, not a promise: a request refused again gets the same answer.
  Rejected: the end of the running requests — unknown (a stream lasts as long as it
  generates); the slot expiry — what made agents sleep a minute for nothing.
- **Rate-limit headers** (settled 2026-09-24): `x-ratelimit-limit-requests`,
  `x-ratelimit-remaining-requests`, `x-ratelimit-reset-requests`, and the same for
  `-tokens`, on refusals **and** on admitted responses (as OpenAI sends them; clients
  pace themselves on them). Each set comes from the applicable limit of its kind with
  the least remaining (for tokens: minute and hour limits alike); a kind with no
  applicable limit sends no headers; USD limits have none. Remaining is taken after
  the request's own reservation; reset is the time until that limit's window holds
  nothing, as a duration (`1m0s`, `45m0s`) — `2s` on a refusal by a token limit
  blocked only by requests still running (Refusal).
- **Every scope is counted; limits apply on top** (settled 2026-10-07): the hour and
  month counts are kept per scope and type — global and every group on a request's
  path, `tokens_per_hour` and `usd_per_month` — whether or not the scope has a limit of
  that type, in both modes. A limit is a check over its scope's count. Per-minute
  windows are counted only for the limits that have them (they are local shares).
  Rejected: a count only for each configured limit — a limit added by a reload, or
  enforced by a gateway still running a config the control plane moved off, started
  from nothing until totals listed it.
  **A count outlives its scope's config** (settled 2026-10-07): the hour and month
  counts of a scope a reload removes are kept while their current window holds own
  usage or a running request holds a reservation on them — of any amount, zero
  included, and across a window roll-over — and a group created again under its ID
  meanwhile takes them back, in both modes; they are dropped once neither holds, at
  the latest within the hour after, on whatever next uses the limits — no reload or
  totals event is needed. They are outside the `counters-exceeded` bound: each ends
  with its window. Pushed windows
  are dropped once their window has passed. Rejected: rebuilding the counts from the config alone — a deleted
  group's own usage not yet in the totals (in file mode, all of it) was lost, and a
  group created again under its ID started a fresh budget, which the config contract
  says it does not (`CONTROL-PROTOCOL.md`, Config → The group tree: Parents never
  change).
- **Config reload** (settled 2026-09-24): the limits follow the live config, not each
  request's snapshot, and a reload changes no hour or month count: a changed value
  applies at once to the count so far, a new limit checks its scope's count so far,
  and a removed limit's scope keeps being counted. A group deleted and created again
  under the same ID within the window carries on with its ID's count (A count outlives
  its scope's config, above; in control-plane mode the totals carry it too —
  `CONTROL-PROTOCOL.md`, Usage intake → Totals). A
  per-minute limit that still exists — same group (or global) and type — keeps its
  window; a new one starts empty; a removed one is dropped.
- **File mode keeps nothing across a restart** (settled 2026-10-07): every window
  starts empty at each start. File mode is for local and development use
  (Configuration sources).
- **Control-plane mode** (settled 2026-09-24; the protocol side in
  `CONTROL-PROTOCOL.md`, Budgets and Messages → Totals):
  - **Hour and month counts** are `base + own`: `base` is the pushed `used` of the
    count's window (matched by group, or global, and type), counted while it names
    the count's current window; `own` is this
    gateway's usage the control plane has not counted — reservations in flight, and
    settled amounts tagged by **usage generation**. Each record carries the
    generation of the usage batch the control client took it into, read under the
    lock that seals batches, so it is exactly the batch the record is sealed in; the
    record goes to the batch as it settles, and local limits settle from it after
    (settled 2026-09-25, H4). A totals message that shows a batch counted (the
    `counted_through` entry of the gateway's epoch at or past it) drops every generation up to the batch's from `own`, in the same step, under the limiter's lock, as it applies the totals that
    include it; a record that settles after its batch was shown counted (a retried
    attempt's record, published mid-request; a push faster than the request's end) is
    not added to `own` — it is in the base. An ack drops nothing from `own`: it only
    ends the batch's delivery (settled 2026-10-07). Nothing is
    counted twice or missed in between. Rejected: closing a generation when a batch
    seals and tagging usage with the generation open at settlement — the record that
    fills a batch (or one racing an interval seal) was tagged with the next
    generation and stayed counted after its ack, until another batch cleared it.
    Check and reserve stay all-or-nothing under that one lock.
  - **Totals apply whatever the config** (settled 2026-10-07): totals arrive only on
    the stream, in the order the control plane sent them, and each one is applied as
    it comes — its windows become the bases of the counts with the same group (or
    global) and type, whatever config the gateway runs, and its live-gateway count
    applies at once (`CONTROL-PROTOCOL.md`, Messages → Matching totals to limits).
    **The first totals after each stream connect replace every base** (a scope they
    do not list: 0); **later ones replace only the bases they list** — the control
    plane sends only the windows that changed (`CONTROL-PROTOCOL.md`, Config stream →
    Totals). A gateway that rejected a config keeps enforcing its own limits on the
    newest totals, the ones the current config dropped included. Rejected: applying totals only when computed under the applied config
    (settled 2026-09-25, H3), with a mismatch state that refused priced USD-limited
    requests after the grace and a gauge of its own — windows are counted per scope
    and type whatever the config, so they mean the same under every config, and the
    gate left a gateway that rejected a config on stale bases; ordering totals by a
    revision (settled 2026-10-06) — totals travel on the stream only, where the
    sending process keeps their order, and a revision makes a restored store's totals
    look old to every gateway, which then ignores them.
  - **Windows**: a counter's current window is the later of the gateway's UTC hour
    or month and the `window_start` the control plane last pushed, and never goes
    back — a push naming a newer window starts it, even before the gateway's clock
    gets there. In-flight reservations count in the window they were made, as in
    file mode. **One exception** (settled 2026-09-25, N-M2): a window ahead of the
    gateway's clock got there from a push, and a later push naming the gateway's own
    clock window takes it back — only the control plane's newest window moves the
    window past the gateway's clock. Otherwise a control plane whose clock stepped
    ahead, then was fixed, would leave hour and month limits counting nothing pushed
    until the gateway's clock caught up. A push naming an earlier window that is not
    the gateway's clock window (a control plane lagging) moves nothing. A pushed
    window starting more than a minute ahead of the gateway's clock logs `pushed
    window ahead of the gateway's clock` (warn, once per window start; seconds early
    at a boundary is ordinary skew and not logged). **Window incarnations**
    (settled 2026-09-25, the independent audit's finding 7): a window that goes
    back to a start it had before is a new window, not the old one revived — every
    time an hour or month window is cleared (rolled, or taken back) it becomes a new
    incarnation, and a reservation is released only in the incarnation it was made
    in. A reservation made at 10:00, cleared by a 12:00 push, then settled after the
    push naming 10:00 again releases nothing — before, it subtracted a hold that no
    longer existed, and the negative count saturated to "all used", refusing every
    request. A negative count is a bug either way: it is logged (`limit counter went
    negative: clamped to 0`, error) and clamped to 0.
  - **Uncounted usage stays in its own window** (settled 2026-09-25, D4): settled
    `own` usage belongs to the window it was settled in (the record's
    `gateway_time`) and leaves when its generation is counted or when that window
    is left behind — a new window starts with none of it, and a record settled in
    the previous window but reaching the limiter after the boundary is not charged
    to the new one. The control plane counts each record in the same window when it
    is its current or previous one (`CONTROL-PROTOCOL.md`, Usage intake → Counted in
    its own window), so a workload using 60% of an hour limit every hour through a
    two-hour outage is never refused, and after recovery its backlog lands in its
    own hours. Usage older than the previous window counts in the control plane's
    current one; the gateway no longer holds it by then, so until its batch is
    counted it is in neither count — the one under-count this rule accepts, bounded
    by a backlog more than a window old. Rejected: carrying all uncounted usage into
    each new window — through an outage, an hour limit became "since the outage
    began".
  - **Per-minute shares**: each per-minute counter enforces `floor(limit ÷
    live_gateways)` from the latest applied totals — 0 or none yet counts as 1, and
    a nonzero limit never shares below 1 (with more gateways than units, each gets
    1 and the sum can exceed the limit: refusing everything would be worse). A new
    count applies at once; what the window counted stays counted. The
    `x-ratelimit-*` headers and the `429` message show the share — the limit this
    gateway enforces. **A share never makes a request impossible** (settled
    2026-09-25, M7): a reservation above the share but within the full limit is
    admitted when this gateway's window for the limit is empty, and otherwise waits
    for it to empty (`Retry-After`); only a reservation above the full limit is
    "request too large". So 60 000 tokens a minute over 4 gateways (a 15 000 share)
    still serves a request at a 16 384 default output — about one a minute per
    gateway. Each config apply and each change of the live count logs a warning, once,
    for every per-minute token limit whose share is below the default output of a
    model its scope may use (output default × live gateways > limit). Rejected: refusing
    such requests as too large — every ordinary request failed while each
    gateway's share sat idle.
  - **A restart counts from the next totals** (settled 2026-10-07): nothing is kept
    across a restart (Configuration sources). Until the first totals since the start,
    priced USD-limited requests are refused (Control-plane mode → No totals yet); a
    boot with the control plane down serves only the seed's free models, which spend
    no budget.
  - **Outage refusal**: *contact* is bytes on the config stream (heartbeats
    included), and nothing else (settled 2026-10-07: a usage ack brings no totals
    back, so it proves nothing about the bases — `CONTROL-PROTOCOL.md`, Control-plane
    outage); an open stream is contact for as
    long as it stays open (a silent one is closed after 45 s). The gateway is in
    **outage** when no stream is open and there has been no contact for longer
    than `global.control_outage_grace_ms` of the config in force; the clock starts
    at process start, so a gateway booting from its seed config with the
    control plane down is in outage once the grace has passed since it started. In
    outage, a request is refused `503 budget_unavailable` (no `Retry-After`: nobody
    knows when the control plane returns) when its model has a price in force and
    any limit that applies to it — any scope on its path — is a
    `usd_per_month` limit (Unpriced models), before
    anything is reserved; other requests keep serving on the last totals and local
    counting, per-minute limits included. The first contact ends it. Rejected:
    counting the time between heartbeats as no contact — a grace below 15 s would
    then flap with a healthy stream.
  - **Outage log lines** (settled 2026-10-01; the 2026-09-30 review's O6): the
    outage's start and end are logged once each, never per request:
    `control plane outage: priced USD-limited models refused` (warn: `kaiak.reason` — `no
    contact`, or `usage not acknowledged` or `usage not shown counted` (below) with
    `kaiak.control.usage_waiting` — `kaiak.control.since_contact`,
    `kaiak.control.outage_grace`) and `control plane outage over: contact is
    back` (info: `kaiak.lasted`, from the grace running out). Nothing signals the grace running
    out, so each transition is logged when the outage is next decided — by a
    request or a metrics scrape (`kaiak_control_outage`) — and can lag it on an
    idle, unscraped gateway; the fields give the true times. Rejected: a timer of
    its own — a second clock for what the gauge and the requests already decide.
  - **Usage acks count for money limits** (settled 2026-09-25, M16; counted usage
    2026-10-07): while usage waits, an open stream is not enough — the gateway is also
    in outage once it has waited longer than the grace. Usage waits two ways:
    - **for an answer** — batches sealed or queued, not yet acknowledged or refused:
      since the first of them was sealed, or since the control
      plane's last answer to one, whichever is later. A control plane that serves
      config but keeps failing `/v1/usage` cannot count this gateway's spend, and
      each replica would enforce only its own view. This clock restarts on the
      answer.
    - **to be shown counted** — batches of this instance acknowledged but not yet
      covered by the `counted_through` of applied totals: since the oldest of them was
      acknowledged. Totals that stop coming while the stream
      stays open — a control-plane process whose change channel died, or whose totals
      reads keep failing — leave the bases frozen while acks still flow, and each
      replica would again enforce only its own view. An ack does not restart this
      clock; only totals covering the batch end its wait.

    With nothing waiting, the stream alone decides. A gateway that spends nothing has
    nothing waiting, and is never refused for frozen totals it does not use.
    Rejected: closing the stream from the control plane when its totals reads keep
    failing — the control plane cannot see a lost change channel, and this one rule
    covers every cause.
  - **Drift in control-plane mode**: the other gateways' usage not yet reported
    (about one batch interval each) and the push delay; knowledge that a batch was
    counted lags the ack by up to a push (about a second), which only over-counts
    until it arrives. After an outage, the backlog of queued batches lands in the windows it
    was settled in when those are the current or previous ones, else in the current
    ones.
- **Drift in file mode** (principle 6; settled 2026-09-24):
  - requests per minute: none — each request is counted before it runs;
  - tokens (minute, hour): while requests run, their reservation stands in for their
    usage; the overshoot is what in-flight requests process beyond their reservation
    — input the estimate undercounts (text past 4 bytes a token, a media item past
    1000 tokens), and all output of a model with
    no output limit;
  - USD per month: the cost of the requests in flight when the budget is reached
    (cost is known only after);
  - restarts: every window starts empty (File mode keeps nothing across a restart).
- Control-plane outage handling: see `CONTROL-PROTOCOL.md`.

## Accounting

- **Usage units** are a generic map (`tokens_in`, `tokens_cached`,
  `tokens_cache_write`, `tokens_out`, `tokens_reasoning` in v1; `images`,
  `audio_seconds`, `characters` later). Prices and limits refer to units. What each
  token unit means, and which are priced: `CONTROL-PROTOCOL.md`, Config → Units and
  price units.
- **One record per routed request** (settled 2026-09-24): every request that reached
  the routing stage settles into exactly one usage record for its last attempt,
  whatever happened next — plus one per retried attempt whose request reached the
  backend in full and got no answer (Routing and reliability: usage across
  attempts).
  Requests refused before routing (auth, unknown model, bad or oversize body, unknown
  path), the model endpoints and the token-counting endpoints (settled 2026-10-06:
  nothing is generated or billed) produce none.
- Token counts come from the backend's usage report, read from the response in the
  client's format as it is relayed (settled 2026-09-24): a non-stream body's top-level
  `usage`, or a stream's last non-null `usage` — the usage chunk the gateway withholds
  from a client that did not ask for it is still read.
  `prompt_tokens_details.cached_tokens` → `tokens_cached`;
  `prompt_tokens_details.cache_write_tokens` → `tokens_cache_write` (settled
  2026-10-02, every backend type: the field is read wherever the backend reports
  it); `prompt_tokens` minus both → `tokens_in`; `completion_tokens` → `tokens_out`;
  `completion_tokens_details.reasoning_tokens` → `tokens_reasoning`. Missing detail
  fields count 0; inconsistent ones are clamped — cached ≤ prompt first, then
  written ≤ prompt − cached, so the three input units always add up to
  `prompt_tokens`; reasoning ≤ completion. Embeddings count `prompt_tokens` only.
- **Messages usage** (settled 2026-10-06): a body's top-level `usage`; a stream's
  `message_start.message.usage`, then each `message_delta.usage`, the latest value
  of each field winning (vLLM repeats `input_tokens` there; llama-server sends only
  `output_tokens`). `input_tokens` → `tokens_in` (Anthropic's `input_tokens` already
  leaves out cache reads and writes — llama-server's too, run on b9917 and b10802);
  `cache_read_input_tokens` → `tokens_cached`; `cache_creation_input_tokens` →
  `tokens_cache_write`; `output_tokens` → `tokens_out`, thinking included;
  `tokens_reasoning` 0 (the format does not report it). Missing fields count 0.
  **`message_start`'s output count is provisional** (settled 2026-10-06, the
  pre-merge review's H1): it is 0 or 1, sent before anything is generated; only a
  `message_delta`'s or a body's `output_tokens` is the answer's count. A stream that
  ends without one — the client gone, a stall, the drain's cut, the backend dying,
  or a backend that never sends it — keeps the reported input and cache units and
  takes `tokens_out` from the estimate over the content seen (Estimation, below),
  never below the provisional count; the record is flagged `estimated`. Rejected:
  trusting the provisional count — a stopped answer billed its output at zero while
  the backend billed it in full.
- **Responses usage** (settled 2026-10-06): a body's top-level `usage`, or the
  `response.usage` of a stream's `response.completed` or `response.incomplete`.
  `input_tokens` includes the cache, as OpenAI's `prompt_tokens` does:
  `input_tokens_details.cached_tokens` → `tokens_cached`,
  `input_tokens_details.cache_write_tokens` → `tokens_cache_write` (OpenAI's field,
  read wherever reported), `input_tokens` minus both → `tokens_in` (clamped as
  above); `output_tokens` → `tokens_out`; `output_tokens_details.reasoning_tokens` →
  `tokens_reasoning`.
- **The record's scopes** (settled 2026-09-27): a record carries the key's group
  path (`groups`, top-level first) as the request's config snapshot had it at
  authentication — the control plane counts it toward those groups and global
  (`CONTROL-PROTOCOL.md`, Usage intake → Counted toward), never re-deriving the path
  from a later config.
- **Estimation** (settled 2026-09-24): when a backend reports no usage, the record is
  **flagged `estimated`**: input is the request's input estimate (Limits: The input
  estimate — the same figure limits reserved before the request ran), output about 4
  bytes per token (rounded up) over the generated content only — as decoded UTF-8: chat
  `content`, `refusal`, reasoning text (`reasoning_content` or `reasoning`, one of
  them), tool-call names and arguments; completions `text`; Messages `text`,
  `thinking` and `tool_use` inputs (stream deltas `text_delta`, `thinking_delta`,
  `input_json_delta`); Responses `output_text` and refusals, reasoning text and
  summaries, and tool-call names with their arguments or a custom tool's input
  (stream deltas likewise: `response.output_text.delta`, `response.refusal.delta`,
  `response.reasoning_text.delta`, `response.reasoning_summary_text.delta`,
  `response.function_call_arguments.delta`, `response.custom_tool_call_input.delta`;
  a tool call's name once, from the `response.output_item.added` event adding its
  item — [B] L2) — settled 2026-10-06. JSON
  structure, roles, signatures, finish reasons and indexes do not count. A non-stream body's choices are kept up to
  4 MiB to be read; past that their raw size counts. The estimated input is all
  `tokens_in`; `tokens_cached`, `tokens_cache_write` and `tokens_reasoning` are 0.
- **Partial** (settled 2026-09-24): a response that stopped early (client disconnect,
  backend failure mid-response, cut off by the drain) is **flagged `partial`** and counts what was relayed
  up to then — the backend's report when one arrived, else the estimate (for
  Messages, the output estimate when only `message_start`'s provisional count
  arrived: Messages usage, above).
  **Client disconnect** cancels the upstream request; tokens generated so far are
  billed. Events are read before they are written, so what the backend sent counts
  even when writing to the client fails.
- **Billed from the moment the request was sent** (settled 2026-09-25, D1; the
  audit's H6): the provider reports when the upstream request was written to the
  backend in full. From then on the backend has the prompt and may process and bill
  it, so any end before the first event — the client gone, the drain's cut, a
  first-event or response timeout, the connection lost — records the input estimated from the
  client's body, no output, flagged `estimated` and `partial`. Before it (connection
  failure, the request not written in full) nothing reached a model: the record
  carries zeros, flagged `partial`, and still exists so every routed request settles
  once. A backend that answers by refusing the gateway's credential processed
  nothing: zeros, `partial`. A backend error status (a `4xx` relayed to the
  client, a `5xx` answered `upstream_error`) records zero units: the backend generated nothing. Rejected: zero units
  for every end before the first event — a client that disconnects while a long
  prompt is prefilled (or a drain that cuts it) got that work free, billed upstream
  and charged to no one.
- **Protocol bound** (settled 2026-09-25, H5): every unit and the cost are clamped to
  2^53 − 1 at settlement — the protocol's integer bound, beyond any limit. A backend
  reporting more is broken or hostile; the clamped record still counts (it spends
  any budget it touches) instead of making its whole batch unacceptable to the
  control plane. Each clamp is logged at warning level with the request ID and
  counted (`kaiak_usage_clamped_records_total`); the record's flags are unchanged.
- **Cost** = units × the prices of one tier of the model's price table entry in force
  at request time (the latest `effective_from` on or before the request's start,
  UTC). The tier is picked from the record's input size,
  `tokens_in + tokens_cached + tokens_cache_write` (an estimated record's is its
  estimated input): the last tier whose `above_input_tokens` is below it, else the
  first; the whole record is priced at that tier (settled 2026-09-29, written tokens
  in the input size settled 2026-10-02; `CONTROL-PROTOCOL.md`, Config → Tiered
  prices).
  Providers report tokens, not money, so the table is required for any priced
  model. Arithmetic
  (settled 2026-09-24): each record's cost is computed once in floating point from the
  per-million prices and rounded to a whole **nano-dollar** (`cost_nano_usd`, an
  integer): records add up exactly downstream, at most half a nano-dollar of rounding
  per record. Records carry the raw units alongside the cost so the control plane can
  re-price.
- **Sinks** (settled 2026-09-24): settled records go to a fan-out of sinks — usage
  metrics and the control-plane sender each register one. A sink is called on the
  request's goroutine as the request finishes and must never block (update memory, or
  enqueue for a background sender). The usage-metrics sink is registered in file mode
  too. Local limits are not a sink: settling needs the
  request's own reservation, so they settle in a request finisher that reads the
  record (Limits).

## Configuration sources

- **The minimal gateway** (settled 2026-09-25; the follow-up audit's E1/E2): in
  control-plane mode `KAIAK_CONTROL_URL` and `KAIAK_CONTROL_TOKEN` — plus the
  backends' API-key variables the config names — are a complete gateway: no seed, a
  read-only filesystem. It boots from the control plane,
  serves, reports usage and status, drains, and **writes nothing anywhere**; with
  the control plane unavailable through the boot wait it exits non-zero at its end
  (Control-plane mode → Boot), and its supervisor restarts it. Every other variable
  below is optional. The gateway is stateless — it writes nothing, ever (Nothing is
  written to disk, below) — and the seed config (the backup for a boot during an
  outage) is an opt-in (`docs/kaiak.md`, principles 1–2).
- `KAIAK_CONFIG_FILE` — file mode: config from a local file, usage totals kept locally
  in memory, no control plane. File mode is for local and development use (settled
  2026-10-07): every count starts from zero at each start.
- `KAIAK_CONTROL_URL` + `KAIAK_CONTROL_TOKEN` — control-plane mode (see
  `CONTROL-PROTOCOL.md`; Control-plane mode below). The URL is the control plane's
  base (`http://` or `https://`, no query); the endpoints are under `<URL>/v1/`. The
  two come together, and never beside `KAIAK_CONFIG_FILE`: any other combination is
  a startup error (settled 2026-09-24).
- `KAIAK_CONTROL_BOOT_WAIT_MS` (default `60000`, above 0) — how long boot keeps
  waiting for the control plane's config on the stream before using the seed
  config, or exiting; what is left of it after the boot
  bounds the wait for the first totals (Control-plane mode → Boot, Readiness waits
  for the first totals).
- `KAIAK_SEED_CONFIG_FILE` — control-plane mode only (beside `KAIAK_CONFIG_FILE` it
  is a startup error): a config file for a boot while the control plane is
  unavailable (Control-plane mode → Boot). It is read and checked completely at
  startup — syntax, schema, semantic rules and backend credentials (an `api_key_env`
  unset is `api-key-env-unset`, as when any config is applied; the audit's N-P4) —
  so an unreadable or invalid seed fails the start, not the outage it is kept for.
  **A seed holds only free models** (settled 2026-09-25, E2): a model with a
  non-empty `prices` list is a startup error naming the model. The seed serves
  while the control plane — which keeps the budgets and the totals — is
  unavailable; a priced model there would be served without the budgets it is
  priced for.
- `KAIAK_BODY_MEMORY_BYTES` (default `536870912`, 512 MiB; whole bytes above 0) —
  the body budget (Request pipeline → request bodies).
- `KAIAK_INSTANCE_ID` — defaults to the hostname (the pod name on Kubernetes). In
  control-plane mode it must have the instance ID shape (`CONTROL-PROTOCOL.md`,
  Messages) — a startup error otherwise, naming the value: a control plane refuses
  every request and batch from an instance ID of another shape.
- **Nothing is written to disk** (settled 2026-09-25, E1; with no opt-in either,
  2026-10-07): usage batches wait in memory until acknowledged (Control-plane mode →
  Usage batches in memory), a fresh usage epoch starts with every process, totals
  live in memory, and file mode keeps nothing across a restart. A pod that dies loses
  only what it had not delivered. Rejected: a default directory — a read-only root
  filesystem (the target Kubernetes setup) would fail the start, and a writable
  scratch directory only moves the loss to the pod's deletion; an opt-in data
  directory (`KAIAK_DATA_DIR`: a last-known-good config, the usage spool on disk, a
  totals cache, a file-mode snapshot) — deployments run on ephemeral disks, where
  none of it survives a restart, and its restart path mis-counted budgets in every
  review that looked at it (a crash forgot spooled spend, a lost index retired unsent
  usage, a restored batch whose cursor had expired was waited on for ever).
- `KAIAK_LISTEN_ADDR` — the API listener (default `:8080`); `KAIAK_ADMIN_ADDR` — the
  admin listener (default `:9090`). Both bind once a config is in force: after the
  startup load (file mode) or the boot and the first totals (control-plane mode,
  which exits instead when it gets no config). An address that cannot be bound is a
  startup error.
- `KAIAK_METRICS_TOKEN` — optional: when set (non-empty), `/metrics` on the admin
  listener requires `Authorization: Bearer <token>` (Observability: admin port).
- `KAIAK_LOG_FORMAT` — `json` (default: one JSON object per line, for production) or
  `text` (development). Any other value is a startup error.
- **OTLP log export** (settled 2026-10-05; Observability → OTLP log export) — the
  standard OpenTelemetry exporter variables, a subset of the OpenTelemetry SDK
  specification's, so a collector sidecar or node agent needs no kaiak-specific
  setup. For each setting the `LOGS` variable wins over the general one:
  - `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` — the URL logs are posted to, used as is;
    else `OTEL_EXPORTER_OTLP_ENDPOINT` with `/v1/logs` appended to its path
    (`http://collector:4318` → `http://collector:4318/v1/logs`). `http://` or
    `https://`, absolute; anything else is a startup error.
  - **On or off**: export is on when either endpoint variable is set, or when
    `OTEL_LOGS_EXPORTER=otlp` — then, with no endpoint, to the specification's
    default `http://localhost:4318/v1/logs` (a sidecar). `OTEL_LOGS_EXPORTER=none`
    or `OTEL_SDK_DISABLED=true` turn it off whatever else is set, so an operator who
    injects an endpoint for other telemetry can opt a gateway out.
    `OTEL_LOGS_EXPORTER` takes `otlp` or `none`; `OTEL_SDK_DISABLED` `true` or
    `false` (any case). Nothing set: off, and nothing is attempted.
  - `OTEL_EXPORTER_OTLP_LOGS_HEADERS` / `OTEL_EXPORTER_OTLP_HEADERS` — headers sent
    with every export, `key=value,key=value`, values percent-decoded (the
    specification's format). Never logged or echoed: an error about them names the
    variable, never a value. No backend can name an `OTEL_` variable as its
    `api_key_env` (Providers: credentials; settled 2026-10-05).
  - `OTEL_EXPORTER_OTLP_LOGS_TIMEOUT` / `OTEL_EXPORTER_OTLP_TIMEOUT` (default
    `10000`) — whole milliseconds above 0: how long one batch may take, its retries
    included.
  - `OTEL_EXPORTER_OTLP_LOGS_PROTOCOL` / `OTEL_EXPORTER_OTLP_PROTOCOL` — only
    `http/json`. The specification's default is `http/protobuf`; here unset means
    `http/json`, the one encoding the gateway writes. `grpc` or `http/protobuf` set
    explicitly is a startup error saying that only `http/json` is supported — a
    gateway that sent JSON to a collector expecting protobuf would fail every
    export instead. Rejected: gRPC and protobuf (no dependency writes them; the
    OTLP/HTTP receiver every collector has accepts JSON).
  - `OTEL_SERVICE_NAME` (default `kaiak`) and `OTEL_RESOURCE_ATTRIBUTES`
    (`key=value,…`, values percent-decoded) — the resource (Observability → OTLP
    log export).
  - A malformed value of any of these is a startup error naming the variable, as
    with every `KAIAK_*` variable — export settings are read before anything else
    starts, and a start failure is written to stderr. Not read: compression (the
    export is uncompressed), certificate and client-key variables (TLS uses the
    system's roots: `SSL_CERT_FILE`, `SSL_CERT_DIR`), `*_INSECURE` (gRPC only), the
    batch processor's `OTEL_BLRP_*` and the attribute limits.
- `KAIAK_DRAIN_GRACE_MS` (default `5000`) and `KAIAK_DRAIN_TIMEOUT_MS` (default
  `60000`) — the drain's grace period and timeout (Lifecycle), whole milliseconds, 0
  or more; anything else is a startup error.
- `KAIAK_DRAIN_FLUSH_RESERVE_MS` (default `10000`, but at most half the drain
  timeout when unset) — control-plane mode: the end of the drain timeout kept for
  the usage flush and the final status (Lifecycle → Draining). Whole milliseconds, 0
  or more, at most the drain timeout — equal cuts in-flight requests as soon as new
  ones are refused and gives the whole timeout to the flush; above it is a startup
  error, not a silent clamp (settled 2026-09-25, E3: an operator who set both
  values meant something the gateway cannot do). The half-timeout cap on the
  default keeps short drains (tests, `docker stop -t`) valid without setting it.
  File mode ignores it: it has no usage to flush.
- `KAIAK_IDLE_TIMEOUT_MS` (default `120000`), `KAIAK_BODY_READ_TIMEOUT_MS` (default
  `60000`), `KAIAK_WRITE_TIMEOUT_MS` (default `60000`) — the client timeouts
  (Lifecycle), whole milliseconds above 0 (a bound of 0 would be none); anything else
  is a startup error.
- `KAIAK_USAGE_MEMORY_BYTES` (default `67108864`, 64 MiB; whole bytes above 0) —
  control-plane mode: the bound on the usage records held in memory waiting for
  delivery, counted as their encoded size (Control-plane mode → Usage batches in
  memory). File mode ignores it.
- `KAIAK_MAX_CONNECTIONS` (default `0`: no cap; whole number, 0 or more) — the most
  connections the API listener keeps open (Lifecycle → Client timeouts).
- Starting with no config source is a startup error. A config file that fails to
  load at startup exits the process with the rejection logged; so does a
  control-plane boot that ends with no config (Control-plane mode → Boot).
- **One apply path** (settled 2026-09-24): every config, whatever its source (file,
  control plane, seed), is validated completely, then swapped in
  atomically, then logged and counted by the same code; invalid config is rejected and
  reported, the running config stays.
- **File-mode reload on SIGHUP**: the gateway re-reads `KAIAK_CONFIG_FILE`. No file
  watcher — watching needs per-OS syscalls or a dependency, and the operator's tooling can
  send the signal (e.g. on a Kubernetes ConfigMap update). Every load logs its trigger
  (`startup`, `sighup`; in control-plane mode `control` and `seed`) and
  result: `config applied`, or `config rejected` with the issue codes and
  `kaiak.config.running=kept` when an older config stays in force. Control-plane
  loads also log `kaiak.config.hash`. Both lines carry `kaiak.config.size` (the
  document's size) and `kaiak.duration` (its validation and snapshot build, to
  the swap or the rejection),
  except a rejection with no document — the file could not be read. SIGHUP in
  control-plane mode is logged and ignored.
- **Rejection codes**: `syntax` (not a single JSON value), `duplicate-member` (an
  object names a member twice, at any depth; the issue's path is the repeated
  member), `schema` (breaks
  `config.schema.json`), the semantic rule codes (`CONTROL-PROTOCOL.md`, Config), and
  the gateway-only `api-key-env-unset` — a backend's `api_key_env` names a variable
  that is unset or empty in the gateway's environment, so the backend could never
  authenticate. It depends on the process environment, so the shared fixtures do not
  cover it.
- **Duplicate members in documents from outside** (settled 2026-09-25; the
  independent audit's finding 1): every JSON document the gateway parses from
  outside — the config file, the seed, `config` stream events, totals, usage acks — is refused
  when an object in it names a member twice, at any depth, before either the schema
  walker or the typed decode runs (`duplicate-member`; for a message it is the
  message's decode error, `CONTROL-PROTOCOL.md` → Messages). Go's two decoders read a
  repeat differently — the generic tree keeps the last, a typed decode into a map
  merges both — so a second `backends` member held a backend the schema never saw
  (one naming `KAIAK_CONTROL_TOKEN` as its credential). One shared
  detector, a token scan with the standard library's JSON decoder: its memory is
  the names of the objects open at the current position, bounded by the document.
- **Control-plane mode** (settled 2026-09-24). Only the `control` package talks to the
  control plane; nothing on the request path waits for it.
  - **Boot** (settled 2026-09-25, E2; the retries D7, settled 2026-09-25; from the
    stream 2026-10-07): the client opens `GET /v1/stream` and waits for its first
    `config` event, opening it again while the control plane is unavailable until
    `KAIAK_CONTROL_BOOT_WAIT_MS` (default 60 s) ends — a jittered delay before each
    retry, drawn uniformly up to a step of 250 ms doubling to at most 2 s — then the
    first source that gives a config:

    | The stream's first config | Seed set | Otherwise |
    | --- | --- | --- |
    | applied | — (serves it) | — |
    | the config rejected (semantic, credentials) — at once, no retry | exit | exit |
    | refused token (`401`) or another `4xx`; a config event the message rules refuse — at once, no retry | exit | exit |
    | unavailable through the whole wait: connection failed or timed out, the stream cut off, no or another `Kaiak-Protocol`, any `5xx`, or a stream with no config published | seed | exit |

    - **Retrying within the wait** (D7): a control plane restarting beside its
      gateways (a rollout, a node drain, a crash) is back within seconds; one
      attempt made every gateway that started meanwhile exit and crash-loop on its
      supervisor's growing backoff. The first failed attempt logs `config not
      received at startup: retrying within the boot wait` (warn; error for a
      protocol mismatch) with `kaiak.control.attempt` and `kaiak.control.boot_wait`,
      later ones at debug; the last failure is logged as before. The retry step is
      shorter than the reconnect backoff's (nothing serves yet), and jittered so
      gateways starting together do not ask together. What waiting cannot fix — a
      refused token, a config the gateway rejects — ends the boot at once. Rejected
      (2026-09-25, reversing E2's "one attempt"): serving the seed config at once
      and catching up — a gateway without a seed has nothing to serve, and the seed
      serves only free models, so a control-plane restart took every
      priced model away from each gateway started meanwhile; the cost is a start
      delayed by up to the wait when the control plane is really down.
    - **Exit** is non-zero with one error line naming the cause — `no config:
      control plane unavailable and no seed config …`, `no config: … answered 401
      unauthorized …`, `no config: the control plane's config was rejected (codes
      …)` — at the end of the boot wait for an unavailable control plane, at once
      otherwise; the supervisor restarts the gateway with its backoff. Rejected:
      starting not ready and waiting (the 2026-09-24 behavior) — a pod that is live
      but never ready hides the cause behind a readiness probe, and a gateway with a
      seed would never use it for a control plane that answers without a config.
    - **The seed is the backup for an unavailable control plane**, never for an
      error the operator must fix: a refused token or a config the gateway rejects
      exits — serving free models while the control plane publishes a config that
      cannot run would hide the error. A missing or other `Kaiak-Protocol` counts as
      unavailable (a proxy answering for a control plane that is down, or a version
      skew that an upgrade ends), and so does any `5xx` and a control plane with
      nothing published yet: it cannot give a config now (settled 2026-09-25).
    - The seed is applied with trigger `seed` and has no `config_hash`: status reports
      `ready` with no applied config hash (the follow-up audit's N-P11), and the
      client keeps opening the stream in the background (reconnect backoff), whose
      config replaces the seed.
  - **Readiness waits for the first totals** (settled 2026-09-25, D8; the
    independent daily-operations review's finding 4): after a boot from the control
    plane, the client follows the control plane and the
    **listeners bind only once the first totals arrive** — they follow the config on
    the stream (`CONTROL-PROTOCOL.md`, Config stream), so the wait is normally
    milliseconds — or once what is left of the boot wait runs out. Logged: `waiting
    for the first totals` (`kaiak.control.totals_wait`), then `first totals received`
    (`kaiak.control.totals_waited`) or `first totals not received within the boot
    wait: priced USD-limited requests are refused until they arrive` (warn). A seed
    boot waits for nothing: it serves only free models. Why: the limiter started from
    nothing and counted every budget as unspent until totals came, so a fresh
    stateless gateway ready before them served a spent budget (the reviewer's binary
    reproduction: `200`, then `429` once the totals arrived) — and each new replica of
    a rollout did so again.
  - **No totals yet** (settled 2026-09-25, D8): until totals have been applied since
    the start, the hour and month spend is *unknown*, which is not "totals with no
    usage" (a scope the first totals on a stream do not list has used nothing). While it is
    unknown, a priced request under a `usd_per_month` limit is refused `503
    budget_unavailable` as in an outage (Limits → Outage refusal), before anything is
    reserved; everything else serves. **Token limits keep counting locally from
    zero**, hourly ones included — matching the outage rule, which refuses only
    USD-limited requests: an hour token limit is a rate guard whose overshoot is at
    most one hour's share, while a budget's is money spent that the month does not
    give back. A gateway that binds after its wait runs out refuses USD-limited
    priced requests from its first request until the totals arrive.
  - **Stream** (settled 2026-10-07): `GET /v1/stream`, no parameters. A `config` event
    whose `config_hash` equals the config the gateway runs, or the one it last
    rejected, is skipped (logged at debug) — one equal to the running config also
    clears a rejection that is set, and reports status (`CONTROL-PROTOCOL.md`,
    Messages → Status: `last_rejection`); any other goes through the apply path,
    **whatever it replaces** — the control plane is the authority on which config is
    current (`CONTROL-PROTOCOL.md`, Current config). `totals` events are decoded and
    handed to the limits consumer, which applies each one (Limits → Control-plane
    mode: Totals apply whatever the config). A `totals` event that cannot be
    decoded **ends the stream** (logged at error level; the reconnect brings complete
    totals — settled 2026-10-07): each later totals event lists only what changed
    since the one before, so a skipped one leaves its changes out until those windows
    change again, and a skipped first one makes the next count as complete, setting
    every base it does not list to 0. Other malformed events are logged and
    skipped. Heartbeats only prove the connection alive: a stream silent for 45 s
    (three missed heartbeats) is closed and reopened — a connection that died
    without a close never ends on its own. Opening the stream (connecting and
    receiving the answer's headers) is bounded by the same 45 s, then the reconnect
    backoff runs (settled 2026-09-25; the audit's H11): an endpoint or proxy that
    takes the request and never answers would otherwise hold the follower forever —
    no config update, no key revocation. Every control-plane answer must also start within 30 s (the HTTP client's
    response-header timeout), a backstop under each request's own bound (status
    10 s, usage batch 30 s). Rejected: a resume position and `resync` (settled
    2026-09-24), and ignoring a config older than the one the gateway runs — the
    control plane sends only its current config, and a gateway refusing an older one
    stays, after a restore, on a config the control plane no longer has.
  - **Reconnect**: before every new attempt after a failure or an ended stream, a
    delay drawn uniformly from 0 to an exponential step — 500 ms doubling per
    attempt, capped at 30 s (full jitter, so gateways that lost the control plane
    together do not return together). A stream that stayed open 30 s resets the
    step. A response with another `Kaiak-Protocol` (or none) is logged at error level
    on every attempt; the gateway keeps retrying on the same backoff and keeps
    serving what it has — an upgrade of either side ends it, and exiting would turn a
    version skew into an outage.
  - **Rejections**: a config the gateway rejects is never applied; its
    `config_hash` and codes are kept for the status report until a later config from
    the control plane is applied (`CONTROL-PROTOCOL.md`, Messages → Status).
  - The client follows the control plane until the drain is over (Lifecycle): requests
    admitted during the grace period run on the newest config and totals.
  - **Usage batches** (settled 2026-09-24; the protocol side in `CONTROL-PROTOCOL.md`,
    Usage batches): the client is a sink on accounting's fan-out; `Record` appends
    the record to the filling batch in memory and never waits. The batch is sealed
    every 5 s, or at once when it reaches 500 records; sealed batches queue in memory,
    each under the next sequence of the epoch, behind the one outstanding. A separate
    goroutine sends the queue's head and nothing else until it is acknowledged or
    refused. An ack removes its batch from the queue and nothing
    else: the batch's usage stays in the limiter's own usage until stream totals show
    it counted (settled 2026-10-07). Acknowledged batches of this instance are
    remembered (batch ID, usage generation and acknowledgement time; no records)
    until applied totals cover them, at most 10 000 — past that the oldest is
    forgotten, which only over-counts until a later batch is shown counted (that
    retires every earlier generation), and the wait to be shown counted (Limits →
    Usage acks count for money limits) keeps its time.
  - **Usage batches in memory** (settled 2026-09-25, E1): queued batches are held in
    memory until acknowledged, under an epoch new with every process (32 random hex
    digits). They are bounded: past
    **`KAIAK_USAGE_MEMORY_BYTES`** held (default 64 MiB: the encoded size of the
    queued batches' records), the oldest queued batches are dropped —
    except the outstanding one, which may be on the wire and whose ack must still
    find it — logged at error level and counted in
    `kaiak_usage_dropped_records_total{reason="memory_bound"}`. A record the checks
    refuse is logged and dropped (nothing to keep it in), a batch the control plane
    refuses likewise. What a pod loses when it ends: the records not yet
    acknowledged if it is killed, only what the drain's flush could not deliver if
    it drains (Lifecycle), and whatever passed the bound during a long outage.
    Rejected: a scratch directory for the queue (a read-only root filesystem, and
    the pod's deletion takes it anyway).
    - **The bound is bytes** (settled 2026-09-25; the independent audit's
      deployment notes): each record counts its encoded size, taken from the
      encoding the record checks at seal time already make (Record checks at seal
      time), so the bound is exact at no extra cost; the queued bytes are exposed as
      `kaiak_usage_queued_bytes`. A typical record encodes to about 500 bytes and
      holds about as much heap (measured: 506 and 536 bytes for a record with
      32-character IDs and a pod-name instance), so the default holds about 130 000
      records — about 145 records/s through the 15-minute outage grace. Rejected: a
      record count (the fixed 10 000 records covered about 100 s at 100 records/s,
      far short of the grace, and records differ in size by their IDs and names).
    - **Record checks at seal time** (settled 2026-09-25, H5): before a batch is
      sealed, each record is checked against the usage record's schema and rules,
      as the control plane will check it. A record that fails would make the control
      plane refuse the whole batch — up to 500 records — so it is dropped alone:
      logged at error level with its request ID, and counted in
      `kaiak_usage_dropped_records_total{reason="invalid"}`; the rest of the batch
      goes on (a batch left empty is dropped, taking no sequence). Settlement
      already clamps every unit and the cost to 2^53 − 1 (Accounting), so this
      guards against what clamping does not cover.
  - **Status reports** (`CONTROL-PROTOCOL.md`, Gateway status): when the client starts,
    when a config stream connects, when the state, the applied config or the
    last rejection changes, when routing changes (a model's queue starting or ending,
    a circuit opening or closing), and every 10 s.
  - **Status minimum gap** (settled 2026-09-24): a report a routing change causes is
    sent at once when the last report is at least 1 s old; otherwise one report goes
    at the end of that second, carrying the state as it is then — however many
    changes fell inside it. Queues and circuits can flip many times a second under
    load; the gap bounds their status traffic to one report a second while still
    telling each change within a second. The other triggers are not held (the drain's
    report must go at once), and any report sent inside the gap covers the pending
    change. Rejected: no gap — a queue flickering empty↔non-empty turns into a report
    per request. State: `ready` while a config is in force — from the control plane
    or the seed (settled 2026-09-25, N-P11: derived from the
    config in force, not from the control plane's, so a seed boot reports ready
    with no applied config hash) — `draining` from the drain's start; `starting` (no
    config in force) is never reported by the binary, which reports only after a
    boot that found a config. The applied config is reported as its hash
    (`applied_config_hash`; null with the seed — settled 2026-10-07).
    Backends (in flight, the configured cap as written — not this gateway's share —,
    deployments' circuits: `closed`, `open`, `half_open`) and models (queued) come
    from routing and the applied config. A failed report is logged when failures start (then at debug level
    until one is delivered again) and is not retried on its own: the next report
    carries the newer state anyway.
- Omitted optional fields get their documented defaults at load time (backend
  timeouts, body cap, `key_id_label`, `group_label`, `control_outage_grace_ms` — 15 minutes, `max_n` — 8,
  `max_concurrent_requests_per_key` — 16; the
  settings of Routing and reliability → Reliability settings and Timeouts); nothing past
  loading sees an unset value.

## Observability

- **Admin port**: `/metrics`, `/healthz` (process alive), `/readyz` (config loaded, not
  draining). Never exposed through an ingress. `/metrics` names every group that owns
  keys, every top-level group, key ID and model and their spend, so it is not for everyone on the network
  (settled 2026-09-25, L2): restrict the admin port to the scraper — on Kubernetes a
  NetworkPolicy admitting only the Prometheus pods (and the kubelet's probes, which
  come from the node) to port 9090 — and, where that is not enough, set
  `KAIAK_METRICS_TOKEN`: `/metrics` then answers `401` (`WWW-Authenticate: Bearer`)
  to any request without `Authorization: Bearer <token>`, compared in constant time
  (SHA-256 digests). `/healthz` and `/readyz` stay open: probes carry no
  credentials, and they reveal nothing. Rejected: a token in config — the scraper's
  secret belongs to the deployment, like the control-plane token.
- **Ops metrics**: request latency, time to first token, tokens/s, errors by class,
  backend health and circuit state, queue depth, in-flight per backend.
- **Usage metrics**: usage records, tokens by unit, estimated cost — labeled by the
  key's group, its top-level group, key ID, model, status. The key-ID and group labels
  can be switched off in config when the series estimate (Cardinality, below) grows
  too large.
- **Metric list** (settled 2026-09-24). Prometheus text format 0.0.4, written by the
  gateway (`kaiak_` prefix, base units, `_total` counters):

  | Name | Type | Labels | Meaning |
  |---|---|---|---|
  | `kaiak_request_duration_seconds` | histogram | `endpoint`, `model`, `status_class` | Arrival to end of response, every client request; its `_count` is the client request count — once per request, whatever its attempts |
  | `kaiak_time_to_first_token_seconds` | histogram | `model`, `backend` | The answering attempt's send to the first stream event carrying generated content, observed as that event arrives; streams only (settled 2026-09-25, D6: from the request's arrival, a failed first attempt's wait was blamed on the backend that then answered) |
  | `kaiak_output_tokens_per_second` | histogram | `model`, `backend` | Decode speed of streams that ran to their end: (`tokens_out` − 1) ÷ time from first content to the last event; needs ≥ 2 output tokens |
  | `kaiak_errors_total` | counter | `class` | Requests that ended in an error (classes below) |
  | `kaiak_request_errors_total` | counter | `key_group`, `root_group`, `key_id`, `model`, `code` | Requests that ended in an error — refusals before routing included — by who sent them and how they ended (Errors by key, below); a series exists once counted |
  | `kaiak_limit_rejections_total` | counter | `scope_kind`, `type` | Requests a limit refused (`rate_limit_exceeded`, `budget_exceeded`) by the kind of scope the limit belongs to (`global`, `group`; settled 2026-09-27) and its type (`requests_per_minute`, `tokens_per_minute`, `tokens_per_hour`, `usd_per_month`); `budget_unavailable` is not counted here — no caller's limit was hit (settled 2026-09-25, D6). No group ID: the log line names it |
  | `kaiak_backend_in_flight_requests` | gauge | `backend` | Requests routed and not yet over; every configured backend present, 0 when idle |
  | `kaiak_backend_max_in_flight` | gauge | `backend` | The backend cap this gateway enforces: its share of `max_in_flight` among the live gateways (Caps across gateways); backends without a cap are absent |
  | `kaiak_queued_requests` | gauge | `model` | Requests waiting in the model's queue; every configured model present, 0 when empty |
  | `kaiak_queue_wait_seconds` | histogram | `model` | Time queued requests waited before getting a slot (refused ones are counted below instead) |
  | `kaiak_queue_rejections_total` | counter | `model`, `reason` | Requests the model's queue refused: `full` (`queue_full`), `timeout` (`queue_timeout`) |
  | `kaiak_retries_total` | counter | `model`, `backend`, `reason` | Retries sent — attempts after an earlier attempt of the same request failed, counted as each is sent — by that attempt's backend and failure: `unavailable`, `timeout`, `server_error`, `rate_limited`, `auth_failed`, `model_missing`, `path_missing`, `endpoint_missing` |
  | `kaiak_upstream_attempts_total` | counter | `backend`, `deployment_model`, `outcome` | Every upstream attempt (first attempts and retries) by its outcome — the circuit breaker's classification, named (below) |
  | `kaiak_upstream_attempt_duration_seconds` | histogram | `backend` | Every upstream attempt from its send to its end: a relayed response to the end of its relay (stream or not), a retried or failed attempt to its failure (a held backend error: its status and first event) |
  | `kaiak_request_attempts` | histogram | `model` | Attempts per routed request, the first included (buckets 1–10) |
  | `kaiak_circuit_open` | gauge | `backend`, `deployment_model` | 1 while the deployment's circuit is open (out of rotation, waiting for a probe), else 0 — half-open reads 0; exactly one sample per configured deployment, however many public models share it (`deployment_model` is the model name on the backend, the log line's `kaiak.deployment.model` — `model` everywhere else is the public name) |
  | `kaiak_circuit_half_open` | gauge | `backend`, `deployment_model` | 1 while the deployment's circuit is half-open (a probe succeeded; the next request is its trial), else 0; one sample per configured deployment, as `kaiak_circuit_open` |
  | `kaiak_deployment_cooling_down` | gauge | `backend`, `deployment_model` | 1 while the deployment cools down after a `429` (Routing and reliability: 429 cooldown), else 0; one sample per configured deployment, as `kaiak_circuit_open` |
  | `kaiak_circuit_transitions_total` | counter | `backend`, `deployment_model`, `to` | Circuit transitions per deployment: `to` is `open`, `half_open` or `closed` |
  | `kaiak_probes_total` | counter | `backend`, `result` | Probes of backends with open circuits: `success`, `failure` |
  | `kaiak_config_loads_total` | counter | `trigger`, `result` | Config loads: `startup`/`sighup`/`control`/`seed`, `applied`/`rejected` |
  | `kaiak_config_last_applied_timestamp_seconds` | gauge | — | Unix time the running config was applied |
  | `kaiak_config_size_bytes` | gauge | — | Size of the running config document in bytes, as applied (a file's bytes, a config event's `config`); absent before the first config (Config load cost, below) |
  | `kaiak_config_apply_duration_seconds` | histogram | `trigger`, `result` | Every config load that had a document, from the start of its validation (syntax, schema, semantic rules, credentials, snapshot build) to the swap or the rejection; labels as `kaiak_config_loads_total`. A file that could not be read is counted there, not here |
  | `kaiak_limits_sync_duration_seconds` | histogram | — | The limiter matching its counters to a newly applied config — done inside the first limiter call after the swap (normally a request's admission), under the limiter's lock, so that request waits for it and every other request waits behind it. Calls that find no new config are not observed |
  | `kaiak_connections_refused_total` | counter | — | API connections closed at accept because `KAIAK_MAX_CONNECTIONS` were open (0 with no cap) |
  | `kaiak_log_export_records_total` | counter | `outcome` | With OTLP log export on (settled 2026-10-05): log records by what became of them — `exported` (accepted by the collector), `failed` (in a batch given up), `dropped` (never sent: a full queue, or still queued at exit; Observability → OTLP log export). All three at 0 from startup; absent when export is off |
  | `kaiak_build_info` | gauge | `version`, `go_version` | Always 1; `version` is the release the binary was built as — the image build links its `git describe` version in (`docs/TECH-STACK.md`, Container images), else the module version Go stamped, else `(devel)` |
  | `kaiak_usage_batch_sends_total` | counter | `result` | Control-plane mode: usage batch sends — `acked`, `rejected` (dropped), `failed` (retried) |
  | `kaiak_usage_queue_batches` | gauge | — | Control-plane mode: sealed usage batches not yet acknowledged (in memory) |
  | `kaiak_usage_queue_records` | gauge | — | Control-plane mode: records in those batches |
  | `kaiak_usage_queued_bytes` | gauge | — | Control-plane mode: encoded bytes of the unacknowledged records held in memory, what `KAIAK_USAGE_MEMORY_BYTES` bounds — every queued record |
  | `kaiak_usage_dropped_records_total` | counter | `reason` | Control-plane mode: usage records dropped before reaching the control plane — `invalid` (failed the record checks, dropped alone), `memory_bound` (over the in-memory bound) |
  | `kaiak_usage_last_ack_timestamp_seconds` | gauge | — | Control-plane mode: Unix time of the last acknowledged batch; absent before one. An ack is not contact for the outage rule (only stream bytes are) |
  | `kaiak_control_connected` | gauge | — | Control-plane mode: 1 while a config stream is open, else 0 |
  | `kaiak_control_last_contact_timestamp_seconds` | gauge | — | Control-plane mode: Unix time of the last contact (stream bytes); the process start before any |
  | `kaiak_control_totals_applied_timestamp_seconds` | gauge | — | Control-plane mode: Unix time stream totals were last applied; absent before any |
  | `kaiak_control_outage` | gauge | — | Control-plane mode: 1 while in outage past the grace (priced money-limited models refused) — the stream down, or usage batches unanswered — else 0 |
  | `kaiak_control_config_rejected` | gauge | — | Control-plane mode: 1 while the latest config received from the control plane was rejected (status `last_rejection` set) and another config stays in force, else 0 (settled 2026-10-07) |
  | `kaiak_usage_records_total` | counter | usage labels | Usage records settled: one per routed request, plus one per retried attempt sent in full and unanswered. Records, not requests — count client requests with `kaiak_request_duration_seconds_count` |
  | `kaiak_usage_clamped_records_total` | counter | — | Usage records whose units or cost passed 2^53 − 1 and were clamped to it (Accounting) |
  | `kaiak_usage_tokens_total` | counter | usage labels, `unit` | Tokens per usage unit (all five token units, zeros included) |
  | `kaiak_usage_cost_usd_total` | counter | usage labels | Estimated cost in USD (the records' nano-dollars ÷ 10⁹, summed exactly as integers) |

  - **Alert on usage delivery, not only the stream** (settled 2026-09-25, M16): a
    control plane can keep the config stream open while it no longer takes usage.
    Alert when `kaiak_usage_queue_batches > 0` and `time() -
    kaiak_usage_last_ack_timestamp_seconds` stays above the outage grace (before any
    ack the metric is absent: alert on a queue that stays non-empty), and on
    `kaiak_control_outage == 1`. A config the gateway rejected shows in its
    status report's `last_rejection`, in `kaiak_config_loads_total{result="rejected"}`,
    and in `kaiak_control_config_rejected`, which stays 1 for as long as the gateway
    runs another config than the control plane's current one.
  - Usage labels (settled 2026-09-27): `key_group` (the key's group ID),
    `root_group` (its top-level group's ID — the key's group itself when that is
    top-level), `key_id`, `model` (public name), `status` — `complete`, or
    `partial` when the record is flagged partial. The two group labels give one
    fixed-size label set whatever the tree's depth: `root_group` sums a whole
    branch (a team, the `users` group); `key_group` is the detail. **Not `group`**
    (settled 2026-09-27): scrape configs commonly set a target label `group`, and
    with `honor_labels: false` Prometheus renames the gateway's to
    `exported_group`, so dashboards on `group` silently find nothing.
    Intermediate levels are not labels — a path of any length would be a label
    set of any width. A group's `labels` never become metric labels. **No
    `backend`** (settled 2026-09-25, H9): it multiplied every group's series by the
    backends serving each model (≈ 240k series per replica at 500 keys × 20 hosts);
    per-backend traffic is the ops metrics' job (`kaiak_upstream_attempts_total`,
    `kaiak_backend_in_flight_requests`). The usage record keeps its deployment.
  - **Upstream attempts** (settled 2026-09-25, M4): per-backend health reads from
    `kaiak_upstream_attempts_total` and `kaiak_upstream_attempt_duration_seconds`,
    fed once per attempt as its slot is released — the same moment and the same
    classification as the circuit breaker's report (Routing and reliability:
    outcome classes), so the two never disagree — not when the request is over:
    a failed first attempt shows while its retry streams for minutes (settled
    2026-09-25, D6; the independent daily-operations review's finding 5), and
    exactly once however the request then ends. Outcomes, a fixed set: success —
    `success`; failures — `unavailable`, `timeout` (a stream's first-event
    timeout), `auth_failed`, `model_missing`, `path_missing`, `server_error` (a backend `5xx` but
    `529`, or a stream opening with an error event naming a failure),
    `broke_off` (broken off, stalled or incomplete after the first event); neutral
    — `endpoint_missing` (settled 2026-10-06), `response_timeout` (a non-stream response timeout, before or after the first
    bytes; before them it is a failure for a half-open trial and from the 3rd in a
    row — Routing and reliability: outcome classes), `rate_limited` (a backend `429` or `529`,
    or an error event naming the backend busy), `client_error` (another backend `4xx`, a
    provider's refusal before sending — `price_option_unsupported`, settled 2026-10-06 —
    or an error event naming the caller's fault),
    `canceled` (the client left, or the drain cut, before the first event),
    `internal` (a gateway fault building the upstream request). Label values are
    config names and the fixed outcomes only, never client input; every configured
    deployment has all fourteen series from the start, at 0 (next bullet).
  - **Series at 0** (settled 2026-09-25, E5; the audit's N-O4): every counter and
    histogram whose label values are fixed or come from the config exists at 0
    from startup, and a config apply creates those of its new models, deployments
    and backends — so `increase(...) > 0` sees the first rejection, opening or
    burst instead of a series born at 1. At startup: `kaiak_errors_total` per class,
    `kaiak_limit_rejections_total` per scope kind × type,
    `kaiak_config_loads_total` and `kaiak_config_apply_duration_seconds` per
    trigger × result (all five triggers, whichever mode),
    `kaiak_limits_sync_duration_seconds`, the usage-delivery counters per result and drop reason,
    `kaiak_usage_clamped_records_total`. Per applied config: per model
    `kaiak_queue_rejections_total` × reason, `kaiak_queue_wait_seconds`,
    `kaiak_request_attempts`; per deployment `kaiak_upstream_attempts_total` ×
    outcome and `kaiak_circuit_transitions_total` × state; per deployment's backend
    `kaiak_upstream_attempt_duration_seconds` and `kaiak_probes_total` × result;
    per model × backend of its deployments `kaiak_retries_total` × reason,
    `kaiak_time_to_first_token_seconds`, `kaiak_output_tokens_per_second`.
    Exceptions, created on first use: the **usage metrics** — their group and key
    labels come from traffic, and every group × key × model at 0 is exactly the
    cardinality the key-ID and group switches exist to avoid; and
    `kaiak_request_duration_seconds` — its `status_class` comes from the answer
    (count client requests with its `_count`, alert on `kaiak_errors_total`).
    Series of models and deployments a reload drops stay until restart, as every
    written series does (counters never go back). Body and header refusals have no
    family of their own: bodies refused are `kaiak_errors_total`
    (`invalid_request`, `server_busy`), oversize headers are answered `431` by the
    HTTP server before the pipeline and not counted.
  - `endpoint`: `chat_completions`, `completions`, `embeddings`, `messages`,
    `messages_count_tokens`, `responses`, `responses_input_tokens` (settled
    2026-10-06), `list_models`, `get_model`, `model_props` (the Anthropic-shaped
    model list counts under `list_models` and `get_model`). `status_class`: `2xx`, `4xx` (499 included), `5xx`.
    **A client that left before any answer stays `4xx`** (settled 2026-10-05): the
    request line leaves out its status (Logs: the request line), the metric keeps
    its own rule — every client request lands in one `status_class`, so `_count`
    stays the client request count. `kaiak_errors_total{class="client_closed"}` and
    `kaiak_request_errors_total{code="client_closed"}` count it, unchanged.
  - Error classes, from the Client API table: `auth` (missing/invalid key),
    `not_found` (`model_not_found`, `unknown_url`), `invalid_request` (the other
    `400`s, `413`, `405`), `rate_limited` (`rate_limit_exceeded`,
    `concurrency_limit_exceeded`), `budget_exceeded`, `budget_unavailable`
    (the outage refusal — platform-side: the caller did nothing wrong),
    `queue_rejected` (`queue_full`, `queue_timeout` — platform-side: the backends
    are at capacity; its own class, apart from `rate_limited` — the caller's limits
    — and from backend faults, because its remedy is capacity or caps),
    `no_healthy_deployment` (every deployment's circuit open — platform-side: the
    backends are failing; its own class because nothing was sent upstream),
    `upstream_unavailable`,
    `upstream_timeout`, `upstream_error` (`upstream_auth_failed`, `upstream_model_missing`, `upstream_path_missing`, `upstream_endpoint_missing`, a backend
    `5xx` — answered `upstream_error` —, a response that broke off upstream), `upstream_rate_limited` (a relayed
    backend `429`, `upstream_overloaded`, a stream ended by an error event naming the
    backend busy), `upstream_client_error` (a relayed backend `4xx` other than
    `429`, `upstream_refused`, a stream ended by an error event naming the caller's
    fault — settled 2026-10-06), `client_closed`, `shutting_down` (`server_shutting_down`, a response the
    drain cut off), `not_ready` (`config_not_loaded`), `server_busy` (the body
    budget spent — platform-side; its own class because its remedy is memory, not
    backend capacity), `internal`. Every class is present from startup, at 0.
  - **Relayed backend errors are classed by whose problem they are** (settled
    2026-09-24): a backend `4xx` is the caller's (context too long, a bad
    parameter) — nothing the platform can fix, so it must not read as a platform
    fault; a `5xx` is the backend's. A backend `429` is platform-side — the
    backend's capacity or quota, the caller did nothing wrong — and has its own
    class, apart from the gateway's own limits (`rate_limited`) and from backend
    faults, because its remedy differs (more capacity or quota, not a fix). Backend
    `401`/`403` stay `upstream_error` via `upstream_auth_failed`: the gateway's
    credential is the platform's problem.
  - **Only config names become label values**: the model only once it passed the
    model-access check, the endpoint only once the path matched one; otherwise the
    label is left out. A client cannot mint series by sending made-up model names or
    paths. No label ever carries a key, a secret or content.
  - An empty label value is left out of the series (Prometheus reads it as the same
    thing): with `group_label` off, series have no `key_group`; with
    `key_id_label` off, no `key_id`.
  - **Cardinality** (settled 2026-09-25, H9). Usage series per replica ≈
    `label sets × models × statuses × 7`, where 7 = records 1 + cost 1 + tokens 5
    (one per unit; settled 2026-10-02, with `tokens_cache_write`), statuses ≤ 2
    (`complete`, `partial`), models = the models each
    label set actually used, and label sets = the distinct group/key combinations
    seen: the keys used with `key_id_label` on (a key belongs to one group, and a
    group to one top-level group, so the group labels add nothing); with it off, the
    groups that own keys; with `group_label` off as well, the top-level groups (every
    key under one top-level group in one set). **`root_group` bounds series only
    when the top-level groups are few** (settled 2026-09-27): a tree whose
    top-level groups are themselves many — people as top-level groups, each with
    a key — has as many `root_group` values as groups, and `group_label` off
    saves nothing; such a tree puts them under one parent (a `users` group) to
    gain from the switch. Hosts do not enter: usage carries no
    backend. Worked example — 20 hosts, 500 keys: 450 personal keys, one per person,
    each person a group under a top-level `users` group, and 50 workload keys, one
    per workload group, in team → project → env → workload branches under 5
    top-level team groups; 3 models per key: both labels on, 500 × 3 × 2 × 7 =
    21,000 series; `key_id_label` off, 500 groups — the same 21,000 here, since every
    key has its own group; `group_label` off too, 6 top-level groups (5 teams +
    `users`), 6 × 3 × 2 × 7 = 252. Multiply by the gateway replicas for the metrics
    store's total, and switch `key_id_label` off first, then `group_label`, when the
    total outgrows what the store is sized for. Series stay until restart once written
    (below). Ops series: request duration `endpoint` × `model` × `status_class` ×
    18 lines (15 buckets + `+Inf`, sum, count); first-token and decode-rate
    histograms model × backend × 15 and 13 lines; queue wait model × 17 lines;
    attempts model × 13 lines; retries model × backend × 8 reasons; limit
    rejections 8 (2 scope kinds × 4 types); upstream attempts
    deployments × ≤ 14 outcomes; attempt duration backends × 18 lines; config apply
    duration 10 × 18 lines (5 triggers × 2 results) and limiter sync 18 — all bounded
    by the config, never by clients or keys.
- **Config load cost** (settled 2026-10-01): large configs (some 20 models over 30
  deployments, thousands of keys) are measured before anything about loading them
  is optimized — `kaiak_config_size_bytes`, `kaiak_config_apply_duration_seconds`,
  `kaiak_limits_sync_duration_seconds` and the `kaiak.config.size` / `kaiak.duration` fields on the
  load lines. The apply is timed from the start of its validation, after any wait for
  another load, to the swap or the rejection; the limiter's sync is timed on its
  own, since it runs later, on the request path, and is the cost requests feel. Both
  histograms share buckets from 0.5 ms to 30 s (0.0005, 0.001, 0.0025, 0.005, 0.01,
  0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30; slower in `+Inf`), resolving a
  small config's sub-millisecond load and a large one's seconds. `result` splits the
  apply histogram because a rejection stops at its first failing stage (a syntax
  error in microseconds): mixed in, rejections would hide what an applied config
  costs. No labels beyond `trigger` and `result` — nothing per key, group or
  backend. Rejected: a size or duration per stage (parse, checks, snapshot build,
  per config section) — kept out until a measurement says which part costs.
- **Two paths, not derived** (principle 7; settled 2026-09-24): usage metrics come
  from a sink on accounting's fan-out, fed each settled record as it is produced —
  the same settlement the usage record carries, so dashboards and billing never count
  tokens two ways, but each path keeps its own state: metrics are in-memory counters
  that reset on restart and are never read back into a record; records never come
  from metrics. Ops metrics are observed by the request pipeline itself; the decode
  rate reads the settled `tokens_out` only as an input.
- **Errors by key** (settled 2026-10-01): `kaiak_request_errors_total` counts every
  request that `kaiak_errors_total` counts, labelled as the usage metrics are —
  `key_group`, `root_group`, `key_id` (all absent without a valid key) and `model`
  (only once it passed the access check, so a client cannot add names) — and by
  `code`: the request log line's `error.type`, or the `kaiak.relay_end` of a response
  broken off after it started (`client_closed`, `upstream_failed`, …). A team's
  dashboard then shows its errors and refusals beside its usage: the usage metrics
  see only settled records, so a refusal before routing never reached them and a
  routed failure looked like an empty success. Its series follow the keys that met
  an error and the codes they met (a fixed set): within the usage series' bound
  (Cardinality) times the codes actually seen. Rejected: these labels on
  `kaiak_errors_total` — its classes are present at 0 from startup for fleet
  alerts, and a per-key family cannot be; carrying outcomes to the control plane
  in usage records and batches — a protocol change, deferred until an app built on
  `kaiak-control` needs them in its own store (`docs/BACKLOG.md`).
- **Key-ID and group switches** (key ID settled 2026-09-24; group 2026-09-27):
  `global.metrics.key_id_label` and `global.metrics.group_label` (default on) are
  read from the live config at each record (and at each error, for
  `kaiak_request_errors_total`). Off, new series carry no `key_id` (no
  `key_group`; `root_group` stays — it keeps a per-branch view, bounded by the
  top-level groups: Cardinality); series already written with one stay until
  restart (counters never go back, and dropping them would make sums fall).
  Switching one off stops growth at once; a restart clears the old series. The
  usage record always keeps its key ID and group path.
- **Logs**: every log line is one structured record on stderr — a JSON object per
  line (`KAIAK_LOG_FORMAT=json`, the default) or `key=value` text — carrying the
  time, the level (`INFO` and above: nothing below is written), the message and the
  line's attributes; with OTLP log export on, the same records also go to a
  collector (OTLP log export, below). Every attribute a line can carry is in the
  field tables below, under the name it is written with. Never the key, a header
  value, a provider secret, prompt or response content, or text a remote party sent
  (No remote text, below). Strings the client
  controls — the method, the path, the model (a refused name is any string) — are
  clipped to their first 256 bytes, then `…`, cut on a character boundary (settled
  2026-09-25, L3): a 1 MiB path must not make a 1 MiB log line. One helper
  (`internal/clip`) serves the log line and error messages.
  - **No remote text** (settled 2026-10-05, the 2026-10-05 review's M2 and M5): a
    line reporting a failure names the local failure class, the status, counts, and
    the gateway's own IDs, config names and addresses — never text or a header value
    received from a remote party (a backend, the control plane, the collector).
    Remote text can carry anything, credentials included: an auth proxy echoing the
    bearer token in its error message; Go's HTTP parser quoting the offending
    header line (`malformed MIME header: missing colon: "Bearer …"`); a control
    URL answering with a credential in `Kaiak-Protocol`. With OTLP log export on,
    a line also leaves the host. So:
    - a transport or protocol failure — the models probe, a request's attempt, the
      control-plane client, the exporter — is logged as the gateway's own words for
      its class (connection refused, timed out, TLS failure, a malformed response),
      never the error text Go built from the bytes received;
    - a `Kaiak-Protocol` mismatch reports the header absent, invalid (not one
      integer), or the number parsed from it — never the value as received;
    - a control-plane error code is logged only when it has the protocol's code
      shape (`CONTROL-PROTOCOL.md`, Shape: errors), else left out; a backend's error
      `code` and `type` only in their identifier shape (Providers: backend error
      bodies). These are the only values a remote party chooses that a line
      carries, each checked against a fixed shape first;
    - the collector's `Status.message` and a partial success's `errorMessage` are
      never logged: its status and the rejected count are.

    Rejected: redacting known secrets from remote text — the gateway knows the
    values it sent, not every encoding of them a remote answer may echo, nor other
    secrets it carries; clipping — it bounds length, not disclosure.
  - **One vocabulary, OpenTelemetry's where the meaning matches** (settled
    2026-10-05): the gateway's log attributes — on stderr and over OTLP alike —
    take the OpenTelemetry semantic-convention name where the convention's
    definition matches what the gateway logs, and a `kaiak.` name for everything
    else. A standard name is used only after reading its definition: a field whose
    meaning differs keeps a `kaiak.` name even where a standard one looks close (the
    tables say why at each such field). Token counts follow the GenAI meaning, so
    the input count is computed, not renamed: `gen_ai.usage.input_tokens` is all
    input, cache reads and writes its parts. One rename for anyone querying the
    previous names, made before the first release (no compatibility: no
    double-writing of old and new names). Usage records, the control protocol and
    the Prometheus metrics keep their own names — each is its own contract.
    Rejected: translating names in the OTLP exporter only — one field would have
    two names, a stderr query and a collector query would differ, and the
    translation table would be a second contract to keep in step; keeping kaiak's
    names — collectors' processors and backends' HTTP and GenAI views read the
    standard names.
  - **Conventions followed**: HTTP, URL, error, exception, server, service, process
    and file attributes from `open-telemetry/semantic-conventions` v1.44.0
    (2026-08-04; the definitions used are unchanged on its `main` on 2026-10-05);
    GenAI attributes from `open-telemetry/semantic-conventions-genai`, `main` at
    commit `e07f4eb` (2026-10-02) — a development-stage convention with no release
    yet. A later rename in a convention is one change to these tables and their
    tests.
  - **Naming the rest** (settled 2026-10-05): every non-standard name starts with
    `kaiak.`. An object with several properties is a namespace
    (`kaiak.limit.scope`, `kaiak.limit.used`); a single fact is one underscored
    name (`kaiak.relay_end`). No name is also a namespace — `kaiak.backend.id`
    beside `kaiak.backend.type`, never `kaiak.backend` — since stores that keep
    attributes as nested objects cannot hold a value and an object at one path.
    Attribute keys are flat dotted strings in the code, never `slog` groups, so
    stderr shows the keys OTLP carries.
  - **Units** (settled 2026-10-05, after review): **names carry no unit**, as in
    OpenTelemetry's conventions; each unit is fixed by kind and stated in the
    tables.
    - A duration is **seconds as a decimal number** (a double) — the request
      line's and a config load's (`kaiak.duration` on the load lines) to the
      microsecond (`0.012345`), the others to the millisecond.
      Seconds are OpenTelemetry's unit for durations
      (`http.server.request.duration`, `gen_ai.response.time_to_first_chunk`) and
      the gateway's Prometheus metrics' (`_seconds`), so a log line and a metric
      read the same number. Rejected: integer milliseconds under `_ms` names — a
      second unit beside the metrics' and the conventions'; Go duration strings
      (`1m0s`) — a query cannot compare them.
    - A size is **bytes**, under OpenTelemetry's `size` form
      (`http.request.body.size`): `kaiak.config.size`, `kaiak.usage.kept_size`.
      Rejected: a `_bytes` suffix.
    - **Money keeps its currency in the name**: `kaiak.usage.cost_usd`, in
      dollars. OpenTelemetry has no convention for currency, and a currency is not
      a unit the conventions leave out of names — a cost without one is ambiguous.
    - Times are RFC 3339 strings in UTC.
    - The rule is for log attributes only: configuration keeps its names and
      units (`KAIAK_*_MS` and `KAIAK_*_BYTES` variables, and the config's `*_ms`
      and `*_bytes` fields, are whole milliseconds and bytes).
  - **The request line**: one per client request, message `request`, written after
    settlement as the request's last act. It is not an OpenTelemetry event: its
    OTLP record carries no `eventName` (settled 2026-10-05) — a collector picks
    request lines by their body, as a stderr query picks them by `msg`. Rejected:
    `eventName: kaiak.request` — a second way to say what the body says, set by an
    exporter that would have to recognise one message among the others.

    | Attribute | Was | Present | Meaning |
    |---|---|---|---|
    | `kaiak.request.id` | `request_id` | always | The request ID (Client API: Request IDs) |
    | `http.request.method` | `method` | always | The method when it is one the HTTP convention knows (`GET`, `POST`, `PUT`, `DELETE`, `HEAD`, `OPTIONS`, `PATCH`, `CONNECT`, `TRACE`, `QUERY`), else `_OTHER` — the convention's rule |
    | `http.request.method_original` | `method` | the method is `_OTHER` | The method as sent, clipped |
    | `url.path` | `path` | always | The path, clipped |
    | `http.response.status_code` | `status` | a response was sent | The status answered. Absent when the client left before any answer — `error.type` is `client_closed` (settled 2026-10-05, the 2026-10-05 review's L6): the HTTP convention sets the status only when a response was sent. Rejected: nginx's `499` — a status no response carried, read by a collector's HTTP views as one. The metrics keep their own rule (Metric list: `status_class`) |
    | `kaiak.request.duration` | `latency_ms` | always | Arrival to the end of the response, in seconds |
    | `kaiak.key.id` | `key_id` | once known | The key ID — a refused key's too, when it has one |
    | `kaiak.key.group` | `group` | once authenticated (settled 2026-09-27) | The key's group: its config ID, the usage metrics' `key_group`; never its `labels` |
    | `kaiak.auth.failure` | `auth_failure` | a `401` | `missing_key`, `malformed_key`, `unknown_key`, `disabled_key`, `expired_key` |
    | `gen_ai.request.model` | `model` | once known | The model the client asked for — the public name, or a refused name — clipped |
    | `gen_ai.request.stream` | `stream` | once the model passed its access check (body endpoints) | The client's `stream` flag |
    | `gen_ai.operation.name` | — (added) | once the path matched a body endpoint | `chat` (chat completions, Messages, Responses), `text_completion` (completions), `embeddings` — the convention's well-known values. The model endpoints and the token-counting endpoints have none (settled 2026-10-06: the convention's `chat` is a chat operation in any of its APIs; counting tokens is no GenAI operation it names) |
    | `error.type` | `error_code` | the request ended in an error | The gateway's error code (Client API table); a relayed backend error status has no gateway error code, so its class: `upstream_client_error`, `upstream_rate_limited` (settled 2026-09-25, D6). A response broken off after it started has `kaiak.relay_end` instead. Low-cardinality, as the convention requires |
    | `kaiak.limit.scope` | `limit_scope` | a limit refusal: `rate_limit_exceeded`, `budget_exceeded`, `budget_unavailable` (settled 2026-09-25, D6 — the line held only the code, and the operator could not tell which of the scopes' limits refused) | `global` or `group` (settled 2026-09-27) |
    | `kaiak.limit.group` | `limit_id` | a refusal by a group's limit | The group's ID — never a key. Absent for a global limit: `kaiak.limit.scope` says `global` (settled 2026-10-05, the 2026-10-05 review's L5): one key for a limit's group on every line, operational ones included, and one way to say "global". Rejected: `kaiak.limit.id`, with `global` for a global limit, beside the operational lines' `kaiak.limit.group` — one concept under two keys and two spellings |
    | `kaiak.limit.type` | `limit_type` | a limit refusal | `requests_per_minute`, `tokens_per_minute`, `tokens_per_hour`, `usd_per_month` |
    | `kaiak.limit.enforced` | `limit` | a limit refusal | The value this gateway enforces — a per-minute limit's share among the live gateways |
    | `kaiak.limit.configured` | `limit_configured` | a limit refusal | The config's value |
    | `kaiak.limit.used` | `used` | a limit refusal but `budget_unavailable` (its spend is unknown) | What the window held — in dollars for a USD limit, on every line that carries it (settled 2026-10-05, the 2026-10-05 review's L4) |
    | `kaiak.limit.requested` | `requested` | a token limit's refusal | The request's reservation: above `kaiak.limit.configured`, a request too large for the limit, not a full window |
    | `kaiak.backend.id` | `backend` | once routed | The last attempt's backend (config ID) |
    | `kaiak.backend.type` | — (added) | once routed | Its type: `openai`, `azure-openai`, `vllm`, `llama-server`, `anthropic`, `azure-anthropic`, `openai-compatible` |
    | `gen_ai.provider.name` | — (added) | once routed to an `openai`, `azure-openai`, `anthropic` or `azure-anthropic` backend | `openai`, `azure.ai.openai`, `anthropic`, `anthropic` — the convention's well-known values. Claude in Foundry is Anthropic's service and API on Azure, and no Azure value names it (`azure.ai.inference` is Azure's Model Inference API), so it is `anthropic` too (settled 2026-10-06). The self-hosted types have no well-known value and leave it out: `kaiak.backend.type` names every type |
    | `kaiak.deployment.model` | `deployment_model` | once routed | The last attempt's model name on its backend (`deployment_model` on the metrics). Not `gen_ai.response.model`: that is the name the backend's answer reports, which the gateway does not read |
    | `kaiak.attempts` | `attempts` | once routed | Attempts made, the first included |
    | `kaiak.tried` | `tried` | more than one attempt | Every attempt in order as `backend/deployment_model:outcome`, the outcome the backend's status or the gateway's error code (`down/m:upstream_unavailable,local/m:200`) |
    | `kaiak.retry_refused` | `retry_refused` | a retry got no slot, or the model's retry budget was spent | `queue_full`, `queue_timeout`, `no_deployment_left`, `retry_budget` |
    | `kaiak.queue.wait_duration` | `queue_wait_ms` | the request entered its model's queue, whatever the outcome | Every attempt's wait, summed, in seconds |
    | `kaiak.time_to_first_token` | `ttft_ms` | a stream that carried generated content | Time to first token in seconds, as the metric measures it: from the answering attempt's send to the first event carrying generated content. Not `gen_ai.response.time_to_first_chunk`: that counts any first chunk, a role-only one included |
    | `kaiak.relay_end` | `relay_end` | a response that stopped early | `client_closed` (the client left or stopped reading), `upstream_failed`, `upstream_stalled`, `upstream_incomplete`, `upstream_timeout` (Providers: upstream failures), `shutdown` (cut off by the drain) |
    | `kaiak.upstream.error.message` | `upstream_error` | an upstream failure | The failure as the gateway saw it, in its own words; may name the backend address, never a credential nor any text the backend sent (Logs: no remote text) |
    | `kaiak.upstream.error.code` | `upstream_error_code` | a backend error status — a `5xx` answered `upstream_error`, a `4xx` relayed — whose body names it | The backend's error `code` — never its message (Providers: backend error bodies) |
    | `kaiak.upstream.error.type` | `upstream_error_type` | as `kaiak.upstream.error.code` | The backend's error `type` |
    | `gen_ai.usage.input_tokens` | `tokens_in` — meaning changed | once settled (routed requests) | **All** input: `tokens_in + tokens_cached + tokens_cache_write`, the backend's `prompt_tokens` (Accounting). kaiak's `tokens_in` — input neither read from nor written to the cache — is this less its two parts below |
    | `gen_ai.usage.cache_read.input_tokens` | `tokens_cached` | once settled | Input read from the cache — part of `gen_ai.usage.input_tokens` |
    | `gen_ai.usage.cache_write.input_tokens` | `tokens_cache_write` (settled 2026-10-02) | once settled | Input written to the cache — part of `gen_ai.usage.input_tokens` |
    | `gen_ai.usage.output_tokens` | `tokens_out` | once settled | All output, reasoning included |
    | `gen_ai.usage.reasoning.output_tokens` | `tokens_reasoning` | once settled | Reasoning output — part of `gen_ai.usage.output_tokens` |
    | `kaiak.usage.cost_usd` | `cost_usd` | once settled | The cost in dollars, for reading |
    | `kaiak.usage.estimated` | `estimated` | once settled | The last record's flag (Accounting: Estimation) |
    | `kaiak.usage.partial` | `partial` | once settled | The last record's flag (Accounting: Partial) |

    Limit values are counts in the limit's unit, USD limits in dollars. Across
    attempts (settled 2026-09-24) the backend, deployment and upstream fields are
    the last attempt's. Token counts and cost are summed over the request's records,
    zeros included.
  - **Operational events** — every other line: startup and stop, config loads,
    control-plane contact, usage delivery, limits, circuits and probes, the drain
    and the listeners. Their messages are sentences
    (`config applied`, `circuit opened`); their attributes, grouped by subject:

    | Attribute | Was | Lines and meaning |
    |---|---|---|
    | `exception.message` | `error` | Any line reporting a failure: the failure in the gateway's own words — a Go error's text when the gateway built it, never text a remote party sent (Logs: no remote text). Not `error.type`, which is a low-cardinality class — this is free text naming files, hosts and causes. `exception.message` is what OpenTelemetry's Go API records an error's text as (`RecordError`); `error.message` is deprecated |
    | `kaiak.reason` | `reason` | Why: the stop (`kaiak stopping`, `kaiak stopped`), the drain's cut (`timeout`, `hurried`), an outage (`no contact`, `usage not acknowledged`, `usage not shown counted`) |
    | `kaiak.trigger` | `trigger` | What asked for it: a config load (`startup`, `sighup`, `control`, `seed`), a probe, a status report, a usage seal or flush, a circuit change (`trial`, the probe's trigger) |
    | `file.path` | `file`, for a path | A file named by its path: the config file and the seed on their load lines |
    | `kaiak.backend.id` | `backend`; `deployment` on `usage out of the protocol's range` | A backend's config ID |
    | `kaiak.deployment.model` | `deployment_model` | A deployment's model name on its backend |
    | `kaiak.duration` | `duration_ms` | How long the line's operation took, in seconds: a config load (its validation and snapshot build, to the swap or the rejection; to the microsecond), a probe |
    | `kaiak.lasted` | `lasted_ms`; `lasted` (a duration string) | How long what ended lasted, in seconds: a config stream, a control-plane outage (from the grace running out) |
    | `process.pid` | `pid` | `kaiak starting`: the process ID |
    | `service.instance.id` | `instance_id` | `kaiak starting`: the instance ID (`KAIAK_INSTANCE_ID`); with OTLP export on, also every record's resource |
    | `kaiak.config.file` | `config_file` | `kaiak starting` in file mode: `KAIAK_CONFIG_FILE` |
    | `kaiak.control.url` | `control_url` | `kaiak starting` in control-plane mode, and every line of the control-plane client: the control plane's URL, credentials redacted |
    | `kaiak.log_export.endpoint` | — (added) | `kaiak starting` with OTLP log export on: the endpoint's `host:port`; absent when export is off. Never a header |
    | `kaiak.signal` | `signal` | `second stop signal: skipping the remaining drain` |
    | `kaiak.config.max_request_body_size`, `kaiak.body_budget.size` | `max_request_body_bytes`, `body_memory_bytes` | The warning that the config's body cap (`max_request_body_bytes`) exceeds the body budget (`KAIAK_BODY_MEMORY_BYTES`) — both in bytes |
    | `kaiak.control.totals_wait` | `wait_ms` | `waiting for the first totals`: the wait's bound, in seconds |
    | `kaiak.control.totals_waited` | `waited_ms` | `first totals received`, `first totals not received within the boot wait…`: the time waited, in seconds |
    | `kaiak.config.hash` | — (added 2026-10-07) | A control-plane config's `config_hash`: control-plane loads, skipped `config` events |
    | `kaiak.config.backends`, `kaiak.config.models`, `kaiak.config.keys` | `backends`, `models`, `keys` | `config applied`: what the config holds |
    | `kaiak.config.size` | `bytes` | `config applied`, `config rejected` with a document: its size, in bytes |
    | `kaiak.config.issue_codes` | `codes` | `config rejected`: the issue codes (an array) |
    | `kaiak.config.running` | `running_config` | `config rejected` while an older config stays in force: `kept` |
    | `kaiak.control.attempt`, `kaiak.control.boot_wait` | `attempt`, `boot_wait_ms` | `config not received at startup…`: the attempt, the boot wait (seconds) |
    | `kaiak.control.delay` | `delay_ms` | `control plane reconnect scheduled`: the delay, in seconds |
    | `kaiak.control.event` | `event` | `stream event ignored: unknown event`: the event's name |
    | `kaiak.control.since_contact`, `kaiak.control.outage_grace`, `kaiak.control.usage_waiting` | `since_contact`, `grace`, `usage_waiting` (duration strings) | `control plane outage: …` (Limits → Control-plane mode: Outage log lines): time since the last contact, the outage grace, how long usage has waited for an ack, or to be shown counted — in seconds |
    | `kaiak.status.state` | `state` | Status report lines: the state reported |
    | `kaiak.usage.batches`, `kaiak.usage.records` | `batches`, `records` | Usage batches and records a line is about (sealed, sent, flushed, dropped) |
    | `kaiak.usage.epoch`, `kaiak.usage.sequence` | `epoch`, `sequence` | A usage batch's ID |
    | `kaiak.usage.kept_records`, `kaiak.usage.kept_size`, `kaiak.usage.max_size` | `kept_records`, `kept_bytes`, `bound_bytes` | Usage dropped to bound memory: the records that stay and their encoded size, and the bound (`KAIAK_USAGE_MEMORY_BYTES`) — sizes in bytes |
    | `kaiak.usage.record_id`, `kaiak.request.id` | `record_id`, `request_id` | A usage record refused by the checks or clamped, and its request |
    | `kaiak.usage.issues` | `issues` | `usage record refused by the protocol's checks…`: what failed |
    | `kaiak.usage.clamped` | `clamped` | `usage out of the protocol's range…`: the units clamped (an array) |
    | `http.response.status_code`, `error.type` | `status`, `code` | `usage batch refused by the control plane…`: the control plane's status and error code — the code only when it has the protocol's code shape (Logs: no remote text) |
    | `kaiak.limit.scope`, `kaiak.limit.group`, `kaiak.limit.type` | `scope`, `group`, `type` | A limit counter's identity: scope kind, group ID (absent for a global limit, as on the request line), type |
    | `kaiak.limit.configured`, `kaiak.limit.enforced`, `kaiak.limit.live_gateways` | `tokens_per_minute`, `share`, `live_gateways` | `per-minute share below the model's default output…`: the configured limit, this gateway's share, the live gateways |
    | `kaiak.model.name`, `kaiak.model.output_default` | `model`, `output_default` | The same line: the model and its default output |
    | `kaiak.limit.window_start`, `kaiak.gateway_time` | `window_start`, `gateway_time` | `pushed window ahead of the gateway's clock` |
    | `kaiak.circuit.failures`, `kaiak.circuit.last_error`, `kaiak.circuit.trial` | `failures`, `last_error`, `trial` | `circuit opened`: the failures in a row, the failure that opened it, `true` when a half-open trial failed |
    | `kaiak.circuit.open_duration` | `open_ms` | `circuit half-open`, `circuit closed`: how long it was open, in seconds |
    | `kaiak.circuit.half_opened` | `circuits_half_open` | `probe succeeded`: circuits it made half-open |
    | `kaiak.backend.base_url`, `kaiak.backend.base_url_hint` | `base_url`, `hint` | `the backend has no models list at its base_url`: the URL and the suggested fix |
    | `kaiak.endpoint`, `kaiak.request.id` | — (added 2026-10-06) | `the backend's server lacks an endpoint its type serves: an older version?` (once per probe interval and backend; Providers → An endpoint missing from a server): the endpoint, by its metric name (`messages`, `responses`, …), and the request that found it — with `kaiak.backend.id` and `kaiak.deployment.model` |
    | `kaiak.backend.type` | — (added 2026-10-06) | `model check not available for this backend type` (`azure-anthropic`, which has no models list): the type — with `kaiak.backend.id`; the request line's field of the same name (above) |
    | `kaiak.drain.grace`, `kaiak.drain.timeout`, `kaiak.drain.flush_reserve`, `kaiak.drain.cut_after` | `grace`, `timeout`, `flush_reserve`, `cut_after` (duration strings) | `draining`: the times in force, in seconds (Lifecycle → Draining) |
    | `kaiak.drain.in_flight` | `in_flight`; `requests` on `drain: cutting off in-flight requests` | Requests in flight |
    | `kaiak.drain.cut_off` | `cut_off` | `drained`: requests cut off |
    | `kaiak.listener.name` | `listener` | `api` or `admin` |
    | `server.address`, `server.port` | `addr` (`host:port`) | `listening`: the bound address and port, split |
    | `kaiak.listener.shutdown_timeout` | `timeout` (a duration string) | `shutdown timed out; closing open connections`: the timeout, in seconds |
    | `kaiak.log_export.failed`, `kaiak.log_export.dropped` | — (added) | `log export failing` (stderr only; OTLP log export, below): records failed and dropped since the previous such line; with `http.response.status_code` (the collector's last answer, when it answered) and `exception.message` (the last error, in the gateway's own words — never the collector's text) |

    The HTTP server's own messages (`http: …`, at warn level) carry no attributes.
- **OTLP log export** (settled 2026-10-05): with an OTLP endpoint configured
  (Configuration sources: OTLP log export), every log line also goes to an
  OpenTelemetry collector over OTLP/HTTP with JSON encoding — for infrastructure
  that cannot read container output. stderr is always written; the export is beside
  it, never instead of it. Configured per gateway from the standard `OTEL_*`
  variables, never from the pushed config: endpoint headers often carry credentials,
  and the config holds no secrets.
  - **Content**: exactly the stderr lines — every line, the request line and every
    operational event, the same level threshold, message and attributes (the field
    tables above). Nothing is added per record but what OTLP's shape needs.
  - **Resource**: `service.name` (`OTEL_SERVICE_NAME`, else a `service.name` in
    `OTEL_RESOURCE_ATTRIBUTES`, else `kaiak`), `service.version` (the build version,
    as `kaiak_build_info` reports it), `service.instance.id` (the instance ID —
    `KAIAK_INSTANCE_ID`, default the hostname — known at start in every mode, so in
    the resource from the first record), plus every other attribute of
    `OTEL_RESOURCE_ATTRIBUTES`. `service.version` and `service.instance.id` are the
    gateway's own and win over the variable's: the instance ID is the name the
    control plane knows the gateway by. One scope, named `kaiak`; no `schemaUrl` —
    the vocabulary follows two conventions at their own versions (Logs, above).
  - **Record mapping**: the line's time → `timeUnixNano` and
    `observedTimeUnixNano`; the level → `severityNumber` (`INFO` 9, `WARN` 13,
    `ERROR` 17; `DEBUG` 5 should one ever be written) and `severityText` (`INFO`,
    `WARN`, `ERROR`); the message → `body` (a string); no `eventName` (Logs: the
    request line), no trace or span IDs. Attributes keep their names and types:
    strings → `stringValue`, integers → `intValue` (a decimal string, as OTLP JSON
    writes 64-bit integers), floats → `doubleValue`, booleans → `boolValue`, string
    arrays → `arrayValue`, times → RFC 3339 strings; anything else → the text the
    JSON handler writes for it (an error's message). A value whose rendering
    panics (its `Error`, `String` or `MarshalJSON`) is written as `slog`'s
    handlers write it —
    `<nil>` for a nil pointer, else `!PANIC: ` and the panic's value (settled
    2026-10-05, the 2026-10-05 review's L3: a typed-nil error crashed the logging
    goroutine with export on, where stderr alone printed `<nil>`). `slog` groups,
    should one appear, are flattened with `.`.
  - **Delivery** — never on the request path: logging a line only puts a copy in a
    bounded queue; a background sender does the rest.
    - The queue holds **10 000** records (about 10 MB at 1 KB a record). Records go out in batches of up to **512**, or every **1 s** when
      fewer are queued, with **one export in flight**. Fixed, not configurable: the
      batch processor's `OTEL_BLRP_*` variables are not read.
    - Each batch is one `POST` to the endpoint (`Content-Type: application/json`,
      `User-Agent: kaiak/<version>`, the configured headers).
    - **What counts as delivered** (settled 2026-10-05, the 2026-10-05 review's
      M3): a `2xx` whose body is empty, or is an `ExportLogsServiceResponse` — a
      JSON object whose `partialSuccess`, when present, is an object with
      `rejectedLogRecords` an integer (or the decimal string OTLP JSON writes) and
      `errorMessage` a string; members OTLP does not define are ignored. A partial
      success counts its rejected records as failed, the rest exported. A `2xx`
      whose body cannot be read — the connection broke, or it is over 4 MiB (the
      OTLP specification's bound) — or is not an `ExportLogsServiceResponse` (a
      login page, truncated JSON) fails the whole batch, **unretried**: the
      collector may have accepted some of its records, and a retry would
      duplicate them. Rejected: "a `2xx` is delivered" — a truncated
      partial-success answer, or a proxy's `200` page, counted a batch exported
      that the collector never confirmed.
    - **No redirects** (settled 2026-10-05, the 2026-10-05 review's M1): the
      exporter does not follow one. A `3xx` fails the batch at once, unretried,
      and `log export failing` reports its status. Go would resend the batch and
      every configured header to the target — `api-key`, `x-honeycomb-team` or
      `DD-API-KEY` to any host, since its redirect rule knows only the standard
      credential headers, and `Authorization` to the same host on another port or
      over plain HTTP — and a `302` to a login page answering `200` would count a
      batch delivered that never arrived. As with the control URL
      (`CONTROL-PROTOCOL.md`, Shape: no redirects), the endpoint must be the
      address that answers.
    - **Retries**: a network error or a `429`, `502`, `503` or `504` is retried
      until the batch's timeout (`OTEL_EXPORTER_OTLP_LOGS_TIMEOUT`, default 10 s)
      runs out — then the batch fails. Any other status fails the batch at once
      (the OTLP specification's retryable set). The wait before a retry is the
      backoff — exponential with jitter, from 0.5 s, doubling, at most 5 s — or
      the collector's `Retry-After` (seconds or an HTTP date) when it is longer:
      `max(Retry-After, backoff)` (settled 2026-10-05, the 2026-10-05 review's
      L1: `Retry-After: 0`, or a date already past, retried back to back for the
      whole timeout). A wait beyond the time left fails the batch at once. A
      `Retry-After` too large to represent saturates instead of reading as
      absent, so it fails the batch by that rule (a valid `Retry-After: 172800`
      was retried after half a second).
    - **A full queue drops the newest records** and counts them. The queue is the
      backlog of an outage: when the collector is back, the oldest records go first.
      Rejected: dropping the oldest — the start of an outage is what explains it.
    - Counted in `kaiak_log_export_records_total{outcome}` (Metric list):
      `exported` (accepted by the collector), `failed` (in a batch given up: an
      unretried status, a redirect, an unreadable answer, retries out of time, a
      partial success's rejections),
      `dropped` (never sent: refused by a full queue, or still queued when the final
      flush ended).
    - **Export problems go to stderr only**, never into the export (no feedback
      loop): one `log export failing` line (warn) at the first failure or drop,
      then at most one a minute while they continue, with the records failed and
      dropped since the previous one, the collector's last status when it answered
      and the last error (Logs: operational events). Header values never appear,
      nor any text the collector sent (Logs: no remote text). **The report at exit
      is never held back** (settled 2026-10-05, the 2026-10-05 review's L2): when
      the final flush ends with records failed or dropped since the last line, one
      more line is written whatever the once-a-minute limit says — drops at exit
      are never silent.
  - **At exit** (settled 2026-10-05): the sender keeps exporting through the drain,
    each line as it is written. Usage comes first — usage is the record, logs are
    not: the final flush runs after the usage flush and the final status
    (Lifecycle → Draining, step 5), as the process's last act, after `kaiak
    stopped`. It sends what is queued until the queue is empty, bounded by the
    drain's deadline (grace + drain timeout from the drain's start) or 1 s from the
    flush's start, whichever is later — so the last lines still go when the usage
    flush took the whole reserve. After a second stop signal, which skips the
    waiting left (Lifecycle → Draining), the 1 s alone bounds it — and a second
    signal arriving during the flush cuts it to that 1 s, so a flush already past
    it ends at once (settled 2026-10-05, the 2026-10-05 review's L2: with a stalled
    collector, a pod lingered until the drain's deadline after `kaiak stopped`,
    the signal unheard). A start that fails
    once the exporter runs (a rejected config at startup, a boot that ends with no
    config) ends with the same flush, bounded by 1 s, after `kaiak stopped with an
    error`, so the line naming the cause reaches the collector. What is still queued
    then is dropped and counted.

## Lifecycle

- **Readiness** only once a config is loaded (file, control plane or seed) and, in control-plane mode, the first totals have arrived or the boot wait
  has run out (Control-plane mode → Readiness waits for the first totals) — which is
  from the first probe: the listeners bind only then. Until they bind, probes find
  the admin port closed (connection refused), liveness included: size the probes'
  startup allowance for the boot wait (`docs/DEPLOYMENT.md`).
- **Client timeouts** (settled 2026-09-25; the audit's H2) — both listeners bound a
  client's progress, never a whole response (a stream runs as long as the backend
  generates and the client reads):
  - request headers within 30 s (fixed), and at most 64 KiB of them — `431` above
    (settled 2026-09-25, N-S5: net/http's default is 1 MiB, buffered for any
    connection, authenticated or not; a key and a few short headers need far less);
  - **idle** keep-alive connections closed after 120 s (`KAIAK_IDLE_TIMEOUT_MS`);
  - **body read**: a request's body must arrive within 60 s of the handler taking it
    (`KAIAK_BODY_READ_TIMEOUT_MS`); a body not in by then answers
    `400 invalid_body`, before any limit or backend work;
  - **answers before the body was read** (a refusal before the inbound stage:
    missing key, draining, …) close the connection after the answer. net/http would
    otherwise first read and discard the unread body — up to 256 KiB — before
    sending anything, so headers declaring a body that never comes held an
    unauthenticated connection forever (the audit's socket reproduction);
  - **write**: each write to the client (a stream event, a flush, a piece of a body)
    must complete within 60 s (`KAIAK_WRITE_TIMEOUT_MS`); a client that stops reading
    is taken as gone — `kaiak.relay_end=client_closed`, the upstream request cancelled,
    the slot freed, usage settled partial; the circuit outcome is the same as for a
    client that left (the backend's status speaks).
  - **connection cap** (settled 2026-09-25; the independent audit's deployment
    notes): optional, `KAIAK_MAX_CONNECTIONS` (0, the default: none). The API
    listener keeps at most that many connections open — idle keep-alive ones
    included, counted before any request on them is authenticated; a connection
    accepted beyond it is **closed at once**, before a byte is read or written, and
    counted in `kaiak_connections_refused_total`. Why close rather than answer
    `503`: closing costs no goroutine, read or write, so a flood cannot make the
    refusals themselves expensive; a client sees a connection closed with no
    answer, like a full backlog. The admin listener is not capped: probes and
    scrapes still pass during a flood. The per-connection bounds above and this cap
    do not replace connection and rate limits at the ingress, which see the clients
    and can refuse them before they reach the pod.
- **A stop signal during the boot wait** (settled 2026-09-25): SIGTERM or SIGINT
  before the listeners bind — control-plane mode, while the boot retries the stream or
  while waiting for the first totals — ends the boot at once: nothing was served
  and no usage exists, so there is nothing to drain; the process logs `kaiak
  stopped` (`kaiak.reason="signal terminated during boot"`) and exits 0. Rejected: acting
  on it only once the listeners bind — a pod deleted during a control-plane outage
  would sit out the whole boot wait (60 s by default) past its termination grace.
- **Draining** on SIGTERM or SIGINT (settled 2026-09-24; SIGINT drains the same way:
  Ctrl-C and `docker stop` users expect a clean stop):
  1. `/readyz` fails with `draining`; status `draining` is sent to the control plane
     (control-plane mode). From here every API response carries `Connection: close`, so keep-alive
     clients reconnect — to another instance once this one is out of rotation. The
     log line `draining` names the times in force (settled 2026-10-01; the
     2026-09-30 review's O8), in seconds: `kaiak.drain.grace`, `kaiak.drain.timeout`
     (`KAIAK_DRAIN_TIMEOUT_MS` as set), `kaiak.drain.flush_reserve` (0 in file
     mode) and `kaiak.drain.cut_after` — timeout less reserve,
     counted from the grace's end: when requests still running are cut (step 4);
  2. new requests are still accepted for the **grace period** (`KAIAK_DRAIN_GRACE_MS`,
     default 5 s): endpoint removal propagates slowly — failing fast here drops
     requests;
  3. then the API listener stops accepting: **new connections are refused** by the OS
     (connection refused). Open connections stay served, so a request that still
     arrives on an idle keep-alive connection gets `503 server_shutting_down` (the
     admission stage) and its connection is closed — a clear answer instead of a
     reset. `http.Server.Shutdown` is not used for this step: it would close idle
     keep-alive connections under clients about to reuse them;
  4. in-flight requests finish, streams included, up to the **drain timeout**
     (`KAIAK_DRAIN_TIMEOUT_MS`, default 60 s: streams can be long) — less the
     **flush reserve** in control-plane mode (`KAIAK_DRAIN_FLUSH_RESERVE_MS`, default
     10 s; settled 2026-09-25, E3): requests still running at *timeout − reserve*
     are cut there, so the records the cut settles still reach the control plane
     within the timeout. Usage batches keep going out on their 5 s seal throughout
     the drain: the client runs until the drain is over. Rejected: flush time added
     after the timeout — the shutdown would outgrow grace + timeout, which
     `terminationGracePeriodSeconds` is sized on, and a SIGKILL mid-flush loses
     exactly what the reserve is for. A request counts
     as in flight from the API handler taking it until its finishers ran — so
     "drained" means its usage record is settled and its limit reservation released.
     At the cut the requests left are **cut off**: their contexts are cancelled
     (the upstream call with them), their connections closed; a relay ends with
     `kaiak.relay_end=shutdown` and a partial record, a request whose response had not
     started answers `503 server_shutting_down` (into a closed connection — it is
     for the log and the record). Requests waiting in a model's queue are in flight
     too: they keep waiting and are served when slots free before the timeout; at
     the cut they leave the queue with the same `503` (no record: never routed).
     Limits settle from those records as usual;
  5. in control-plane mode, the **usage flush** (settled 2026-09-24): the filling batch
     is sealed, and the queued batches are sent until all are acknowledged or
     refused — within what remains of grace + drain timeout, counted from the
     drain's start (at least the reserve) — then a final `draining` status goes out
     (bounded to 2 s). What is not delivered is lost with the process (logged at
     error level, as other lost usage is: `usage not flushed: lost at exit`, with the
     batches and records; settled 2026-10-01, the 2026-09-30 review's O7). Then the
     control-plane client stops, the admin listener stops, `kaiak stopped` is logged and, with OTLP log export on, the
     export's final flush runs (Observability → OTLP log export: at exit; settled
     2026-10-05: after usage — usage is the record, logs are not); the process
     exits 0. The admin listener serves throughout:
     `/healthz` stays 200, `/metrics` answers.
  - A **second** SIGTERM/SIGINT during the drain skips whatever waiting remains:
    in-flight requests are cut off at once, then step 5 runs (the usage flush seals
    the last batch without waiting for acks — what is not delivered is lost — and
    the final status is skipped) and the
    process exits 0 — like a timed-out drain, the shutdown completed; the cut requests are in the
    log and in their partial records. A second signal arriving later, during the
    final log flush, cuts that flush to its 1 s (Observability → OTLP log export:
    at exit; settled 2026-10-05).
  - A listener that fails at runtime starts the same drain, and the process exits 1
    with the error.
  - Kubernetes: `terminationGracePeriodSeconds` must exceed grace + drain timeout +
    about 10 s — after the drain, the final status report (≤ 2 s), the admin
    listener's shutdown (≤ 5 s with a scrape open), and with OTLP log export on its
    final flush — 1 s
    when the drain used its whole time. Default 5 + 60 + 10 s → at least 75 s; the
    Kubernetes default of 30 s would SIGKILL long streams mid-drain.
  - No draining metric: `/readyz` already says it, and a scrape during a
    seconds-long drain adds little.
