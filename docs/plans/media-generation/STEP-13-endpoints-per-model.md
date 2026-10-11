# Step 13 — endpoints per model

**Status:** not started

## Intent

A model can declare which client API endpoints it serves, opt-in: an optional
`endpoints` list on the model in config. Absent, the model serves every endpoint its
deployments' backend types serve, as today. Present, the gateway refuses every other
endpoint with `400 endpoint_not_served` before routing, so no server sees it, and
`/v1/models` lists the declared endpoints.

The list only narrows. It closes, where it is set:
- the cost the live run found (`docs/BACKLOG.md`, Endpoints per model) — speech sent to
  a vLLM-Omni image model runs a whole image generation before its `400`;
- chat sent to a llama-server encoder reranker (`500`, a circuit failure) or to a
  causal one (`200` with nonsense);
- the neutral `upstream_endpoint_missing` round trips to vLLM rerankers, embedding
  models and chat models;
- `/v1/models` listing endpoints a loaded model does not serve.

Decided with the user (2026-10-11): opt-in, in this branch before the release, no
version bump (an optional additive field, `CONTROL-PROTOCOL.md` → Types bump no
version; it ships inside config format 6).

## Files likely touched

- `docs/specs/GATEWAY.md`:
  - Model metadata: the field and what it does;
  - Providers → Endpoint support: a model narrows its types' set;
  - Request pipeline stage 4: routing takes the declared set;
  - `/v1/models` and the Anthropic-shaped list: `endpoints` is the declared set;
  - the error table row for `endpoint_not_served`.

  Each dated 2026-10-11, with what was rejected:
  - a per-backend list (settled 2026-10-06 — a backend serves several models);
  - a list that widens what a type serves;
  - a default-deny list.
- `docs/specs/CONTROL-PROTOCOL.md`: the model's fields; the rule that every listed
  endpoint is served by at least one of the model's deployments' types; no version
  bump.
- `protocol/schema/config.schema.json`:
  - `models.*.endpoints`: an array of the gateway's endpoint names, unique, at least
    one;
  - the endpoint names as an enum.
- `protocol/fixtures/`:
  - a valid config with lists;
  - invalid cases: an unknown name, a duplicate, an empty list, an endpoint none of
    the model's deployment types serves.
- **Both halves refuse the same configs.** The "served by a deployment's type" rule
  needs the endpoint-support table on both sides. Make it shared data in `protocol/`
  (e.g. the backend-type fixtures gaining the endpoints each type serves) that both
  halves test against, so neither can drift. Choose the cleanest way and say why.
- `control/kaiak-control/`: the type, validation, the schema copy synced.
- The gateway:
  - `config/`: the field, the semantic check;
  - `server/`: the routing filter where `servingDeployments` / `serves` already
    narrow by type; `/v1/models` and the Anthropic list;
  - tests.
- `examples/config.json`:
  - `qwen-image` → `images_generations`;
  - `qwen3-tts` → `audio_speech`, `audio_speech_batch`;
  - the reranker → `rerank`;
  - `bge-m3` → `embeddings`;
  - others left open.
- `docs/DEPLOYMENT.md`:
  - set the list on every vLLM-Omni model, reranker and embedding model;
  - the speech-to-image warning now points to the list.
- `docs/BACKLOG.md`: "Endpoints per model" resolved and removed; "Omni chat models"
  stays.
- The live kit: `live-image` and `live-tts` declare their endpoints. The
  `speech-to-image` check then expects kaiak's `400 endpoint_not_served`, with no
  backend request. vLLM-Omni's own answer was recorded in step 12.

## Decisions made during planning

- **The refusal is the existing `400 endpoint_not_served`**, with the same wording and
  shape per API. The model already passed its access check, so naming the endpoint
  leaks nothing.
- **The endpoint-missing memory and readings stay** for models without a list. They
  cover a different case — a server that lacks an endpoint its type serves — so this
  is not a second safety net.

## Acceptance criteria

- **Component tests:**
  - the config: schema, the semantic rule, the shared fixtures on both halves;
  - routing refuses an undeclared endpoint before any backend request;
  - a declared endpoint routes as before;
  - `/v1/models` and the Anthropic list show the declared set;
  - an absent list changes nothing.
- **e2e:**
  - speech sent to an image model whose list is `images_generations` → `400
    endpoint_not_served`, no backend request;
  - chat sent to a model whose list is `rerank` → the same.
- **The example config passes on both halves.**
- `scripts/check-all.sh` green 3× in a row.

## Result

(filled in when the step is done)
