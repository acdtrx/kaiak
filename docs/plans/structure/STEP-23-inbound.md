# Step 23 — inbound: one parse prologue; two JSON-walk helpers

**Status:** done (2026-10-08)

## Intent

The three format parsers share one prologue that reads the output-limit keys from the
endpoint table, and the inbound rules walk JSON with two helpers instead of ten copies
that have drifted.

## Findings

- server S4 / decision 6:
  - `inboundFields.OutputLimits []*int64`, aligned with the endpoint row's
    `outputLimitKeys`; the `MaxTokens`, `MaxCompletionTokens`, `MaxOutputTokens` fields
    and the accessor closures in `params.go` go;
  - `parseOwnedFields` does the common part once (decode, model, stream and the
    output-limit keys when the endpoint generates), then the format's own checks, then
    `EstimateInput`;
  - **behaviour change** (decision 10): embeddings and completions no longer type-check
    `max_tokens` / `max_completion_tokens` they ignore (completions still reads
    `max_tokens`); those keys pass through untouched. Check `GATEWAY.md`'s owned-field
    list (Client API) and align it with the settled output-limit keys if it names them
    for embeddings.
- server S5: `eachObject(raw, param, func(at, members) *apiError)` (lists → objects,
  repeats refused, non-objects skipped; a strict variant for the tools list) and
  `objectMembers(raw, at)`. **Fixes** (decision 10): a Messages message object with a
  repeated member is refused `400 invalid_request_error` with the member's path, like
  every sibling.

## Files likely touched

- `gateway/internal/server/{inbound,inbound_messages,inbound_responses,params}.go` and
  the inbound tests.
- `docs/specs/GATEWAY.md` (Client API owned fields, only if it disagrees).

## Decisions made during planning

- The error answers' `param` paths stay exactly as the tests pin them.

## Removal checklist (clean at phase end)

- `git grep -nP 'MaxCompletionTokens|MaxOutputTokens|MaxTokens\b' gateway/internal/server` → none.
- `git grep -n 'decodeMembersRepeats' gateway/internal/server` → only inside the two helpers.

## Acceptance criteria

- New tests: a repeated member in a Messages message object is refused; an embeddings
  request with `max_tokens: "x"` passes through (it was refused before).
- `server_test.go`, `messages_test.go`, `responses_test.go` and the inbound tests pass
  otherwise unchanged.
- `scripts/check-all.sh` green. Live-test kit against `vllm` and `llama-server` on the
  DGX (`docs/testing/LIVE-BACKENDS.md`), the DGX's model restored afterwards.
  **Phase 5 ends here.**

## Result

**Bug proof** (before any code change)

- New case in `TestInboundRefusalsPastTheTopLevel` (`inbound_test.go`), "messages
  message member named twice": a message whose first `content` holds a block with a
  stored-file source and whose second `content` is the string `"a"`. Want: `400
  duplicate_member` naming `messages[0].content`, nothing at the backend.
- On the old code it failed: the inbound stage let it through, so it went on to the
  next stage and got `404 model_not_found`. The gateway's stored-file walk had read the
  second `content` only, so the stored file in the first went unchecked. A backend
  that keeps the first occurrence would have been handed a stored file.
- On the new code it passes: the message object is walked by `eachObject`, which
  refuses repeats like every other object.

**What changed**

- **Two JSON-walk helpers** (`inbound.go`, server S5).
  - `objectMembers(raw, at) (members, ok, apiErr)` decodes one JSON object. `ok` is
    false when `raw` is not one object. A repeated member gives `duplicate_member`,
    with the path `at.<name>`, or just `<name>` at the top level (`at` = `""`).
  - `eachObject(raw, param, visit)` walks a list of objects. Each entry's path is
    `param[i]`. Repeats are refused. A value that is not a list holds none, and an
    entry that is not an object is skipped.
  - `eachObjectStrict` is the same walk for the tool lists the gateway reads itself:
    - an absent or `null` list holds none;
    - a list that is not an array is refused `invalid_type` (`an array`);
    - an entry that is not an object is refused `invalid_type` (`an object`).

    The two share `visitObjects`. `objectVisitor` is the one visitor type.
  - Rewritten on the helpers:
    - the 5 list walkers: `refuseHostedTools` (now one line over
      `eachObjectStrict`), `refuseStoredFiles`, `refuseStoredFileBlocks`,
      `refuseResponsesInputItems` and `refuseStoredFileParts`;
    - the 5 object checks: `stream_options`, `thinking`, a block's `source`, a
      shell's `environment` and `tool_choice`;
    - the top-level decode in `parseOwnedFields`.
  - Gone: `decodeMembers`, `decodeMembersRepeats` and the `toolJudge` type.
    `objectMembers` is now the decoder; only the first repeat was ever used.
