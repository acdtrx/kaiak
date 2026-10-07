# Step 25 — backend-verify table; the sample's page feed and verify CLI

**Status:** not started

## Intent

What a backend type means for verification is one exhaustive table the compiler holds to
`BACKEND_TYPES`; the sample's page feed cannot write after it ended; the verify CLI stops
re-checking what the library checks.

## Findings

- edges F1: `const TYPE_RULES: Record<BackendType, TypeRules>` (`url`, `headers`,
  `list: "models" | "base-models" | "none"`, `listQuery`, `recognizeServer`,
  `entryContextField`), read once by `verifyBackend`; the six branch points and their
  "default means OpenAI" `else`s go. Rows read 1:1 against the gateway modules and the
  step-4 backend-type fixture.
- edges F2: `readContextLength(doc, url, field, model)` used three times;
  `describeAnswer(answer, url, timeoutMs)` for the non-JSON kinds, `modelsListOf` adding
  the code; `models` built with `map`, `modelAt` gone. Note strings stay as they are.
- edges F8 (in place) / bug: the page feed gets a `closed` mark checked by every write
  and an `error` listener, as the gateway stream has. **Fixes** (decision 10) the
  write-after-`end()` crash at shutdown.
- edges F9: the verify CLI passes the type string through (the library's
  `verify-input-invalid` speaks); the capability list reads a constant `kaiak-control`
  exports (export one if none fits), or stays with a comment if exporting would be for
  the sample alone — decide in the step and say why.

## Files likely touched

- `kaiak-control/src/backend-verify/index.ts` and tests.
- `control/sample/src/page/feed.ts` (+ a test), `control/sample/src/verify/index.ts`.

## Removal checklist (clean at phase end)

- `git grep -nE 'readVllmEntry|readAnthropicEntry|modelAt|isBackendType' control/` → none
  (adjust to the names the step removes; list them in the Result).

## Acceptance criteria

- A page-feed test: ending the feed while a push is rendering writes nothing after
  `end()` and raises no uncaught error.
- backend-verify's tests pass unchanged; a new `BackendType` without a row fails `tsc`
  (checked once by hand, noted in the Result).
- `npm test`, `npm run lint` green; `scripts/check-all.sh` green, or reds named.
