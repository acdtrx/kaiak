# Step 2 — `backend-verify` in kaiak-control

**Status:** not started

## Intent

Implement `verifyBackend` as the spec says, as its own kaiak-control subsystem, exported from the
package entry.

## Files likely touched

- `control/kaiak-control/src/backend-verify/index.ts` (+ tests).
  - Node's built-in `fetch`; no new dependency.
  - URLs built the way the gateway builds them (`GATEWAY.md` → Base URLs):
    - openai-compatible: `<base_url>/models`;
    - azure-openai: `<base_url>/openai/v1/models`;
    - llama-server `/props` at `base_url` minus `/v1`.
  - Credential headers as the gateway sends them: `Authorization: Bearer`, or
    `api-key` for Azure.
- `control/kaiak-control/src/schemas/`: lenient ajv schemas for the fields read from
  a models list and from `/props`. Or local to the subsystem if that is the pattern;
  follow what's there.
- `control/kaiak-control/src/index.ts`: export `verifyBackend` and its types.

## Decisions made during planning

- **No retries.** The caller re-runs it.
- **One fetch of the models list; `/props` only when the list says llama-server.**
  Sequential; nothing parallel is needed for one backend.
- **Timeout and abort** via `AbortSignal.any([callerSignal,
  AbortSignal.timeout(ms)])`.
- **Body reads capped**, 1 MiB like the gateway's probe.
- **Tests use an in-process `node:http` server** per shape (vLLM, llama-server, OpenAI,
  Azure, broken). Response bodies are copied from the live answers captured on
  2026-09-29 (llama-server b9917), trimmed.

## Acceptance criteria

- Tests cover:
  - each source;
  - each failure code;
  - `base_url` without `/v1` (skipped `/props`, noted);
  - a `/props` missing fields (absent, not guessed);
  - an `owned_by` other than `llamacpp` (no `/props` call);
  - the credential never appearing in a report or error;
  - the abort signal.
- `npm test` and `npm run lint` green from `control/`. The boundary lint accepts the
  new subsystem.

## Result

(not started)
