# kaiak gateway operational audit

2026-09-25 · Reviewed the supplied plain copy at `/tmp/kaiak`.

Five operational problems were confirmed. The most consequential are repeated selection of a quota-exhausted backend despite healthy spare capacity, and admitting budgeted traffic before a new stateless replica receives its initial totals. Two request-estimation problems affect supported vision and completion-batch workflows. Attempt metrics also conceal completed failures until the entire retried request finishes.

## Scope and evidence

- Scope: `gateway/` and its `protocol/` contract, with `control/` examined and exercised as the counterpart. Control-plane durability was excluded.
- Read the requested project philosophy, rules, gateway/protocol specifications, architecture and deployment documentation, then traced admission, routing, providers, accounting, control synchronization, lifecycle and metrics.
- Ran the full `scripts/check-all.sh` successfully in `/tmp/kaiak-audit/work`, an isolated copy. This included gateway race tests, static checks, the live-test kit self-test, 472 control tests, TypeScript/boundary checks and the cross-half test with two gateways. No live vLLM or Azure endpoint was exercised.
- Added targeted reproduction tests and microbenchmarks only in that copy. Four component probes and an actual-binary startup probe passed under the race detector; their assertions establish the problematic behavior, not its correction.
- Final SHA-256 comparison found all 557 original files unchanged, with no additions or removals: [source-verification.json](source-verification.json). The requested `pgrep -fl "kaiak|fakebackend|sample/src/main"` check returned no matches after testing; no audit processes remain.
- Severity: **High** means substantial legitimate traffic failure or material loss of budget enforcement; **Medium** means a supported workflow fails, output is degraded, or operational detection is materially delayed. Frequency describes the stated scenario, not every installation. Findings are ordered by frequency, then impact.

Full-suite output, from [check-all.log](check-all.log):

```text
boundaries ok
==> cross-half e2e (sample control plane + two gateways)
ok   kaiak/e2e   53.326s
all checks passed
```

## 1. Inline images are counted as hundreds of thousands of text tokens

**FREQUENCY — daily for clients sending inline images; absent for text-only workloads.**
**Severity — Medium. Confidence — confirmed by reading and a running gateway-handler probe.**

**Location:** `gateway/internal/server/limits.go:27`; `gateway/internal/server/params.go:79`; `gateway/internal/accounting/meter.go:32`, `:184`, `:201`.

**Impact:** Valid vision requests can receive a gateway `429` before reaching a backend. With a larger or absent token limit, the same estimation error can instead reduce an omitted output limit to 256 tokens. If backend usage is unavailable, it also inflates estimated input accounting. These are consequences of applying a text-byte heuristic to encoded binary content, not a minor tokenizer approximation.

**Concrete scenario and evidence:** A vision-capable model has a 32,768-token context, a 16,384-token default output limit and a global limit of 100,000 tokens/minute. Submit a valid 512×512 PNG as an OpenAI `image_url` data URL, with `detail: "low"`. The test image was deliberately high-entropy: 1,049,622 PNG bytes and 1,399,679 JSON bytes, comfortably below the configured 4 MiB body cap. The gateway returned:

```text
HTTP 429; backend calls=0
Request too large: it needs 350176 tokens (input estimate plus output limit
per sequence) and the global limit is 100000 tokens per minute.
```

Both admission and default-output fitting use approximately `len(entire request JSON)/4`, including the base64 image. Compressibility therefore changes the supposed token cost of an image of identical dimensions. Retrying cannot resolve this deterministic rejection. Normal backend usage reports correct final accounting when present; the bad estimate still controls admission, and survives into settlement when usage is missing.

**Fix sketch:** Separate request-byte limits from token estimation. Estimate textual input from relevant text fields, including tools, and image input with an explicit model-aware policy for dimensions/detail, with a documented fallback where exact rules are unavailable. Reuse that semantic estimate for admission, default-output fitting and partial accounting. Add tests comparing different encodings/compressibility of the same image dimensions, plus missing-usage settlement.

## 2. Completion batches silently lose most of their default output allowance

