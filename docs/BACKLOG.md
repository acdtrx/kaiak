# Backlog

Deferred bugs and ideas (see `AGENTS.md`, Scope). Entries that grow graduate to
`docs/plans/<topic>/`. A resolved entry is removed in the same change — git history is
the archive; this file lists only what is still open.

Entry format: a bolded title, what it is in a sentence or two, and the **revisit
trigger** — the observed condition that would make it worth building (features earn
their place). Date rulings inline: `(ruled YYYY-MM-DD: wait to see if needed.)`.
Group entries under headings as themes emerge.

## Providers and APIs

- **Bedrock provider** — translating provider (Converse API), binary stream framing,
  hand-written SigV4. Its usage report's `cacheWriteInputTokens` maps to
  `tokens_cache_write`; Bedrock prices writes by cache lifetime, which the one unit
  does not split (`docs/specs/CONTROL-PROTOCOL.md`, Config → Units and price units).
  Revisit trigger: a Bedrock-only model is needed by a real user.
  (ruled 2026-09-24: deferred from v1.)
- **Anthropic Messages inbound** — second inbound format (`/v1/messages`, `x-api-key`
  auth). Revisit trigger: a client that only speaks the Anthropic API needs access.
- **OpenAI Responses API inbound** — used by newer clients (e.g. Codex CLI, Agents SDK).
  Needs sticky routing: `previous_response_id` state lives on one backend, so follow-ups
  must return to it. Revisit trigger: a client in use requires `/v1/responses`.
- **Image and audio models** — OpenAI-shaped `/v1/images/generations`,
  `/v1/audio/speech`, `/v1/audio/transcriptions`; new usage units (`images`,
  `audio_seconds`, `characters`). Job-queue APIs (ComfyUI-style) need a separate adapter
  or gateway. Revisit trigger: a self-hosted image or speech model is deployed.
- **Reasoning-effort discovery** — read the supported `reasoning_efforts` from backends
  instead of declaring them. No backend reports the list today: llama-server's `/props`
  has only a yes/no (`chat_template_caps.supports_reasoning_effort`), vLLM and cloud
  APIs nothing. Efforts stay declared and remain the control plane's responsibility;
  `verifyBackend` (`docs/specs/BACKEND-VERIFY.md`) reports only that yes/no, as a hint.
  A heuristic over the chat template (`reasoning_effort`, `enable_thinking`, `<think>`)
  could guess more. Revisit trigger: a backend starts reporting its supported effort
  values, or operators keep getting efforts wrong at add time. (ruled 2026-09-29.)
- **Numeric reasoning efforts** — some newer models take a number (a thinking budget
  or level) instead of `low`/`medium`/`high`. A per-model mapping from effort names to
  the backend's numbers (or a numeric pass-through) would let clients keep sending the
  OpenAI names. Revisit trigger: a deployed model needs a numeric effort that clients
  cannot send as it is. (ruled 2026-09-29.)
- **llama-server specifics** — concurrency cap from its slot count (`/props`
  `total_slots`; `verifyBackend` already reads `/props`, not that field), in the
  `llama-server` module. Revisit trigger: the smaller llama-server deployment goes
  ahead.
- **More from llama-server in `verifyBackend`** — router mode (one server, several
  models; `/props?model=<name>` describes each — today `/props` is read only when the
  list has one model) and the server's default sampling parameters
  (`default_generation_settings.params`) as candidate model `defaults`. Revisit
  trigger: a router-mode llama-server is deployed, or operators copy sampling
  defaults by hand. (ruled 2026-09-29: out of the first `verifyBackend`.)
- **Backend credentials in config** — the key value in config instead of an
  `api_key_env` naming a gateway environment variable. Gains: one place to manage
  keys, rotation by config push instead of a gateway rollout, gateways need only the
  control URL and token. Costs: every copy of the config becomes a secret —
  control-plane config versions (old keys after rotation), the gateway's
  last-known-good cache and seed file, the control stream (TLS mandatory), and every
  place config is shown, diffed, echoed in errors or used as a fixture needs masking;
  it reverses the settled "secrets never in config" (`docs/DEPLOYMENT.md` → Secrets
  and trust). Revisit trigger: the user decides (under consideration 2026-09-29), or
  keeping gateway Secrets in step with config becomes a real burden.
