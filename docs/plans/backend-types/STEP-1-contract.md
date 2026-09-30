# Step 1 — contract

**Status:** not started

## Intent

Write the five backend types, the service tier by type and the version bumps into the
contract both halves test against. Every later step of phase 1 implements what this
step writes.

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
  - Wrong model on a host: the missing-model code per module;
  - the data directory's last-known-good config format version.
- `docs/specs/CONTROL-PROTOCOL.md`: config `format_version` 4, protocol version 4
  (with the reason, decision 7 of the overview).
- `docs/specs/BACKEND-VERIFY.md`: the types it takes, and the note when the
  recognized server is not the declared type.
- `protocol/schema/config.schema.json`: the type enum and its description,
  `api_key_env` required for `openai`, `format_version` 4. Then
  `npm run sync-schemas` from `control/`.
- Other schemas and fixtures carrying the protocol version (status
  `protocol_version`, …).
- `protocol/fixtures/config/**`: every fixture to format 4 (one-off script, not
  committed); a valid fixture holding one backend of every type; invalid fixtures
  with `cases.json` entries: `openai` without `api_key_env`, and
  `backend-type-unknown` kept (its value must not be one of the new names).

## Decisions made during planning

- `openai` without `api_key_env` gets its own stable code, as `azure-openai` has
  (`azure-without-api-key-env.json` is the model): check what code that fixture names
  and mirror it.

## Acceptance criteria

- The specs state the five types, the service tier by type, the new versions and the
  replaced decision — dated, with reasons.
- Schema copies in sync (`npm run sync-schemas`).
- Every fixture at format 4; the new valid and invalid fixtures listed in
  `cases.json` with their codes.
- Suite run and recorded. Expected reds: both halves fail the format-4 fixtures and
  the version checks (step 2 clears kaiak-control's, step 3 the gateway's).

## Result

(to be filled when the step is done)
