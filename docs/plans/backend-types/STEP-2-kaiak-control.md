# Step 2 — kaiak-control

**Status:** not started

## Intent

`kaiak-control` and the sample accept, validate and verify the five backend types.

## Files likely touched

- `control/kaiak-control/src/config/types.ts`: `BackendType` gains `openai`, `vllm`,
  `llama-server`.
- The check for `openai` without `api_key_env`, if it is not schema-only.
- `control/kaiak-control/src/backend-verify/index.ts`: every type accepted (the input
  check reads the schema's enum already — confirm); `<base_url>/models` for every type
  but `azure-openai`; the bearer credential likewise; a note when `recognizeServer`
  finds `vllm` or `llama-server` and the declared type is another one (wording in
  `BACKEND-VERIFY.md`).
- `control/sample/src/verify/index.ts`: `--type` takes the five types (default
  stays `openai-compatible`); usage text.
- `control/kaiak-control/GUIDE.md`: the types, which one forces the service tier,
  choosing a type.

## Decisions made during planning

- The sample's `--type` list is read from `kaiak-control`'s type, not a second
  hand-written list, if the type is exported as a value; otherwise one list beside the
  type.

## Acceptance criteria

- kaiak-control passes every shared config fixture with the codes `cases.json` names.
- `verifyBackend` tests: each type reaches the right URL with the right header; a
  vLLM answer under `openai-compatible` and a llama-server answer under `vllm` carry
  the note; a matching type carries none.
- The sample's `verify --type` accepts the five types and refuses another with its
  message; exit codes unchanged.
- `npm test` and `npm run lint` green. Suite recorded; expected red: the gateway
  (step 3) and the cross-half e2e (steps 3–4).

## Result

(to be filled when the step is done)