- **One parse prologue** (server S4, decision 6).
  - `parseOwnedFields` runs these steps in order:
    1. decode;
    2. `model`;
    3. `stream`, except on the token-counting endpoints;
    4. the row's `outputLimitKeys`, into `inboundFields.OutputLimits []*int64` (aligned
       with the keys);
    5. `Sequences = 1`;
    6. the format's own checks;
    7. `EstimateInput`.
  - The three format parsers keep only their own checks:
    - OpenAI: sequences, embedding inputs, `stream_options`;
    - Messages: hosted tools, stored files, thinking budget;
    - Responses: stateful fields, hosted tools, input items.
  - Gone:
    - the fields `MaxTokens`, `MaxCompletionTokens` and `MaxOutputTokens`;
    - `readModelAndStream`;
    - the `outputLimitKey` type and its accessor closures (`maxTokensKey`, …).
  - `endpoint.outputLimitKeys` is a `[]string`. `applyModelParams` walks
    `OutputLimits` next to the key names.
- **Behaviour change** (decision 10). Embeddings no longer reads `max_tokens` or
  `max_completion_tokens`, and completions no longer reads `max_completion_tokens`.
  Those keys reach the backend untouched, whatever their value.
  - New test `TestOutputLimitKeysTheEndpointDoesNotTakePassUnchecked`
    (`limits_test.go`). It sends embeddings `max_tokens: "x"`, embeddings
    `max_completion_tokens: "x"` and completions `max_completion_tokens: "x"`. Each
    gets `200` and the backend receives `"x"`.
  - On the old code all three were refused `400 invalid_type`. This was checked by
    running the test with the old non-test files restored.
- **`GATEWAY.md`**
  - Client API → owned fields: the OpenAI list named `max_tokens` and
    `max_completion_tokens` for the whole format. It now names each endpoint's
    output-limit keys: chat both, completions `max_tokens`, embeddings none (settled
    2026-10-08: a key the endpoint does not take is not read).
  - "Every member the gateway reads is read once", and the `duplicate_member` row in
    the error table, now include a Messages message (settled 2026-10-08).
  - Request pipeline → duplicate members said "repeats deeper in the body (inside
    `messages`, say) are the backend's business". That was already untrue for blocks.
    It now says the objects the gateway reads are held to the same rule; repeats in
    other objects pass.
- Non-test Go: 187 lines added, 258 removed, over 5 files.

**Decisions made during the step**

- **No `generates` field.**
  - `stream` is read when `!counts`. Embeddings keeps reading `stream`, as before:
    step 22 noted it reads `stream` without generating.
  - The output-limit keys come from the row, which is empty for embeddings and the
    counting endpoints. "Generates", for the prologue's purpose, is "has keys".
- **A separate strict function, not a flag.** `eachObjectStrict` is a separate
  function sharing `visitObjects`, so call sites do not carry a bare `true`/`false`.
- **`objectMembers` returns `(members, ok, apiErr)`.** The review sketched `(map,
  *apiError, ok)`; the error comes last here, as Go convention has it. Every caller
  decides what "not an object" means for it:
  - top level: `invalid_json`;
  - `stream_options`, `thinking`, `environment`: `invalid_type`;
  - `tool_choice`: `invalid_type` (`a string or an object`);
  - a block's `source` and list entries: skipped.
- **Order of type errors on chat (flag).** The prologue reads the keys in the row's
  order: `max_completion_tokens`, then `max_tokens`. The old parser read `max_tokens`
  first. So a chat request with *both* keys of the wrong type now gets an error naming
  `max_completion_tokens`, where it used to name `max_tokens`.
  - No test pins this.
  - The range checks in `applyModelParams` already used the row order, so type and
    range errors now agree.
  - Every request with one malformed key gets the same answer as before.
- **Test fixture adapted.** `TestParseOwnedFieldsKeepsTheRawBody` reads
  `OutputLimits[0]` (`max_completion_tokens`, 64) and `OutputLimits[1]`
  (`max_tokens`, nil) instead of the two removed fields. Its assertions are the same.
  No other test changed.

