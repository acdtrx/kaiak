# Step 2 — `backend-verify` in kaiak-control

**Status:** done (2026-09-29)

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
- `docs/ARCHITECTURE.md`: the new subsystem in the `kaiak-control` graph, and a
  control plane → backends arrow (called by the app only, off the request path).

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

Built:
- `control/kaiak-control/src/backend-verify/index.ts` — `verifyBackend(options)` and
  the report types (`VerifyBackendOptions`, `BackendReport`, `VerifiedModel`,
  `VerifiedCapabilities`, `Source`, `MetadataFragment`, `VerifyFailureCode`,
  `VerifiedServer`), exported from the package entry.
- `backend-verify.test.ts` — 22 tests against in-process `node:http` fakes on
  127.0.0.1 (llama-server b9917 chat and embedding, vLLM 0.30.0, OpenAI-style,
  Azure-style, broken ones), bodies trimmed from the 2026-09-29 live answers.
- `docs/ARCHITECTURE.md`: the subsystem in the kaiak-control list and graph
  (`backend-verify → config, schemas`); a dotted control plane → backends arrow and a
  data-flow line (app-called, off the request path). `docs/TECH-STACK.md`: ajv's
  inventory line names the backend-answer checks.

Decisions made while implementing:
- Input rules reuse the config schema through `definitionChecker` with a JSON pointer
  under `$defs` (`backend/properties/type`, `backend/properties/base_url`,
  `backend_model_name`); a reported context length (vLLM `max_model_len`,
  `/props` `n_ctx`) is kept only when it passes config's own `context_length` rule
  (`model/properties/metadata/properties/context_length`), so the fragment always fits
  config.
- The lenient answer schemas and the credential / timeout rules live in the subsystem,
  on its own ajv instance (not in `schemas/`, which loads only `protocol/schema/`).
- A body cut off mid-read (connection reset after the status) is `not-a-models-list`
  ("the body was cut off"), not `unreachable`: an HTTP answer did arrive.
- `unreachable` messages carry only the platform's error code (e.g. `ECONNREFUSED`),
  never an error message. Non-`2xx` bodies are cancelled unread.
- A present-but-non-object parent on a `/props` path (`modalities: "vision"`) is
  malformed (note naming the field), an absent one is missing.
- Unknown-server and empty-list backends get one top-level note; azure-openai gets
  its note and `metadata: {}` when a model is named.

Spec edits (`docs/specs/BACKEND-VERIFY.md`), wording only:
- `timeoutMs` is a positive integer up to 2147483647: `AbortSignal.timeout` refuses
  longer delays, so a larger value is now `verify-input-invalid` instead of a throw
  from the platform.
- Headers: "nothing else of its own" — Node's `fetch` adds its fixed defaults (`Host`,
  `Connection`, `Accept-Encoding`, `Accept-Language: *`, `Sec-Fetch-Mode`), which the
  helper cannot remove.

Verification (2026-09-29, in the worktree):
- `npm test` (control/): `tests 532 · pass 532 · fail 0` (22 new).
- `npm run lint`: `tsc` clean, `boundaries ok`.
- `scripts/check-all.sh`: gateway checks passed, control tests and lint green,
  cross-half e2e `ok kaiak/e2e 43.851s`, `all checks passed`.
