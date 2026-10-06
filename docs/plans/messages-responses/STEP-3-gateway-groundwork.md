# Step 3 — gateway groundwork

**Status:** not started

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

(filled in when the step is done)
