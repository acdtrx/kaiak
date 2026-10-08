# Step 4 — e2e

**Status:** done — reviewed and committed 2026-10-09

## Intent

Rerank is proven end to end against the fakes:
- through a running gateway;
- across both halves, with a rerank request settling at the sample control plane.

## Files likely touched

- `gateway/internal/fakebackend/`: a llama-server-shaped rerank answer (`object`,
  `usage`, `results` without `document`), selectable beside step 3's vLLM shape.
- `gateway/e2e/`:
  - rerank on a `vllm`-typed and a `llama-server`-typed backend: answer relayed with
    the public model name, usage record with `tokens_in` and its cost, and the log
    line's operation;
  - the cap refused before the backend;
  - usage missing, so estimated and flagged;
  - `endpoint_not_served` for a model with only `openai-compatible` deployments.
- `gateway/e2e/sample_test.go` (build tag `crosshalf`): a rerank request settles at
  the sample, and its totals and cost show there.

## Decisions made during planning

- **The fake's two answer shapes come from the servers' sources** recorded in step 1.
  No live captures: the DGX is busy, and step 8 checks the real shapes.

## Acceptance criteria

- The e2e tests above pass under `-race`, uncached (`scripts/check-gateway.sh`).
- The cross-half e2e passes.
- **Phase 1 end:** `scripts/check-all.sh` green and recorded, with no expected reds.

## Result

### What changed

