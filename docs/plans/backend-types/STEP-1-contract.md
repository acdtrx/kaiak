# Step 1 — contract

**Status:** done (2026-10-01)

## Intent

Write the five backend types and the service tier by type into the contract both
halves test against. Every later step of phase 1 implements what this step writes.

## Files likely touched

- `docs/specs/GATEWAY.md` → Providers:
  - the list of types and what each covers (`openai-compatible` is the generic type
    for any other OpenAI-format server);
  - a new dated decision (settled 2026-09-30) replacing the 2026-09-29 sentence "a new
    backend type … is added only when a real difference needs one", with the reason
    and what was rejected;
  - Service tier: forced for `openai` and `azure-openai` only; the other three pass
    the client's field untouched;
  - Base URLs and credentials: `openai`, `vllm`, `llama-server` as `openai-compatible`;
    `openai` needs `api_key_env`;
  - Wrong model on a host: the missing-model code per module.
- `docs/specs/CONTROL-PROTOCOL.md`: the backend types in the config section; versions
  unchanged (format 3, protocol 3 — overview decision 4, recorded with its reason).
- `docs/specs/BACKEND-VERIFY.md`: the types it takes, and the note when the
  recognized server is not the declared type.
- `protocol/schema/config.schema.json`: the type enum and its description,
  `api_key_env` required for `openai`. Then `npm run sync-schemas` from `control/`.
- `protocol/fixtures/config/**`: a valid fixture holding one backend of every type;
  invalid fixtures with `cases.json` entries: `openai` without `api_key_env`, and
  `backend-type-unknown` kept (its value must not be one of the new names).

## Decisions made during planning

- `openai` without `api_key_env` gets its own stable code, as `azure-openai` has
  (`azure-without-api-key-env.json` is the model): check what code that fixture names
  and mirror it.

## Acceptance criteria

- The specs state the five types, the service tier by type, the unchanged versions
  and the replaced decision — dated, with reasons.
- Schema copies in sync (`npm run sync-schemas`).
- The new valid and invalid fixtures listed in `cases.json` with their codes.
- Suite run and recorded. Expected reds: both halves fail the new fixtures (step 2
  clears kaiak-control's, step 3 the gateway's).

## Result

- **Specs.** `GATEWAY.md` → Providers: the five types as a list; the 2026-09-29
  module decision loses its "a new type only when a real difference needs one"
  sentence, and a new bullet **A type per server** (settled 2026-09-30) replaces it,
  with the reason and the rejected alternative; Base URLs, credentials (`openai`
  needs `api_key_env`), Passthrough edits, Service tier (forced on `openai` and
  `azure-openai` only; the rejected alternative: forcing it on `openai-compatible`
  too), Wrong model on a host (missing-model code per module) and the probe URL by
  type. `CONTROL-PROTOCOL.md` → Config: a **Backend types** bullet with the
  versions-stay decision (settled 2026-10-01, overview decision 4, with its reason
  and rejected bump); the `defaults` bullet's service-tier clause made type-aware.
  `BACKEND-VERIFY.md`: `type` takes the five; the models-list URL and bearer header
  for every type but `azure-openai`; recognition and `model-not-listed` for every
  type but `azure-openai`; the note's wording.
- **Schema.** The type enum (`openai-compatible`, `openai`, `azure-openai`, `vllm`,
  `llama-server`) and its description; the `api_key_env` rule's `if` is now
  `type ∈ {openai, azure-openai}`; the `base_url` and `api_key_env` descriptions name
  the types. `npm run sync-schemas` run.
- **Fixtures.** `valid/backend-types.json` (one backend of each type, one model
  deployed on all five); `invalid/openai-without-api-key-env.json` (from
  `azure-without-api-key-env.json`); `cases.json` gains its entry and the
  `backend-type-unknown` reason names the five types (its value stays `bedrock`).

Decisions made during the step:

- The `openai` fixture is `kind: "schema"` with **no code**, as its Azure model is:
  schema violations carry no per-rule code (`CONTROL-PROTOCOL.md`, Config).
- `service_tier` stays refused in a model's `defaults` on every type: a model's
  deployments can span types, and where the tier is not forced it is the client's.
- `verifyBackend` reads what the **recognized** server serves (`/props` for
  llama-server), whatever the declared type; the mismatch note names both:
  `the models list says the server is <server>: use type "<server>", not "<type>"`.
  No note for an `"unknown"` server.

Suite (`scripts/check-all.sh`, 2026-10-01): stops at the gateway's first red, so
the rest was run by hand.

- Gateway `go test -race ./...`: one red, the expected one —
  `internal/config` `TestValidFixtures/backend-types.json` (the gateway knows two
  types); step 3 clears it. Every other package green, `e2e` included; gofmt, vet,
  staticcheck clean. `openai-without-api-key-env.json` already passes in the gateway,
  for now as an unknown type — step 3 makes it fail on the missing key.
- Live-test kit: vet, staticcheck, gofmt clean; self-test 16 passed, 0 failed.
- Control `npm test`: 560 passed, 0 failed — **no red**: kaiak-control validates
  with ajv over the synced schema, so both new fixtures already pass with the
  expected outcome. The overview's expected kaiak-control red does not occur; step 2
  still owns `BackendType`, `verifyBackend` and the sample's `verify`. `npm run lint`
  green.
- Cross-half e2e (`TestAcrossHalves`): green.
