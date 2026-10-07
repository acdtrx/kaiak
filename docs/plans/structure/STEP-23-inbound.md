# Step 23 — inbound: one parse prologue; two JSON-walk helpers

**Status:** not started

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

- `git grep -nE 'MaxCompletionTokens|MaxOutputTokens|MaxTokens\b' gateway/internal/server` → none.
- `git grep -n 'decodeMembersRepeats' gateway/internal/server` → only inside the two helpers.

## Acceptance criteria

- New tests: a repeated member in a Messages message object is refused; an embeddings
  request with `max_tokens: "x"` passes through (it was refused before).
- `server_test.go`, `messages_test.go`, `responses_test.go` and the inbound tests pass
  otherwise unchanged.
- `scripts/check-all.sh` green. Live-test kit against `vllm` and `llama-server` on the
  DGX (`docs/testing/LIVE-BACKENDS.md`), the DGX's model restored afterwards.
  **Phase 5 ends here.**
