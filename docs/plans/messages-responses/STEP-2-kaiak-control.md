# Step 2 — kaiak-control

**Status:** done (2026-10-06)

## Intent

Bring `kaiak-control` and the sample to config format 5 and protocol 5:
- the two new backend types
- `defaults` gone
- `backend-verify` for the new types
- examples and GUIDE updated

## Files likely touched

- `control/kaiak-control/src/config/` (types, semantic rules: `api_key_env` required
  for the new types; the `defaults` checks gone) and `src/protocol/` (version 5).
- `control/kaiak-control/src/backend-verify/`:
  - `anthropic`: `GET <baseUrl>/models` with `x-api-key` and
    `anthropic-version: 2023-06-01`, reporting model IDs and whatever metadata the
    list carries.
  - `azure-anthropic`: no request, a note that Foundry has no models list.
  - The `type` enum.
- `control/kaiak-control/GUIDE.md`: config publishing (types, no defaults), the
  backend-verify section, the written-against line.
- `control/sample/`: config examples and the page (no defaults column or section, if
  any).
- `examples/config.json`, `examples/local-config.json`: `defaults` removed; an
  `anthropic` backend example if it reads well.

## Decisions made during planning

- The `azure-anthropic` note's wording mirrors the azure-openai one (`models: []`, a
  top-level note).
- `verifyBackend` does not check that a server has the Messages or Responses
  endpoints. Endpoint support is fixed by the type (OVERVIEW decision 12), and
  checking it would add requests that serve no current need.

## Acceptance criteria

- `npm test` and `npm run lint` from `control/` pass, the shared fixtures included.
- The `backend-verify` tests cover `anthropic` (list read, headers sent, key never
  logged) and `azure-anthropic` (note, no request).
- The GUIDE and the examples carry no `defaults`.
- Suite run and recorded. Expected red: the gateway's fixture and version tests and
  the cross-half e2e (step 3).

## Result

**What changed**

- `control/kaiak-control/src/config/types.ts`: `BACKEND_TYPES` gains `anthropic` and
  `azure-anthropic` (the schema's order); `Model.defaults` and `DefaultValue` removed;
  `format_version: 5`. `api_key_env` for the new types is enforced by the synced
  schema (step 1); no semantic rule was needed.
- `control/kaiak-control/src/protocol/index.ts`, `src/messages/types.ts`: protocol 5.
- `control/kaiak-control/src/backend-verify/index.ts`:
  - `anthropic` reads `GET <baseUrl>/models?limit=1000` with `x-api-key` and
    `anthropic-version: 2023-06-01`, takes `context_length` from each entry's
    `max_input_tokens`, skips server recognition (reports `unknown`), and keeps
    `model-not-listed`.
  - `azure-anthropic` sends no request and reports `models: []`, the note
    `Microsoft Foundry has no models list: the backend was not contacted`, and
    `metadata: {}` when a model was named, as azure-openai does.
- Tests:
  - `backend-verify.test.ts`: the per-type URL and header test now covers every
    type (azure-anthropic by its own test). New tests: `anthropic` (the report
    whole, headers, model-not-listed, a refused credential with no leak) and
    `azure-anthropic` (no request, the note, metadata).
  - Version literals moved to 5 in the protocol, control-plane, Fastify, sample
    app, page and config-file tests, and in `limits.test.ts`.
  - The slow-reader test pads its ~1 MiB configs with 256 labelled groups instead
    of a model default.
  - The type-list messages in `backend-verify.test.ts` and the sample's
    `verify.test.ts`.
- `examples/config.json` (both models' `defaults` removed) and
  `examples/local-config.json`: format 5.
- `control/kaiak-control/GUIDE.md`:
  - the written-against line
  - §7: `format_version: 5`; the Anthropic types; which client APIs each type
    serves; `base_url` for the new types; "Models carry no defaults"; Anthropic
    standard price in Prices; the "cannot be a model default" sentence gone
  - §8: `anthropic` in the report's `server`; `azure-anthropic` is not checked

**Discrepancies and notes**

- No `anthropic` backend was added to `examples/config.json`. A realistic example
  needs a priced Claude model, and no rate was verified in this step. An unpriced
  one would read as "Claude is free" under the example's USD limit.
- Left for step 7, which owns operator docs. Text that still describes model
  defaults:
  - `docs/DEPLOYMENT.md:399` (`qwen3-32b` vs `-thinking`: "different defaults";
    the two models now differ only in output limit)
  - `docs/DEPLOYMENT.md:442` ("Model defaults" bullet)
  - `docs/architecture/gateway.html:330` ("the model's defaults")
- Left for steps 3 and 6: `docs/testing/LIVE-BACKENDS.md`'s `-chat-defaults` mentions
  (lines 64, 194, 302, 308, 365, 375) go with the kit's `chatDefaults` option.
- The sample `verify` usage line now lists seven types (generated from
  `BACKEND_TYPES`) and runs past 100 columns. Left as is.

**Suite** (2026-10-06)

- control `npm test`: 576 tests, 576 pass. `npm run lint`: passes (tsc, boundaries
  ok).
- `scripts/check-all.sh`: stops at the gateway half. gofmt, vet and staticcheck
  pass. `go test -race` fails in `cmd/kaiak`, `internal/config`, `internal/control`
  and `internal/server` — format 5 / protocol 5 and the new types, plus
  `TestEveryErrorCodeHasItsClass` for the five new codes. **Expected; cleared by
  step 3.** Every other gateway package passes.
- Cross-half e2e, run on its own: fails. The gateway's e2e writes a format-4 config,
  which the sample now rejects (`/format_version must be equal to constant`).
  **Expected; cleared by step 3.**