- **Active capability probes in `verifyBackend`** — opt-in requests of about one
  token to observe what no backend reports passively. Checked live on vLLM 0.30.0
  (2026-09-29, Qwen3.8-27B-NVFP4 started with vision on, then off): `/v1/models`,
  `/tokenize`, `/version` and `/metrics` are identical either way; a chat with a 1×1
  image answers `200` with vision and `400 "At most 0 image(s) may be provided in one
  prompt"` without; a reasoning parser shows as a `reasoning` field on the reply;
  `tool_choice: "auto"` should be refused without `--enable-auto-tool-choice`
  (untested). Works on any OpenAI-compatible server; results would be labelled
  observed, not reported. Revisit trigger: operators keep declaring vLLM
  capabilities wrong. (ruled 2026-09-29: the helper stays passive.)
- **Metadata drift warning in the gateway** — the gateway's background model check
  already fetches each backend's models list on config apply; comparing vLLM's
  `max_model_len` in that answer with the declared `context_length` would warn about a
  backend restarted with other flags at no extra request (llama-server would need
  `/props`, an extra request). Revisit trigger: a stale declared context length causes
  failed requests in practice. (ruled 2026-09-29: metadata is filled at add time by
  `verifyBackend`; the gateway does no extra backend work.)
- **Cloud workload identity** — AWS IRSA / Pod Identity, Azure managed identity instead
  of static keys. Revisit trigger: static credentials are not allowed in the target
  cluster.
- **llama-server quirks** — behaviours of llama-server that break what the gateway
  assumes of a backend, for the `llama-server` backend module (2026-09-30 review,
  L1–L3; all reproduced live):
  - *Multi-sequence usage* (L1): for a `/v1/completions` `prompt` list (several
    prompts in one request, each answered separately) llama-server's `usage` counts
    one prompt, and for `n` > 1 (several alternative answers) one choice; the gateway
    settles limits, budgets and records from that report — up to 1/16 or 1/`max_n`
    of what ran. Fix: refuse prompt lists and `n` > 1 on llama-server, or floor the
    usage at the gateway's own figures and flag the record `estimated`.
  - *Own parameter names* (L2): llama-server reads `n_predict` before
    `max_completion_tokens`/`max_tokens` and `n_cmpl` before `n` (and, on
    `/v1/completions`, `max_completion_tokens` before `max_tokens`), so a client
    passes the output ceiling, the `-1` refusal, `max_n` and the reservation. Fix: own
    those names (check, lower, reserve) or refuse them.
  - *Client errors answered 500* (L3): llama-server answers `500 server_error` to
    every internal exception except `invalid_argument` (`tools/server/server.cpp`),
    including a `response_format` schema its grammar converter cannot handle; five
    such requests open the circuit for every key, and one per probe interval keeps it
    open, billed $0. Fix: treat a llama-server `500` as the client's error (relayed,
    not retried, not a circuit failure); a dead server still opens the circuit
    through refused connections, timeouts and 502/503/504.
  - *A `base_url` without `/v1` half works* (found 2026-10-01, run on build 11146):
    llama-server serves `/chat/completions` and `/models` without the prefix as
    with it, but `/completions` and `/embeddings` there are its own formats (`200`,
    `{"content": …}` and `[{"embedding": [[…]]}]`, no OpenAI `usage`), so chat works,
    the models list and the config-apply check pass, and completions and embeddings
    reach clients in a shape they cannot read, with usage estimated. No `404`, so the
    wrong-path rule (`GATEWAY.md`, Wrong path to a host) cannot see it. Fix: the
    module refuses (config issue) or warns about a `base_url` not ending in `/v1`.
  Until then: `global.max_n: 1` and `max_sequences_per_request: 1` refuse prompt lists
  and `n` > 1 on every backend (not `n_cmpl`). Revisit trigger: llama-server serves a
  priced or limited model to clients that are not fully trusted, or a circuit opens
  on a llama-server model without the server being down, or a llama-server
  completions or embeddings client gets an answer it cannot read. (ruled 2026-09-30: deferred;
  the `llama-server` type comes first.)

## Pipeline stages

- **Response caching.** Revisit trigger: repeated identical requests show up in usage
  data at a volume worth serving from cache.
- **Guardrails** (input/output checks). Revisit trigger: a concrete policy requirement.
- **Prompt templating.** Revisit trigger: a concrete use case.
- **Prompt/response logging to an external system** — off by default, opt-in per key or
  group. Revisit trigger: a destination system is chosen.

## Routing

- **Completion probe for open circuits** — optionally probe an open circuit with a tiny
  real completion (e.g. 1 output token) instead of `GET …/models`, configured per
  backend or model (e.g. only on vLLM), so a server that is up but cannot serve the
  model stays open. Revisit trigger: a circuit closes on a successful models probe
  while the model itself still fails. (ruled 2026-09-24: deferred from P3.)

