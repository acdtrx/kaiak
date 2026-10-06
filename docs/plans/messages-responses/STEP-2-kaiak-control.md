# Step 2 — kaiak-control

**Status:** not started

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

(filled in when the step is done)
