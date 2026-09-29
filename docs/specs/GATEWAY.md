# Gateway

> The data plane: client API, request pipeline, routing, limits, accounting, lifecycle.
> Principles in `docs/kaiak.md`; the control-plane side in `CONTROL-PROTOCOL.md`.
> Decisions below are settled 2026-09-24 unless dated otherwise. Config field names and
> shapes are the config document's (`CONTROL-PROTOCOL.md`, Config); this file records
> only how the gateway acts on them.

## Client API

- **Endpoints (v1)**: `POST /v1/chat/completions`, `POST /v1/completions`,
  `POST /v1/embeddings`, `GET /v1/models`, `GET /v1/models/{id}`,
  `GET /v1/models/{id}/props`. Streaming responses are SSE, exactly as OpenAI streams
  them.
- **Auth**: `Authorization: Bearer <key>` (scheme case-insensitive). Keys carry a
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
  API being implemented dictates the shape. Codes the gateway answers with so far
  (settled 2026-09-24):

  | Situation | Status | `type` | `code` |
  |---|---|---|---|
  | No `Authorization` header | 401 | `invalid_request_error` | `missing_api_key` |
  | Not `Bearer <key>`; unknown, disabled or expired key | 401 | `invalid_request_error` | `invalid_api_key` |
  | Unknown model, or model not allowed for the key | 404 | `invalid_request_error` | `model_not_found` |
  | Body is not a JSON object | 400 | `invalid_request_error` | `invalid_json` |
  | The top-level object, or `stream_options`, names a member twice (`param` names it; Request pipeline → duplicate members) | 400 | `invalid_request_error` | `duplicate_member` |
  | Body could not be read | 400 | `invalid_request_error` | `invalid_body` |
  | `model` missing or empty (`param: "model"`) | 400 | `invalid_request_error` | `missing_required_parameter` |
  | An owned field has the wrong type (`param` names it) | 400 | `invalid_request_error` | `invalid_type` |
  | `n` or `best_of` above `global.max_n` (`param` names it) | 400 | `invalid_request_error` | `n_too_large` |
  | `max_tokens` or `max_completion_tokens` below 0 or above the model's `context_length` (`param` names it; Limits → Output limit out of range) | 400 | `invalid_request_error` | `invalid_value` |
  | More generated sequences than `global.max_sequences_per_request` (`param`: `prompt`, `n` or `best_of`), or more embeddings inputs than `global.max_embedding_inputs` (`param: "input"`; Limits → Output multiplicity) | 400 | `invalid_request_error` | `invalid_value` |
  | Body over `max_request_body_bytes`, or over the whole body budget (`KAIAK_BODY_MEMORY_BYTES`) when that is smaller | 413 | `invalid_request_error` | `request_too_large` |
  | Path matches no endpoint | 404 | `invalid_request_error` | `unknown_url` |
  | Known path, wrong method (`Allow` header set) | 405 | `invalid_request_error` | `method_not_allowed` |
  | Backend unreachable: connect refused or timed out, DNS, TLS, connection lost before the first event | 502 | `server_error` | `upstream_unavailable` |
  | Backend refused the gateway's own credential (backend `401`/`403`) | 502 | `server_error` | `upstream_auth_failed` |
  | Backend answered `404` saying the deployment's model does not exist there (Providers: wrong model on a host) | 502 | `server_error` | `upstream_model_missing` |
  | A stream got no first event within the backend's first-event timeout, or a non-stream response did not arrive within its response timeout | 504 | `server_error` | `upstream_timeout` |
  | Backend answered a `5xx` (its error code and type logged, its text neither logged nor relayed; its `Retry-After` kept) | the backend's `5xx` | `server_error` | `upstream_error` |
  | Gateway fault building the upstream request | 500 | `server_error` | `internal_error` |
  | Client left before any answer (logged only, never received) | 499 | `invalid_request_error` | `client_closed` |
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
  | A USD limit covers the request's priced model and the control plane has been out of reach past `control_outage_grace_ms`, or its newest totals have been for another config that long, or no totals for the applied config have arrived since the start (control-plane mode) | 503 | `server_error` | `budget_unavailable` |

  Upstream error messages never name the backend or its address; the log line does
  (settled 2026-09-24). A client string an error message echoes (the method, the
  path, the model name) is clipped to its first 256 bytes, then `…` (settled
  2026-09-25, L3).
  A `401` carries `WWW-Authenticate: Bearer`. Owned fields are `model`, `stream`,
  `max_tokens`, `max_completion_tokens`, `stream_options.include_usage`, `n` and
  `best_of` (chat and completions), `prompt` (completions; only how many prompts it holds),
  `input` (embeddings; only how many inputs it holds),
  read by exact key; a `null` value counts as absent.
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
   "reasoning_efforts": ["low", "high"]}
  ```

  `id` is the public model name; `created` is always `0` — models are declared, not
  created at a moment the gateway knows, and a fixed value keeps listings identical
  across reloads and replicas; `owned_by` is always `"kaiak"`; `reasoning_efforts` is
  `[]` when none are declared.
- **`/v1/models/{id}/props`** is the entry plus what the gateway applies to requests
  (settled 2026-09-24): `defaults` — the declared defaults exactly as configured (`{}`
  when none) — and `output_limit` — `{"default": n, "ceiling": n}`, or `null` when the
  model declares none.

## Model metadata

- **Declared** in config; the gateway does no discovery from backends and serves what
  config says. Filling the declaration in is the control plane's job:
  `kaiak-control`'s `verifyBackend` (`docs/specs/BACKEND-VERIFY.md`) reads what a
  backend reports when an operator adds it or a model (settled 2026-09-29).
  Gateway-side discovery is not planned.
- **The gateway applies declared defaults**: parameters a request omits are filled in
  from the model's declared defaults, so `props` describes what actually runs. Defaults
  are top-level request parameters, any name, any non-null JSON value (the config
  leaves them open so backend-specific parameters pass through); a parameter the
  request sets is never touched — an object default is not merged into the client's
  object. A model with no `output_limit` (e.g. an embedding model) gets no output
  limit set or lowered.
- **Defaults rule** (settled 2026-09-24): every body endpoint — chat, completions and
  embeddings — gets each declared default the request leaves unset; a `null` value
  counts as unset (as it does for OpenAI) and is replaced. Defaults are declared per
  model, and a model serves the endpoints its kind has, so they are not filtered by
  endpoint: an embedding model declares embedding parameters (`encoding_format`,
  `dimensions`, …) and the backend judges them. Defaults are added in key order.

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
2. **Inbound format** — parse the client API (v1: OpenAI): read the body up to the cap
   (request bodies, below), parse only the fields the gateway owns, keep the raw bytes for passthrough. Later
   formats (Anthropic Messages, Responses) are additional inbound plugs. Then the
   model-access check (Client API) — the second half of auth, which needs the model —
   and the model's request parameters: declared defaults and the output limit (Model
   metadata, Limits), which fix the request's effective output limit before limits
   reserve it.
3. **Limits** — check every applicable scope; reserve the output limit (below). The
   reservation settles in a request finisher that runs after accounting's settlement
   and reads its usage record (settled 2026-09-24).
4. **Routing** — resolve alias → deployment; queue for a concurrency slot.
5. **Provider** — send upstream. Same-format pairs pass through (principle 4); others
   translate.
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
one owned object the provider edits, does — is refused, `400 duplicate_member`,
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

## Providers (v1)

- **openai-compatible** — vLLM, llama-server, SGLang, OpenAI. Streaming requests get
  `stream_options.include_usage` set so the final chunk reports tokens.
- **azure-openai** — Azure's OpenAI-compatible `/openai/v1/` API: the same wire format
  and model naming, `api-key` header auth, no `api-version`. The classic
  deployment-in-URL API is not supported.
- **Base URLs** (settled 2026-09-24): an openai-compatible `base_url` is what an
  OpenAI client would use, API version path included (`http://vllm:8000/v1`); the
  gateway appends the endpoint path (`/chat/completions`, …). An azure-openai
  `base_url` is the resource endpoint (`https://<resource>.openai.azure.com`); the
  gateway appends `/openai/v1/` and the endpoint path.
- Credentials are referenced by environment-variable name in config, never inline:
  openai-compatible sends `Authorization: Bearer <value>` (nothing when the backend has
  no `api_key_env`), azure-openai sends `api-key: <value>`. An `api_key_env` starting
  with `KAIAK_` is refused by the schema, both halves (settled 2026-09-25, N-S2):
  those variables hold the gateway's own settings and tokens
  (`KAIAK_CONTROL_TOKEN`, `KAIAK_METRICS_TOKEN`), and a backend credential is sent to
  the backend's URL — which the config author chooses, and the background model
  check calls with no client request. The provider refuses such a name again where
  it reads the credential (settled 2026-09-25, the independent audit's finding 1:
  a config repeating `backends` hid one from the schema): a backend naming a
  `KAIAK_` variable gets no credential at all, so the value never leaves.
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
  pipeline's job, the same for every provider.
