# kaiak independent audit

Date: 2026-09-25. Scope: the supplied plain copy at `/tmp/kaiak`, for approximately 20 vLLM hosts, Azure OpenAI, many owners and keys, monthly dollar/token budgets, and multiple gateway replicas on Kubernetes.

**Assessment:** fix the budget-enforcement and connection-lifecycle findings before using this deployment to enforce real spending. The existing suite is substantial and passes, but the additional probes reproduced defects outside its coverage. No critical finding was established. Findings below comprise **6 high, 3 medium, and 1 low**, ordered by severity. The control-plane replication finding is conditional on running multiple control-plane instances; multiple gateways alone do not trigger it.

The source was not modified. Tests ran in `/tmp/kaiak-audit/source`; probes and logs are retained alongside this report. All 448 source-file SHA-256 hashes matched the initial manifest after testing. No real provider credentials or paid backend calls were used.

## Findings

### 1. High — totals for a rejected config can erase enforcement of a spent budget

**Locations:** `gateway/cmd/kaiak/main.go:481`; `gateway/internal/limits/limits.go:195`; `gateway/internal/limits/shared.go:73`; `gateway/internal/control/client.go:375`; `gateway/internal/config/loader.go:123`.

**What is wrong:** `limitsTotals` discards `config_version`. The limiter accepts the new complete window list, resets a retained limit's base to zero when its identity is absent, and retires acknowledged local generations. A rejected config leaves the gateway enforcing old limit identities while the control plane counts only the new ones. Acknowledging delivery does not establish that the supplied totals cover the gateway's retained policy.

**Scenario:** v1 has a $1/month team limit covering `[m1]`. Usage reaches $1.10. Publish v2, expanding that limit to `[m1,m2]` and adding an Azure backend whose credential environment variable is missing on one gateway. That gateway correctly retains v1. The control plane sends the v2 window and acknowledges the gateway's usage. Its old `[m1]` base becomes zero and its acknowledged local amount disappears. It admits more requests; each subsequent acknowledged batch can reset its effective usage again. The healthy stream prevents outage refusal. This can continue throughout the month, beyond the documented reporting-interval overshoot.

**Evidence:** `TestAuditMismatchedTotalsEraseRetainedBudget` reproduced `used before=1100000000; after newer config's totals=0; next request admitted`. The gateway-only credential rejection and continued totals consumption are confirmed by reading the apply/client paths.

**Fix sketch:** retain config identity through totals consumption. Do not clear a counter or retire its local usage unless the replacement totals demonstrably include that policy's usage. Either supply totals for retained policies or explicitly refuse affected budgeted traffic during an incompatible config mismatch. Test rejection, subsequent acks, recovery, and a month boundary together.

**Confidence:** confirmed by running the limiter mechanism and reading the production wiring. This exposes an unsafe consequence of the documented matching rule, not merely an undocumented implementation discrepancy.

### 2. High — batch boundaries can permanently double-count local usage and falsely exhaust budgets

**Locations:** `gateway/internal/accounting/accounting.go:112`; `gateway/internal/control/usage.go:130`; `gateway/internal/control/usage.go:154`; `gateway/internal/limits/limits.go:329`; `gateway/internal/limits/window.go:216`.

**What is wrong:** accounting publishes a record to the sender before the limits finisher settles it. Publishing the record that fills a batch synchronously seals the current generation. The later limits settlement tags that same record with the *next* generation. Its ack adds it to the pushed base but cannot remove this incorrectly tagged local copy. An interval seal racing settlement produces the same problem.

**Scenario:** the record filling the default 500-record batch consumes the remaining part of an owner's allowance. Its batch closes generation G; local settlement enters G+1. After the ack, the owner's usage is counted in both the control total and G+1. If this causes rejection and no other traffic produces another batch, empty seal ticks return without advancing anything. Fresh totals repeat the duplicate; the phantom local amount can also carry into later shared windows. Recovery requires another batch to clear the generation or a restart, rather than simply the next totals push.

