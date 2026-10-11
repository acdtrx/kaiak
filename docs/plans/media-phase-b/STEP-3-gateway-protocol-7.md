# Step 3 — the gateway at protocol 7

**Status:** not started

## Intent

Move the gateway to protocol 7 and take out what the new rules make unnecessary:
- the version and the two config rules;
- routing and refusals that no longer filter deployments by type;
- the endpoint memory;
- the endpoint-missing class: a route-missing answer on a listed endpoint becomes one
  reading on every type, `502 upstream_path_missing` at once, neutral and not retried
  (OVERVIEW decision 22).

## Files likely touched

- `gateway/internal/config/`:
  - `schema.go`: `protocol_version` in place of `format_version`; `endpoints`
    required; `FormatVersion` removed;
  - one version constant for the whole gateway, read by the config check and the
    control client (`control.ProtocolVersion` today) — its home is this step's call,
    without an import cycle;
  - `semantic.go`: the one-type rule; `endpoint-not-served` against the model's type;
  - `snapshot.go`: `Model.Admits` reads the list alone; the model's type if routing
    or the model lists need it.
- `gateway/internal/control/`: the version constant (as above).
- `gateway/internal/server/`:
  - `pipeline.go`: `findServingDeployments` becomes the list check alone;
  - `attempts.go`: the per-endpoint view of deployments (`servingDeployments`,
    `serves`) and the memory's `exclude` and `remember` removed; the model's
    deployments route as they are;
  - `endpointmemory.go` and its tests removed;
  - `models.go`: model entries show the list;
  - `errors.go`: `errEndpointNotServed` removed; the not-listed refusal stays;
  - `metrics.go`, the request line, `errorAnswers`: `upstream_endpoint_missing` gone;
  - tests: every test config builds models with a list (`server_test.go`'s model
    helper, `withAnthropicModels`, `withSpeechModels`, `withEndpointLists`); tests of
    removed behaviour deleted, not adapted into something else.
- `gateway/internal/provider/`:
  - `notfound.go`, `send.go`: one route-missing reading per type, whatever the
    endpoint; core endpoints and `vllm`'s no-core rule removed;
    `CodeEndpointMissing` removed;
  - `llama_server.go`, `vllm_omni.go`: the `501` and image `503` readings removed
    (plain `5xx` from now on);
  - `provider.go`: what the core-endpoint notion leaves behind.
- `gateway/internal/server/` and `gateway/internal/routing/`: the wrong path's outcome
  class (neutral) and retry rule (none — the rule refusing every deployment on that
  backend for the request goes).
- `gateway/cmd/kaiak/`: the memory's wiring (`Retain` on config apply) removed; tests'
  configs.
- `gateway/e2e/`: every config; tests of the endpoint memory or the endpoint-missing
  reading replaced by the one "not found" reading where a behaviour remains.
- `gateway/internal/fakebackend/`: its route-missing answers stay.
- `scripts/live/`: the configs it writes (`protocol_version: 7`, a list per model); its
  wrong-endpoint checks, now refusals by the list or the "not found" reading.

## Decisions made during planning

- **The "not found" answer errors out** (decision 22, settled with the user): the
  client gets `502 upstream_path_missing` at once; nothing is retried and the circuit
  does not count it; its message names both causes — a wrong `base_url`, or a list
  naming an endpoint the server does not serve. The backend's text is not relayed, as
  today.
- **Removal discipline:** removed code is gone — no renamed
  leftovers, no compatibility shims; a grep checklist recorded in the result
  (`EndpointMissing`, `endpoint_missing`, `MissingEndpoints`, `coreEndpoints`,
  `FormatVersion`, `format_version`, `servingDeployments`, `errEndpointNotServed`).

## Acceptance criteria

- **Component tests:**
  - the config rules and the version (missing list, two types, an unserved entry,
    version 6, a `format_version` member) against the shared fixtures;
  - an unlisted endpoint refused before limits, with the one message, on every body
    route (the route-by-route test from the phase A review, kept);
  - a route-missing answer on each type — `vllm`, `llama-server`, `vllm-omni`, the
    OpenAI and Anthropic types — answered `502 upstream_path_missing` at once, not
    retried, not counted by the circuit;
  - llama-server's `501` and vLLM-Omni's image `503` read as plain `5xx`, as any
    server error.
- **Gateway e2e and the cross-half e2e** green at protocol 7.
- `scripts/check-all.sh` green — **phase 1 ends green.**
