# Step 5 — wrong-endpoint answers

**Status:** done — reviewed and committed 2026-10-09

## Intent

A request for an endpoint the self-hosted server's loaded model does not serve no
longer counts as the deployment failing. Two examples:
- chat sent to a reranker or embedding model on vLLM;
- rerank sent to a llama-server not started with `--reranking`.

Such a request answers `502 upstream_endpoint_missing`, is retried on other
deployments, and is neutral for the circuit, as step 1 settled (OVERVIEW decision 14).
The deployment keeps serving the endpoints it has.

## Files likely touched

- `gateway/internal/provider/`:
  - `vllm`: no core endpoints. Its route-missing answer reads as the endpoint missing
    everywhere.
  - `llama-server`: its `501`s as step 1 decided each one.
  - `send.go`: the `501` reading, which today looks only at `404` and `405`.
  - Whatever helper only `vllm` used for core endpoints becomes unused and is removed.
- `gateway/internal/server/`:
  - the endpoint memory and the per-request refusal held per deployment, if step 1
    confirmed it (`endpointmemory.go`, `attempts.go`).
- Tests beside each change:
  - module tests: each signature on each endpoint;
  - server tests: memory per deployment; a newer server found again after the
    interval.
- `gateway/e2e/`:
  - a chat request to a reranker deployment answering vLLM's route-missing shape is
    neutral, and the same deployment serves rerank right after with its circuit
    closed;
  - a `vllm` backend with a wrong `base_url` gives the endpoint-missing answer and
    warning, and its circuit does not open.

## Decisions made during planning

- **No new mechanism.** The existing endpoint-missing outcome, its warning and its
  memory carry the fix. Nothing new probes the backend to tell a wrong `base_url` from
  a model without the endpoint (AGENTS.md → Debugging, no stacked safety nets).
- **The `openai-compatible` reading stays as it is** (decision 14).

## Acceptance criteria

- The tests above pass.
- No test that asserted `upstream_path_missing` for a `vllm` backend is deleted
  silently. Each one is either rewritten to the settled rule, with the rule cited, or
  kept where the rule still holds.
- **Phase 2 end:** `scripts/check-all.sh` green and recorded.

## Result

### What changed