- `gateway/internal/fakebackend/`:
  - `rerank.go`: `RerankShape` — `VLLMRerank` (the zero value, step 3's answer) and
    `LlamaServerRerank`: `model`, `object: "list"`, `usage`, `results` with `index`
    and `relevance_score` only, no `id`, as llama.cpp's `format_response_rerank`
    builds it (`tools/server/server-common.cpp`, master, re-read for this step). The
    scoring, sort and `OmitUsage` are shared. `top_n` read per server: vLLM keeps all
    at 0; llama-server cuts to `top_n` whenever one is sent, 0 to none, and keeps all
    without one (`json_value(body, "top_n", documents.size())`, then
    `resize(min(top_n, n))`); `null` reads as absent in both.
  - `fakebackend.go`: `SetRerankShape`, held beside the models list; the package
    comment names both shapes.
- `gateway/e2e/rerank_test.go` (new), `TestRerank`: one gateway, a `vllm` backend
  and a `llama-server` backend, each on a fake backend of its own answering in its
  server's shape, an `openai-compatible` one, `max_rerank_documents` 3, rerankers at
  $2 per million input tokens:
  - per type: answer `200` JSON under the public model name (the deployed name
    nowhere), the panda document first, cut to `top_n` 2, the answer's members and
    each result's exactly the server's (`id,model,results,usage` and
    `document,index,relevance_score`; `model,object,results,usage` and
    `index,relevance_score`), `usage` relayed; the backend got `/v1/rerank` with the
    client's bytes but the model (`top_n`, `return_documents` untouched); the log line
    `gen_ai.operation.name` `rerank`, `gen_ai.request.stream` false, input 40,
    output 0, `kaiak.usage.cost_usd` 0.00008, not estimated or partial; the usage
    metrics' input tokens and cost series under operation `rerank`;
  - per type, usage missing (`OmitUsage`): input = `EstimateInput(Rerank, body)` (more
    than the body alone), output 0, estimated, not partial, cost from the estimate;
  - 4 documents → `400 invalid_value` on `documents`, naming no document; the
    `openai-compatible` model → `400 endpoint_not_served` naming `/v1/rerank`;
    neither fake received a request.
  - Checked: answering the vllm row in llama-server's shape fails the member checks.
- `gateway/e2e/sample_test.go` (`crosshalf`): the config gains a `reranker` on `ls`
  (llama-server, the working fake answering rerank in llama-server's shape) at $1
  per million input tokens, and `global.max_rerank_documents` 2. New subtest before
  the budget one: a rerank on gw-a (40 prompt tokens) answers in llama-server's
  shape; its record at the sample is `tokens_in` 40, the rest 0, on `ls`, 40000
  nano-USD, reported; research's USD total shows it, and the hourly token total
  counts it; 3 documents are refused `invalid_value` on both gateways, no backend
  request — the field validated by `kaiak-control`, pushed, applied. The budget
  subtest now waits for research's total at 40000 + 150000 (`researchSpent`, kept
  like `tokens`): the rerank stays below the $0.0001 budget, so the probe and the
  priced spend behave as before.
- `gateway/internal/server/rerank_test.go`: the llama-server case of
  `TestRerankThroughThePipeline` answers in llama-server's shape.
- `docs/specs/GATEWAY.md` → Providers → Rerank answers: an event stream answering a
  rerank request is no rerank answer — never complete, so once relayed it ends
  `upstream_incomplete`, a backend failure (settled 2026-10-08). The code does this:
  `rerankStream.complete()` is false, `upstreamResponse.end` turns the EOF into
  `ErrIncomplete`, the relay ends `upstream_incomplete` with the connection cut, and
  `classifyAttempt` counts it toward the circuit. Covered by `provider`
  `TestRerankAnswerCompleteness/an event stream` (rerank's part) and by the generic
  incomplete-response tests (`server` `timeouts_test.go`, `messages_test.go`); no new
  test. An event stream with no data event is the existing "ended before its first
  event" case (`502 upstream_unavailable`), hence "once relayed".
- `docs/TECH-STACK.md` → Testing and `docs/ARCHITECTURE.md` → `fakebackend`: rerank
  in both shapes.
- `gateway/internal/accounting/accounting.go`: `UsageRecord.Operation`'s comment
  lists `rerank` (step 3 left it at three operations).

### Design choices

- **The shape is the backend's, not the reply's.** Which server a fake plays is a
  property of the backend, like its models list, so `SetRerankShape` sits beside
  `SetModels` and survives `SetReply` — the tests script usage and faults per reply
  without restating the server. One e2e run has a fake per rerank backend. Rejected:
  a `Reply` field (lost on every `SetReply`), choosing by model name or path (both
  servers serve `/v1/rerank`).
- **`cmd/fakebackend` has no shape flag yet:** nothing runs a rerank through the
  process. Step 6's self-test, when it adds rerank checks for `llama-server`, needs
  one (e.g. `-rerank-shape`).
- **The e2e test does not use the `passthrough` helper:** its checks need the answer
  body (shape, results), which the helper's `check` does not get, and rerank has no
  stream or tier cases. It keeps the suite's conventions: `backendEntry`, the
  settled log line, `openAIErrorCode`, metric series.
- **The cross-half cost shows in research's USD window,** the one total the sample
  keeps money in; the rerank is priced below the budget so the budget subtest keeps
  its meaning.

### Suite (2026-10-09)

`scripts/check-all.sh`, twice (each runs `scripts/check-gateway.sh` first), both
green; no expected reds — phase 1 ends here:

- Run 1 (3 m 31 s): gofmt, vet, staticcheck 2026.2.1, telemetry boundary; `go test
  -race -count=1` every package `ok` (`e2e` 123.4 s, `internal/server` 15.0 s); live
  kit `self-test passed for vllm, llama-server, openai, azure-openai, anthropic,
  azure-anthropic, vllm with two backends`; `gateway checks passed`; control
  `tests 630, suites 45, pass 629, fail 0, skipped 1`; `boundaries ok`; cross-half
  `ok kaiak/e2e 70.419s`; `all checks passed`.
- Run 2 (3 m 25 s): the same stages `ok` (`e2e` 118.3 s, `internal/server` 14.0 s);
  self-test passed for the same kinds; `gateway checks passed`; control `tests 630,
  pass 629, fail 0, skipped 1`; `boundaries ok`; cross-half `ok kaiak/e2e
  70.426s`; `all checks passed`.
- After the runs only a comment in `sample_test.go` was reflowed (gofmt and
  `go vet -tags crosshalf` clean).
