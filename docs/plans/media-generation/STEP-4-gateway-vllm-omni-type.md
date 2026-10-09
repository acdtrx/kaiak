# Step 4 — gateway: the `vllm-omni` backend type

**Status:** done — reviewed and committed 2026-10-09

## Intent

The `vllm-omni` backend type exists: the config accepts it, its module builds URLs and
credentials, and its probe reads its models list. It claims the media endpoints only
(OVERVIEW decision 8), which steps 5–8 add, so until then it serves nothing reachable.
A request for any other endpoint to a model whose deployments are all on `vllm-omni` is
kaiak's `400 endpoint_not_served`. Its three media endpoints are core endpoints (a
route-missing `404` is a wrong `base_url`), and its wrong-endpoint and wrong-model
readings follow step 1's spec (the image route's `503 No diffusion stage…` is the
endpoint missing; image generation's `400 Model mismatch` is a missing model).

## Files likely touched

- `gateway/internal/provider/vllm_omni.go` (new), `provider.go` (the `kinds` map, its
  GenAI provider name), and the endpoint-support table test.
- `gateway/internal/config/` — the type.
- The fake backend: plays a `vllm-omni` server (its route-missing and model-mismatch
  answers).
- Tests: the module's URLs, credential, probe, and the wrong-endpoint and wrong-model
  signatures on each endpoint.

## Decisions made during planning

- **Share by purpose, not by shape** (CODING-RULES §2). `vllm-omni` is vLLM's server
  with more routes, so the module may share vLLM's signatures where they are the same
  server code. Where vLLM-Omni answers differently (step 1), it has its own.

## Acceptance criteria

- **Module tests:** each signature on each endpoint.
- **A chat, completions, embeddings, Messages, Responses or rerank request** to a model
  whose deployments are all on `vllm-omni` answers `400 endpoint_not_served`, before
  any backend.
- **Phase 1 end:** `scripts/check-all.sh` green and recorded.

## Result

### What changed

- `gateway/internal/config/`: `BackendVLLMOmni` (`"vllm-omni"`) in `snapshot.go`, and
  in the schema walker's type enum (`schema.go`), in the schema's order.
- `gateway/internal/provider/vllm_omni.go` (new) — the module:
  - URL `base_url` + `/` + endpoint path, credential as a bearer token (none
    without `api_key_env`), probe `GET <base_url>/models` read as an OpenAI list
    (version-path hint on a `404`), as `vllm`;
  - `vLLMOmniEndpoints` — the media endpoints alone, **empty**: no media endpoint
    is a client endpoint yet, so every request to a model whose deployments are all
    `vllm-omni` is `400 endpoint_not_served`;
  - `Send`: `core: true` on every endpoint it is sent; missing model by the
    self-hosted rule (`missingModelNamedOrCoded("model_not_found")`, vLLM's model
    check, which speech and the batch run); unknown path `{"detail": "Not Found"}`
    (FastAPI's) → `upstream_path_missing`; no 501 reading (a 405 and a 501 are
    relayed);
  - `imageModelMismatch` (a `400` whose message begins `Model mismatch: request
    specifies '<deployment model>'`) and `noDiffusionStage` (a message equal to `No
    diffusion stage found in multi-stage pipeline.`) — the image route's readings,
    read from the OpenAI error shape, **not wired yet** (below);
  - no GenAI provider name (a self-hosted type: GATEWAY.md, request-line table).
- `provider.go`: the `kinds` entry. `notfound.go`: `fastAPIDetail(answer,
  texts...)`, FastAPI's `{"detail": …}` body, shared by purpose — vLLM-Omni is
  vLLM's FastAPI app; `vllm.go` now reads its `Not Found` / `Method Not Allowed`
  through it (no behaviour change). `send.go`: the module list in the core's comment.
- `docs/ARCHITECTURE.md`: `vllm-omni` in the provider module list (the rest of the
  doc's media update is step 10's).
- The fake backend: **unchanged** — its models list (`SetModels`) and FastAPI
  route-missing answer (`SetNoRoute`) already play a `vllm-omni` server for this
  step; speech and image answers come with steps 5–8.

### Design choices

- **The image readings are module functions tested directly, wired in step 8.**
  They are endpoint-specific (a `400` and a `503` on image generation only — the
  spec's safety argument for reading the `400`'s message holds on that route alone),
  and the wire core peeks only `404`, `405` and `501` today. Wiring them needs the
  `ImagesGenerations` endpoint and a core change — `deploymentFailure` reading a
  module's `400` as the model missing and its `503` as the endpoint missing — best
  designed and tested with the endpoint in hand.