**Evidence:** a reduced `BatchMaxRecords=1` probe uses the real `Client.Record`/`SealUsage` ordering: 600 actual tokens against a 1,000-token limit become 1,200 locally after acknowledgment; a one-token request is refused. Reapplying totals does not fix it. The same branch executes for the 500th record under defaults.

**Fix sketch:** give each record an immutable accounting-generation association and make local settlement/publication/sealing agree atomically. Preserve per-attempt attribution when one request produces several records. Simply moving settlement before the sink without coordinating the seal creates the opposite race. Test the final record in a full batch, interval sealing, fast acknowledgments, and no later traffic.

**Confidence:** confirmed by running code; deterministic at full-batch boundaries.

### 3. High — slow bodies and stalled relays can hold resources indefinitely, including without authentication

**Locations:** `gateway/internal/server/listener.go:43`; `gateway/internal/server/inbound.go:36`; `gateway/internal/provider/openai.go:92`; `gateway/internal/server/upstream.go:396`.

**What is wrong:** the HTTP server sets only a header deadline. There is no request-body deadline or client write-progress deadline. The provider removes its only response timer after the first event and has no subsequent read-progress deadline. Backend caps/queues cannot bound connections held before routing; routed requests retain slots until the relay ends. A byte-size cap is not a time bound.

**Scenario A, reproduced:** send complete HTTP headers for `POST /v1/chat/completions`, omit Authorization, declare `Content-Length: 1`, and never send that byte. Although authentication runs before application body reading, Go's HTTP response machinery tries to consume the remaining small body while writing the rejection. The connection remains blocked without a body deadline. Repeating this consumes connections and goroutines without a valid key or rate-limit reservation.

**Scenario B:** a valid client uploads most of a 16 MiB body and stalls before limits run; or stops reading a large response. Separately, a backend sends one SSE event and then stalls. These requests have no progress timeout. With enough routed stalls, all slots remain occupied, queues time out, accounting remains unsettled, and circuits never receive a completed failure.

**Evidence:** the real-listener socket probe received no 401 after its bounded 250 ms observation; inspection confirms no later body deadline exists. The listener probe reports zero body/write timeouts. Post-first-event unbounded reads/writes are confirmed by code inspection; no large exhaustion attack was run.

**Fix sketch:** bound request-body reads and rejected-body disposal, apply client write-progress deadlines, and add configurable upstream idle/progress timeouts. Keep a progressing long stream valid without imposing a short total duration. Bound connections/admissions before full body buffering as well as backend work. Test slow unauthorized bodies and cancellation of stalled reads/writes.

**Confidence:** confirmed by running the unauthorized socket path and reading the remaining lifecycle paths.

### 4. High — opening the control stream can hang forever and prevent config updates/revocation

**Locations:** `gateway/internal/control/client.go:152`; `gateway/internal/control/stream.go:41`; `gateway/internal/control/stream.go:53`; `gateway/internal/control/transport.go:141`.

**What is wrong:** `openStream` runs before the stream idle timer is installed. The default HTTP client has neither a total timeout nor a response-header timeout for this request. A server or intermediary that accepts the request but sends no headers traps the config follower indefinitely; reconnect backoff is never reached.

**Scenario:** after a healthy snapshot, `/v1/stream` is accepted by a stuck proxy/backend handler. Operators revoke a key or lower a budget on the control plane. The gateway remains ready on its old config and never reconnects to get the change. If usage acks keep arriving, they continue refreshing contact, so even money-limited traffic need not fail closed. If the whole control plane stops responding, money traffic eventually refuses, but unbudgeted traffic retains stale auth indefinitely.

**Evidence:** a fake HTTP endpoint accepted the stream request and withheld headers. With `IdleTimeout=20ms`, the follower was still blocked after 150 ms and returned only when the probe explicitly canceled its parent context.

