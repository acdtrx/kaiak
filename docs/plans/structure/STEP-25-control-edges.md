# Step 25 — backend-verify table; the sample's page feed and verify CLI

**Status:** done (2026-10-08)

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

- `git grep -nE 'readVllmEntry|readAnthropicEntry|modelAt|modelsListUrl|requestHeaders' control/`
  → none; `git grep -n isBackendType control/sample` → none (the names the step
  removed; see the Result).

## Acceptance criteria

- A page-feed test: ending the feed while a push is rendering writes nothing after
  `end()` and raises no uncaught error.
- backend-verify's tests pass unchanged; a new `BackendType` without a row fails `tsc`
  (checked once by hand, noted in the Result).
- `npm test`, `npm run lint` green; `scripts/check-all.sh` green, or reds named.

## Result

**The crash, proved first** (`control/sample/src/page/feed.test.ts`, new, 4 tests). The
feed is driven without a server: the reply is a real `ServerResponse` with no socket,
so it never emits `close` and an ended stream stays in the `end()`→`close` window as
long as the test wants; every source read waits on a gate the test releases; the
heartbeat interval is mocked (`t.mock.timers`). No sleeps: the renders run on
promises alone and Node emits the write-after-end error on the next tick, so one
`setImmediate` settles both. On the code before the fix (feed.ts from HEAD, run once
by hand) all four fail, the first three with the real
`Error [ERR_STREAM_WRITE_AFTER_END]: write after end` raised as an uncaught `error`
event:
- ending the feed while a browser's connect render runs (gate held, `endAll`, release);
- ending the feed while a push renders (`sectionsChanged`, gate held, `endAll`,
  release);
- a heartbeat tick after `endAll`;
- an `error` emitted on the response (no listener: the error itself is thrown).
With the fix all four pass; each also asserts nothing was written to the response
once it had ended.

**What changed**

- backend-verify (edges F1): `const TYPE_RULES: Record<BackendType, TypeRules>`, read
  once (`TYPE_RULES[settings.type]`) by `verifyBackend`. `TypeRules` is
  `{ list: "none"; why } | { list: "models" | "base-models"; url(base, path);
  headers(credential?); listQuery?; recognizeServer; entryContextField? }`: a type
  with no list carries no URL or headers. Rows: the four OpenAI-shaped types share one
  row value (`underBase`, `bearer`, recognized); `azure-openai` `/openai/v1/<path>`,
  `api-key`, `base-models`; `anthropic` `<base>/<path>` + `?limit=1000`, `x-api-key` +
  `anthropic-version`, not recognized, `max_input_tokens`; `azure-anthropic` `none`
  ("Microsoft Foundry has no models list"). Read 1:1 against
  `gateway/internal/provider/{openai_compatible,openai,vllm,llama_server,azure_openai,anthropic}.go`
  (`url`, `header`) and `protocol/fixtures/backend-types/`. `modelsListUrl`,
  `requestHeaders`, the two early-return `if`s on a type name, the anthropic
  recognition skip and the entry-reader `if`/`else if` chain are gone; `vllm`'s
  `max_model_len` stays keyed by the recognized server (`VLLM_CONTEXT_FIELD`). The
  notes chain is now "list empty → empty note; else recognizing type with an unknown
  server → not-recognized note" — the same outputs per case as before.
  `getJson` takes `GetOptions { headers, timeoutMs, signal? }` built once per check,
  so the `/props` request carries the same headers as before.
- backend-verify (edges F2): `readContextLength(doc, url, field, model)` used three
  times (`max_model_len`, `max_input_tokens`, `/props` `n_ctx`);
  `describeAnswer(answer, url, timeoutMs)` for every non-JSON answer kind, used by
  `propsOf` and by `modelsListOf`, which adds the code and keeps its own 401/403 and
  3xx wording; `models` built with one `map`. `readVllmEntry`, `readAnthropicEntry`,
  `modelAt` gone. Every note and failure string byte-identical (the 28
  backend-verify tests, which match on them, pass unchanged).
