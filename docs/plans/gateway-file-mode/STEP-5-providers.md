# Step 5 — providers

**Status:** done (2026-09-24) — phase 2 continues with step 6; suite green, no expected reds.

## Intent

Send requests upstream and relay responses, streamed and not: the openai-compatible and
azure-openai providers, plus the fake backend every later test uses.

## Files likely touched

- `gateway/internal/provider/` — provider interface, openai-compatible, azure-openai
- `gateway/internal/fakebackend/` — OpenAI-compatible fake that can stream, stall,
  fail, hang, omit usage, and record what it received

## Decisions made during planning

- The provider interface takes the pipeline's request context and returns a response
  stream the accounting stage can observe; it must fit a translating provider later
  (Bedrock) without pipeline changes — a doc comment states this constraint.
- Passthrough edits only: backend-side model name, output limit, and
  `stream_options.include_usage: true` on streams. When the client did not ask for
  usage, the gateway **strips the usage-only final chunk** so the client sees the
  stream it requested. Unknown fields survive (test with an unknown field round-trip).
- azure-openai = openai-compatible against `<endpoint>/openai/v1/` with `api-key`
  header; one shared implementation with the auth header as the difference — not two
  copies.
- Credentials are read from the named env var at config load; a missing env var is a
  config validation error.
- Client disconnect cancels the upstream request via `context`.
- Streaming: flush every SSE event as it arrives — no buffering.

## Acceptance criteria

- Tests against the fake backend: non-stream and stream relay byte-faithful except the
  owned edits; usage chunk stripped/kept per client request; unknown fields preserved;
  disconnect cancels upstream; connect and first-byte timeouts return OpenAI-shaped
  errors; azure request carries `api-key` and the v1 path.

## Result

Commands run (2026-09-24):

- `scripts/check-gateway.sh` — gofmt, `go vet`, staticcheck 2026.2.1 clean;
  `go test -race ./...` **pass** (`cmd/kaiak`, `auth`, `config`, `provider`,
  `server`, `state`; `fakebackend` has no tests of its own). Also `-count=5` on
  `provider` and `server` — pass, no flakes.
- `npm test` in `control/` — 86 tests **pass**; `npm run lint` — `tsc` clean,
  `boundaries ok`.
- Manual, built binary against the fake backend run from a throwaway main (deleted):
  config with an openai-compatible backend (`Qwen/Qwen3-32B` behind public
  `qwen3-32b`) and an azure-openai one (`text-embedding-3-small` behind `embed`),
  credentials from env. Non-stream chat → 200, backend body identical to the client's
  but the model (`1.000000000000000000001` in an unknown field kept exactly), client
  `x-request-id: manual-1` echoed and forwarded; streamed chat → SSE relayed, usage
  chunk withheld, backend received `stream_options.include_usage: true`; embeddings →
  Azure path `/openai/v1/embeddings` with `api-key`, no `Authorization`; backend
  stopped → `502 upstream_unavailable`, the log line naming the refused address.
  Backend requests carried `Authorization: Bearer <backend secret>`, never the client
  key; client key and backend secrets appear 0 times in the log. Scratch files deleted.

Delivered:

- `gateway/internal/provider`:
  - `Provider.Send(ctx, *Request) (Response, error)`; `Request{Endpoint, Deployment,
    Body, Stream, IncludeUsage, RequestID}`; `Response{Status(), Header(), Stream(),
    Next() (Event, error), Close()}`; `Event{Data, Payload, Hidden}`; `*Error{Code}`
    with `upstream_unavailable`, `upstream_timeout`, `upstream_auth_failed`.
  - `Registry` (`NewRegistry(lookupEnv)`, `For(backend)`): one `http.Client` per
    backend, rebuilt only when its connect timeout changes; credentials read from the
    named env var.
  - One implementation for both backend types (URL layout and credential header
    differ); byte-splicing body editor; WHATWG SSE block reader (CR, LF, CRLF; raw
    bytes kept for forwarding; 16 MiB block bound).
