# Plan: gateway in file mode (P1)

## Goal

A usable single-instance gateway: `kaiak` with a JSON config file and no other process,
serving OpenAI-format traffic to vLLM and Azure backends, enforcing keys and limits,
counting tokens and cost, exposing metrics, draining cleanly. It is also the
foundation the control-plane plan (P2) and the reliability plan (P3) build on.

Plan order (settled 2026-09-24): P1 gateway file mode → P2 control plane
(`protocol/` messages, `kaiak-control`, sample, gateway control-plane mode, spool,
last-known-good) → P3 routing reliability (retries, fallbacks, circuit breaker,
concurrency cap and queue).

## Scope

- The **config document** as a contract: JSON Schema and fixtures in `protocol/`,
  validated by `kaiak-control` (ajv) and by the gateway (strict decode + validation).
- Minimal `control/` scaffolding — only what checks the schema against the fixtures.
- Gateway: file mode with reload, API and admin listeners, request pipeline, auth,
  openai-compatible and azure-openai providers (passthrough, streaming), multiple
  deployments with load balancing, `/v1/models` and `props`, declared defaults and
  output limits, local limit counters for every window, accounting, metrics, logs,
  readiness, draining, the file-mode usage snapshot.
- An end-to-end test of the binary, and a live-test kit for real backends. The
  container image is out (ruled 2026-09-24: moved to `docs/BACKLOG.md`, Delivery —
  revisit at the first cluster deployment).

## Out of scope

- Everything control-plane (P2): protocol messages, `kaiak-control` beyond fixture
  validation, the sample, `keygen`, spool, last-known-good config.
- Retries, fallbacks, circuit breaker, concurrency cap and queue (P3).
- Backlog items (`docs/BACKLOG.md`).

## Constraints

- Zero third-party Go dependencies; staticcheck runs by pinned `go run`.
- One pipeline, no side doors; only provider code talks to backends; passthrough
  keeps unknown fields; nothing sensitive in logs or metrics (`AGENTS.md`).
- Every data-directory file carries a format version.

## Risks

- **SSE passthrough fidelity** — injecting `stream_options.include_usage` changes the
  stream the client sees; handled in step 5 (strip the usage-only chunk when the client
  did not ask for it). Test against a real vLLM before calling the phase done.
- **Schema/decoder drift** — two validators for one document. Mitigated by running the
  same valid/invalid fixtures through both from step 3 on.
- **Plan size** — 11 steps. Phases are real stopping points; each leaves a working,
  green state.

## Phases and steps

- **Phase 1 — contract and skeleton** (steps 1–3): repo tooling, the config contract,
  config loading. Ends with: `kaiak` loads and validates a config file, reloads on
  SIGHUP, and both halves agree on the fixtures.
  1. `STEP-1-scaffolding.md`
  2. `STEP-2-config-contract.md`
  3. `STEP-3-gateway-config.md`
- **Phase 2 — serving path** (steps 4–6): requests flow client → gateway → backend.
  Ends with: an authenticated chat completion, streamed and not, reaches vLLM or Azure
  through the gateway.
  4. `STEP-4-server-pipeline-auth.md`
  5. `STEP-5-providers.md`
  6. `STEP-6-routing-models.md`
- **Phase 3 — limits and accounting** (steps 7–9): every request is counted, priced,
  limited and measured.
  7. `STEP-7-accounting.md`
  8. `STEP-8-limits.md`
  9. `STEP-9-metrics.md`
- **Phase 4 — lifecycle and delivery** (steps 10–11): clean shutdown, e2e, live-test
  kit.
  10. `STEP-10-draining.md`
  11. `STEP-11-image-e2e.md`

## End-to-end verification

1. `go test -race ./...`, `go vet`, staticcheck, `npm test`, `npm run lint` all green.
2. The e2e test (step 11) runs the built binary against the fake backend: auth reject,
   streamed and non-streamed completions, embeddings, `/v1/models` filtered per key,
   a per-minute `429` with headers, a USD/month limit tripping, the snapshot surviving a
   restart, SIGHUP reload with a bad config kept out, SIGTERM drain finishing an
   in-flight stream, `/metrics` reflecting the traffic.
3. Manual, with the user, when access is available (not blocking — run with the
   step 11 live-test kit, `docs/testing/LIVE-BACKENDS.md`): the built binary against a
   real vLLM host and an Azure deployment, streaming chat through an OpenAI client
   library, `/metrics` checked.

**Status (2026-09-24):** automated verification complete — items 1 and 2 green
(`scripts/check-gateway.sh`, which also runs the live-test kit's self-test against
the fake backend for all three kinds; `npm test`, `npm run lint`). Item 3 is pending
the user's vLLM and Azure access.