- Sample page feed (edges F8 in place + the bug, decision 10): a per-stream `closed`
  mark checked by every write; `close()` is idempotent and is the first thing every
  ending does (`end()`, the stall timer, a failed connect render — each before
  `end`/`destroy`), so a render or heartbeat finishing after the end writes nothing;
  the connect render checks `closed` instead of `open.has(browser)`; an `error`
  listener on the response logs "page stream: the connection failed; ending the
  stream" and closes. As `fastify/gateway-stream.ts` does; no shared SSE writer.
- Verify CLI (edges F9): the `--type` string goes to `verifyBackend` as given (one
  cast, commented); the CLI's own `isBackendType` and its message are gone, the
  library's `verify-input-invalid` ("type must be one of \"openai-compatible\", …")
  speaks. The "still to decide" capability list reads `MODEL_CAPABILITIES`; the
  hint lookup's hand-written union cast is `keyof VerifiedCapabilities`.
- `kaiak-control` `config`: new `MODEL_CAPABILITIES` (`["streaming", "tools",
  "vision", "reasoning"]`) and `ModelCapability`; `ModelMetadata.capabilities` is
  `Record<ModelCapability, boolean>` (same shape); exported from the package entry;
  `config.test.ts` pins it to the schema's required list, in order, as
  `BACKEND_TYPES` is pinned. GUIDE §8 ("metadata is partial") names it
  in one parenthesis.

**The `tsc` check** (by hand, reverted): adding `"bedrock"` to `BACKEND_TYPES` makes
`npm run lint` fail with `backend-verify/index.ts: error TS2741: Property 'bedrock' is
missing in type … but required in type 'Record<…, TypeRules>'`. `types.ts` restored
byte for byte.

**Decisions made during the step**

- **`TypeRules` is a union**, not the flat shape the report sketched: the `none` row
  has no URL or headers to read 1:1 with anything (its gateway module has them, but
  verification never sends), and carries the not-checkable reason (`why`) so
  `verifyBackend` holds no type-specific text; the base-models note names
  `settings.type` (same output).
- **`MODEL_CAPABILITIES` exported from `config`**: it is the config schema's own
  required list, the `ModelMetadata` type now derives from it (a use inside the
  library), and it is the `BACKEND_TYPES` precedent — a value a host's form lists. A
  capability added to the schema now fails the pin test until the constant follows,
  and the sample needs no edit. `reasoning_efforts` stays literal in the CLI: it is
  optional and conditional, not in that list.
- **The bad-type CLI assertions follow the library's message.** Two assertions in
  `verify/verify.test.ts` pinned the removed CLI wording (`--type must be one of …,
  not "bedrock"`); the behaviour they guard — a bad type is refused with a message,
  exit 1 and the usage, before anything is sent — stays, so the patterns now match the
  library's message exactly (same strictness) rather than being deleted. Flagged for
  review against decisions 16/17.
- **`close()` before `destroy()` too** (stall, failed connect render), as the gateway
  stream does: "closing is the first thing every ending does".

**Report vs code** (034329e line numbers; code at bd9e5ba): the findings held as
written. F1's "six branch points": the two early returns, the recognition skip, the
reader chain, `modelsListUrl`, `requestHeaders` — all gone. F9's ":76 / :98–100 /
:123 / :129–135" matched.

**Tests** (before → after)

- Total (`npm test` from `control/`): 624 (623 pass, 1 skipped) → 629 (628 pass, 1
  skipped).
- `backend-verify.test.ts`: 28 → 28, unchanged file.
- Sample page: `events.test.ts` 6 → 6, `page.test.ts` 13 → 13, `feed.test.ts` new, 4.
- `verify/verify.test.ts`: 10 → 10 (two patterns changed, above).
- `config/config.test.ts`: 164 → 165 (the `MODEL_CAPABILITIES` pin).

**Removal checklist**

- `git grep -nE 'readVllmEntry|readAnthropicEntry|modelAt|modelsListUrl|requestHeaders' control/`
  → none.
- `git grep -n isBackendType control/sample` → none. (`control/` as a whole finds
  `backend-verify/index.ts`'s own `isBackendType` schema checker: the library's input
  check, the one the CLI now relies on — kept by design.)

**Suite**: `npm test` (629 tests, 628 pass, 1 skipped, 0 fail) and `npm run lint`
(`tsc` + "boundaries ok") from `control/`, green. `scripts/check-all.sh` was not run
here (step 28 is in flight under `gateway/` in this worktree); it runs at phase end.
