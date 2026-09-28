# Step 11 — end-to-end test and live-test kit

**Status:** done (2026-09-24). Ends phase 4 and the plan. The container image moved
to `docs/BACKLOG.md`, Delivery (ruled 2026-09-24, user decision: deferred from P1,
revisit at the first cluster deployment) — the file keeps its name.

## Intent

Ship the deliverable — the static binary — and prove the whole gateway end to end as
a real process; give the user a kit to check it against real backends once access
arrives.

## Files likely touched

- `gateway/e2e/` — Go test that builds the binary, starts the fake backend, runs
  `kaiak` as a subprocess with a config file, drives it over HTTP
- `examples/config.json` — a documented example config (vLLM + Azure)
- `README.md` — run locally; `AGENTS.md` commands line
- **Live-test kit** (added 2026-09-24, user request): `scripts/live/` — run the built
  binary against a real backend and exercise it end to end, plus
  `docs/testing/LIVE-BACKENDS.md` — the runbook. Covers vLLM, Azure OpenAI and
  OpenAI; Azure is written without access, so the runbook states what was assumed and
  what to check first when access arrives.

## Decisions made during planning

- The e2e test covers the scenario list in `OVERVIEW.md` → End-to-end verification.
- Integration against a real vLLM/Azure is opt-in via env vars, never needed for green.
- Live-test kit: one config template per backend kind with credentials by env var;
  checks for models listing, chat non-stream and stream with and without client
  usage, embeddings, output-limit ceiling, a limit tripping, usage and cost on the log
  line and in `/metrics`; each prints pass/fail. Azure targets the `/openai/v1/` API
  with `api-key`.
- Minting a key before P2's `keygen`: the README shows the one-line `shasum` recipe.
- (2026-09-24) No container image in P1: `Dockerfile` and `.dockerignore` are not
  written; the settled shape stays in `docs/TECH-STACK.md` (Build & image) and the
  work in the backlog. The manual live check runs the binary.

## Acceptance criteria

- The e2e test passes under `go test -race ./...`.
- The live-test kit runs green against the fake backend standing in for each kind.
- Manual verification with the user (OVERVIEW step 3) — when the user's vLLM and
  Azure access are available; not blocking the plan's completion.
- Phase 4 end and plan end: full suite green, committed.

## Result

Commands run (2026-09-24):

- `scripts/check-gateway.sh` — exit 0: gofmt, `go vet`, staticcheck 2026.2.1 clean
  for the gateway and for `scripts/live`; `go test -race ./...` **pass** (every
  package, `e2e` included); live-kit self-test **13 passed, 0 failed, 0 skipped** for
  each of vllm, openai, azure-openai. Whole script ≈ 8 s warm.
- `go test -race -count=10 ./e2e` — pass (≈ 2.3 s per run: one test, 12 subtests),
  no flakes.
- `npm test` in `control/` — 88 tests **pass** (the example-config test added);
  `npm run lint` — `tsc` clean, `boundaries ok`.
- Kit failure paths, by hand against the runnable fake: `-profile no-usage` → `chat`
  "no usage in the answer", every `usage-log/*` "estimated=true", `chat-stream-usage`
  "0 usage chunks", exit 1; Azure kind with a wrong key → `502` with
  `gateway: error_code=upstream_auth_failed upstream_error=… backend live answered
  401`. No process or temporary directory left behind (checked after each run).

Delivered:

- `gateway/internal/fakebackend`: `NewAt(addr)` (a fixed or free port);
  `Reply.EventDelay` (slow streams), `Reply.HonorMaxTokens` (cut the answer at the
  request's `max_completion_tokens`/`max_tokens`, `finish_reason: "length"`),
  `Reply.RequireHeader`/`RequireValue` (answer `401` to a wrong backend credential).
- `gateway/internal/fakebackend/cmd/fakebackend`: the fake as a process — `-addr`,
  `-profile normal|no-usage|slow`, `-auth none|bearer|api-key` + `-key`, `-quiet`;
  prints `listening <url>`, logs each request, stops on SIGINT/SIGTERM; a 49-word
  answer so a small ceiling cuts it.
- `gateway/e2e`: `TestMain` builds `kaiak` (with `-race` when the test runs with it,
  and fails the test on a race report from the subprocess); the harness runs it on
  `127.0.0.1:0` ports learned from its `listening` log lines, waits on log lines
  (config applied/rejected, a request's settled line, draining) — no sleeps. Scenarios,
  in one process then a restart on the same data dir:
  auth reject (missing / unknown key) · `/v1/models` filtered per key (+ `404` on the
  model endpoint) · non-streamed chat (public model name, backend got the deployment
  model and the output-limit default, exact units and cost in the log line) · streamed
  chat without client usage (usage chunk withheld, backend got `include_usage`, exact
  count) · streamed chat with client usage (usage chunk relayed) · embeddings ·
  requests/min `429` with `Retry-After` and `x-ratelimit-*` · USD/month budget
  tripping · SIGHUP with a broken file (`config rejected … running_config=kept`,
  traffic still served) then a valid one (new model live) · `/metrics` (error
  classes, config loads, usage requests/tokens/cost, in-flight 0) · SIGTERM exit 0 with
  the shutdown snapshot · restart: budget still spent, per-minute window fresh ·
  SIGTERM drain: `/readyz` 503, new connections refused, the in-flight stream runs
  to `[DONE]`, record not partial, `drained` without cut-offs, exit 0.
- `scripts/live` (module `kaiak-live`, standard library only, imports nothing from
  the gateway): `go -C scripts/live run . -kind vllm|openai|azure-openai -base-url …
  -model … [-embeddings-model …] [-api-key-env NAME]`, `-self-test [-kind …]`,
  `-max-output`, `-ceiling`, `-price-in/-price-out`, `-chat-defaults`, `-kaiak`,
  `-keep`, `-v`, `-request-timeout`. Generates the config (fresh key + hash; public
  models `live-chat`, `live-capped`, `live-rpm`, `live-embed`), builds and runs
  `kaiak`, runs 13 checks (auth-reject, models, chat, stream without/with usage,
  embeddings, a `usage-log/*` check per request, output-ceiling, rate-limit,
  metrics) plus a clean-exit check, prints PASS/FAIL/SKIP and exits 1 on any failure;
  stops the gateway and removes its files on every exit path (Ctrl-C and a closed
  output pipe included).
- `docs/testing/LIVE-BACKENDS.md` — prerequisites, one command block per kind with
  its URL/model/auth notes, the self-test, what each check proves, reading failures,
  "Azure: assumptions made without access", vLLM checks worth doing by hand, a client
  library snippet.
- `examples/config.json` — two vLLM hosts (Qwen3-32B with thinking off and on as two
  public models), a vLLM embeddings host with a key, an Azure resource with two priced
  deployments; teams, workloads, users, limits, keys. Kept valid by
  `TestExampleConfigs` (gateway) and the "example configs" suite (kaiak-control), both
  reading `examples/*.json`.
- `scripts/check-gateway.sh` also lints `scripts/live` and runs its self-test.
- Docs: `README.md` (build, mint a key, config, run, checks), `AGENTS.md` commands,
  `docs/ARCHITECTURE.md` (fakebackend command, e2e, live kit), `docs/BACKLOG.md`
  (Delivery → Container image), `OVERVIEW.md` (scope, verification status).

Decisions beyond the plan:

- **The kit is a Go program**, not curl + jq: SSE parsing (usage-only chunk, `[DONE]`
  position, per-chunk model name) and log-line matching are clearer and sturdier in
  code, and it runs anywhere Go does. A separate module keeps it out of the gateway's
  module and binary; it duplicates the small log reader of the e2e harness rather than
  sharing test code across modules.
- **The self-test runs in `scripts/check-gateway.sh`** (≈ 2 s), so the kit cannot rot
  while no real backend is at hand.
- **The fake checks the backend credential and path per kind** in the self-test, so a
  config the kit generates wrongly (Azure base URL with `/v1`, missing `api-key`)
  fails there, not first against Azure.
- **The ceiling check asks through `max_tokens` on vLLM** (exercising the gateway
  lowering the client's own key) and through `max_completion_tokens` on the cloud
  kinds (their reasoning models refuse `max_tokens`).
- **Kit prices are placeholders** (1 / 2 USD per million by default) so the cost path
  is always exercised; `-price-in 0 -price-out 0` skips it.
- **Separate commit before this one**: relayed backend errors split by whose problem
  they are — `upstream_client_error` (4xx), `upstream_rate_limited` (429),
  `upstream_error` (5xx and `upstream_auth_failed`), recorded in `GATEWAY.md`
  (user request during this step).

Open items:

- Manual live checks (OVERVIEW step 3) — pending the user's vLLM host and Azure
  access; run with `docs/testing/LIVE-BACKENDS.md`.
- Container image — backlog (Delivery).
- Go toolchain: `go.mod` and the local toolchain are 1.26.6; 1.26.8 is out. A bump is
  a separate change.