**Report vs code** (034329e; code after step 22)

- S4's closures had moved: step 22 put them on the endpoint rows (`pipeline.go`), and
  `params.go` held them as `maxTokensKey` and its two siblings. Otherwise as reported.
- S5's counts hold: 5 list walkers and 5 object-with-repeats checks, plus the top-level
  decode the review did not count.
- The drift is as reported:
  - `refuseStoredFiles` dropped the message object's repeats;
  - `refuseHostedTools` alone refused non-objects. It keeps doing so as
    `eachObjectStrict`.
- The Messages message-repeat bug is confirmed by the failing test above.

**Tests** (top-level from `go test -list`; in brackets, passes including subtests)

- `internal/server`: 188 (457) → 189 (462). The additions:
  - one new top-level test with 3 subtests;
  - one new case in `TestInboundRefusalsPastTheTopLevel`.
- `server_test.go`, `messages_test.go`, `responses_test.go` and the other inbound
  tests pass with no assertion changed (`TestParseOwnedFieldsKeepsTheRawBody`: field
  reads only, see above).

**Removal checklists, phase 5** (`git grep --untracked`)

- Step 20:
  - `isUsageOnlyChunk|stripUsage|\(\*responsesStreamEnd\)` in `provider`: no
    `isUsageOnlyChunk` and no `(*responsesStreamEnd)`.
  - `stripUsage` appears only in the core, as step 20 recorded:
    - `passthroughBody` (`body.go`);
    - `sendWire` (`send.go`);
    - `upstreamResponse` (`response.go`);
    - `streamFormat.requestEdits`;
    - the `passthroughBody` tests.
  - `git ls-files gateway/internal/provider/wire.go` → none.
- Step 21: `listsModels|ListsModels` in `gateway/` → none.
  `git ls-files gateway/internal/routing/modelcheck.go` → none.
- Step 22: `func providerEndpoint|func \(e endpoint\) (path|name|operationName|counts|namesModel)|func errorShapeOf`
  → none.
- Step 23:
  - `MaxCompletionTokens|MaxOutputTokens|MaxTokens\b` in `server` has one hit:
    `responses_test.go:312`, `fakebackend.Reply{HonorMaxTokens: true}`. That is a
    different identifier, the fake backend's option, which matches only because `\b`
    has no left anchor. `\bMaxTokens\b` → none.
  - `decodeMembersRepeats` → none: `objectMembers` is the decoder now.
  - Also clean over `gateway/`, `docs/specs`, `docs/ARCHITECTURE.md` and `README.md`:
    `readModelAndStream|toolJudge|decodeMembers\b|outputLimitKey\b|maxTokensKey|maxOutputTokensKey|maxCompletionTokensKey`.

**Suite**: `scripts/check-all.sh` passed (exit 0). Phase 5 is green.

```
==> gofmt / go vet / staticcheck 2026.2.1 (gateway)
==> go test -race -count=1 (gateway): ok cmd/kaiak, e2e (107s), accounting, auth, clip,
    config, control, limits, logattr, metrics, netfail, otlplog, provider, routing,
    schemacheck, server, sse
==> live-test kit: gofmt / vet / staticcheck; self-test passed for vllm, llama-server,
    openai, azure-openai, anthropic, azure-anthropic, vllm with two backends
gateway checks passed
==> npm test (control): tests 617, pass 616, fail 0, skipped 1
==> npm run lint (control)
==> cross-half e2e: ok kaiak/e2e (65s)
all checks passed
```

**Live check** (main session, 2026-10-08; phase 5 ends here). The DGX was not swapped:
the kit ran against what was serving, ordinary requests only; embeddings on
`llama-embed.local:11435` (`/models/qwen3-embedding-0.6b-q8_0.gguf`).

- `vllm`, vLLM with `unsloth/Qwen3.8-27B-NVFP4` on `dgx.local:11434` (`-max-output 4096`,
  thinking off through `-*-params`): **29 passed, 0 failed, 1 skipped**
  (`messages-cache`: the server reported no cache read).
- `llama-server`, `unsloth/Qwen3.8-Flash-Next-GGUF:UD-Q4_K_XL` on `dgx.local:11434`
  (the user had switched the DGX to it; `-max-output 4096`): **29 passed, 0 failed,
  2 skipped** (`messages-cache` as above; `endpoint-not-served`: llama-server serves
  every endpoint the kit checks)..