- **Passthrough edits** (settled 2026-09-24) are the only changes to the client's
  body: `model` becomes the deployment's model name; a stream whose client did not set
  `stream_options.include_usage: true` gets it set (other `stream_options` members
  kept); declared defaults and the output limit are set as the Model metadata and
  Limits sections say. Edits splice the owned
  values into the raw bytes — every other byte (unknown fields, their values, key
  order, whitespace) reaches the backend unchanged. Each owned key is edited once: a
  body repeating a top-level member never gets here (Request pipeline → duplicate
  members), and the editor refuses a repeated key it edits as a gateway fault
  (settled 2026-09-25).
- **Usage chunk** (settled 2026-09-24): when the gateway set `include_usage` for a
  client that did not ask, the usage-only chunk (`choices: []`, non-null `usage`) is
  withheld from the client; accounting still sees it. Other chunks may carry
  `"usage": null` (OpenAI adds it once `include_usage` is set); they pass as they are.
- **Headers** (settled 2026-09-24), both ways an allowlist:
  - to the backend: `Content-Type: application/json`, `Accept`, `User-Agent: kaiak`,
    `X-Request-Id`, and the backend credential. No client header is forwarded — the
    client's `Authorization` (its kaiak key), cookies and vendor headers never reach
    a backend.
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
  Observers (accounting) read stream payloads as the backend sent them.
- **Upstream failures** (settled 2026-09-24; timeouts and completeness 2026-09-25):
  before the first event, failures are gateway errors (Client API table) — a
  stream's first-event timeout runs until its first event (not just response
  headers), a non-stream response's response timeout until its body ends (Routing and
  reliability: timeouts). A backend `4xx` is relayed as is, body included
  (backends answer in the OpenAI error shape), except `401`/`403`: the client
  authenticated to the gateway, so a refused backend credential is the gateway's
  fault (`502 upstream_auth_failed`). **Backend error bodies** (settled 2026-09-25,
  L5): a `4xx` is the caller's actionable error (context too long, a bad parameter)
  and passes untouched — its text may name backend internals (the server's own
  limits, a parameter it rejects, its version); that is accepted, as rewriting it
  would take the fix away from the caller. A `5xx` is the backend's fault and
  nothing the caller can act on, while its text can name hosts, devices or stack
  traces: the gateway answers its own `upstream_error` under the backend's status
  and `Retry-After`/`Retry-After-Ms`, and logs only the backend's error `code` and
  `type` when its body names them (`upstream_error_code`, `upstream_error_type`:
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
  never looks complete; the log line records `relay_end`: `upstream_failed` (the
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
  vLLM; `{"error": "<text>"}` — Ollama): the message names the deployment's
  backend-side model as a whole word (vLLM: ``The model `…` does not exist.``), or
  the code is `model_not_found` (OpenAI) or `DeploymentNotFound` (Azure). Any other
  `404` stays the caller's and is relayed as it came. The probe checks the model too
  (Routing and reliability → Circuit mechanics), and every applied config is checked
  once in the background: each backend's models list is fetched (up to 8 backends at
  once, off the request path, never delaying or refusing the config) and each
  deployment whose model is not listed gets a warning
  (`the backend does not list the deployment's model`: backend, deployment model);
  an unreachable backend is logged at info. A server that ignores the request's
  model name (llama-server) must still be configured with a name it lists.
  Rejected: relaying the `404` — the client would read its own model name as wrong,
  and a fast `404` made the wrong host the least loaded one, so it drew traffic.
- **Complete responses** (settled 2026-09-25; the audit's M13): HTTP framing ending
  cleanly does not make an answer whole — a backend whose generator dies can end its
  response on an event boundary. A successful (`2xx`) stream is complete once it
  carried `[DONE]`, or once every choice it carried (by `index`) has a non-null
  `finish_reason` — servers that end without `[DONE]` still end whole; a usage-only
  chunk after that changes nothing, and `[DONE]` alone suffices (OpenAI-compatible
  servers send it last). A successful JSON body is complete once its top-level value
  closed. A successful answer that ends before it is complete is a backend failure:
  `relay_end=upstream_incomplete`, a circuit failure, usage settled partial, never
  retried once bytes reached the client; one that ends before its first event (an
  empty stream or body) is a connection lost before the first event
  (`502 upstream_unavailable`, retried). A body cut short of its `Content-Length`, or
  of its chunked framing, already breaks off at the connection (`upstream_failed`).
  Error answers (`4xx` relayed, `5xx` answered by the gateway) are not checked for
  completeness.
  Rejected: counting `content_filter` or other finish reasons differently — any
  finish reason is the backend saying the choice ended.
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
    | Backend `5xx` | yes — `server_error` |
    | Backend `429` | yes — `rate_limited`; the deployment cools down (429 cooldown, below) |
    | Backend `401`/`403` (`upstream_auth_failed`) | yes — `auth_failed`; every deployment of the model on that backend is refused for the request |
    | Backend `404` naming the deployment's model (`upstream_model_missing`) | yes — `model_missing` |
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
    the request ends with its last attempt's answer (`retry_refused:
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
    queue's. The log line says why (`retry_refused`).
  - **Retry budget** (settled 2026-09-25; the audit's L1): per model, the retries
    sent in the last 10 s may reach 20% of the attempts sent in that window, and at
    least 10 — so a model with little traffic still retries, while a failure across
    every deployment does not multiply the load on a struggling fleet by the attempt
    count. A retry past the budget is not sent: the request ends with its attempt's
    answer, as a retry that got no slot, `retry_refused: "retry_budget"`. Counted
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
  5xx, a refused credential, a missing model, broken-off responses — the full list
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
  | Backend `5xx` (answered `upstream_error`) | failure |
  | Backend `401`/`403` (`upstream_auth_failed`) | failure |
  | Backend `404` naming the deployment's model (`upstream_model_missing`) | failure |
  | Response broken off upstream after the first event (`relay_end=upstream_failed`) | failure |
  | Stream silent for the stall timeout after the first event (`relay_end=upstream_stalled`) | failure |
  | Successful response ended before it was complete (`relay_end=upstream_incomplete`) | failure |
  | Non-stream response timeout before the first bytes (`upstream_timeout`) | neutral — but a failure for a half-open trial, and from the 3rd in a row on the deployment with no success between |
  | Non-stream response timeout after the first bytes (`relay_end=upstream_timeout`) | neutral |
  | Backend `429` | neutral |
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
  - The probe: `GET` on the models list (`<base_url>/models` for openai-compatible,
    `<base_url>/openai/v1/models` for azure-openai) with the backend's credential,
    over the backend's connection pool, bounded by its connect timeout plus 5 s.
    Success is a `2xx` carrying an OpenAI models list (settled 2026-09-25, H8); a
    circuit goes half-open only when the list has its deployment's backend-side
    model among `data[*].id` — one whose model is missing stays open while the
    backend's others go half-open, its prober keeps probing, and the first such probe
    logs `circuit kept open: the backend does not list the deployment's model` (warn).
    An azure-openai list names models, not the deployment names requests carry, so
    it is not checked there.
  - A probe is an invocable mechanism (`ProbeNow`, with the trigger that invoked it);
    the prober's timer is its trigger `interval`. A probe cut short by shutdown is
    neither counted nor logged.
  - Log lines: `circuit opened` (warn: backend, deployment model, failures — or
    `trial: true` when a half-open trial failed, or the probe's `trigger` when a
    failed probe re-opened a half-open circuit —, `last_error` — the failure that
    opened it), `circuit half-open` (info: trigger, `open_ms`), `circuit closed`
    (info: trigger `trial`, `open_ms`), `probe succeeded` (info when it made circuits
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
  - The request log line carries `queue_wait_ms` for every request that entered its
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
    connection is cut, `relay_end=upstream_timeout`, neutral, usage partial;
  - `stall_timeout_ms` (default 120 s) — a **streaming** request, the longest silence
    between data events once the first event arrived. It runs only while the gateway
    waits on the backend (time spent writing to a slow client is not the backend's
    silence). **Only data events are progress** (settled 2026-09-25, E8): comment
    blocks (`: ping`) and blank keep-alives are relayed but do not reset it — a
    backend pinging while it produces nothing has stalled; backends that reason
    silently raise their `stall_timeout_ms` instead. Running out ends
    the relay as a backend failure: the connection is cut, `relay_end=upstream_stalled`,
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
  request passes every scope's limits that cover its model. A group's limits are its
  effective ones, its parent's `child_defaults` merged in.
- **Types**: requests/min, tokens/min, tokens/hour, USD/month (extensible). Each limit
  names the models it covers. Per-minute windows are **sliding**; hourly windows are
  fixed UTC hours; USD months are calendar months, UTC.
- **Where each window is enforced** (principle 6):
  - **Per-minute windows** — locally in each gateway, on a **share**: limit ÷ the
    live-gateway count the control plane pushes (Control-plane mode, below). Drift:
    bounded by skew in traffic across replicas within the minute. Known limitation: HTTP keep-alive pins a client to one
    replica, so a scope driven by a single client gets about its share, not the full
    limit (demand-weighted shares are in `docs/BACKLOG.md`).
  - **Hourly and longer windows** — tracked by the control plane from usage records; the
    gateway enforces the pushed totals plus its own usage not yet counted against the
    limit (`CONTROL-PROTOCOL.md`, Budgets), each pushed window matched to its counter
    by limit identity (`CONTROL-PROTOCOL.md`, Messages → Matching totals to limits).
    Drift: in-flight requests plus the other gateways' unreported usage, about one
    batch interval each.
- **File mode** (single instance only): the same local window counter enforces every
  window, at the full limit; windows longer than a minute are kept in the file-mode
  usage snapshot so a restart does not reset them. The gateway only enforces limits —
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
  - The estimate has two figures: the **total** (every prompt of a completion batch —
    what limits reserve and estimated records bill) and the **input one sequence
    sees** (the request less every prompt of a batch but the largest — what the
    injected default is fitted to: the context belongs to each generated sequence,
    not to the batch). Chat has one prompt, so the two are equal.
- **Output limit**: each model has an output-limit **default** (applied when the request
  sets none) and **ceiling** (requests above it are lowered). While a request runs, its
  output limit counts as used against its scopes' local allowance; the unused part is
  released at settlement.
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
  declares no limit and the client sent none).
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
  small number. `n` and `best_of` cannot be model defaults. Rejected: a per-model
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
  rate limits; the log line's `error_code` tells the two apart. No metric is
  labelled by key for it (series per key would be unbounded; the per-key usage
  metrics already exist). Rejected: `requests_per_minute` before the body — it
  bounds arrivals, not requests held open; one counter per key is simpler.
- **Local counters** (settled 2026-09-24): one window counter per limit and scope,
  counting requests, tokens or nano-USD (accounting's cost unit) against an effective
  limit — the configured value in file mode, the pushed share for per-minute windows
  in control-plane mode. Sliding minutes count one-second buckets over the last 60 s;
  hours and months reset at their UTC boundary. A limit applies to a request when its
  model set covers the request's model (`models` omitted: every model, counted
  together); the scopes checked are global and every group on the key's path, a
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
  outage or mismatch refusals — and none is spent by it. A free vLLM model under a
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
  `tokens_in + tokens_cached + tokens_out` (cached input counts: every token the
  backend handled; reasoning is inside `tokens_out`); the record's cost is added to
  USD counters; the request stays counted, whatever its outcome — a request that
  failed upstream or was dropped still took a slot. A request with zero units (no
  backend answer, backend error status) releases its token reservation. A reservation
  counts in the bucket or window where it was made and is released only if that
  bucket or window is still current; the actual amount counts at settlement time, so
  every token counts in exactly one window.
- **Refusal** (settled 2026-09-24): `429` in the OpenAI shape (table above). The
  message names the kind of scope — `group limit` or `global limit` (settled
  2026-09-27) — its value and use, never a group ID or a group's `labels`: which group refused is
  the operator's to read in the log line (Observability → Logs: `limit_scope`,
  `limit_id`), and a client need not learn the tree's names. A token request larger
  than the limit's full value says so.
  `Retry-After` (whole seconds, rounded up) is the time until every refusing limit
  has room: a sliding minute frees room as its oldest buckets expire; an hour or
  month at its end. OpenAI clients retry `429` on their own; for a spent budget they
  get the month end in `Retry-After`.
- **Rate-limit headers** (settled 2026-09-24): `x-ratelimit-limit-requests`,
  `x-ratelimit-remaining-requests`, `x-ratelimit-reset-requests`, and the same for
  `-tokens`, on refusals **and** on admitted responses (as OpenAI sends them; clients
  pace themselves on them). Each set comes from the applicable limit of its kind with
  the least remaining (for tokens: minute and hour limits alike); a kind with no
  applicable limit sends no headers; USD limits have none. Remaining is taken after
  the request's own reservation; reset is the time until that limit's window holds
  nothing, as a duration (`1m0s`, `45m0s`).
- **Config reload** (settled 2026-09-24): the counters follow the live config, not
  each request's snapshot. A limit that still exists — same identity: group (or
  global), type and model set, order ignored (settled 2026-09-27) — keeps its
  counter, and a changed value applies at once
  to the count so far; a new limit starts empty; a removed limit's counter is
  dropped. A group deleted and created again under the same ID is such a removed
  limit returning: in control-plane mode the pushed totals give it its window's
  spend back (`CONTROL-PROTOCOL.md`, Usage intake → Totals); in file mode its
  counters start empty.