- `gateway/internal/provider/`:
  - `vllm.go`: no core endpoints (`core` unset). vLLM's route-missing answers — the
    `404 {"detail":"Not Found"}` and the `405 {"detail":"Method Not Allowed"}` — read
    as the endpoint missing on every endpoint, chat, completions and embeddings
    included.
  - `llama_server.go`: `modeMissing(endpoint)` — on embeddings and rerank, a `501`
    whose `type` is `not_supported_error` is the endpoint missing (message not
    read); on any other endpoint it returns nil, so a `501` stays a backend `5xx`.
    `File Not Found` keeps its reading: wrong path on OpenAI's three, endpoint
    missing elsewhere.
  - `send.go`: `wireCall.modeMissing` (unset: a `501` is the backend's `5xx`);
    `deploymentFailure` reads a `501` only when the module set it; one
    `endpointMissing` builder for the `404`/`405` and the `501`. Its error now reads
    `backend <id> answered <status>: its server does not serve <url>` (was `… has no
    <path> endpoint (an older version?)`).
  - `notfound.go`: `readNotFound` → `peekAnswer`, `maxNotFoundBody` →
    `maxPeekedAnswer` — the reader now serves a `501` too; the file header says
    "an error answer".
  - `openai_rules.go`, `provider.go`: comments to the settled rule (`openAICore`
    serves openai, azure-openai, llama-server and openai-compatible;
    `CodeEndpointMissing` names every cause).
- `gateway/internal/server/`:
  - `endpointmemory.go`: keyed by `routing.DeploymentID` and endpoint (was backend
    and endpoint); `remember(config.Deployment, …)`; `exclude` leaves out
    deployments, keeping "every deployment remembered → all tried again";
    `Retain(models)` forgets every deployment the applied config no longer has.
  - `attempts.go`: remembers the attempt's deployment; the warning `the deployment's
    server does not serve an endpoint its type serves`, once per interval and
    deployment, with `kaiak.request.id`, `kaiak.backend.id`,
    `kaiak.deployment.model`, `kaiak.endpoint`; `AttemptEndpointMissing` left
    `attemptRules`, so a retry refuses only the deployment tried, and the model's
    other deployments, on the same backend too, stay open to it; comments.
  - `api.go`: comment.
- `gateway/internal/metrics/ops.go`: the `endpoint_missing` comment.
- `gateway/cmd/kaiak/main.go`: `missingEndpoints.Retain(applied.Models)`.
- `gateway/internal/fakebackend/fakebackend.go`: `SetNoRoute(body, endpoints...)` —
  the 404 body for a path the backend has no route for (vLLM's `{"detail":"Not
  Found"}`), and the endpoints it has no route for (a vLLM reranker: no chat route).

### Removed

- `vllm`'s use of `openAICore` (the helper stays for the other four OpenAI-format
  types, which still call it).
- The `AttemptEndpointMissing: {backendWide: true}` rule.
- The old warning and error texts; the names `readNotFound` and `maxNotFoundBody`.
- Tests replaced, as listed under "Tests rewritten from the old rule"; the test
  helper `rememberedBackends` (now `rememberedDeployments`).

### Design choices

- **`modeMissing` is a reader the module sets per endpoint**, like `missingModel` and
  `unknownPath`. The core reads a `501`'s body only where a module says it can mean
  the endpoint missing. Every other `501` passes unread, as before.
- **The endpoint-missing error names the URL.** On `vllm`, a wrong `base_url` now
  shows only as the endpoint missing. The URL in `kaiak.upstream.error.message`
  makes that mistake readable from the request line.
- **The memory key is `routing.DeploymentID`,** the circuits' identity. Two public
  models sharing a deployment share its entry, as they share its circuit.
- **`Retain` works per deployment:** an entry for a removed deployment whose backend
  stays would otherwise outlive it until the backend left.
- **The fake's no-route answer belongs to the backend**, set like `SetRerankShape`.
  It covers both e2e cases: a vLLM reranker without a chat route, and a wrong
  `base_url` reaching vLLM. `cmd/fakebackend` has no flag for it: nothing runs it
  through the process yet. Step 6's live-kit self-test may need one.
- **"Found again after the interval"** runs with the minimum probe interval (100 ms)
  and a 150 ms sleep. The test only needs time to pass, so load cannot make it
  flaky. The exclusion tests use a 1 h interval.

### Spec points (no spec edit)

- No contradiction found between the spec and the code.
- The spec does not say when an entry is forgotten on config apply. This is an
  implementation detail, settled above.
- STEP-6 lists "the wrong-path check updated for `vllm` (step 5)", but the live kit
  has no wrong-path check. `scripts/live` has no `path_missing`, `endpoint_missing`
  or `404` check, and its self-test does not touch this rule. Nothing changed there.
  Step 6's new wrong-endpoint check is the first one.
- Left for step 6, as STEP-1 noted: `docs/DEPLOYMENT.md` :390 and :935 still say an
  endpoint missing means an older server ("upgrade it").
- `gateway/e2e/messages_test.go` and `responses_test.go` describe their scenario as
  "a server lacking the endpoint". That still holds: an older vLLM without the
  endpoint.

### Tests rewritten from the old rule

Each now cites `GATEWAY.md` → Providers → Wrong path to a host (`vllm`: none) and An
endpoint missing from a server (`vllm` has no core endpoints; remembered per
deployment), and → Routing and reliability: retries.

1. `provider/endpoints_test.go` `TestEndpointMissingFromAServer` →
   `TestWrongEndpointAnswersByModule`:
   - its rows (vllm, llama-server, openai, azure-openai) are kept and run on every
     endpoint each type serves;
   - its "a 405 on chat is relayed" check, made on `vllm`, now expects the endpoint
     missing: `vllm` has no core endpoints;
   - the rule "a 405 on a core endpoint is relayed" is kept, checked on llama-server
     and openai-compatible.
2. `provider/unknownpath_test.go` `TestUnknownPathByModule`: `vllm`'s `{"detail":"Not
   Found"}` on chat is now `upstream_endpoint_missing`. Every other module is
   unchanged; `openai-compatible` still reads that body as a wrong path, since its
   server is unknown.
3. `server/pathmissing_test.go`: the `vllm` row left `pathModules`. It asserted
   `upstream_path_missing` and an open circuit in `TestUnknownPathIsTheDeploymentsFailure`.
   It became `TestVLLMRouteMissingIsTheEndpointMissing`:
   - retried on the other deployment;
   - the circuit stays closed even with `failure_threshold` 1;
   - `endpoint_missing` in the retry and attempt metrics;
   - with no deployment left, `502 upstream_endpoint_missing` without the backend's
     text, and no usage.

   The other four types' rows stand: their rule holds.
4. `server/retry_test.go` `TestAttemptRules`: "endpoint missing" is no longer
   backend-wide.
5. `server/endpoints_test.go` `TestRefusalAndEndpointMissingClassification`:
   `avoidAfter` refuses the tried deployment alone, not its backend's siblings.
6. `server/endpointmemory_test.go`: `TestAppliedConfigForgetsARemovedBackendsMissingEndpoints`
   became `TestAppliedConfigForgetsARemovedDeploymentsMissingEndpoints`. It now also
   checks a removed deployment whose backend stays.
7. `e2e/messages_test.go`: the pinned warning text is now the new one, with the
   deployment's model and the endpoint.
8. `e2e/backendtypes_test.go` `TestWrongBaseURLIsWarnedAtApply` became
   `TestWrongBaseURLOnVLLM`. Its assertions are kept, and it is extended (below).

No test asserts `upstream_path_missing` for a `vllm` backend any more.

### New tests

- **Provider:** `TestWrongEndpointAnswersByModule` (68 cases), covering:
  - vllm's `404` and `405` → endpoint missing on all 7 of its endpoints; a `501` on
    vllm is relayed;
  - llama-server `File Not Found` → path missing on its three core endpoints,
    endpoint missing on the other five;
  - llama-server `501 not_supported_error` → endpoint missing on embeddings and
    rerank, relayed on the other six;
  - llama-server `501` of type `server_error`, and a `405`, → relayed everywhere;
  - openai-compatible `501` and `405` → relayed;
  - openai and azure-openai unknown path → path missing on their core endpoints,
    endpoint missing on their Responses endpoints.
- **Server** (`endpointmemory_test.go`, on router-mode llama-server backends):
  - `TestMissingEndpointIsRememberedPerDeployment`: a chat model's `501` on rerank
    keeps the reranker beside it on the same backend in rerank routing. The warning
    is logged once, at WARN, with the request, backend, deployment model and
    endpoint. Chat keeps working and the circuit stays closed.
  - `TestMissingEndpointRetriesOnASiblingAndIsLeftOut`: an embeddings `501` is
    retried on the sibling variant on the same backend, which then takes all
    requests for the interval.
  - `TestMissingEndpointIsFoundAgainAfterTheInterval`.
  - `TestEveryDeploymentRememberedIsTriedAgain`: both variants are tried on every
    request; 2 warnings over 3 requests; circuits closed.
- **E2E:**
  - `TestRerank/vllm/chat to the reranker`: the vl fake has no route for the
    generating and embedding endpoints. 6 chat requests each answer
    `502 upstream_endpoint_missing` without the backend's text. The circuit is
    closed, `endpoint_missing` attempts = 6, the warning at WARN names the
    deployment and `chat_completions`, and rerank then answers `200`.
  - `TestWrongBaseURLOnVLLM`, after the config-apply warning: 6 chat requests to the
    `vllm` backend whose `base_url` lacks `/v1` each answer
    `502 upstream_endpoint_missing`. The circuit is closed, `endpoint_missing`
    attempts = 6, and one warning names the first request.
- **Mutation checks** (each reverted afterwards):
  - giving `vllm` OpenAI's core endpoints again fails the provider table (6 cases)
    and both e2e tests;
  - a llama-server `501` reader that never matches fails 2 cases;
  - keying the memory by backend fails all 5 memory tests;
  - restoring the backend-wide retry refusal fails 3 memory tests, `TestAttemptRules`
    and `TestRefusalAndEndpointMissingClassification`.

### Suite (2026-10-09, end of phase 2)

- `scripts/check-gateway.sh`: gofmt, vet, staticcheck, telemetry boundary pass.
  `go test -race -count=1`: every package `ok` (`e2e` 119.4 s, `internal/server`
  13.6 s, `internal/provider` 2.3 s). The live kit's lint passes, and its self-test
  reports "self-test passed for vllm, llama-server, openai, azure-openai, anthropic,
  azure-anthropic, vllm with two backends" and "gateway checks passed".
- `scripts/check-all.sh`, run 1 (3 min 30 s): gateway checks passed (`e2e` 122.7 s).
  Control `npm test`: tests 630, pass 629, fail 0, skipped 1. `npm run lint`:
  "boundaries ok". Cross-half e2e: `ok kaiak/e2e 70.519s`. "all checks passed".
- `scripts/check-all.sh`, run 2 (3 min 27 s): gateway checks passed (`e2e` 119.7 s).
  Control: tests 630, pass 629, fail 0, skipped 1. "boundaries ok". Cross-half:
  `ok kaiak/e2e 70.934s`. "all checks passed".
