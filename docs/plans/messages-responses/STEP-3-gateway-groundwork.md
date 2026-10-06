# Step 3 — gateway groundwork

**Status:** done (2026-10-06)

## Intent

Prepare the gateway for more client APIs without adding one yet:
- defaults removed
- config format 5 and protocol 5
- a seam where each client API format plugs its pieces in, with the OpenAI format
  moved behind it unchanged
- endpoint support per backend type, and routing that respects it
- `x-api-key`
- the two Anthropic backend modules

At the end the suite is green, and nothing a client sees has changed except
`defaults` leaving `props` and `endpoints` joining model entries.

## Files likely touched

- `gateway/internal/config/`:
  - schema, document, snapshot: the new types; `defaults` gone with
    `refusedDefaults`
  - format 5
  - `api_key_env` required for the new types
- `gateway/internal/server/params.go`: the defaults loop goes. The output limit stays,
  its keys now coming from the endpoint's format.
- `gateway/internal/server/models.go`: `props` without `defaults`; `endpoints` on
  entries.
- `gateway/internal/server/pipeline.go`, `inbound.go`, `errors.go`, `api.go`.
  - The endpoint table gains each endpoint's format.
  - Each format owns its pieces:
    - owned-field parsing
    - output-limit keys
    - the error writer
    - the input estimate entry
    - usage reading
    - completeness
    - model rewriting
  - These live where those concerns live today (inbound, accounting, provider), split
    per format by purpose (CODING-RULES §2), not one mega-switch.
- `gateway/internal/provider/`: an endpoint-support declaration per module; new
  `anthropic.go` and `azure_anthropic.go` modules:
  - URL layouts
  - credential headers and `anthropic-version`
  - `service_tier: "standard_only"`
  - price-option refusals as a caller error the pipeline answers with `400`
  - the 529 mapping
  - wrong-model and wrong-path signatures
  - probe: models list for `anthropic`, always-pass for `azure-anthropic`
  - the config-time model check skipped for `azure-anthropic`
- `gateway/internal/routing/`: only deployments whose backend serves the endpoint are
  eligible. A model with none answers `400 endpoint_not_served` before queueing.
- `gateway/internal/auth/`: `x-api-key` when `Authorization` is absent.
- `gateway/internal/control/`: protocol 5.
- `gateway/internal/state/`: the `last-known-good.json` format.
- `gateway/internal/fakebackend/`: answers as an Anthropic-type server where step 3's
  tests need it (models list, error shapes).
- The live kit's config generation: no `chatDefaults`.

## Decisions made during planning

- **The seam is internal.** No new exported package unless a boundary needs it.
  Packages stay acyclic, compiler-enforced.
- **The Anthropic modules carry their own wire behaviour** over the shared core where
  the core is format-neutral (sending, framing, timeouts). Where the core assumes the
  OpenAI format (usage chunk stripping, `[DONE]`), that moves into the OpenAI format's
  pieces.
- **The price-option refusal happens in the module, before sending.** It is not a
  backend failure, is never retried, and does not touch the circuit. The provider
  interface gains a way to return a caller error, documented on the interface.
- **`upstream_endpoint_missing`** (OVERVIEW decision 14) is built here. It applies to
  endpoints a type claims beyond OpenAI's three, so in this step it covers the
  Anthropic modules' Messages path. Steps 4–5 extend it.

## Acceptance criteria

- **Existing behaviour unchanged:** every existing test passes unchanged, except tests
  of defaults, which are removed, and tests whose fixtures moved to format 5 /
  protocol 5.
- **New tests:**
  - routing by endpoint support
  - `endpoint_not_served`
  - `x-api-key` auth, including both headers present
  - the Anthropic modules: URLs, headers, `standard_only`, each price-option refusal,
    529 as `5xx`, wrong model, wrong path, probes, the skipped model check
  - `endpoints` on model entries
- `scripts/check-gateway.sh` green.
- `scripts/check-all.sh` green: **phase 1 ends here**, committed.

## Result

**What changed**

- **Config** (`gateway/internal/config/`): format 5; types `anthropic` and
  `azure-anthropic`; `api_key_env` required for the four cloud types
  (`keyedBackendTypes`); `defaults` gone from the document, the snapshot and the
  schema, together with `refusedDefaults` and the parameter-name pattern.
- **Versions:** protocol 5 (`control.ProtocolVersion`, the fake control plane);
  `last-known-good.json` format 6. Inline test configs in `cmd/kaiak`, `e2e`, `auth`,
  `config`, `limits`, `server` and the live kit moved to format 5.
- **Provider** (`gateway/internal/provider/`):
  - `Endpoint` gains `Messages`, `MessagesCountTokens`, `Responses` and
    `ResponsesInputTokens`, each with its path, plus a `Format` (OpenAI, Messages,
    Responses).
  - **Endpoint support:** one table, `kinds`, the one place a type is named. Each type
    has the endpoints it serves (each module file declares its list), whether it has a
    models list, and its constructor. `Serves(type, endpoint)` and `ListsModels(type)`
    are exported.
  - **Wire core:**
    - `wireCall.core` marks a type's core endpoints (OpenAI's three, or `messages`).
      A wrong-path answer there stays `upstream_path_missing`; on any other endpoint
      it is the new `upstream_endpoint_missing`.
    - Beyond the core endpoints a 405 is read too. vLLM's `unknownPath` also reads
      `{"detail":"Method Not Allowed"}`.
    - `include_usage` is set only on OpenAI-format streams.
  - **`RefusalError`** (code, param, message) is a provider's refusal before sending.
  - **New modules `anthropic.go` and `azure_anthropic.go`:**
    - URL layouts, credential headers and `anthropic-version: 2023-06-01`.
    - `standard_only` on `anthropic` Messages requests.
    - Wrong-model and wrong-path readings.
    - Probe: the paged models list (`?limit=1000`) for `anthropic`; always-pass with
      no request for `azure-anthropic`.
  - **`anthropic_price.go`** holds the shared price-option check (`speed`,
    `inference_geo`, every `cache_control.ttl: "1h"` in the four places the spec
    names), with exact keys and parameter paths in `param`.