- **`core: true` unconditionally**: every endpoint vllm-omni serves is core
  (GATEWAY.md, An endpoint missing from a server), and routing sends it nothing
  else.
- **Shared, not copied**: the missing-model rule is the core's self-hosted one; the
  FastAPI reader moved into the core (CODING-RULES §2) — the module does not call
  vLLM's (A type per server: modules self-contained over the core).

### Tests

- `provider/vllm_omni_test.go`:
  - `TestVLLMOmniURLCredentialAndProbe` — URL for the three media paths, the
    bearer credential with and without a key, the probe's one `GET /v1/models` with
    the credential and its served/unserved names, a `404` list's version-path hint;
  - `TestVLLMOmniAnswerReadings` — through `Send` on every endpoint (as a stand-in
    for the media endpoints): request path and credential; FastAPI's `Not Found` →
    `upstream_path_missing` naming the URL, no backend text; vLLM's model `404` in
    both shapes → `upstream_model_missing`; another model's, a caller's `404`,
    other servers' unknown-path answers, a `405`, a `501` → relayed whole
    (112 subtests);
  - `TestVLLMOmniImageReadings` — the `400` mismatch (the deployment's model first;
    a longer name, another model, the name only as the server's, vLLM's `<path>`
    sanitizing → not) and the `503` (exact message only).
- `provider/endpoints_test.go`: the endpoint-support test now fails on a spec
  column it does not know and on an `Endpoint` in no column; the media column is in
  `endpointColumns` with **no endpoints yet**, so the `vllm-omni` row is checked on
  every existing column (all `—`).
- `provider/provider_test.go`: `TestProviderNames` — `vllm-omni` has none.
- `server/endpoints_test.go`: `TestVLLMOmniServesNoOtherEndpoint` — every body
  endpoint `vllm-omni` does not serve (now all eight: chat, completions,
  embeddings, Messages and its count, Responses and its count, rerank) answers
  `400 endpoint_not_served` in its own error shape, naming the route, on the log
  line; nothing reaches the backend, no record; `/v1/models/omni-only` lists the
  endpoints the type serves (`[]` today).
- `metrics/metrics_test.go`: the media-units test's backend is now `vllm-omni`.
- Cleared: the 15 reds step 3 left (`internal/config` 13, `cmd/kaiak`
  `TestServingStatusCoversTheAppliedConfig`, `provider`
  `TestBackendTypeFixtures/vllm-omni`) and `TestKindsAreTheSchemaBackendTypes`,
  which the fixture test's panic had hidden.

### Left for steps 5–8

- Add each media `Endpoint` to `vLLMOmniEndpoints` and to the media column of
  `endpointColumns` (`provider/endpoints_test.go`); `TestEndpointSupportFollowsTheSpec`
  fails until it is in a column, and then checks the spec's `yes`/`—` for it.
- `TestVLLMOmniAnswerReadings` covers each new endpoint automatically (it loops over
  every `Endpoint`); its comment's "stand-in" wording goes once the module serves
  them. `TestVLLMOmniServesNoOtherEndpoint` skips what the type serves and checks
  the model entry's list.
- **Step 8:** wire `imageModelMismatch` (`400` → `upstream_model_missing`) and
  `noDiffusionStage` (`503` → `upstream_endpoint_missing`, neutral, remembered) for
  `ImagesGenerations` in `Send`, with the wire core reading those statuses for a
  module that sets them; extend the `CodeEndpointMissing` comment (it names
  llama-server's 501 only); add the image rows to `TestWrongEndpointAnswersByModule`
  or the vllm-omni tests.
- The fake backend's speech/image answers and their route-missing behaviour (step 5's
  "non-speech model" answer).

### Suite (2026-10-09)

**Phase 1 end: green.** `scripts/check-gateway.sh` once, then `scripts/check-all.sh`
three times (the first while a test's error-shape check was being tightened, so the
two after it are the record) — every run `all checks passed`, exit 0:

- **Gateway:** gofmt, vet (crosshalf tag included), staticcheck 2026.2.1 on go1.27.1,
  telemetry boundary pass; `go test -race -count=1 ./...` — all 20 packages `ok`,
  `e2e` included (≈119 s).
- **Live-test kit:** gofmt, vet, staticcheck pass; `-self-test` passed for vllm,
  llama-server, openai, azure-openai, anthropic, azure-anthropic, vllm with two
  backends (37/36/22/21/16/16/34 passed, 0 failed).
- **Control `npm test`:** 655 tests, 654 pass, 0 fail, 0 cancelled, 1 skipped.
  **`npm run lint`:** `boundaries ok`.
- **Cross-half e2e:** `ok kaiak/e2e` (≈70 s), `all checks passed`.