**FREQUENCY — daily for completion batch jobs that omit the output limit and cross the aggregate-size threshold.**
**Severity — Medium. Confidence — confirmed by reading and executing the request parser and parameter stage.**

**Location:** `gateway/internal/server/params.go:79`, `:97`.

**Impact:** Supported batched `/v1/completions` requests get much shorter answers than equivalent individual requests, without a validation error. Batch users can see truncated work despite every prompt fitting comfortably in the model context.

**Concrete scenario and evidence:** Submit 16 prompts, each 12,000 bytes of ordinary text, to a model with a 32,768-token context and default output limit 16,384. This fits the default 16-sequence ceiling. Each prompt is roughly 3,000 input tokens under the gateway's own heuristic, leaving room for the configured output. The parameter-stage probe instead observed:

```text
16 prompts x 12000 bytes, context=32768 output default=16384:
injected output per prompt=256
```

The code subtracts the entire batch's roughly 48,000-token estimate from one context window, then applies the 256-token floor. A context window belongs to each generated sequence; the batch's total is relevant to aggregate admission, not the per-prompt context calculation. Explicitly setting an output limit avoids this particular default-injection path, so this does not affect all batches.

**Fix sketch:** Keep separate estimates for total input and maximum input per prompt. Use the latter to fit the common completion output limit, and the total for token reservations. Count integer-token prompts directly. Test single versus batched equivalent prompts, unequal prompt lengths and token-ID arrays.

## 3. A quota-exhausted backend attracts new requests and exhausts the spillover budget

**FREQUENCY — occasional; requires sustained quota throttling on one eligible deployment while another has active requests and spare capacity. Can recur daily in quota-constrained Azure workloads.**
**Severity — High. Confidence — confirmed by reading and a running two-backend probe.**

**Location:** `gateway/internal/routing/routing.go:347`; `gateway/internal/server/upstream.go:119`, `:233`, `:427`; `gateway/internal/server/retrybudget.go:14`, `:66`.

**Impact:** Most requests can fail with `429` despite usable capacity on another deployment. Client retries add load and latency without removing the persistent bad routing choice.

**Concrete scenario and evidence:** Deployment A immediately returns `429` with `Retry-After: 60`; B is healthy with one active slot and additional capacity. The probe held a router slot on B to represent an existing long stream, then sent 100 sequential requests within the retry-budget window:

```text
quota-limited backend calls=100
successful spillovers=24
client 429=76
healthy backend calls=24
```

Least-inflight selection repeatedly prefers A's zero active requests to B's occupied slot. A `429` is correctly neutral for the failure circuit, but refusal avoidance applies only to that individual request; no shared quota cooldown keeps A out of subsequent first attempts. Retrying onto B spends the model-wide retry budget, whose steady-state allowance is 20% of attempts. Once it is constrained, requests return A's `429` without trying the healthy deployment. The exact 24/76 split is this probe's result, not a universal production ratio.

**Fix sketch:** Track a bounded quota cooldown at the appropriate configured deployment/resource scope. Honor supported retry-delay headers and exclude the throttled target from new dispatches until eligible again; define a conservative bounded delay for throttles without one. Preserve circuit neutrality for quota responses and the retry budget's protection against widespread faults. Test a long healthy stream alongside a throttled target, cooldown expiry and the all-targets-throttled case. Simply increasing retries leaves the routing mechanism intact.

## 4. A new stateless replica serves budgeted requests before it knows existing spending

**FREQUENCY — occasional; a startup ordering gap exists on every fresh stateless boot, but meaningful exposure requires traffic before initial totals arrive. Delayed control-plane streams/usage responses during a rollout make it substantial.**
**Severity — High. Confidence — confirmed by reading, a component probe and an actual gateway-binary reproduction.**

**Location:** `gateway/cmd/kaiak/main.go:394`, `:431`, `:452`; `gateway/internal/server/admin.go:57`; `gateway/internal/limits/limits.go:182`, `:311`.

**Impact:** A new replica can accept paid traffic for a budget the control plane already knows is exhausted. Multiple new replicas can each temporarily operate from an empty spending baseline. This is additional to the documented overshoot from in-flight requests and unreported recent usage.