- **Routing:** the config-time model check skips types without a models list, with the
  info line `model check not available for this backend type`.
  `NewModelChecker` takes `provider.ListsModels`.
- **Server** (`gateway/internal/server/`):
  - Defaults removed from `params.go`; the output limit is the one parameter set.
  - Model entries gain `endpoints`; `props` drops `defaults`.
  - `bodyEndpoints` drives routes and `takesBody`.
  - Inbound parsing dispatches by the endpoint's format, with the OpenAI parser moved
    unchanged into `parseOpenAIFields`.
  - **Routing by endpoint support** (`servingDeployments`): the model itself when
    every deployment serves the endpoint, a filtered copy when some do, and
    `400 endpoint_not_served` before routing when none does. Routing itself is
    unchanged, and its dispatcher cache is keyed by model pointer, so a filtered copy
    stays correct.
  - **The refusal** is answered `400` with the provider's code and param, never
    retried, neutral for the circuit, attempt outcome `client_error`, meter refused.
  - **`upstream_endpoint_missing`:**
    - `502`, retry reason `endpoint_missing`.
    - Every deployment on that backend is refused for the request.
    - Neutral for the circuit, attempt outcome `endpoint_missing`, meter refused.
    - A warning line names the backend, deployment and endpoint.
  - **Errors and metrics:** the five new codes have their classes.
    `metrics.AttemptEndpointMissing` and the retry reason exist at 0 from the start.
  - `gen_ai.provider.name` is `anthropic` for both Anthropic types.
- **Auth:** `Authenticate` takes the `x-api-key` value as well. `Authorization` comes
  first; the missing-key message names both headers.
- **Live kit:** `-chat-defaults` is replaced by `-chat-params`. The kit adds the
  object to every chat request it sends, since the gateway no longer sets defaults.
  The runbook's mentions are renamed.
- **Spec fixes** (`docs/specs/GATEWAY.md`):
  - fourteen attempt-outcome series, not thirteen
  - `client_error` also covers a provider's refusal before sending
  - `messages/count_tokens` is neither refused nor edited by the Anthropic modules
    (nothing is billed there)
- **Tests:**
  - **Provider:**
    - both Anthropic types on the wire: path, headers, model, tier per endpoint
    - each price-option refusal and its param, nothing sent, count_tokens not
      refused, standard values passing, exact keys
    - self-hosted types passing the options untouched
    - 404 readings: missing model, wrong path on messages, endpoint missing on
      count_tokens; 529 relayed
    - probes
    - endpoint support checked row by row against the spec's table
    - endpoint missing for vLLM (404 and 405), llama-server, OpenAI, Azure; a 405 on
      chat relayed
    - no usage edit outside the OpenAI format
  - **Server:**
    - `endpoint_not_served` (nothing sent, no record, logged)
    - mixed-model routing to the serving deployments only
    - `endpoints` on entries
    - `x-api-key`, alone and with `Authorization`
    - refusal and endpoint-missing classification, retry and `avoidAfter`
  - **Routing:** the model check skip.
  - The defaults test is rewritten as `TestOutputLimitIsTheOneParameterSet`;
    `TestModelEntryAndProps` follows the new shapes.

**Decisions made in this step** (please review)

1. **The per-format pieces of the response side are not split yet.** These are the
   meter's usage reading, stream completeness, and nested model rewriting. They stay
   OpenAI-only until steps 4–5 add the second and third format, so no interface with
   one implementation lands ahead of its need.
   - The provider's `Endpoint`/`Format`, the per-format body edits and the inbound
     dispatch are in place.
   - Nothing routes a Messages or Responses request yet.
   - The inbound dispatch panics on a format with no parser, which no route reaches.
   - The Anthropic modules' Messages **streams** get correct completeness and
     `message_start` rewriting only in step 4. Their non-stream answers are complete
     today.
2. **The price-option refusal and `standard_only` apply to `messages` only, not
   `messages/count_tokens`.** The spec now says so.
3. **A refusal's attempt outcome is `client_error`.** The spec's outcome list says so.
4. **A filtered model copy per request, rather than a change to routing,** for mixed
   models. It allocates only when a model's deployments differ in endpoint support.
5. **The fake backend needed no Anthropic shapes in this step.** The provider tests
   use a small recording server (`anthropic_test.go`); steps 4–5 teach the fake
   backend both formats.

**Flagged for step 7:** model defaults are still described in `docs/DEPLOYMENT.md`
(`:399`, `:442`) and `docs/architecture/gateway.html` (`:330`). `docs/DEPLOYMENT.md:763`'s
"Wrong model, path or credential" alert could list `endpoint_missing`. No
spec/code disagreement found beyond the three spec fixes above.

**Suite** (2026-10-06): `scripts/check-gateway.sh` passed: gofmt, vet, staticcheck,
`go test -race` (e2e included), and the live kit's lint and self-test (vllm,
llama-server, openai, azure-openai, vllm with two backends).
`scripts/check-all.sh` passed: gateway checks, control `npm test` (fail 0), `npm run
lint`, and the cross-half e2e (`ok kaiak/e2e 48.858s`), ending "all checks passed".
**Phase 1 ends green.**