- `gateway/internal/fakebackend`: scripted OpenAI-compatible server (`New`,
  `SetReply`, `Requests`, `Arrivals`, `Request.Canceled`) — normal/stream answers,
  status failures, stall, pace, hang, cut, omitted usage, OpenAI's `include_usage`
  behavior (usage-only final chunk, `"usage": null` on the others).
- `gateway/internal/server`: stages `routing` (first deployment) and `provider`
  (send + relay) between `model_access` and `respond`, which now answers only the
  model endpoints (`501` until step 6); `NewAPI(holder, providers, logger)`; log
  fields `backend`, `deployment_model`, `upstream_error`, `relay_end`.
- Docs: `GATEWAY.md` — error rows, passthrough edits, usage-chunk rule, header
  allowlists, relaying, upstream failure mapping, connection pooling, connect-timeout
  mapping, log fields; `ARCHITECTURE.md` — `provider` and `fakebackend` entries.

Decisions beyond the plan:

- **Deployment choice is the model's first deployment** — an honest placeholder for
  the choice, not balancing; step 6 replaces `chooseDeployment` with least-in-flight.
- **Pull-style events, observer in the pipeline**: the provider returns events; the
  relay loop in `server` is where every event, hidden ones included, passes — step 7
  adds its observer call there and reads the end state (`relay_end`, EOF) after.
  No goroutines in the relay path.
- **Output-limit seam**: the body editor applies a list of member edits
  (`setValue`, or a function of the current value); step 6 adds a request field for
  the pipeline's parameter edits and appends them in `passthroughBody`. No field was
  added now (nothing would fill it).
- **Byte splicing, not re-encoding**: edits replace owned values at their byte
  offsets, so key order and whitespace survive too — stronger than the brief's
  "values survive". A body whose owned values need no change goes out identical.
- **First-byte timeout runs to the first event**, not to response headers: servers
  send stream headers before generating, so a header-based timer would miss a stalled
  generation. Send reads the first event before returning, so the client has seen
  nothing when a timeout or connection failure is answered.
- **Connect timeout → 502 `upstream_unavailable`** (a connect failure), first-byte
  timeout → 504 `upstream_timeout`.
- **Backend 401/403 → 502 `upstream_auth_failed`** instead of relaying: the client
  authenticated to the gateway, so relaying would tell it its own key is bad. Other
  backend error statuses are relayed with their body.
- **Mid-response backend failure cuts the client connection** (`http.ErrAbortHandler`
  after the log line), so a truncated stream or body never ends cleanly.
- **Client disconnect before the answer** is logged as `499 client_closed`.
- **Header allowlists both ways**: upstream gets only content type, accept,
  user agent, request ID and the backend credential; the client gets only the
  backend's `Content-Type` and `Retry-After`.
- **Compression off, redirects not followed, `HTTP_PROXY`/`HTTPS_PROXY` honored,
  256 idle connections per host.**
- **No "always runs" runner phase yet**: relaying finishes inside the provider stage
  and `Close` is deferred there, so nothing needs it now; steps 7/8 add it for
  release/settlement.
- Response `model` is the backend's name, relayed unchanged (rewriting would mean
  re-encoding every chunk).

Deviations: none from the acceptance criteria.

Open doubts:

- `"usage": null` appears on every chunk once the gateway sets `include_usage` (OpenAI
  behavior; vLLM omits it) — clients that did not ask see an extra null field.
  Removing it would mean re-encoding every chunk; left as is.
- No `X-Accel-Buffering: no` / `Cache-Control` on relayed streams: an nginx-style
  ingress in front of the gateway may buffer SSE unless configured. Worth checking in
  the step 11/real-cluster run.
- The "real vLLM before calling the phase done" risk in `OVERVIEW.md` remains for
  the phase-2 manual check with the user.