**Fix sketch:** bound connection/response-header establishment separately from the lifetime of the SSE body, then hand off to the existing idle timer. Test header stalls, protocol-error bodies that stall, cancellation, and reconnect after recovery.

**Confidence:** confirmed by running code and reading default client construction.

### 5. High — multi-output requests bypass the intended token reservation bound

**Locations:** `gateway/internal/server/inbound.go:15`; `gateway/internal/server/params.go:45`; `gateway/internal/server/limits.go:25`; `gateway/internal/provider/openai.go:139`.

**What is wrong:** the reservation adds one effective output limit to the input estimate. `n`, completion prompt batches, and defaults affecting output multiplicity are passed through without increasing the reservation. A backend output limit is per output sequence, not necessarily per HTTP request. vLLM documents those semantics for [`max_tokens` and `n`](https://docs.vllm.ai/en/latest/api/vllm/sampling_params/).

**Scenario:** an owner has 1,000 tokens left. Submit chat with `max_tokens:100,n:64` and a short prompt to a backend allowing that `n`. The request reserves about 100 output tokens and is admitted, but can generate 6,400 output tokens. A single HTTP/backend slot can therefore consume many sequences. Final reported usage is counted, but only after the work and spending occurred; this is not the documented input-estimation error. Batched completions multiply the same gap.

**Evidence:** the pipeline probe reports exactly 100 reserved output tokens for both `n=1` and `n=64`; the raw multiplicity field remains intact. No live vLLM generation was performed.

**Fix sketch:** validate output multiplicity as an admission concern and reserve the effective number of generated sequences times their output cap, including declared defaults and completion batches. Explicitly cap/refuse unsupported multiplicity such as backend-specific search modes when a defensible reservation cannot be calculated. Use overflow-checked arithmetic.

**Confidence:** confirmed by running gateway code; backend semantics verified against primary documentation.

### 6. High, conditional — two control-plane instances can count the same batch twice despite atomic storage

**Locations:** `control/kaiak-control/src/usage/index.ts:101`; `control/kaiak-control/src/usage/index.ts:127`; `control/kaiak-control/src/usage/index.ts:199`; `control/kaiak-control/src/storage/types.ts:93`.

**What is wrong:** de-duplication reads `lastBatch` before a separate `saveCountedBatch`. Both serializing queues are private to one `createUsage` instance. The storage contract requires an atomic write of totals/records/cursor but does not require a conditional de-duplication check within that transaction. An otherwise compliant durable store cannot supply cross-instance exactly-once behavior through this API alone.

**Scenario:** deploy two real control-plane pods sharing storage, or overlap old/new pods during a rollout. A delayed original usage POST reaches A and its gateway retry reaches B. Both read the old cursor, both classify it as new, and both atomically add its cost and advance the same cursor. Owners are billed twice and their budgets exhaust early. This does **not** require two gateways sharing an instance ID.

**Evidence:** two `createUsage` objects sharing the supplied atomic memory store concurrently accepted the same fixture batch. Both returned `first`; two records were stored and the token total became 2,104 instead of 1,052.

**Fix sketch:** move compare/de-duplicate/add/cursor advancement into one store transaction with an explicit duplicate outcome, or enforce a single fenced control-plane writer. Before claiming control-plane HA, also coordinate config version allocation and cross-process config/totals subscriptions; those are process-local too. Document and enforce single-writer deployment until then.

**Confidence:** confirmed by running code. Conditional on multiple control-plane instances, including rollout overlap; several gateways talking to one control-plane instance are covered by the existing serialization.

### 7. Medium — a clean HTTP EOF between SSE events is falsely recorded as a complete response

**Locations:** `gateway/internal/sse/sse.go:82`; `gateway/internal/server/upstream.go:398`; `gateway/internal/server/upstream.go:298`; `gateway/internal/accounting/meter.go:167`.

**What is wrong:** HTTP EOF is treated as successful completion without checking OpenAI stream termination. A stream can end on an event boundary after partial content, with no terminal choices or `[DONE]`, and still get `partial=false` and a successful circuit outcome. The SSE parser checks framing truncation, not application-level completion.

**Scenario:** a backend/proxy emits one complete content event, then ends its HTTP response normally because its generator failed. The gateway closes cleanly, estimates usage if the final usage event never came, and reports success. Clients that iterate until EOF can accept an incomplete answer; operations and accounting lose the partial/failure signal promised by the spec.

**Evidence:** a real fake backend returned one `delta.content` event with no finish reason or `[DONE]`. The relay produced `relay_end="" partial=false`.

**Fix sketch:** track provider-level terminal state separately from HTTP/SSE framing. EOF before a valid terminal state must settle partial usage and surface an upstream failure; do not retry after bytes reached the client. Distinguish normal content-filter termination from a broken stream.

**Confidence:** confirmed by running code.

### 8. Medium — accepted inline URL credentials are exposed on the unauthenticated sample page

**Locations:** `protocol/schema/config.schema.json:192`; `gateway/internal/config/schema.go:22`; `control/sample/src/page/sections.ts:167`; `control/sample/src/page/index.ts:54`.

**What is wrong:** the backend URL pattern permits URL userinfo, including passwords. Both validators accept it even though the contract requires provider secrets to be environment references. The sample page renders the whole URL without authentication. HTML escaping prevents markup injection, but does not hide a credential. The URL also enters config snapshots and last-known-good storage.

**Scenario:** an operator supplies `http://user:password@backend:8000/v1` for a backend behind basic auth. It passes validation. Anyone able to reach the sample page reads that password, even though key hashes and the normal API-key environment values are omitted. This requires credential-bearing config; it does not expose correctly configured `api_key_env` values.

**Evidence:** Go and TypeScript both accepted a URL containing the synthetic `audit-provider-secret`; unauthenticated `GET /` returned 200 and included it verbatim.

**Fix sketch:** parse and reject userinfo in backend base URLs in both halves and shared fixtures. Keep credential transport in explicit secret references. Render a credential-free URL defensively and review other displays of configured URLs.

**Confidence:** confirmed by running both validators and the page.

### 9. Medium — the sample watcher misses Kubernetes-style projected config updates

**Location:** `control/sample/src/config-file/index.ts:132`.

**What is wrong:** the directory watcher reloads only when the event names the configured file, or has no name. A projected-volume-style update atomically switches `..data`; the user-visible `config.json` symlink does not change. Those events are filtered out.

**Scenario:** mount the sample config as a projected ConfigMap file and update a key revocation or spending limit. The on-disk file resolves to new content, but the sample keeps publishing its previous config. Gateways remain connected and appear healthy, so this can leave stale access/budgets indefinitely. Gateway file mode's explicit SIGHUP contract is separate and is not implicated.

**Evidence:** an isolated `config.json -> ..data/config.json` setup switched `..data` from a version containing limit 1,000 to one containing 2,000. Reading the path returned 2,000, but publication count stayed at one and the only reload trigger was `startup`. The successful probe ran with filesystem watching enabled; no Kubernetes cluster was used.

**Fix sketch:** recognize the projected volume's atomic directory/symlink changes or debounce relevant directory changes and rely on the existing unchanged-content check. Preserve event-driven reloads and test symlink replacement as well as ordinary rename-over-file saves.

**Confidence:** confirmed by running the symlink-update mechanism; Kubernetes deployment consequence follows from that update shape.

### 10. Low — aliases sharing a deployment emit duplicate circuit metric samples

**Location:** `gateway/internal/metrics/ops.go:158`.

**What is wrong:** the circuit gauge iterates every public model/deployment and emits a closed sample whenever the deployment is absent from the open map. It does not mark closed deployments as already emitted. Two aliases for the same backend/model therefore emit identical metric names and label sets in one scrape, violating series uniqueness in the exposition.

**Scenario:** expose `llama` and `alias` with different defaults/access rules, both targeting `local/llama`. While its circuit is closed, `/metrics` emits the same `kaiak_circuit_open{backend="local",deployment_model="llama"} 0` twice. Consumers may reject or discard duplicate samples; observability changes with alias count and circuit state. Prometheus ingestion behavior was not exercised.

**Evidence:** the registry probe counted two identical sample lines from one scrape.

**Fix sketch:** collect the distinct configured `DeploymentID`s first, then emit exactly one value for each, including any open deployments that need reporting.

**Confidence:** confirmed by running the metric producer.

## Deployment fitness and explicit limitations

These points distinguish documented tradeoffs and delivery gaps from the defects above.

- **The sample is not an authoritative production billing service.** `control/sample/src/app/index.ts:43` creates an in-memory store. Restarting it loses acknowledged totals and de-duplication state; its recent-record view is bounded. The separate real control plane needs durable storage and a retention policy. Its page is intentionally unauthenticated, exposing owner identities, topology, limits and usage; keep that surface private. Findings 6 and 8 are additional defects, not complaints about its declared demo scope.
- **Money limits are soft, and disconnect billing is estimated.** USD is not reserved before execution. Queued/admitted work and concurrent replicas can overshoot. Before any backend event, client cancellation normally records zero; missing final usage after a disconnect is estimated from observed bytes. Hidden reasoning and unobserved generated output cannot be recovered that way. This matches current policy but does not establish exact agreement with Azure invoices. Retries before first output may also repeat paid inference; timeout attempts account for estimated input only.
- **Partial control-plane failure exceeds the simple “one batch interval” bound.** The outage predicate in `gateway/internal/limits/shared.go:108` treats a live config stream as contact even if `/usage` keeps failing. Each replica continues toward its own local view while the others' usage remains unreported. Account for this longer uncertainty period in spending policy and monitor ack age/spool growth independently of stream health. Total transport outage has the explicit default 15-minute grace, with additional allowance regained after a last-known-good restart before fresh totals arrive.
- **Spool durability has operational conditions.** Atomic batch/index writes, resend IDs and directory fsync are present. Normal crashes can still lose the filling batch (documented). If disk writes fail, `gateway/internal/control/spool.go:216` retains sealed records in memory and intake continues; prolonged disk exhaustion can lead to growing memory and loss beyond the normal five-second bound. An unbounded spool is explicitly documented. Kubernetes storage sizing, restart identity, disk/memory alerts and a policy for sustained write failure are required; these were inspected, not stress-tested to OOM or power failure.
- **Capacity is local to each gateway.** `gateway/internal/routing/routing.go:371` enforces a backend cap against that process's count. With N gateways, one vLLM host can receive approximately N times the configured cap. Set caps with fleet size and backend-side scheduling in mind. Small per-minute limits also have the documented minimum-one-per-replica behavior, and keep-alive affinity can strand other replicas' allowance.
- **Scale needs representative measurements.** Routing over roughly 20 deployments is straightforward, but usage metrics multiply owners/keys × models × hosts × status/units and remain allocated for the process lifetime. Disable the optional key label before high key cardinality, and measure remaining owner cardinality. Every totals response enumerates the current configured limit windows; broadcasts repeat that work per gateway. No sustained 20-host/many-owner load test was run, so the passing functional suite is not a throughput certification.
- **Cluster delivery is unfinished.** No Dockerfile/container build exists in this snapshot; `docs/BACKLOG.md:76` explicitly defers it until the first cluster deployment, which is now the stated use case. The default drain needs more than 65 seconds plus shutdown overhead; the spec calls this out. Preserve unsent spool storage across the failure modes for which recovery is expected. The reusable TS library currently resolves schemas relative to the repository (`control/kaiak-control/src/schemas/index.ts:23`); an extracted package/container must carry them at a supported location.
- **Provider validation remains necessary.** Azure `/openai/v1/` URL construction and `api-key` headers, OpenAI-compatible bearer auth, passthrough, usage flags, cached/reasoning token handling, and public model rewriting were inspected and exercised with fakes. No live Azure/vLLM endpoint or actual deployed model/version was available. Run the supplied live kit against both, including streamed/non-streamed reasoning, embeddings, content filtering, disconnects and multiplicity.
- **Minor documentation drift:** `docs/TECH-STACK.md:51` calls the spool append-only JSON lines; the implementation and gateway spec correctly describe one versioned JSON file per batch. The broader architecture and shared-fixture contracts generally match the code, subject to the findings above.

## Verification and retained evidence

All commands below ran against the isolated copy. Initial sandbox attempts failed to access the default Go cache, resolve dependency hosts, or bind test sockets; reruns with an audit-local cache and authorized execution completed. Those initial environment failures are not product findings.

- `go test -race ./...`: passed, including gateway e2e. Log: `go-test.log`.
- `npm ci`, `npm test`, `npm run lint`: passed; 380 tests, zero failures. Logs: `npm-ci-escalated.log`, `control-test.log`, `control-lint.log`.
- `scripts/check-all.sh`: passed on the unmodified copied source, including gofmt, vet, staticcheck, race tests, live-kit self-test, control checks and cross-half e2e. Log: `check-all-escalated.log`.
- Audit probes: `go-probes.log`, `go-extra-probes.log`, `socket-probe.log`, `control-probes.log`, `page-secret-probe.log`. Go probes deliberately assert the observed faulty behavior, so their `PASS` means reproduction, not a fix. Their source is in `probes/`; Go filenames indicate the original test package. An unsuccessful malformed-JSON secret-leak hypothesis is also recorded in the JS log and was not promoted to a finding.

Selected actual output:

```text
ℹ tests 380
ℹ pass 380
ℹ fail 0
boundaries ok
==> cross-half e2e (sample control plane + two gateways)
ok  kaiak/e2e  43.290s
all checks passed

control total=600; gateway used=1200/1000; 1-token request rejected
retained $1 budget: used before=1100000000; after newer config's totals=0; next request admitted
stream open still blocked after 7.5x IdleTimeout
n=1; output reservation=100
n=64; output reservation=100
no finish_reason, no [DONE]: relay_end="" partial=false
two CP cores / shared atomic memory store: first first records stored 2
ConfigMap symlink rotation: current file limit=2000; publish count=1
Unauthenticated GET /: 200 plaintext credential exposed: true
identical kaiak_circuit_open series occurs 2 times in one scrape
```

After all tests and probes, `pgrep -fl "kaiak|fakebackend|sample/src/main"` returned no matches (exit 1). Source integrity verification is recorded in `source-integrity.log`.

## Areas checked and found sound within the tested scope

- Key hashing/lookup, disabled and expiry checks, owner/model authorization, and indistinguishable absent-versus-forbidden models; client authorization/cookies are not forwarded to providers.
- Control token digest comparison, protocol/instance validation before route processing, strict message validation, shared valid/invalid fixtures, and single-instance duplicate batch intake.
- Cached input is separated from uncached input; reasoning remains inside output for pricing; cached-price fallback, request-start price selection, integer nano-dollar aggregation and all applicable ownership scopes.
- Retry classification and avoidance, one request-limit reservation per request, per-attempt timeout records, queue cancellation, slot release and circuit/prober lifecycle in the normal tested paths; race detector clean.
- Versioned atomic state writes with fsync, spool-before-send ordering, lost-ack resends with stable batch IDs, last-known-good startup, and ordinary hour/month rollover behavior under the existing suite.
- Drain readiness/grace/cancellation/settlement/flush ordering and second-signal handling, including cross-half tests; no leftover processes after verification.
- Request/SSE size limits, response-header allowlists, public-model rewriting, bounded usage/choice capture, model-label cardinality protection against arbitrary client names, and escaped sample-page HTML/CSP. No raw-key or prompt-content logging was found on the ordinary request path.