- **Always-on active health checks** — probe healthy deployments too, not only open
  circuits. Revisit trigger: failures surface on real traffic that a periodic probe
  would have caught first. (ruled 2026-09-24: deferred from v1.)

## Limits

- **Live count excludes draining gateways** — a draining gateway stays in
  `live_gateways` until it goes silent, so every rollout temporarily halves backend-cap
  and per-minute shares (queueing and 429s while hosts have room). Fix: the control plane
  leaves draining gateways out of the count (a protocol decision on both halves), or a
  final "stopped" status. Interim: `minReadySeconds`. Revisit trigger: rollout-time
  `queue_full`/`queue_timeout` or per-minute refusals seen in practice. (ruled
  2026-09-25: backlog.)
- **Fair dispatch across keys and groups** — the proper replacement for per-key
  concurrency caps: when a slot frees, dispatch rotates across keys or groups instead
  of strictly oldest-first, so one busy key or group cannot starve others on a
  saturated model. Revisit trigger: one key or group seen crowding others into
  `queue_full`/`queue_timeout`.
- **Cut-across memberships** — let a group also count toward a second parent besides
  its own (e.g. env `rag-prod` under project `rag` and under a team-wide `prod`
  budget), so its usage and limits reach two branches of the tree. Today a group has
  exactly one parent, and a limit spanning another axis of the tree can only be
  written at a common ancestor. Revisit trigger: a team needs a hard cap on a project
  across its environments *and* team-wide environment budgets at the same time.
  (ruled 2026-09-27: tree only.)

- **Per-key concurrency split across gateways** — the per-key concurrent-request limit
  (`global.max_concurrent_requests_per_key`, default 16) is per gateway, so N
  replicas allow up to N × the limit; split it by live gateways like `max_in_flight`
  (D3). Revisit trigger: a key reaches
  N × its limit across replicas in practice, or keys are sized expecting a fleet-wide
  bound. (ruled 2026-09-25: per gateway first.)
- **Temporary key suspension on abusive patterns** — suspend a key for a while when it
  shows behaviour legitimate clients rarely do: repeatedly holding requests open
  without sending bodies, declaring large bodies it never sends, hitting the
  per-key concurrency limit continuously, or tripping body-read timeouts. The
  suspension answers `429`/`403` with a clear code, is logged and metered, expires on
  its own, and is reported to the control plane so it can be lifted or made
  permanent. Revisit trigger: the per-key limit and the body-read deadline prove
  insufficient against a misbehaving client in practice. (ruled 2026-09-25.)

- **Demand-weighted shares** — split per-minute limits **and backend `max_in_flight`
  caps** by each gateway's observed demand instead of evenly: gateways already report
  in-flight per backend and queued per model; the control plane weights each host's
  cap and each per-minute limit by recent demand (minimum 1 per gateway) and sends each
  gateway its own shares in its own totals message. Fixes sustained skew in seconds,
  not millisecond bursts (the queue absorbs those). Revisit trigger: a scope hits its
  per-minute limit well below the configured value, or a gateway answers
  `queue_full`/`queue_timeout` while the fleet-wide in-flight on that model's backends
  is below their caps.
- **Interim usage records for long streams** — periodic estimated records during a
  stream, superseded by the final one (cumulative per request; the control plane counts
  the latest). Revisit trigger: budget overshoot from long streams shows up in practice.
  (ruled 2026-09-24: deferred from v1.)
- **Fleet-wide per-minute token limits** — count per-minute token windows in the
  control plane, as hour windows are, so a gateway enforces `pushed totals + its own
  usage not yet counted` instead of `floor(limit ÷ live gateways)`: uneven traffic
  across gateways stops costing a scope part of its limit (seen 2026-10-05: a 2M
  limit refused at 1M on one of two gateways). Reuses the usage batches (sealed every
  5 s) and the totals push (at most once a second); about 6 s behind. Drawbacks: the
  limit becomes soft — at a window's start every gateway sees it empty, so cap each
  gateway's not-yet-counted usage at its share; requests in flight are invisible to
  other gateways, and a long generation counts all its tokens in the minute it ends
  (a slow backend dumps minutes of output into one window — interim records, above,
  are the companion fix); outage fallback to shares, and a protocol change on both
  halves. Requests-per-minute limits stay shares (records carry no request count).
  Revisit trigger: per-minute refusals keep showing one gateway's window full while
  the others have room, after cache reads left the token count (2026-10-05). (ruled
  2026-10-05: postponed — the user is weighing the end-of-stream dump.)

