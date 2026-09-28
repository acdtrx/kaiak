# Step 4 — server, pipeline, auth

**Status:** done (2026-09-24) — phase 2 continues with step 5; suite green, no expected reds.

## Intent

The two listeners, the ordered pipeline skeleton, and the stages that need no backend:
inbound parsing, auth, request IDs, error shape, body cap, the request log line,
health endpoints.

## Files likely touched

- `gateway/internal/server/` — API and admin listeners, pipeline stages as explicit
  ordered steps, OpenAI error writer, request-ID handling, log line
- `gateway/internal/auth/` — bearer parsing, SHA-256 lookup, expiry and disabled
  checks, owner resolution (workload + team, or user)

## Decisions made during planning

- Pipeline is a fixed ordered list of stage functions over a request context struct —
  no generic middleware chain; a new stage is a new entry (seam for later stages).
- Inbound format stage parses only what the gateway owns (model, stream, output-limit
  fields, `stream_options`) and keeps the raw body for passthrough.
- Auth failures: `401` with OpenAI error shape; unknown model or not allowed: `404`
  `model_not_found` (OpenAI's behavior) — the key never learns other models exist.
- Constant-time hash comparison is unnecessary with a hash-keyed map lookup; the key
  itself is never logged — the log line carries the key ID.
- `/readyz` fails until config is loaded; `/healthz` always 200 while the process runs.

## Acceptance criteria

- Tests: missing/bad/expired/disabled key → 401; oversize body → 413; request ID echoed
  or generated; log line has no key or content; admin endpoints on the admin port only.

## Result

Commands run (2026-09-24):

- `scripts/check-gateway.sh` — gofmt, `go vet`, staticcheck 2026.2.1 clean;
  `go test -race ./...` **pass** (`cmd/kaiak`, `internal/auth`, `internal/config`,
  `internal/server`, `internal/state`). Also `-count=5` — pass, no flakes.
- `npm test` in `control/` — 86 tests **pass**; `npm run lint` — `tsc` clean,
  `boundaries ok`.
- Manual, built binary with a scratch copy of `valid/full.json` (a generated key's
  hash in place of `k-eval-ci`'s, `eval-pipeline` narrowed to `qwen3-32b`, API-key
  env vars set, listeners on `127.0.0.1:18080` / `:19090`): no key → `401
  missing_api_key` with `WWW-Authenticate: Bearer` and a generated `X-Request-Id`;
  `gpt-4.1` (exists, not allowed) and `nope-00` (unknown) → `404 model_not_found`, same
  body but the name; `qwen3-32b` → `501 not_implemented`, `x-request-id: manual-1`
  echoed; `GET /v1/models/qwen3-32b/props` → `501`; `GET /v1/chat/completions` →
  `405`; admin `/readyz` and `/healthz` → `200`; `/readyz` on the API port → `404
  unknown_url`. One log line per request with `key_id=k-eval-ci`; the key appears 0
  times in the log. SIGINT → `kaiak stopped`, exit 0. Scratch files deleted.

Delivered:

- `gateway/internal/auth`: `Authenticate(snapshot, authorizationHeader, now)` →
  `Identity{KeyID, Workload, Team, User}` or `*auth.Error{Code, Message, KeyID}`
  (codes `missing_key`, `malformed_key`, `unknown_key`, `disabled_key`, `expired_key`,
  `model_not_found`); `Identity.AuthorizeModel(model)` — the one model-access check.
- `gateway/internal/server`:
  - `request` — the per-request struct: status-recording writer, `*http.Request`,
    endpoint, request ID, start time, snapshot (taken once), identity, model, raw
    body, owned fields (`Stream`, `MaxTokens`, `MaxCompletionTokens`, `IncludeUsage`),
    plus log-line fields.
  - `newPipeline()` — the fixed ordered stage list: `auth` → `inbound` →
    `model_access` → `respond`. A stage returns `*apiError` to end the request.
  - `NewAPI(holder, logger)` — routes the six endpoints through the pipeline;
    OpenAI-shaped `404 unknown_url` and `405 method_not_allowed` (with `Allow`) for
    everything else; request ID and the log line for every answer.
  - `NewAdmin(holder)` — `/healthz`, `/readyz` (503 `config not loaded` until the
    holder is loaded). `notReadyReason` is the seam where draining (step 10) joins.
  - `Listen` / `Listener.Serve(ctx)` — bind at startup, serve until cancel, then
    `Shutdown` with a 5 s timeout (then `Close`). This is the interim stop; step 10
    replaces it with the drain sequence. `ReadHeaderTimeout` 30 s, no write timeout.
- `cmd/kaiak`: `KAIAK_LISTEN_ADDR` (`:8080`), `KAIAK_ADMIN_ADDR` (`:9090`); both
  listeners bind after the startup config load; a listener that fails at runtime
  stops `run` with its error; every goroutine is waited for.
- Docs: `GATEWAY.md` — listen env vars, auth and expiry rules, model access, error
  code table, request-ID rules, log line fields, pipeline order;
  `ARCHITECTURE.md` — `server` and `auth` descriptions.

Decisions beyond the plan:

- **Auth runs before inbound**, not after as `GATEWAY.md` listed: key auth needs only
  headers, and reading first would let an unauthenticated client make the gateway
  buffer up to the body cap. Model access runs after inbound (it needs the model) and
  lives in `auth` — one check, one place; routing may assume the model exists.
  Recorded in `GATEWAY.md` (Request pipeline).
- **Terminal stage `respond` answers `501 not_implemented`** (`server_error`) for
  authenticated, valid requests: the honest state until routing and the provider
  exist. Step 5/6 replace it with the routing and provider stages and the model
  endpoints' bodies. No placeholder stages for limits/routing/accounting were added —
  the list is where they go.
- **One pipeline for all six endpoints**: the model endpoints run the same stages;
  `inbound` does nothing for bodyless endpoints and `model_access` skips only
  `/v1/models` (names no model). The route sets the model for the path endpoints.
- **Owned fields read by exact key** from a top-level map: `encoding/json` matches
  struct fields case-insensitively, so `"Model"` would have been read as the model
  while the backend saw an unknown field.
- **Invalid `x-request-id` is replaced, not refused** — a bad tracing header should
  not fail a request.
- `401` for disabled/expired keys says so in the message (only a real key holder sees
  it); the log line carries `auth_failure` and, for those two, the key ID.
- Error bodies are written without HTML escaping (`<key>` stays `<key>`).

Deviations: none from the acceptance criteria.

Open doubts:

- Default API port `:8080` is also llama-server's default port (and the `minimal.json`
  fixture points its backend at `localhost:8080`): running both on one machine needs
  `KAIAK_LISTEN_ADDR`. Kept as briefed; easy to change.
- A model whose public name itself ends in `/props` cannot be fetched with
  `GET /v1/models/{id}` (the suffix selects props). Rare; recorded in `GATEWAY.md`.
- Accounting/limits will need a "runs even when an earlier stage failed" step (release
  reservations); the current stage runner stops at the first error. Steps 7/8 extend
  the runner, not the stages.