**Concrete scenario and evidence:** Start a gateway without a data directory against a control plane whose monthly budget is already spent. Permit configuration and status traffic, but hold initial `/v1/stream` and `/v1/usage` responses to represent delayed background synchronization. Wait for the gateway's readiness check, then issue a paid request. Release synchronization and repeat:

```text
binary is ready with spent budget, initial stream/usage delayed: HTTP 200
same binary after first totals: HTTP 429
```

Boot obtains configuration before starting the listeners, but background synchronization starts separately. Readiness checks only loaded configuration and drain state. The shared limiter starts with no pushed totals, and a missing counter is treated as zero spending, even though no complete initial totals snapshot has been received. A healthy configuration fetch also means this is not immediately treated as a prolonged control-plane outage. With prompt totals delivery, the gap can be too short for normal readiness polling to expose; the test deliberately prolonged it rather than claiming every rollout overspends.

**Fix sketch:** Represent “initial totals unknown” separately from “complete totals contain no usage.” Require matching initial totals before admitting requests governed by shared spending budgets, either through a boot snapshot containing both or an asynchronous admission/readiness gate. Preserve the intended free-model seed behavior and keep control-plane I/O off the request path. Test exhausted budgets, delayed initial totals, version matching and multiple fresh replicas.

## 5. Completed backend failures remain invisible in attempt metrics while the retry streams

**FREQUENCY — occasional for the failure-detection impact: a failed first attempt followed by a long successful response. TTFT publication is delayed on ordinary streams too.**
**Severity — Medium. Confidence — confirmed by reading and scraping metrics during a running retry stream; alert consequences follow from that timing.**

**Location:** `gateway/internal/server/metrics.go:14`, `:24`, `:29`, `:66`; `gateway/internal/server/upstream.go:330`. Documentation disagreement: `docs/specs/GATEWAY.md:1547`.

**Impact:** Backend error rates and retry counters lag the events operators need to see. During a long generation, a failed backend can appear healthy in those series; when streams end, the historical failures appear in a later rate window. TTFT dashboards also omit currently running requests whose first token has already arrived.

**Concrete scenario and evidence:** A returns `500`; its retry on B sends a streaming event and remains open. Scrape after that event, then close the client and scrape again:

```text
failed first attempt visible while successful retry streams=false
visible after client request ends=true
```

The probe inspected `kaiak_upstream_attempts_total{backend="local",deployment_model="first",outcome="server_error"}`. Attempt release immediately reports to the circuit breaker, but only stores the result/duration for metrics. `observeRequest` publishes every attempt and retry at the end of the whole client request. Thus a multi-minute successful retry postpones evidence of the earlier failure by minutes. Circuit state is updated promptly and can still expose sustained faults; intermittent faults need not open it. The specification explicitly promises attempt-metric updates when the attempt releases its slot, which the implementation does not satisfy.

**Fix sketch:** Emit each attempt's outcome/duration when it is released, retry counts when retries are sent, and TTFT when first content arrives. Keep final request duration, settled usage and output-rate measurements at request completion. Test that scrapes observe the completed first attempt while a later attempt remains open, with exactly-once counting after cancellation and completion.

## Performance measurements

Measured on Darwin/arm64, Apple M5 Max, without the race detector for benchmarks. These isolate local work; they do not predict Linux pod throughput, TLS cost, network behavior or model-server latency.

| Operation | Time | Allocation per operation |
| --- | ---: | ---: |
| Existing full-queue routing scan: 5 models × 20 shared backends, 500 waiters/model | 5.34–5.47 µs | 0 B, 0 allocations |
| Owned-field parsing, 4 KiB text body | 9.71–9.91 µs | ~22.5 KB, 29 allocations |
| Owned-field parsing, 64 KiB text body | 124.1–124.5 µs | ~337.9 KB, 33 allocations |
| Owned-field parsing, 256 KiB text body | 459.5–471.6 µs | ~1.32 MB, 37 allocations |
| Owned-field parsing, 1 MiB text body | 1.21–1.24 ms | ~5.26 MB, 42 allocations |
| Accounting meter, one 191-byte content event | 0.821–0.834 µs | 272 B, 6 allocations |