- **Model-set edits keep the spend** (settled 2026-09-25, D5): a new limit with no
  counter of its identity, whose group (or global) and type match a limit the reload
  drops — its model set changed — takes over that counter: its count (and in
  control-plane mode its pushed base, until totals for the new config name the new
  identity, and its uncounted usage). Several such predecessors (ambiguous — e.g.
  two limits merged into one) carry the one with the most used, logged as a
  warning; a predecessor taken by several new limits is copied into the others
  (without its in-flight reservations). Each carry-over is logged. Predecessors
  come from the config the gateway applied before, which is not the previous
  published version when it skipped some (`CONTROL-PROTOCOL.md`, Budgets →
  Model-set edits: each side carries against the config it last held). The control
  plane carries its windows the same way (`CONTROL-PROTOCOL.md`, Budgets →
  Model-set edits). Rejected: model set in the identity with nothing carried —
  adding a model to a spent monthly budget forgave the month.
- **File-mode usage snapshot** (settled 2026-09-24; only with a data directory —
  without one, a restart starts with empty windows): `limits.json` in the data
  directory, format version 2 (settled 2026-09-27: limits are named by group), holding
  each hour and month window with settled usage (`group` — absent for a global
  limit —, `type`, `models`, `window_start`, `used`) — unsettled reservations
  are left out. Written every 30 s and on shutdown; restored at startup, where a
  window is kept only if its limit still exists and its window is the current one.
  Per-minute windows are not kept. A snapshot that cannot be read is logged and the
  gateway starts with empty windows (it is a cache).