- **Carry-over copy reads the claimer's pushed base** — in control-plane mode, when
  one dropped limit is taken over by several new limits (a model-set edit that splits
  one limit into several), the first takes the counter and the others get a copy;
  each copy's pushed base is looked up under the first new limit's key, so if that
  key already had pushed totals of its own (a limit returning within its window),
  the copies start from that total instead of the predecessor's. Rare, and the next
  totals (about a second) replace every base. Kept here rather than in `GATEWAY.md`:
  it is a known imprecision of the code, not an agreement. Fix: read the
  predecessor's pushed base before the first claim renames it. Revisit trigger: a
  budget seen briefly misapplied after a model-set edit that splits one limit into
  several.

- **Output limit derived from remaining budget** — lower a request's output limit to
  what the remaining budget can pay for; refuse below a minimum instead of truncating.
  Revisit trigger: budget overshoot from single long requests shows up in practice.

## Gateway edge cases (follow-up audit lows, 2026-09-25)

- **Body budget per model** — the body budget (`KAIAK_BODY_MEMORY_BYTES`) is one pool
  for all models: bodies waiting in a saturated model's queue hold it, and requests
  for other models are answered `503 server_busy` (N-C5). Split it per model, or cap
  what one model's queued bodies may hold. Revisit trigger: `server_busy` answers for
  one model while another model's queue is full.
- **Live-gateway count during an outage** — the count backend caps and per-minute
  limits split by is the control plane's last pushed one: during an outage it never
  falls, so after a scale-down the remaining gateways keep small shares; after a
  control-plane restart the first totals count few live gateways and briefly
  over-share (N-C6). Revisit trigger: capacity refused (`queue_full`, per-minute
  limits) during an outage at a fleet size below the last count, or a cap overshoot
  seen right after a control-plane restart.
- **Old-epoch spool order** — with the opt-in data directory, queued batches of
  earlier epochs are sent in epoch-ID order (random), not by age; an index lost twice
  in a row can let a batch of an older epoch arrive after a newer epoch was counted,
  which the control plane counts again (N-M3, the earlier audit's L8). Order by age
  (file time, or a counter in the index). Revisit trigger: the data directory is used
  in production, or a double count is traced to it.
- **Slow readers hold slots** — there is no bound on a whole response: a client
  reading slowly holds its backend slot for response size ÷ read rate (the stall
  timer pauses while writing to the client, by design — the time is not the
  backend's) (N-S7). Revisit trigger: slots held by slow readers show up as queueing
  (`kaiak_backend_in_flight_requests` at the cap while the backends are idle).
- **Removed backends' connection pools** — a request still running under an older
  config (or a probe of a backend a reload dropped) can re-create a removed
  backend's pool until the next config apply prunes it again (the earlier audit's
  L10, hygiene). Revisit trigger: idle connections to removed backends are seen, or
  reloads that remove backends become frequent.

## Control plane

- **Publish `kaiak-control` to a registry** — as `@acdtrx/kaiak-control`, so apps
  install it instead of a submodule or `file:` checkout (`GUIDE.md` §3). Blocker: the
  package ships `.ts` source and Node refuses to strip types under `node_modules`
  (`ERR_UNSUPPORTED_NODE_MODULES_TYPE_STRIPPING`), so it needs a `tsc` build to
  `dist/` (JS + declarations, `rewriteRelativeImportExtensions`), with `exports`
  pointing there and a `development` condition keeping the repo on source — a dated
  amendment to `TECH-STACK.md`'s native-TypeScript, no-build choice. Also: drop
  `private`, add `repository`, `homepage`, `engines`, `publishConfig.access: public`
  (scoped packages default to restricted), a package README pointing to `GUIDE.md`;
  the version follows the release tags (the library and the gateway must share the
  protocol version); a publish job in `release.yml` on `vX.Y.Z`. Registry: npmjs.com
  (installs with no setup; trusted publishing from GitHub Actions stores no token
  and records provenance; needs an npm user or free organization named `acdtrx` to
  own the scope) or GitHub Packages (scope must match the owner; installing needs a
  GitHub token and an `.npmrc` line even for public packages). Revisit trigger: the
  user decides — apps outside the repo are being built on it and asked for a
  published package. (under consideration 2026-09-29)
- **Per-instance gateway tokens** — one token per gateway (or per instance-name
  pattern) instead of the shared `KAIAK_CONTROL_TOKEN`, so the control plane can bind
  a caller to the instance IDs it may report under; today any token holder can report
  usage and status for any instance (`CONTROL-PROTOCOL.md`, Control-plane processes →
  Trust model). Revisit trigger: gateways run where their token cannot be kept as
  tightly as the control plane's own secrets (another team's cluster, a customer
  site), or a forged usage or status report is seen.
