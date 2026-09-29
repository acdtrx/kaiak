# kaiak

> The philosophy doc: what kaiak is and the principles that bind its design. Read before
> any design work. Subsystem contracts live in `docs/specs/`; stack choices in
> `docs/TECH-STACK.md`.

## What it is

kaiak is a self-contained LLM gateway: one binary that sits between clients and model
backends, speaks the OpenAI API to clients, routes to self-hosted servers (vLLM,
llama-server, SGLang) and cloud providers (OpenAI, Azure OpenAI; Bedrock later), and
enforces access keys, rate limits and budgets while counting every token and dollar.

It has **no UI and no admin API of its own**. Everything it needs to know — backends,
models, prices, keys, groups, limits — comes from a **control plane** over a small
protocol, and everything it learns — usage, status — goes back the same way. A UI is
built separately, on the control plane.

The name reads the same both ways (the gateway carries traffic in both directions) with
*ai* at its center.

## The pieces

- **Gateway** (`kaiak`) — the data plane. Serves client traffic, enforces what the
  control plane decided, reports what happened. Deployed as N identical replicas.
- **`kaiak-control`** — a library implementing the control-plane side of the protocol:
  serving config, taking in usage, keeping totals, deciding budgets, tracking gateway
  status. Storage is pluggable.
- **Sample control plane** — a thin app on `kaiak-control`: config from a file, state in
  memory, one read-only page showing the config and everything the gateways report.
  For local runs, demos and validating the gateway end to end.
- **The real control plane** (with the UI) — a separate project that imports
  `kaiak-control`. Not built here.

## Principles

1. **The gateway serves; the control plane decides and keeps records.** The control
   plane is the source of truth for config, usage totals and budgets. The gateway holds
   **no state by default** (settled 2026-09-25): usage not yet acknowledged waits in
   memory, and nothing is written to disk — pods run read-only, with no volume. A data
   directory (last-known-good config cache, usage spool, totals cache) is an opt-in.
   Losing a pod loses nothing authoritative — only usage it had not delivered, which a
   drain delivers first.
2. **The gateway never depends on the control plane to serve traffic.** Once running
   it keeps serving through a control-plane outage. At boot it takes its config from
   a file, or from the control plane — and when the control plane is unavailable,
   from its **seed config**, the backup of a stateless gateway: free local models only
   (settled 2026-09-25; with a data directory, the last-known-good copy comes first).
   With no config from any of them it refuses to start, so its supervisor retries.
   What it cannot know during an outage (live budget totals) is handled by an
   explicit, configured policy — never by blocking requests on the control plane.
3. **Self-contained.** One static binary, no external services, no third-party code.
   Runs locally with a config file and nothing else; runs on Kubernetes unchanged.
4. **Passthrough first.** The common case — an OpenAI-format client talking to an
   OpenAI-compatible backend — is forwarded with minimal edits. Converting through an
   internal format is for when the two sides genuinely speak different APIs; it loses
   backend-specific fields, so it is never the default path.
5. **Seams, not features.** The gateway is built around four plug-in points: inbound
   API formats, the request pipeline, providers, and usage units. v1 fills each with the
   minimum; caching, guardrails, prompt logging, new APIs, new providers and non-token
   models (image, audio) arrive later as new plugs, not as rewrites.
6. **Limits are soft, with a known bound.** With N replicas and no shared store, exact
   global enforcement is impossible without coordination on the request path. kaiak
   accepts a documented overshoot — requests in flight plus one reporting interval —
   instead. Every limit states where it is enforced and how far it can drift.
7. **Two usage paths, never mixed.** Metrics (Prometheus) are for dashboards: cheap,
   approximate, reset-tolerant. Usage records (to the control plane) are for billing:
   exact, idempotent, durable. Neither is derived from the other.
8. **Strict config, safe swaps.** Unknown fields and wrong types fail loudly. A config
   that fails validation never replaces a working one; the rejection is reported back.
   A request finishes under the config it started with.
9. **Nothing sensitive leaves the request.** Client keys are stored as hashes and
   appear everywhere else only as key IDs. Provider secrets come from the environment.
   Prompts and responses are not logged.

## Domain model (v1)

- **Backend** — one serving endpoint (URL, provider type, credentials by env
  reference, concurrency cap and queue, health check).
- **Model** — the public name clients use; one or more **deployments** (backend +
  backend-side model name), identical copies load-balanced among; metadata (context
  length, default sampling parameters, supported reasoning efforts, capabilities) —
  declared in config (`kaiak-control`'s `verifyBackend` fills it in from what the
  backend reports when the model is added; the gateway discovers nothing);
  output-limit default and ceiling; prices (per usage unit, with an effective date);
  which limit types apply.
- **Key** — belongs to one **group**; hashed; can expire or be disabled. Keys are
  created by the control plane and reach the gateway as hashes in config. A key has
  **no limits of its own**: its usage counts toward its group and every ancestor.
- **Group** (settled 2026-09-27) — a node of one generic tree: a parent (none for a
  top-level group), the models it allows, its own limits, defaults for its direct
  children (allowed models, limits) and free-form labels only the control plane
  reads. At most 8 levels; the order of levels is free per branch (team → project →
  env → workload, team → env → project, a `users` group whose children are people
  with personal keys), and neither half gives levels meaning. Allowed models
  intersect down the path; no list anywhere on the path means every model. A group
  never changes parent: a move is a delete and a create. Who manages which group is
  a control-plane concern, not part of the gateway config.
- **Global** — the implicit root above every top-level group: limits across
  everything.
- **Limits** — any scope (a group, or global) × any type (requests/min, tokens/min,
  tokens/hour, USD/month, …) × a set of models. A request must pass the limits of
  every scope on its key's path — its group, each ancestor, global. Months are
  calendar months in UTC. Unpriced models cost 0 and are held by request and token
  limits.

## v1 scope

- Inbound: OpenAI `/v1/chat/completions`, `/v1/completions`, `/v1/embeddings`,
  `/v1/models` (extended with metadata), `/v1/models/{id}/props`.
- Providers: OpenAI-compatible (vLLM, llama-server, SGLang, OpenAI), Azure OpenAI
  (its OpenAI-compatible `/openai/v1/` API).
- Routing: aliases, multiple deployments, load balancing, retries before the
  first byte, circuit breaker driven by real-traffic failures (probing only while open),
  per-backend concurrency cap with a bounded queue.
- Keys, the group tree, global limits; output-limit default and ceiling
  per model; output limit reserved against the allowance while a request runs.
- Accounting: token usage from the backend (estimated and flagged when missing), cost
  from the price table, billing of partial output on client disconnect.
- Control-plane protocol; file mode; seed config; opt-in data directory
  (last-known-good config, usage spool).
- Ops and usage metrics; readiness, liveness, draining.
- `kaiak-control` and the sample control plane with its read-only page.

## Out of v1 (see `docs/BACKLOG.md`)

Bedrock; Anthropic Messages inbound; OpenAI Responses API; image and audio models;
response caching, guardrails, prompt templating, prompt logging; output limit derived
from remaining budget; cloud workload identity (AWS IRSA / Azure managed identity);
reasoning-effort discovery; interim usage reports for long streams; always-on active
health checks; demand-weighted per-minute limit shares.
Never planned: OpenAI Batch API, Assistants API.