No routing or accounting bottleneck was demonstrated at the proposed fleet size. Large request parsing deserves capacity planning: at 100 requests/s of 64 KiB, this stage alone allocates approximately 34 MB/s. That is allocation throughput, not retained heap, and excludes provider rewriting and the rest of the pipeline. It is not presented as an independently established production defect.

Evidence: [routing-bench.log](routing-bench.log), [data-path-bench.log](data-path-bench.log).

## Reproduction and review limits

The copied gateway contains `internal/server/audit_probe_test.go`, `internal/accounting/audit_bench_test.go` and `e2e/audit_probe_test.go`. From `/tmp/kaiak-audit/work/gateway`, rerun with a writable Go cache and permission to bind local sockets:

```sh
go test -race -run '^TestAudit' -v ./internal/server
go test -race -run '^TestAuditBoot' -v ./e2e
go test -run '^$' -bench BenchmarkDispatchFullScan -benchmem -count=3 ./internal/routing
go test -run '^$' -bench '^BenchmarkAudit' -benchmem -count=2 ./internal/server ./internal/accounting
```

Probe outputs: [probes.log](probes.log), [boot-probe.log](boot-probe.log). No source fixes were attempted.

Client compatibility was checked against gateway tests and request/response code, with the current official [OpenAI Python retry implementation](https://raw.githubusercontent.com/openai/openai-python/main/src/openai/_base_client.py) consulted for retry behavior. No SDK-version matrix, coding-assistant product integration or real Azure quota test was run. Client retries are version- and configuration-dependent; they cannot repair deterministic request estimation, and can amplify the quota-routing scenario.

Several operational constraints are documented tradeoffs rather than additional findings: per-key concurrency 16 and maximum 16 generated sequences require batch clients to bound parallelism; embedding input count is capped separately; minute limits are replica shares rather than a perfectly pooled bucket; paid budgets allow in-flight/reporting overshoot; cancelled streams without final usage are estimates, not a cloud billing reconciliation; long reasoning calls need suitable timeouts; termination grace must accommodate the configured drain and flush reserve. Responses/Assistants/Batch API absence is explicitly outside the supported API contract.

## Areas checked and found sound

Within the reviewed paths and executed tests, apart from the findings above:

- **Pipeline and passthrough:** Supported chat/completion/embedding routes pass through authentication, admission, routing and accounting. Provider rewriting preserves unknown OpenAI-format fields while owning model/output/usage edits. Existing coverage exercises structured/tool content and usage shapes, including cached and reasoning tokens.
- **Streaming and retries:** Streaming usage is requested upstream and filtered according to client options. Retry decisions distinguish pre-relay failures from responses already committed to clients, avoid replaying a stream after visible output, and distinguish client cancellation from backend failure. Retries are bounded, and uncertain sent attempts receive explicit partial/estimated accounting.
- **Backend lifecycle:** Concurrency slots, bounded queues, cancellation, circuit opening/half-open probing and configuration-driven routing changes have substantive coverage. Circuit results are recorded promptly even where attempt metrics lag. Full-queue dispatch scanning was inexpensive in the measured fleet-sized case.
- **Accounting changes and windows:** Request accounting retains the applicable configuration/pricing context; exact integer monetary units avoid floating-point accumulation. Shared totals have configuration/window/generation handling and tests for local usage reconciliation. Window-boundary and replica behavior have explicit contracts; distributed budgets are not advertised as transactional reservations.
- **Control protocol and operation:** Gateway and counterpart exercise versioned schemas, snapshots, streamed updates, usage acknowledgements and totals through shared fixtures and the passing cross-half test. Once initialized, background control-plane communication does not block the client request path. Stateless mode and opt-in cache/spool responsibilities are clearly distinguished.
- **Shutdown and observability:** Readiness becomes false during drain; request draining and usage flushing have separate time allowances and executable coverage. Metrics use controlled configuration labels rather than client keys or prompt content. Request/error/accounting logs avoid those sensitive payloads in the reviewed paths. Deployment guidance covers probe ports, read-only operation, timeouts and drain sizing; the concrete metric-timing discrepancy is recorded above.
