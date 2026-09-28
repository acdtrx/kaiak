# Step 6 — routing and the model surface

**Status:** done (2026-09-24) — phase 2 complete in code, suite green, no expected
reds; the phase's manual acceptance item (chat, stream and embeddings through a real
vLLM and Azure, with the user) is still pending.

## Intent

Resolve a public model name to a deployment, balance across deployments, apply
declared defaults and output limits, and serve the model-listing endpoints.

## Files likely touched

- `gateway/internal/routing/` — model resolution, deployment choice
- `gateway/internal/server/` — `/v1/models`, `/v1/models/{id}`, `/v1/models/{id}/props`

## Decisions made during planning

- **Load balancing: least in-flight, round-robin on ties** — cheap, adapts to slow
  backends; weights only if a real need shows up. No fallback or retry here (P3).
- Declared defaults fill only parameters the request omits; output-limit default
  applies when the request sets none, the ceiling lowers larger values. Both
  `max_tokens` and `max_completion_tokens` are honored (the one the client used is the
  one edited).
- `/v1/models` lists only the models the key's owner may use, OpenAI fields plus kaiak
  metadata; `props` returns declared metadata and the output-limit default/ceiling.

## Acceptance criteria

- Tests: two deployments share load; defaults filled but never override the client;
  ceiling applied; model lists differ per key owner; `props` matches config.
- Manual (with the user): chat, stream and embeddings through a real vLLM and Azure.
  **Deferred (2026-09-24):** the user provides a vLLM host later and Azure access
  later still; both run with the live-test kit from step 11. Phase 2 closes on the
  automated suite.
- Phase 2 end: full suite green, committed.

## Result

Commands run (2026-09-24):

- `scripts/check-gateway.sh` — gofmt, `go vet`, staticcheck 2026.2.1 clean;
  `go test -race ./...` **pass** (`cmd/kaiak`, `auth`, `config`, `provider`,
  `routing`, `server`, `state`). Also `-count=5` on `server`, `routing`, `provider` —
  pass, no flakes.
- `npm test` in `control/` — 86 tests **pass**; `npm run lint` — `tsc` clean,
  `boundaries ok`.
- Manual, built binary against the fake backend run from a throwaway main (deleted
  with every scratch file): `qwen3-32b` on two openai-compatible deployments
  (`Qwen/Qwen3-32B`, `Qwen/Qwen3-32B-AWQ`) with defaults `temperature`/`top_k` and
  output limit 512/4096; `embed` on an azure-openai backend. Non-stream chat with
  `max_tokens: 9000` → backend got `max_tokens: 4096` plus both defaults, client got
  `"model":"qwen3-32b"`; streamed chat → backend got `max_completion_tokens: 512`,
  every client chunk named `qwen3-32b`, `X-Accel-Buffering: no`; embeddings → model
  rewritten, no output-limit key added; `/v1/models` listed two models for the
  workload key and only `embed` for the user key; `props` showed defaults and
  `output_limit`, `404 model_not_found` for the user key. Sequential requests
  alternated between the deployments; after a SIGHUP reload, three held streams went
  A, B, A, a fourth request went to B (fewer in flight), and after the clients left
  (logged `relay_end: client_closed`) the next one went to A again. Secrets and keys
  appeared 0 times in the log.

Delivered:

- `gateway/internal/routing`: `Router` (`New`, `Acquire(model) (Deployment,
  release)`, `InFlightByBackend()` for step 9's metrics). Least in flight, ties take
  turns per public model; counts keyed by backend ID + backend model, so they outlive
  reloads; entries at zero are removed.
- `gateway/internal/provider`: `Request.PublicModel` and `Request.Params` (top-level
  parameters the pipeline sets, spliced like the model edit); a streaming
  response-model rewriter (JSON bodies edited as they pass, each stream chunk edited
  in its data lines — the SSE reader now reports each data line's span in the raw
  block).
- `gateway/internal/server`: stage `model_params` (after `model_access`: defaults,
  output limit, `request.outputLimit` for step 8), `routing` through the router with
  release as a request finisher, `models` (the three model endpoints) replacing the
  `501`; `X-Accel-Buffering: no` on relayed streams; `request.finishers`, run by a
  deferred `finish` whatever stage ended the request — the "always runs" seam steps
  7/8 need. `NewAPI(holder, providers, router, logger)`.
- `gateway/internal/auth`: `Identity.AllowedModels()` for the listing.
- Docs: `GATEWAY.md` — model endpoint shapes, defaults rule, pipeline order, response
  model name, `X-Accel-Buffering`, load-balancing rule, output-limit keys and
  effective limit; `ARCHITECTURE.md` — `routing` and `server` entries.

Decisions beyond the plan:

- **Defaults apply on every body endpoint, embeddings included**: defaults are per
  model and a model serves its own kind of endpoint, so an embedding model's
  defaults are embedding parameters; filtering by endpoint would guess wrong. `null`
  counts as unset and is replaced (OpenAI's reading of `null`).
- **Chat output-limit default goes under `max_completion_tokens`**: OpenAI's current
  field; vLLM (prefers it over `max_tokens`), SGLang, llama-server (alias of
  `n_predict`, checked in its source), OpenAI and Azure read it, and OpenAI/Azure
  reasoning models refuse `max_tokens`. Completions uses `max_tokens` (its only
  field); a `max_completion_tokens` sent to completions is not an output-limit key
  there and passes untouched.
- **Negative output-limit values are lowered to the ceiling**: llama-server reads
  `-1` as unlimited, which would slip past the ceiling.
- **Effective output limit** = the largest output-limit key after edits (both set →
  the larger), `nil` when the model declares none and the client sent none or a
  negative value.
- **Model params are their own stage before routing** (and before step 8's limits),
  not part of routing: limits must reserve the effective output limit before a
  deployment is chosen.
- **In-flight release through request finishers**, deferred in the handler: runs on
  every outcome, including the connection-cutting panic after a mid-stream failure.
- **Response model rewrite lives in the provider**: the provider returns events in
  the client's format, and the client's format carries the client's model name — a
  translating provider sets it the same way. `Event.Payload` stays the backend's
  bytes for observers.
- **`created: 0`** on every model entry (stable across reloads and replicas).
- Props = the model entry plus `defaults` (`{}` when none) and `output_limit`
  (`null` when none).

Deviations: none from the acceptance criteria. The manual check against a real vLLM
and Azure is the user's, still open.

Open doubts:

- Azure answers with its dated model version (`gpt-4o-2024-08-06`); the rewrite hides
  it behind the public name. Operators who want the real version see it nowhere now
  (it is not logged either).
- The router's tie cursor map keeps one small entry per public model name ever
  routed; models removed by reloads leave theirs behind (bounded by config churn).
- `"usage": null` on chunks when the gateway set `include_usage` (step 5's doubt)
  remains.
