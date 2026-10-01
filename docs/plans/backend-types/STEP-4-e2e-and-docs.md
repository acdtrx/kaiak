# Step 4 — e2e and docs

**Status:** done (2026-10-01)

## Intent

Close phase 1: the types work end to end through both halves, and everything an
operator reads names them.

## Files likely touched

- `gateway/e2e/`: a scenario routing to backends of several types through one
  gateway (the fake backend serves both URL layouts).
- `examples/*.json`: the self-hosted backends take their server's type.
- `docs/DEPLOYMENT.md`: choosing a type; which types force the standard tier (and
  that an OpenAI deployment kept as `openai-compatible` runs on the project's own
  tier).
- `README.md`: config snippets and any mention of the types.
- `docs/ARCHITECTURE.md` and `docs/architecture/`: the provider modules, if they list
  them.
- `docs/testing/LIVE-BACKENDS.md`: `-kind llama-server`.
- A repo-wide grep for prose saying `openai-compatible` covers vLLM, llama-server or
  OpenAI.

## Decisions made during planning

- None beyond the overview.

## Acceptance criteria

- The e2e scenario passes: requests to an `openai`, a `vllm`, a `llama-server` and
  an `azure-openai` backend each succeed, and only the `openai` and `azure-openai`
  requests reach the backend with the forced tier.
- Both example configs validate in both halves.
- The grep finds nothing stale.
- **Phase end:** `scripts/check-all.sh` green (3× in a row, as before a merge), output
  recorded.

## Result

- **e2e.** New `gateway/e2e/backendtypes_test.go`, `TestBackendTypes`: one gateway,
  four backends on one fake backend — `openai` and `vllm` and `llama-server` at
  `<fake>/v1`, `azure-openai` at the fake's root — each with a public model of its
  own and a distinct backend-side model name. Per type, a chat without a tier and a
  chat with `"priority"`: each answers `200` and settles on its backend (log line);
  the fake recorded it at the module's path (`/v1/…`, `/openai/v1/…` for Azure), for
  the deployed model, with the module's credential (`Authorization: Bearer` for
  `openai`, `Api-Key` for `azure-openai`, none for `vllm`, `llama-server`); `openai`
  and `azure-openai` sent `"service_tier":"default"` both times, `vllm` and
  `llama-server` sent none, then the client's `"priority"` untouched. Mutation check
  (reverted): the tier edit moved from `openai.go` to `vllm.go` fails the four
  `openai` and `vllm` cases.
- **Examples.** `examples/config.json`: `vllm-gpu-1`, `vllm-gpu-2`, `vllm-embed` →
  `vllm`. `examples/local-config.json` keeps `openai-compatible`: its backend is the
  fake backend, not one of the typed servers. Both validate in both halves
  (`TestExampleConfigs` run with `-count=1`; kaiak-control's `example configs`).
- **DEPLOYMENT.md.** Config for many hosts opens with **A backend's `type` is its
  server's**: the five types, choosing one, `openai`/`azure-openai` require
  `api_key_env` and force the standard tier, the other three pass the client's tier
  — an OpenAI deployment kept as `openai-compatible` runs on the tier the client
  asks for, or the project's own — and `verify` notes a vLLM or llama-server answer
  under another type. Upgrades: a type a gateway does not know rejects the config
  there (format version unchanged) — upgrade gateways before publishing it.
- **README.md.** The quick start's config step names the types (and the example's
  embeddings host as vLLM); the documentation map's live-test row names
  llama-server.
- **Architecture.** `ARCHITECTURE.md`: `provider` lists the five modules; the live
  kit names llama-server. `architecture/gateway.html`: the package table and the
  extension table list the five modules; the backends box in the request-flow
  diagram reads "OpenAI format · a module per server" (it read "OpenAI-compatible ·
  vLLM · SGLang · Azure"); llama-server specifics "in its module".
- **LIVE-BACKENDS.md.** A `llama-server` command section (`base-url` with `/v1`, the
  listed id or `--alias`, no key unless `--api-key`, embeddings on their own server);
  the generated backend has the type `-kind` names; self-test "all four kinds";
  prerequisites, the output-ceiling row and the `404` reading cover llama-server.
- **Live kit.** `twobackends.go`'s failover prompt says "Ctrl-C its server process"
  (it said vLLM whatever the kind).

Decisions made during the step:

- The e2e scenario uses one fake backend for all four backends, telling requests
  apart by backend-side model name, as the step file says; it sends chat only — the
  per-endpoint tier rules are the provider table's (`TestServiceTierByModule`).
- `DEPLOYMENT.md` gains the Upgrades bullet on an unknown type (overview decision 4's
  consequence for operators); no other upgrade note, the release notes carry the
  tier change.

Grep (2026-10-01), excluding `docs/reviews/`, `docs/plans/`, fixtures and tests:
`openai-compatible`, `OpenAI-compatible`, `azure-openai`, `Azure OpenAI`,
`service_tier`/standard tier, and two-type lists (`two types`, `both types`,
`openai-compatible (or|and) azure`, `-kind vllm|openai|azure-openai`). Stale and
fixed: `docs/kaiak.md` (v1 scope: "OpenAI-compatible (vLLM, llama-server, SGLang,
OpenAI)"), `docs/ARCHITECTURE.md` and `architecture/gateway.html` (two-module
lists), `docs/BACKLOG.md` (llama-server specifics "works today as a plain
OpenAI-compatible backend"), `AGENTS.md` commands line (`-kind` list without
llama-server; `verify` without `--type`), README's live-test row,
`LIVE-BACKENDS.md` (three kinds). Left as they are, being current: the wire-format
sense of "OpenAI-compatible" (fake backend, Azure's `/openai/v1/` API, passthrough
rules), the kit's embeddings server as `openai-compatible` (step 3's decision), the
Azure section's standard-processing bullet (Azure only).

Suite — `scripts/check-all.sh`, 2026-10-01, 3× in a row, all **exit 0, "all checks
passed"** (runs 2 and 3 after `go clean -testcache`, so every package ran):

1. gofmt, vet, staticcheck clean (gateway, kit); `go test -race ./...` every package
   ok (`e2e` 90.5 s); kit self-test passed for vllm, llama-server, openai,
   azure-openai (13 passed each) and vllm with two backends (16 passed); control
   `npm test` 565 passed, 0 failed; `npm run lint` clean, boundaries ok; cross-half
   e2e ok (49.0 s).
2. As run 1; every gateway package run fresh (`e2e` 98.6 s, `internal/control`
   17.7 s); 565 passed; cross-half e2e ok (43.9 s).
3. As run 2 (`e2e` 95.1 s); 565 passed; cross-half e2e ok (43.9 s).
