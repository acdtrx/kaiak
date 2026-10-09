# Step 7 — review and green

**Status:** done — reviewed and committed 2026-10-09

## Intent

An independent review checks the plan's code, and the branch is green and ready to
merge except for the live run (step 8).

## Files likely touched

- Fixes from the review, each with its test.
- `docs/BACKLOG.md`: entries the review or the plan left open, each with its revisit
  trigger.
- `docs/plans/rerank/OVERVIEW.md`: the review outcome and the verification status.

## Decisions made during planning

- **The review runs as before:** a plain copy reviewed in Codex, plus agent reviews
  of the worktree, over the branch diff against `v0.12.1`. Findings are merged and
  re-checked against the code.
- **Findings are triaged by frequency** (kaiak working style). Rare edge findings stay
  recorded in the review rather than fixed.

## Acceptance criteria

- Every review finding is fixed or answered.
- `scripts/check-all.sh` green 3× in a row. Suite recorded.

## Result

Two agent reviews of the worktree over `v0.12.1..rerank`, then the Codex review of a
plain copy; each finding re-checked by the main session or by a repro before the fix. llama.cpp sources read at `master`
de7fa0a (2026-10-08).

| # | Severity | Source | Finding | Done / recorded |
|---|---|---|---|---|
| 1 | high | agent reviews | llama-server reads `texts` (TEI's format) in place of `documents` — past the cap — and answers it with no `usage`: a `texts` list of any length settled as one document's estimate | Refused: `400 tei_format_unsupported`, `param: "texts"`, any value including `null`, every backend type (`server/inbound.go` `parseRerankFields`, `errors.go` `errRerankTEIFormat`). `GATEWAY.md` → owned fields: rerank (dated 2026-10-09, with the reason) and the error table; `DEPLOYMENT.md` → Rerankers |
| 2 | low | agent reviews | `rerankDocuments` decoded the whole list before the cap (4 MiB of `[0,0,…]`: ~268 MB, ~77 ms) | Counted without decoding: one pass over the already-validated value, commas outside strings and nested values; same rule (list → elements, `[]` → 0, anything else, absent or null → 1). 4 MiB list: 0 allocations, ~4.5 ms. A `json.Decoder` walk was measured and not taken: `Token()` allocates per string or non-zero number; decoding each element into a no-op `Unmarshaler` holds allocations flat but costs ~131 ms, more CPU than before. `promptCount` unchanged: `BACKLOG.md` → Limits, Counting prompts and embeddings inputs allocates per element |
| 3 | low | agent reviews | `accounting.SaturatingMul` untested (`return a*b` passed every test) | `TestSaturatingMul` at the int64 edge (mutation `return a*b` now fails) |
| 4 | medium | agent reviews | Only `endpoint_missing` must be remembered, and `Retain` wired on apply: neither tested (both mutations passed) | `TestOnlyAMissingEndpointIsRemembered` (missing model, refused credential, wrong path: nothing remembered, no warning); `cmd/kaiak` `TestAppliedConfigForgetsADroppedDeploymentsMissingEndpoint` through `newGraph`'s applier (drop and re-add a deployment: warned afresh). Both mutations now fail |
| 5 | low | agent reviews | `remember` extended a live entry on every failing attempt: with every deployment remembered, steady traffic warned once per episode, not once per interval | `remember` no longer extends a live entry: it ends one interval after it was set. Routing unchanged (a left-out deployment gets no attempt inside its interval). `GATEWAY.md` → Remembered per deployment: the interval runs from the answer that set it (2026-10-09). `TestMissingEndpointWarnsOncePerInterval` (steady traffic, 100 ms interval: 3 to elapsed/interval+1 warnings; the old code gave 1), `TestMissingEndpointEntryLastsOneInterval` (exact, synthetic clock) |
| 6 | low (docs) | agent reviews | Router-mode llama-server said to answer a missing model `404 File Not Found`; it answers `400 invalid_request_error`, `model 'X' not found` (`server-models.cpp`, `router_validate_model`) | `llama_server.go` comment and `GATEWAY.md` → Wrong path to a host corrected; behaviour unchanged. `BACKLOG.md` → llama-server router mode: a missing model |
| 7 | low (docs) | agent reviews | A wrong `base_url` on llama-server also reads as `endpoint_missing` on its non-core endpoints; the docs said so for vLLM only | `GATEWAY.md` (Wrong path to a host; An endpoint missing from a server — explicit), `DEPLOYMENT.md` → Clients, `LIVE-BACKENDS.md` → Reading failures (both passages) |
| 8 | low (docs) | agent reviews | The "Wrong model, path or credential" alert included `endpoint_missing`, now mostly client mistakes against healthy deployments | `endpoint_missing` dropped from it; new ticket alert "Endpoint missing on every attempt": `endpoint_missing` attempts in 30 min `unless` any other outcome on the same deployment. Upgrade note added |
| 9 | backlog | agent reviews | Chat to a llama-server encoder model (BERT-style reranker or embedder) answers `500` (`the current context does not support logits computation`), counted toward the circuit | Not fixed: added to `BACKLOG.md` → llama-server quirks, Client errors answered 500 (with the interim advice); one sentence in `GATEWAY.md` → An endpoint missing from a server, and in `DEPLOYMENT.md` → Rerankers |
| 10 | nit | agent reviews | The query's own estimate included the `:` and whitespace before its value; the spec says its value's | Aligned: the query is measured from its value's first byte (`accounting/estimate.go` `valueOffset`); test cases for whitespace around the colon and a query that is a media item |
| 11 | medium | Codex review | `promptOnlyUnits` (rerank and embeddings) took a `usage` with `completion_tokens` alone, or `prompt_tokens: null` beside it, as a report, the absent input read as 0: the record settled as exact zero usage (`tokens_in` 0, not estimated, cost 0) | Fixed: only a `usage` with `prompt_tokens` (a number) is a report; anything else is no report and the input estimate settles, flagged `estimated`. **Applies to embeddings too** (same helper, same purpose; the rule there predates the plan). `GATEWAY.md` → Accounting: Token counts (embeddings) and Rerank usage (2026-10-09); `DEPLOYMENT.md` says nothing contrary. Doc comments of `promptOnlyUnits`, `openAIUsage.parse`, `rerankUsage` |

**Tests added or changed:**
- `server`: `TestRerankRefusesTEIFormat`; `TestRerankDocumentsAreCountedWithoutDecoding`
  (nested and mixed elements, escapes, cross-checked against decoding; 0 allocations
  for 10 and 900 001 documents); `TestMissingEndpointWarnsOncePerInterval`;
  `TestMissingEndpointEntryLastsOneInterval`; `TestOnlyAMissingEndpointIsRemembered`;
  `TestEveryErrorCodeHasItsClass` learns `tei_format_unsupported`.
- `cmd/kaiak`: `TestAppliedConfigForgetsADroppedDeploymentsMissingEndpoint` (new
  `graph_test.go`).
- `accounting`: `TestSaturatingMul`; `TestEstimateRerankInput` expects the query's
  value alone, two cases added.
- `e2e` `TestRerank` → refused before any backend: a `texts` request to the
  llama-server reranker.
- Ported from the Codex review's reproductions, as ordinary tests:
  - `accounting` `TestRerankUsage`: `completion_tokens` alone (0 and 7) and
    `prompt_tokens: null` → the input estimate, `estimated`.
    `TestMissingUsageIsEstimatedFromContent` → embeddings: the same three usages and
    `{"total_tokens":40}` → estimated.
  - `server` `TestRerankUsageMissingIsEstimated` → a table: no usage, and a reranker
    answering `usage: {completion_tokens: 0}` — one record, `tokens_in` the request's
    `EstimateInput` total, `estimated`, cost `tokens_in` × $2/M.
  - `accounting` `TestEstimateRerankInputOfAnyQueryAndDocuments`: queries `null`,
    `false`, a number past int64, a string with escapes, an object with a repeated
    key, 9998 nested arrays around `"q"`; documents `["a","b","c"]` and
    `[null,{},[1,2,3]]` — the estimate is the body's plus the query's twice
    (`text(body) + 2*text(query)`, `LargestPrompt` the body's). The reviewer's formula
    measures the query's value alone, as the spec's rule does (finding 10); passing
    before the fix and after.
  - Without the fix (the old condition restored) every new usage case fails:
    `TestRerankUsage` ×3, the embeddings subtest ×3 (`total_tokens` alone passes, as
    before), `TestRerankUsageMissingIsEstimated/completion_tokens_alone`.
- Mutation checks run: `SaturatingMul` as `a*b`; the remembering guard matching
  model_missing, auth_failed and path_missing; `Retain` removed from `newGraph`;
  `remember` extending again — each fails its test.

**Suite (2026-10-09):**
- `scripts/check-gateway.sh`: green — gofmt, vet, staticcheck 2026.2.1, telemetry
  boundary, `go test -race -count=1` (every package `ok`, `kaiak/e2e` 121 s), the
  live-kit lint and self-test (`self-test passed for vllm, llama-server, openai,
  azure-openai, anthropic, azure-anthropic, vllm with two backends`), `gateway checks
  passed`.
- `scripts/check-all.sh` ×2: green both times — gateway checks passed; control
  `npm test` pass 629, fail 0, skipped 1; `npm run lint` `boundaries ok`; cross-half
  e2e `ok kaiak/e2e` (70.9 s, 70.3 s); `all checks passed`.

**Suite after finding 11 (2026-10-09):**
- `scripts/check-gateway.sh`: green — gofmt, vet, staticcheck 2026.2.1, telemetry
  boundary, `go test -race -count=1` (every package `ok`, `kaiak/e2e` 123.8 s), the
  live-kit lint and self-test (`self-test passed for vllm, llama-server, openai,
  azure-openai, anthropic, azure-anthropic, vllm with two backends`), `gateway checks
  passed`.
- `scripts/check-all.sh`: green — gateway checks passed (`kaiak/e2e` 119.8 s); control
  `npm test` tests 630, pass 629, fail 0, skipped 1; `npm run lint` `boundaries ok`;
  cross-half e2e `ok kaiak/e2e` (70.5 s); `all checks passed`.

**Green 3× in a row on the final code (24e6248, 2026-10-09):** the run above, then
two more `scripts/check-all.sh` runs by the main session — each `all checks passed`;
gateway `kaiak/e2e` 121.1 s and 117.9 s; control tests 630, pass 629, fail 0;
cross-half e2e `ok` 70.4 s and 74.6 s.