- **Totals size bound** — every usage ack and totals push carries the full totals:
  one window per configured hour or month limit with spend, about 115 B each with
  ordinary IDs. The effective-limits bound (50 000) keeps that to about 5.8 MB, under
  the 16 MiB message cap, but long group IDs and model sets can bring it close, and
  every gateway receives it with every ack and push (2026-09-27 review, R5; past the
  cap acks fail, batches retry forever, and after the outage grace priced
  USD-limited models answer `503 budget_unavailable` fleet-wide). Fixes: totals as
  deltas since a revision, or only the windows that changed. Revisit trigger: totals
  messages above a few MiB, or active windows in the tens of thousands.
- **Seal usage batches by encoded size** — a gateway seals a batch at 500 records or
  5 s, never by size; a batch whose backend model names are made of characters Go's
  JSON encoder writes as six bytes (`<`, `>`, `&`) can pass the control plane's 2 MiB
  usage body limit and is then refused and set aside uncounted (2026-09-27 review,
  R6; with every other field at its longest a full batch is about 0.7 of it). Fix: seal when the encoded size
  would pass a margin under the limit. Revisit trigger: a usage batch refused for
  its size (`413` from the control plane).
- **Sample status page render time** — the sample renders the group tree
  quadratically (each node scans every scope for its children, key lists copied per
  key): about 0.6 s at 10 000 groups, on every page load and publish, on the event
  loop the protocol plugin shares (2026-09-27 review, R7). Sample only. Fix: index
  children and keys by parent once per render. Revisit trigger: the sample used with
  thousands of groups.

## Observability

- **Control-plane config publish cost** — `publishConfig`'s validation and
  resolution time and the encoded snapshot size are not measured: every gateway
  receives that snapshot, and a gateway refuses one over 16 MiB. The gateway side is
  measured (`GATEWAY.md` → Observability: Config load cost). Revisit trigger:
  publishes feel slow, or a gateway's `kaiak_config_size_bytes` comes near the
  16 MiB snapshot cap.
- **Request outcomes in the control plane** — an app built on `kaiak-control` sees
  usage but not failures in its own store: a usage record has no outcome field, only
  the `estimated` and `partial` flags, and a backend error status (a `4xx` relayed, a
  `5xx` answered `upstream_error`) settles as zero units with **neither flag** — the
  same as a success that used no tokens (`accounting/meter.go`, Settle); `estimated`
  marks a success the backend reported no usage for, not a failure. A request refused
  before routing sends nothing at all. Errors and refusals per key and team are in
  Prometheus (`kaiak_request_errors_total`, `GATEWAY.md` → Errors by key); this is
  the same data in the app's store. A plan was drafted and shelved (commit
  `ec05ca9`, `docs/plans/request-outcomes/`): `status` and `error_code` on every
  usage record, refusal counts per key, model and code in each usage batch, protocol
  version 4, `CountedBatch.refusals` in the store interface. Revisit trigger: the
  management app (or another host) needs failures and refusals in its own database
  rather than from Prometheus. (ruled 2026-10-01: metrics first. The user leans
  toward the observability path instead (OpenTelemetry export, below; 2026-10-02).
- **OpenTelemetry export** — request outcomes, timings and errors in an
  OpenTelemetry-style backend, kept apart from the control plane's job (config,
  usage, budgets). Three layers, cheapest first:
  - *Logs* — planned (2026-10-05, `docs/plans/otlp-logs/`): every log line in one
    vocabulary, OpenTelemetry's names where they fit, exported over OTLP/HTTP when
    an `OTEL_*` endpoint is set (`GATEWAY.md` → Observability: Logs, OTLP log
    export); a collector can also still read stderr (OTel Collector `filelog`,
    Vector, Fluent Bit). Either path may drop lines under pressure: fine for
    outcomes, never for billing (usage records stay the record).
  - *Metrics* — `/metrics` is Prometheus text, which the Collector's `prometheus`
    receiver scrapes; native OTLP metrics only if a backend cannot scrape.
  - *Traces* — new work. Confined test first: forward the W3C `traceparent` header
    to backends (vLLM can continue the trace) and log `trace_id` on the request line,
    linking client traces to gateway lines. Span export (per request, per attempt,
    queue wait) needs OTLP: the OTel Go SDK is a third-party dependency (a dated
    ruling in `TECH-STACK.md`), or a hand-written OTLP/HTTP exporter using its JSON
    encoding.
  Revisit trigger (traces; the logs layer is planned): a question about a request
  that the log line cannot answer, or a client team that traces its own calls and
  needs the gateway's part linked in.