- **Control-plane mode** (settled 2026-09-24; the protocol side in
  `CONTROL-PROTOCOL.md`, Budgets and Messages → Totals):
  - **Hour and month counters** count `base + own`: `base` is the pushed `used` of
    the counter's window (matched by limit identity; a counter the latest applied
    totals do not list has a base of 0, and a limit a reload adds takes its base
    from them), counted while it names the counter's current window; `own` is this
    gateway's usage the control plane has not counted — reservations in flight, and
    settled amounts tagged by **usage generation**. Each record carries the
    generation of the usage batch the control client took it into, read under the
    lock that seals batches, so it is exactly the batch the record is sealed in; the
    record goes to the batch as it settles, and local limits settle from it after
    (settled 2026-09-25, H4). A totals message that shows a batch counted (its ack,
    or `counted_through` at or past it) drops every generation up to the batch's
    from `own`, in the same step, under the limiter's lock, as it applies the totals
    that include it (when they are newer); a record that settles after its batch was
    shown counted (a retried attempt's record, published mid-request; an ack faster
    than the request's end) is not added to `own` — it is in the base. Nothing is
    counted twice or missed in between. Rejected: closing a generation when a batch
    seals and tagging usage with the generation open at settlement — the record that
    fills a batch (or one racing an interval seal) was tagged with the next
    generation and stayed counted after its ack, until another batch cleared it.
    Check and reserve stay all-or-nothing under that one lock.
  - **Totals follow their config** (settled 2026-09-25, H3): a totals message names
    the config it was computed under (`config_epoch`, `config_version`), and its
    windows become the bases only when that is the applied config. Otherwise —
    typically a config this gateway rejected, or totals for a publish the stream has
    not delivered yet — the bases and `own` stay as they are: generations the message
    shows counted leave `own` only once totals that include them are applied, and
    the message waits, applied as soon as its config is (replaced by newer totals
    meanwhile). Its live-gateway count applies at once. While the newest totals are
    for another config for longer than `global.control_outage_grace_ms`, a request
    for a priced model covered by a `usd_per_month` limit is refused `503
    budget_unavailable`, as in an
    outage: the control plane counts other limits, so this config's spend is
    unknown (the rejection is in the gateway's status report). The mismatch shows
    in `kaiak_control_config_mismatch` (1 from the moment it starts, before the
    grace; settled 2026-09-25, N-P3) — its own gauge, not `kaiak_control_outage`:
    the control plane is reachable, and the remedy is the config (fix what this
    gateway rejected), not the control plane. The refusal keeps its code
    `budget_unavailable`. Rejected: applying
    another config's windows by limit identity — a limit the rejected config
    changed had no window, so a spent budget reset to zero on every ack; and
    dropping acknowledged usage while ignoring the windows, which loses it.
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
    model it covers (output default × live gateways > limit). Rejected: refusing
    such requests as too large — every ordinary request failed while each
    gateway's share sat idle.
  - **Restart keeps the last totals** (settled 2026-09-25, M5; only with a data
    directory — without one, a restart counts from the next totals: a boot with the
    control plane down serves only the seed's free models, which spend no budget): `limits.json` is
    neither read nor written; instead `totals.json` (format version 2, settled
    2026-09-27: counters are named by limit identity, group or global) holds each
    hour and month counter's pushed base (`base_window_start`, `base`) and the usage
    the control plane had not counted (`window_start`, `uncounted`), under the
    config the counters were matched to (`config_epoch`, `config_version`) and the
    live-gateway count. It is written whenever totals are applied and at shutdown —
    never per request — and restored at boot, after the config boot and before
    traffic. A file of another config than the one booted (a fresh snapshot of a
    newer version, another store) is discarded and logged: its bases describe other
    limits. Restored uncounted usage counts only when its window is still current
    and is tagged with the newest generation of the batches restored from the usage
    spool, so it leaves once they are counted; with none restored it leaves with the
    first totals applied. So a gateway restarted with the control plane down keeps
    enforcing a spent budget — and hour limits for as long as the outage lasts —
    instead of counting from zero. Restored totals do not end the outage rule: with
    no contact since the start, priced USD-limited requests are refused once the
    grace has passed (Outage refusal), restored or not. Per-minute windows are never
    kept. The file is a cache: the control plane's next totals replace it, and the
    control plane stays the record. Rejected: counting from the next pushed totals —
    every restart forgot the month until the control plane answered.
  - **Outage refusal**: *contact* is a config snapshot fetched, bytes on the config
    stream (heartbeats included) or a usage ack; an open stream is contact for as
    long as it stays open (a silent one is closed after 45 s). The gateway is in
    **outage** when no stream is open and there has been no contact for longer
    than `global.control_outage_grace_ms` of the config in force; the clock starts
    at process start, so a gateway booting from its last-known-good or seed config with the
    control plane down is in outage once the grace has passed since it started. In
    outage, a request is refused `503 budget_unavailable` (no `Retry-After`: nobody
    knows when the control plane returns) when its model has a price in force and
    any limit that applies to it — any scope, model set covering the model — is a
    `usd_per_month` limit (Unpriced models), before
    anything is reserved; other requests keep serving on the last totals and local
    counting, per-minute limits included. The first contact ends it. Rejected:
    counting the time between heartbeats as no contact — a grace below 15 s would
    then flap with a healthy stream.
  - **Usage acks count for money limits** (settled 2026-09-25, M16): while usage
    batches wait for an answer (sealed or queued, not yet acknowledged or refused),
    an open stream is not enough — the gateway is also in outage once they have
    waited longer than the grace: since the first of them was sealed (or restored at
    boot), or since the control plane's last answer to one, whichever is later. A
    control plane that serves config but keeps failing `/v1/usage` cannot count this
    gateway's spend, and each replica would enforce only its own view. With nothing
    waiting, the stream alone is contact, as before. The ack clock restarts on the
    answer, before its totals are applied.
  - **Drift in control-plane mode**: the other gateways' usage not yet reported
    (about one batch interval each) and the push delay; knowledge that a batch was
    counted can lag (a push or ack not arrived yet), which only over-counts until
    it does. After an outage, the backlog of spooled batches lands in the windows it
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
  - restarts: per-minute windows start empty (at most one minute of allowance
    regained); hour and month windows lose at most 30 s of settled usage after a
    crash, nothing after a clean shutdown.
- Control-plane outage handling: see `CONTROL-PROTOCOL.md`.

## Accounting

- **Usage units** are a generic map (`tokens_in`, `tokens_out`, `tokens_cached`,
  `tokens_reasoning` in v1; `images`, `audio_seconds`, `characters` later). Prices and
  limits refer to units. What each token unit means, and which are priced:
  `CONTROL-PROTOCOL.md`, Config → Units and price units.
- **One record per routed request** (settled 2026-09-24): every request that reached
  the routing stage settles into exactly one usage record for its last attempt,
  whatever happened next — plus one per retried attempt whose request reached the
  backend in full and got no answer (Routing and reliability: usage across
  attempts).
  Requests refused before routing (auth, unknown model, bad or oversize body, unknown
  path) and the model endpoints produce none.
- Token counts come from the backend's usage report, read from the response in the
  client's format as it is relayed (settled 2026-09-24): a non-stream body's top-level
  `usage`, or a stream's last non-null `usage` — the usage chunk the gateway withholds
  from a client that did not ask for it is still read. `prompt_tokens` minus
  `prompt_tokens_details.cached_tokens` → `tokens_in`; `cached_tokens` →
  `tokens_cached`; `completion_tokens` → `tokens_out`;
  `completion_tokens_details.reasoning_tokens` → `tokens_reasoning`. Missing detail
  fields count 0; inconsistent ones are clamped (cached ≤ prompt, reasoning ≤
  completion). Embeddings count `prompt_tokens` only.
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
  them), tool-call names and arguments; completions `text`. JSON structure, roles,
  finish reasons and indexes do not count. A non-stream body's choices are kept up to
  4 MiB to be read; past that their raw size counts.
- **Partial** (settled 2026-09-24): a response that stopped early (client disconnect,
  backend failure mid-response, cut off by the drain) is **flagged `partial`** and counts what was relayed
  up to then — the backend's report when one arrived, else the estimate.
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
- **Cost** = units × the model's price table entry in force at request time (the
  latest `effective_from` on or before the request's start, UTC). Providers report
  tokens, not money, so the table is required for any priced model. Arithmetic
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
  backends' API-key variables the config names — are a complete gateway: no data
  directory, no seed, a read-only filesystem. It boots from the control plane,
  serves, reports usage and status, drains, and **writes nothing anywhere**; with
  the control plane unavailable through the boot wait it exits non-zero at its end
  (Control-plane mode → Boot), and its supervisor restarts it. Every other variable
  below is optional. The gateway is stateless by default: the data directory (a
  disk cache and spool) and the seed config (the backup for a boot during an
  outage) are opt-ins (`docs/kaiak.md`, principles 1–2).
- `KAIAK_CONFIG_FILE` — file mode: config from a local file, usage totals kept locally
  (snapshotted to the data directory when one is set), no control plane.
- `KAIAK_CONTROL_URL` + `KAIAK_CONTROL_TOKEN` — control-plane mode (see
  `CONTROL-PROTOCOL.md`; Control-plane mode below). The URL is the control plane's
  base (`http://` or `https://`, no query); the endpoints are under `<URL>/v1/`. The
  two come together, and never beside `KAIAK_CONFIG_FILE`: any other combination is
  a startup error (settled 2026-09-24).
- `KAIAK_CONTROL_BOOT_WAIT_MS` (default `60000`, above 0) — how long boot keeps
  asking an unavailable control plane for the config snapshot before using the
  last-known-good or seed config, or exiting; what is left of it after the boot
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
- `KAIAK_DATA_DIR` — the data directory, **optional, no default** (settled
  2026-09-25, E1). Unset, the gateway writes nothing anywhere: usage batches wait
  in memory until acknowledged (Control-plane mode → Usage batches in memory), a
  fresh usage epoch starts with every process, there is no last-known-good copy,
  totals live in memory only, file mode keeps no limits snapshot, and no lock is
  taken. A pod that dies loses only what it had not delivered. Rejected: a default
  directory — a read-only root filesystem (the target Kubernetes setup) would fail
  the start, and a writable scratch directory only moves the loss to the pod's
  deletion. Set, the persistence below is kept exactly: the directory (relative to
  the working directory, or absolute) is created at startup if missing. Every file
  in it is written atomically
  (temporary file + rename) inside a `{ format_version, data }` envelope; a file with
  another format version is deleted and the deletion logged. File mode keeps the
  usage snapshot there (`limits.json`, Limits); control-plane mode the last-known-good
  config (`last-known-good.json`), the last applied totals (`totals.json`, Limits →
  Control-plane mode) and the usage spool (`usage-spool.json`, `usage-batch-*.json`,
  `usage-rejected-*.json`; Control-plane mode → Usage spool).
  **One gateway per data directory** (settled 2026-09-25; the audit's M11): the
  gateway holds an exclusive advisory lock (`flock`) on `kaiak.lock` in the
  directory for its whole life; a second gateway on the same directory refuses to
  start, naming the lock file. The kernel drops the lock when the process ends, however
  it ends, so a crash leaves nothing to clean up; the file itself stays and means
  nothing without the lock. On platforms without `flock` (non-Unix) the lock is not
  taken — supported deployments are Unix. `flock` over some network filesystems is
  not enforced across hosts: give each replica its own volume.
- `KAIAK_LISTEN_ADDR` — the API listener (default `:8080`); `KAIAK_ADMIN_ADDR` — the
  admin listener (default `:9090`). Both bind once a config is in force: after the
  startup load (file mode) or the boot and the first totals (control-plane mode,
  which exits instead when it gets no config). An address that cannot be bound is a
  startup error.
- `KAIAK_METRICS_TOKEN` — optional: when set (non-empty), `/metrics` on the admin
  listener requires `Authorization: Bearer <token>` (Observability: admin port).
- `KAIAK_LOG_FORMAT` — `json` (default: one JSON object per line, for production) or
  `text` (development). Any other value is a startup error.
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
  memory; Usage spool → Sustained write failure). File mode ignores it.
- `KAIAK_MAX_CONNECTIONS` (default `0`: no cap; whole number, 0 or more) — the most
  connections the API listener keeps open (Lifecycle → Client timeouts).
- Starting with no config source is a startup error. A config file that fails to
  load at startup exits the process with the rejection logged; so does a
  control-plane boot that ends with no config (Control-plane mode → Boot).
- **One apply path** (settled 2026-09-24): every config, whatever its source (file,
  control plane, last-known-good copy), is validated completely, then swapped in
  atomically, then logged and counted by the same code; invalid config is rejected and
  reported, the running config stays.
- **File-mode reload on SIGHUP**: the gateway re-reads `KAIAK_CONFIG_FILE`. No file
  watcher — watching needs per-OS syscalls or a dependency, and the operator's tooling can
  send the signal (e.g. on a Kubernetes ConfigMap update). Every load logs its trigger
  (`startup`, `sighup`; in control-plane mode `control`, `last-known-good` and `seed`) and
  result: `config applied`, or `config rejected` with the issue codes and
  `running_config=kept` when an older config stays in force. Control-plane loads also
  log `config_version`. SIGHUP in control-plane mode is logged and ignored.
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
  outside — the config file, the seed, control-plane snapshots and `config` stream
  events (and so the last-known-good copy's config), totals, usage acks — is refused
  when an object in it names a member twice, at any depth, before either the schema
  walker or the typed decode runs (`duplicate-member`; for a message it is the
  message's decode error, `CONTROL-PROTOCOL.md` → Messages). Go's two decoders read a
  repeat differently — the generic tree keeps the last, a typed decode into a map
  merges both — so a second `backends` member held a backend the schema never saw
  (one naming `KAIAK_CONTROL_TOKEN` as its credential). Data-directory files are
  refused the same way when read (a repeat means the file was edited). One shared
  detector, a token scan with the standard library's JSON decoder: its memory is
  the names of the objects open at the current position, bounded by the document.
- **Control-plane mode** (settled 2026-09-24). Only the `control` package talks to the
  control plane; nothing on the request path waits for it.
  - **Boot** (settled 2026-09-25, E2; the retries D7, settled 2026-09-25):
    `GET /v1/config`, asked again while the control plane is unavailable until
    `KAIAK_CONTROL_BOOT_WAIT_MS` (default 60 s) ends — a jittered delay before each
    retry, drawn uniformly up to a step of 250 ms doubling to at most 2 s — then the
    first source that gives a config:

    | Snapshot fetch | Data directory with a last-known-good copy | Otherwise, seed set | Otherwise |
    | --- | --- | --- | --- |
    | applied | — (serves the snapshot) | — | — |
    | the config rejected (semantic, credentials) — at once, no retry | last-known-good | exit | exit |
    | refused token (`401`) or another `4xx`; a snapshot message the rules refuse — at once, no retry | last-known-good | exit | exit |
    | unavailable through the whole wait: connection failed or timed out, body cut off, no or another `Kaiak-Protocol`, any `5xx` (`503 config-unavailable` included) | last-known-good | seed | exit |

    - **Retrying within the wait** (D7): a control plane restarting beside its
      gateways (a rollout, a node drain, a crash) is back within seconds; one
      attempt made every gateway that started meanwhile exit and crash-loop on its
      supervisor's growing backoff. The first failed attempt logs `config snapshot
      not fetched at startup: retrying within the boot wait` (warn; error for a
      protocol mismatch) with `attempt` and `boot_wait_ms`, later ones at debug; the
      last failure is logged as before. The retry step is shorter than the
      reconnect backoff's (nothing serves yet), and jittered so gateways starting
      together do not ask together. What waiting cannot fix — a refused token, a
      config or snapshot the gateway rejects — ends the boot at once. Rejected
      (2026-09-25, reversing E2's "one attempt"): serving the last-known-good or
      seed config at once and catching up — a stateless gateway has neither, and
      the seed serves only free models, so a control-plane restart took every
      priced model away from each gateway started meanwhile; the cost is a start
      delayed by up to the wait when the control plane is really down.
    - **Exit** is non-zero with one error line naming the cause — `no config:
      control plane unavailable and no seed config …`, `no config: … answered 401
      unauthorized …`, `no config: the control plane's config version N was rejected
      (codes …)` — at the end of the boot wait for an unavailable control plane, at
      once otherwise; the supervisor restarts the gateway with its backoff.
      Rejected: starting not ready and waiting (the 2026-09-24 behavior) — a pod
      that is live but never ready hides the cause behind a readiness probe, and a
      gateway with a seed would never use it for a control plane that answers
      without a config.
    - **The seed is the backup for an unavailable control plane**, never for an
      error the operator must fix: a refused token or a config the gateway rejects
      exits — serving free models while the control plane publishes a config that
      cannot run would hide the error. A missing or other `Kaiak-Protocol` counts as
      unavailable (a proxy answering for a control plane that is down, or a version
      skew that an upgrade ends), and so does any `5xx`: the control plane cannot
      give a config now (settled 2026-09-25).
    - The **last-known-good copy** keeps its place before the seed (with a data
      directory): it is a config the control plane sent, and it also covers a
      rejected snapshot, as before. A copy whose position is malformed (an epoch
      that is not 32 lowercase hex digits, a version outside 1 to 2^53 − 1) is
      discarded with a warn line (`last-known-good config discarded: malformed
      position`) — the stream would resume from it and be refused on every
      reconnect (settled 2026-09-25, N-P10).
    - The seed is applied with trigger `seed`, never saved as last-known-good (only
      configs the control plane sent are) and carries no control-plane version:
      status reports `ready` with no applied version (the follow-up audit's N-P11),
      totals wait for a control-plane config, and the client keeps fetching the
      snapshot in the background (reconnect backoff), whose config replaces the seed.
  - **Readiness waits for the first totals** (settled 2026-09-25, D8; the
    independent daily-operations review's finding 4): after a boot from the control
    plane or the last-known-good copy, the client starts following the control
    plane and the **listeners bind only once the first totals arrive** — they
    follow the config on the stream's connect (`CONTROL-PROTOCOL.md`, Config
    stream), so the wait is normally milliseconds — or once what is left of the
    boot wait runs out. Logged: `waiting for the first totals` (`wait_ms`), then
    `first totals received` (`waited_ms`) or `first totals not received within the
    boot wait: priced USD-limited requests are refused until they arrive` (warn).
    Totals for another config than the applied one end the wait too (the control
    plane has answered; matching ones may take until the operator fixes a config),
    but not the refusal below. A seed boot waits for nothing: it serves only free
    models. Why: the limiter started from nothing and counted every budget as
    unspent until totals came, so a fresh stateless gateway ready before them served
    a spent budget (the reviewer's binary reproduction: `200`, then `429` once the
    totals arrived) — and each new replica of a rollout did so again. Rejected:
    answering from the boot snapshot alone — the snapshot carries no totals, and
    adding them there is a protocol change for what the stream already delivers.
  - **No totals yet** (settled 2026-09-25, D8): until totals computed under the
    applied config have been applied — or restored from `totals.json` (Limits →
    Control-plane mode: Restart) — the hour and month spend is *unknown*, which is
    not "totals with no usage" (a counter the complete totals do not list has used
    nothing). While it is unknown, a priced request under a `usd_per_month` limit is
    refused `503 budget_unavailable` as in an outage (Limits → Outage refusal),
    before anything is reserved; everything else serves. **Token limits keep
    counting locally from zero**, hourly ones included — matching the outage rule,
    which refuses only USD-limited requests: an hour token limit is a rate guard
    whose overshoot is at most one hour's share, while a budget's is money spent
    that the month does not give back. A gateway that binds after its wait runs out
    refuses USD-limited priced requests from its first request until the totals
    arrive.
  - **Stream**: `GET /v1/stream?since=<version>&config_epoch=<epoch>`, where the
    version is the latest config taken from the control plane — applied, or rejected
    (so a rejected config is not replayed on every reconnect), or the last-known-good
    one after such a boot — and the epoch the one it counts in. A `config` event of
    that epoch at or below that version is ignored (logged at debug); a newer one, or
    one of another epoch whatever its version (`CONTROL-PROTOCOL.md`, Config versions →
    Config epoch), goes through the apply path. `resync` → the snapshot is fetched
    again and applied **whatever its version** (a restarted control plane counts from
    1), then the stream reopens after it. A `400 since-invalid` on opening the
    stream (the control plane refuses the position sent) is handled the same way:
    the snapshot is fetched and the stream reopens after its position (settled
    2026-09-25, N-P10) — retrying the same position would be refused forever.
    `totals` events are decoded and handed to the limits
    consumer — their totals only when their revision is newer than the last applied,
    what they show counted always (`CONTROL-PROTOCOL.md`, Messages → Totals);
    malformed events are logged and skipped. Heartbeats only prove the
    connection alive: a stream silent for 45 s (three missed heartbeats) is closed
    and reopened — a connection that died without a close never ends on its own.
    Opening the stream (connecting and receiving the answer's headers) is bounded
    by the same 45 s, then the reconnect backoff runs (settled 2026-09-25; the
    audit's H11): an endpoint or proxy that takes the request and never answers
    would otherwise hold the follower forever — no config update, no key
    revocation — while usage acks kept the gateway out of outage. Every
    control-plane answer must also start within 30 s (the HTTP client's
    response-header timeout), a backstop under each request's own bound (snapshot
    30 s, status 10 s, usage batch 30 s).
  - **Reconnect**: before every new attempt (snapshot or stream) after a failure or an
    ended stream, a delay drawn uniformly from 0 to an exponential step — 500 ms
    doubling per attempt, capped at 30 s (full jitter, so gateways that lost the
    control plane together do not return together). A stream that stayed open 30 s
    resets the step. A response with another `Kaiak-Protocol` (or none) is logged at
    error level on every attempt; the gateway keeps retrying on the same backoff and
    keeps serving what it has — an upgrade of either side ends it, and exiting would
    turn a version skew into an outage.
  - **Rejections**: a config the gateway rejects is never applied nor saved; it
    (`version`, `codes`) is kept for the status report until a later config from the
    control plane is applied (`CONTROL-PROTOCOL.md`, Messages → Status).
  - **Last-known-good** (with a data directory only; `last-known-good.json`, format
    version 3 — settled 2026-09-27, the config format 2 inside): the config
    document exactly as the control plane sent it, its version and config epoch
    (so the stream after a last-known-good boot resumes in that epoch, and a control
    plane on another store answers `resync`), written after each
    successful apply from the control plane (never a rejected one, never on a boot
    from the copy itself). At boot it is loaded through the apply path, so a copy that
    no longer passes (an API-key variable now unset) is rejected like any config.
  - The client follows the control plane until the drain is over (Lifecycle): requests
    admitted during the grace period run on the newest config and totals.
  - **Usage batches** (settled 2026-09-24; the protocol side in `CONTROL-PROTOCOL.md`,
    Usage batches): the client is a sink on accounting's fan-out; `Record` appends
    the record to the filling batch in memory and never waits. The batch is sealed
    every 5 s, or at once when it reaches 500 records; sealed batches go to the batch
    store — the spool with a data directory, memory without — each under the next
    sequence of the epoch, and queue behind the one outstanding. A separate goroutine
    sends the queue's head and nothing else until it is acknowledged or set aside;
    the store is written by another, so neither the network nor the disk ever waits
    on the other. One sealer, queue and sender serve both stores: only where a
    queued batch is kept differs. Each ack's totals go to the totals consumer — the
    same one the stream's `totals` events feed, called one at a time, under the same
    revision rule — with the acknowledged batch counted.
  - **Usage batches in memory** (no data directory; settled 2026-09-25, E1): queued
    batches are held in memory until acknowledged, under an epoch new with every
    process. They are bounded like the spool's unwritable case: past
    **`KAIAK_USAGE_MEMORY_BYTES`** held (default 64 MiB, queued and sealed records
    by their encoded size), the oldest queued batches are dropped —
    except the outstanding one, which may be on the wire and whose ack must still
    find it — logged at error level and counted in
    `kaiak_usage_dropped_records_total{reason="memory_bound"}`. A record the checks
    refuse is logged and dropped (nothing to keep it in), a batch the control plane
    refuses likewise. What a pod loses when it ends: the records not yet
    acknowledged if it is killed, only what the drain's flush could not deliver if
    it drains (Lifecycle), and whatever passed the bound during a long outage.
    Rejected: a scratch directory for the spool (a read-only root filesystem, and
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
  - **Usage spool** (with a data directory; settled 2026-09-24; format version 2
    because records carry `groups`, settled 2026-09-27): one file per sealed batch,
    `usage-batch-<epoch>-<sequence>.json` (the batch message exactly as sent), and an
    index, `usage-spool.json` (instance, epoch, next sequence). The files are the
    queue: memory holds only their IDs and record counts, and the batch being sent,
    so a long outage costs disk, not memory, and each seal writes one batch — never
    the whole queue.
    - **Write policy**: a seal writes the batch file, then the index past it; only
      then is the batch queued, so no batch is ever sent before it and its sequence
      are durable, and a restart never reuses a sequence for other records. An ack
      deletes the batch's file. A failed write leaves the batch sealed in memory,
      unchanged, and is retried at the next seal tick (logged at error level); usage
      waits rather than risking a sequence bound to two record sets. The filling
      batch is not written: it is sealed every 5 s and at the drain, so a separate
      copy would buy nothing.
    - **Sustained write failure** (settled 2026-09-25, L16): sealed batches waiting
      for the spool are kept in memory up to **`KAIAK_USAGE_MEMORY_BYTES`** (the
      same bound and setting as without a data directory: one knob for the usage
      memory a pod may hold); past that the oldest sealed batches are dropped,
      logged at error level and counted in
      `kaiak_usage_dropped_records_total{reason="spool_unwritable"}`.
      A disk that stays full or read-only would otherwise grow the process until it
      is killed, losing every record in memory anyway. Rejected: refusing new
      requests while usage cannot be spooled (serving never depends on the control
      plane or its bookkeeping), and an unbounded buffer.
    - **Record checks at seal time** (settled 2026-09-25, H5): before a batch is
      written, each record is checked against the usage record's schema and rules,
      as the control plane will check it. A record that fails would make the control
      plane refuse the whole batch — up to 500 records set aside — so it is set
      aside alone: written to `usage-rejected-record-<record ID>.json` (kept with the
      refused batches, the newest 10), logged at error level with its request ID, and
      counted in `kaiak_usage_dropped_records_total{reason="invalid"}`; the rest of the
      batch goes on (a batch left empty is dropped, taking no sequence). Settlement
      already clamps every unit and the cost to 2^53 − 1 (Accounting), so this
      guards against what clamping does not cover.
    - **What a crash can lose**: the records settled since the last seal — at most
      5 s of usage, or fewer than 500 records. Everything sealed is resent after the
      restart with its ID; a batch the control plane counted before the crash is
      acknowledged again without counting.
    - **Epoch**: 32 random hex digits, new whenever a fresh spool starts — no index,
      an index of another format version (discarded, logged), an unreadable one, or
      another instance's. The sequence continues across restarts otherwise.
      Batches already queued are delivered whatever the index said, each under its
      own ID (another instance's under that instance): other epochs first, one epoch
      at a time — in epoch-ID order, which is random, not by age (known limit:
      `docs/BACKLOG.md`, Old-epoch spool order) — then the current epoch's.
    - **Queue bound**: none — usage is billing data. A warning is logged each time
      the queue passes another 1000 batches (about 300 MB of full batches), and the
      depth is a metric; disk space is the operator's to watch. (In memory, with no
      data directory, the queue is bounded: Usage batches in memory.)
    - **Refused batches** are moved to `usage-rejected-<epoch>-<sequence>.json` for
      inspection; the newest 10 are kept, older ones deleted as new ones arrive. A
      queued file that cannot be read is set aside the same way.
    - The spool is the one data file whose loss drops data: flush it (graceful
      shutdown) before an upgrade that changes its format version.
  - **Status reports** (`CONTROL-PROTOCOL.md`, Gateway status): when the client starts,
    when a config stream connects, when the state, the applied config version or the
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
    per request. State: `ready` while a config is in force — from the control plane,
    the last-known-good copy or the seed (settled 2026-09-25, N-P11: derived from the
    config in force, not from a control-plane version, so a seed boot reports ready
    with no applied version) — `draining` from the drain's start; `starting` (no
    config in force) is never reported by the binary, which reports only after a
    boot that found a config. The applied config is reported as its version and
    epoch (`applied_config_version`, `applied_config_epoch`; both null with the seed —
    settled 2026-09-25, N-P1), since a version compares only within its epoch.
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
  | `kaiak_limit_rejections_total` | counter | `scope_kind`, `type` | Requests a limit refused (`rate_limit_exceeded`, `budget_exceeded`) by the kind of scope the limit belongs to (`global`, `group`; settled 2026-09-27) and its type (`requests_per_minute`, `tokens_per_minute`, `tokens_per_hour`, `usd_per_month`); `budget_unavailable` is not counted here — no caller's limit was hit (settled 2026-09-25, D6). No group ID: the log line names it |
  | `kaiak_backend_in_flight_requests` | gauge | `backend` | Requests routed and not yet over; every configured backend present, 0 when idle |
  | `kaiak_backend_max_in_flight` | gauge | `backend` | The backend cap this gateway enforces: its share of `max_in_flight` among the live gateways (Caps across gateways); backends without a cap are absent |
  | `kaiak_queued_requests` | gauge | `model` | Requests waiting in the model's queue; every configured model present, 0 when empty |
  | `kaiak_queue_wait_seconds` | histogram | `model` | Time queued requests waited before getting a slot (refused ones are counted below instead) |
  | `kaiak_queue_rejections_total` | counter | `model`, `reason` | Requests the model's queue refused: `full` (`queue_full`), `timeout` (`queue_timeout`) |
  | `kaiak_retries_total` | counter | `model`, `backend`, `reason` | Retries sent — attempts after an earlier attempt of the same request failed, counted as each is sent — by that attempt's backend and failure: `unavailable`, `timeout`, `server_error`, `rate_limited`, `auth_failed`, `model_missing` |
  | `kaiak_upstream_attempts_total` | counter | `backend`, `deployment_model`, `outcome` | Every upstream attempt (first attempts and retries) by its outcome — the circuit breaker's classification, named (below) |
  | `kaiak_upstream_attempt_duration_seconds` | histogram | `backend` | Every upstream attempt from its send to its end: a relayed response to the end of its relay (stream or not), a retried or failed attempt to its failure (a held backend error: its status and first event) |
  | `kaiak_request_attempts` | histogram | `model` | Attempts per routed request, the first included (buckets 1–10) |
  | `kaiak_circuit_open` | gauge | `backend`, `deployment_model` | 1 while the deployment's circuit is open (out of rotation, waiting for a probe), else 0 — half-open reads 0; exactly one sample per configured deployment, however many public models share it (`deployment_model` is the model name on the backend, as on the log line — `model` everywhere else is the public name) |
  | `kaiak_circuit_half_open` | gauge | `backend`, `deployment_model` | 1 while the deployment's circuit is half-open (a probe succeeded; the next request is its trial), else 0; one sample per configured deployment, as `kaiak_circuit_open` |
  | `kaiak_deployment_cooling_down` | gauge | `backend`, `deployment_model` | 1 while the deployment cools down after a `429` (Routing and reliability: 429 cooldown), else 0; one sample per configured deployment, as `kaiak_circuit_open` |
  | `kaiak_circuit_transitions_total` | counter | `backend`, `deployment_model`, `to` | Circuit transitions per deployment: `to` is `open`, `half_open` or `closed` |
  | `kaiak_probes_total` | counter | `backend`, `result` | Probes of backends with open circuits: `success`, `failure` |
  | `kaiak_config_loads_total` | counter | `trigger`, `result` | Config loads: `startup`/`sighup`/`control`/`seed`/`last-known-good`, `applied`/`rejected` |
  | `kaiak_config_last_applied_timestamp_seconds` | gauge | — | Unix time the running config was applied |
  | `kaiak_connections_refused_total` | counter | — | API connections closed at accept because `KAIAK_MAX_CONNECTIONS` were open (0 with no cap) |
  | `kaiak_build_info` | gauge | `version`, `go_version` | Always 1; `version` is the release the binary was built as — the image build links its `git describe` version in (`docs/TECH-STACK.md`, Container images), else the module version Go stamped, else `(devel)` |
  | `kaiak_usage_batch_sends_total` | counter | `result` | Control-plane mode: usage batch sends — `acked`, `rejected` (set aside), `failed` (retried) |
  | `kaiak_usage_spool_batches` | gauge | — | Control-plane mode: sealed usage batches not yet acknowledged (in the spool or in memory) |
  | `kaiak_usage_spool_records` | gauge | — | Control-plane mode: records in those batches |
  | `kaiak_usage_queued_bytes` | gauge | — | Control-plane mode: encoded bytes of the unacknowledged records held in memory, what `KAIAK_USAGE_MEMORY_BYTES` bounds — every queued record with no data directory; with one, the sealed records the spool could not write |
  | `kaiak_usage_dropped_records_total` | counter | `reason` | Control-plane mode: usage records dropped before reaching the control plane — `invalid` (failed the record checks, set aside alone), `spool_unwritable` (over the in-memory bound while the spool cannot be written), `memory_bound` (over the in-memory bound with no data directory) |
  | `kaiak_usage_last_ack_timestamp_seconds` | gauge | — | Control-plane mode: Unix time of the last acknowledged batch; absent before one |
  | `kaiak_control_connected` | gauge | — | Control-plane mode: 1 while a config stream is open, else 0 |
  | `kaiak_control_last_contact_timestamp_seconds` | gauge | — | Control-plane mode: Unix time of the last contact (stream bytes, a snapshot, an ack); the process start before any |
  | `kaiak_control_totals_applied_timestamp_seconds` | gauge | — | Control-plane mode: Unix time totals were last applied (a newer totals event or ack computed under the applied config); absent before any. It stops moving while the totals are for another config (Limits → Control-plane mode) |
  | `kaiak_control_outage` | gauge | — | Control-plane mode: 1 while in outage past the grace (priced money-limited models refused) — the stream down, or usage batches unanswered — else 0 |
  | `kaiak_control_config_mismatch` | gauge | — | Control-plane mode: 1 while the newest totals are for another config than the applied one (typically one this gateway rejected), else 0 — from the start, before the grace; past the grace priced money-limited models are refused `budget_unavailable` (Limits → Control-plane mode: totals follow their config) |
  | `kaiak_usage_records_total` | counter | usage labels | Usage records settled: one per routed request, plus one per retried attempt sent in full and unanswered. Records, not requests — count client requests with `kaiak_request_duration_seconds_count` |
  | `kaiak_usage_clamped_records_total` | counter | — | Usage records whose units or cost passed 2^53 − 1 and were clamped to it (Accounting) |
  | `kaiak_usage_tokens_total` | counter | usage labels, `unit` | Tokens per usage unit (all four token units, zeros included) |
  | `kaiak_usage_cost_usd_total` | counter | usage labels | Estimated cost in USD (the records' nano-dollars ÷ 10⁹, summed exactly as integers) |

  - **Alert on usage delivery, not only the stream** (settled 2026-09-25, M16): a
    control plane can keep the config stream open while it no longer takes usage.
    Alert when `kaiak_usage_spool_batches > 0` and `time() -
    kaiak_usage_last_ack_timestamp_seconds` stays above the outage grace (before any
    ack the metric is absent: alert on a spool that stays non-empty), on
    `kaiak_control_outage == 1`, and on `kaiak_control_config_mismatch == 1` held
    for a few minutes (a totals push for a config the stream has not delivered yet
    is a brief mismatch).
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
    timeout), `auth_failed`, `model_missing`, `server_error` (a backend `5xx`),
    `broke_off` (broken off, stalled or incomplete after the first event); neutral
    — `response_timeout` (a non-stream response timeout, before or after the first
    bytes; before them it is a failure for a half-open trial and from the 3rd in a
    row — Routing and reliability: outcome classes), `rate_limited` (a backend `429`), `client_error` (another backend `4xx`),
    `canceled` (the client left, or the drain cut, before the first event),
    `internal` (a gateway fault building the upstream request). Label values are
    config names and the fixed outcomes only, never client input; every configured
    deployment has all twelve series from the start, at 0 (next bullet).
  - **Series at 0** (settled 2026-09-25, E5; the audit's N-O4): every counter and
    histogram whose label values are fixed or come from the config exists at 0
    from startup, and a config apply creates those of its new models, deployments
    and backends — so `increase(...) > 0` sees the first rejection, opening or
    burst instead of a series born at 1. At startup: `kaiak_errors_total` per class,
    `kaiak_limit_rejections_total` per scope kind × type,
    `kaiak_config_loads_total` per trigger × result (all five triggers, whichever
    mode), the usage-delivery counters per result and drop reason,
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
  - `endpoint`: `chat_completions`, `completions`, `embeddings`, `list_models`,
    `get_model`, `model_props`. `status_class`: `2xx`, `4xx` (499 included), `5xx`.
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
    `upstream_timeout`, `upstream_error` (`upstream_auth_failed`, `upstream_model_missing`, a backend
    `5xx` — answered `upstream_error` —, a response that broke off upstream), `upstream_rate_limited` (a relayed
    backend `429`), `upstream_client_error` (a relayed backend `4xx` other than
    `429`), `client_closed`, `shutting_down` (`server_shutting_down`, a response the
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
    `label sets × models × statuses × 6`, where 6 = records 1 + cost 1 + tokens 4
    (one per unit), statuses ≤ 2 (`complete`, `partial`), models = the models each
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
    top-level team groups; 3 models per key: both labels on, 500 × 3 × 2 × 6 =
    18,000 series; `key_id_label` off, 500 groups — the same 18,000 here, since every
    key has its own group; `group_label` off too, 6 top-level groups (5 teams +
    `users`), 6 × 3 × 2 × 6 = 216. Multiply by the gateway replicas for the metrics
    store's total, and switch `key_id_label` off first, then `group_label`, when the
    total outgrows what the store is sized for. Series stay until restart once written
    (below). Ops series: request duration `endpoint` × `model` × `status_class` ×
    18 lines (15 buckets + `+Inf`, sum, count); first-token and decode-rate
    histograms model × backend × 15 and 13 lines; queue wait model × 17 lines;
    attempts model × 13 lines; retries model × backend × 6 reasons; limit
    rejections 8 (2 scope kinds × 4 types); upstream attempts
    deployments × ≤ 12 outcomes; attempt duration backends × 18 lines — all bounded
    by the config, never by clients or keys.
- **Two paths, not derived** (principle 7; settled 2026-09-24): usage metrics come
  from a sink on accounting's fan-out, fed each settled record as it is produced —
  the same settlement the usage record carries, so dashboards and billing never count
  tokens two ways, but each path keeps its own state: metrics are in-memory counters
  that reset on restart and are never read back into a record; records never come
  from metrics. Ops metrics are observed by the request pipeline itself; the decode
  rate reads the settled `tokens_out` only as an input.
- **Key-ID and group switches** (key ID settled 2026-09-24; group 2026-09-27):
  `global.metrics.key_id_label` and `global.metrics.group_label` (default on) are
  read from the live config at each record. Off, new series carry no `key_id` (no
  `key_group`; `root_group` stays — it keeps a per-branch view, bounded by the
  top-level groups: Cardinality); series already written with one stay until
  restart (counters never go back, and dropping them would make sums fall).
  Switching one off stops growth at once; a restart clears the old series. The
  usage record always keeps its key ID and group path.
- **Logs**: one structured line per request (message `request`): request ID, method,
  path, status, latency, key ID and model once known, the error code on failure, and
  for a `401` the auth failure reason (`missing_key`, `malformed_key`, `unknown_key`,
  `disabled_key`, `expired_key`). Never the key, a header value, or prompt or response
  content. Strings the client controls — `method`, `path`, `model` (a refused name
  is any string) — are clipped to their first 256 bytes, then `…`, cut on a
  character boundary (settled 2026-09-25, L3): a 1 MiB path must not make a 1 MiB
  log line. One helper (`internal/clip`) serves the log line and error messages.
  Once authenticated, the key's group (settled 2026-09-27): `group` — its config ID,
  the usage metrics' `key_group`; never its `labels`. Once the model passed its
  access check (body endpoints): `stream`. A limit refusal (`rate_limit_exceeded`,
  `budget_exceeded`, `budget_unavailable`; settled 2026-09-25, D6 — the line held
  only the code, and the operator could not tell which of the scopes' limits
  refused): `limit_scope` (`global` or `group`; settled 2026-09-27), `limit_id` (the
  group's ID, `global` for a global limit — never a key), `limit_type`,
  `limit` (the value this gateway enforces — a per-minute limit's share among the
  live gateways), `limit_configured` (the config's value), `used` (what the
  window held; absent for `budget_unavailable`, whose spend is unknown) and, for a
  token limit, `requested` (the request's reservation: `requested` above
  `limit_configured` is a request too large for the limit, not a full window) —
  counts in the limit's unit, USD limits in dollars.
  Once routed: `backend` and `deployment_model`; on an upstream failure,
  `upstream_error` (may name the backend address, never a credential); for a
  backend error status — a `5xx` answered `upstream_error`, a `4xx` relayed —
  `upstream_error_code` and `upstream_error_type` when its body names them — never
  its message (Providers: backend error bodies); a relayed backend error status
  has no gateway error code, so `error_code` is its class
  (`upstream_client_error`, `upstream_rate_limited`; settled 2026-09-25, D6);
  `ttft_ms` for a stream that carried generated content — the time to first
  token as the metric measures it, from the answering attempt's send; for a
  response that stopped early, `relay_end` (`client_closed` — the client left or
  stopped reading, `upstream_failed`, `upstream_stalled`, `upstream_incomplete`,
  `upstream_timeout` — Providers: upstream failures, `shutdown` — cut off by the
  drain); `queue_wait_ms` once the request entered its
  model's queue (every attempt's wait, summed). Across attempts (settled
  2026-09-24): `backend`, `deployment_model` and the upstream fields are the last
  attempt's; `attempts` (routed requests); `tried` when there was more than one —
  every attempt in order as `backend/deployment_model:outcome`, the outcome the
  backend's status or the gateway's error code
  (`down/m:upstream_unavailable,local/m:200`); `retry_refused` when a retry got no
  slot (`queue_full`, `queue_timeout`, `no_deployment_left`) or the model's retry
  budget was spent (`retry_budget`).
  Once settled (routed requests): `tokens_in`, `tokens_cached`, `tokens_out`,
  `tokens_reasoning`, `cost_usd` (in dollars, for reading) — summed over the
  request's records — and the last record's `estimated`, `partial`. The line is written after settlement, as the request's
  last act.

## Lifecycle

- **Readiness** only once a config is loaded (file, control plane, last-known-good, or
  seed) and, in control-plane mode, the first totals have arrived or the boot wait
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
    is taken as gone — `relay_end=client_closed`, the upstream request cancelled,
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
  before the listeners bind — control-plane mode, while snapshot fetches retry or
  while waiting for the first totals — ends the boot at once: nothing was served
  and no usage exists, so there is nothing to drain; the process logs `kaiak
  stopped` (`reason="signal terminated during boot"`) and exits 0. Rejected: acting
  on it only once the listeners bind — a pod deleted during a control-plane outage
  would sit out the whole boot wait (60 s by default) past its termination grace.
- **Draining** on SIGTERM or SIGINT (settled 2026-09-24; SIGINT drains the same way:
  Ctrl-C and `docker stop` users expect a clean stop):
  1. `/readyz` fails with `draining`; status `draining` is sent to the control plane
     (control-plane mode). From here every API response carries `Connection: close`, so keep-alive
     clients reconnect — to another instance once this one is out of rotation;
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
     `relay_end=shutdown` and a partial record, a request whose response had not
     started answers `503 server_shutting_down` (into a closed connection — it is
     for the log and the record). Requests waiting in a model's queue are in flight
     too: they keep waiting and are served when slots free before the timeout; at
     the cut they leave the queue with the same `503` (no record: never routed).
     Limits settle from those records as usual;
  5. in control-plane mode, the **usage flush** (settled 2026-09-24): the filling batch
     is sealed and written, and the queued batches are sent until all are
     acknowledged or set aside — within what remains of grace + drain timeout, counted
     from the drain's start (at least the reserve) — then a final `draining` status
     goes out (bounded to 2 s). What is not delivered stays in the spool for the next
     start, or, with no data directory, is lost with the process (logged `usage not
     flushed: lost at exit`, with the batches and records). Then the
     control-plane client stops, the file-mode usage snapshot (or the totals cache)
     is written when there is a data directory, the admin
     listener stops, the process exits 0. The admin listener serves throughout:
     `/healthz` stays 200, `/metrics` answers.
  - A **second** SIGTERM/SIGINT during the drain skips whatever waiting remains:
    in-flight requests are cut off at once, then step 5 runs (the usage flush seals
    the last batch without waiting for acks — kept in the spool with a data
    directory, lost without — and the final status is skipped) and the
    process exits 0 — like a timed-out drain, the shutdown completed; the cut requests are in the
    log and in their partial records.
  - A listener that fails at runtime starts the same drain, and the process exits 1
    with the error.
  - Kubernetes: `terminationGracePeriodSeconds` must exceed grace + drain timeout +
    about 10 s — after the drain, the final status report (≤ 2 s), the admin
    listener's shutdown (≤ 5 s with a scrape open) and, with a data directory, the
    snapshot and totals writes (default 5 + 60 + 10 s → at least 75 s; the Kubernetes default of 30 s
    would SIGKILL long streams mid-drain).
  - No draining metric: `/readyz` already says it, and a scrape during a
    seconds-long drain adds little.
