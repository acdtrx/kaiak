# Independent code and security audit — kaiak

Date: 2026-10-05. **8 confirmed findings: 2 high, 4 medium, 2 low. No critical finding.** Two additional risks are separated below as unconfirmed suspicions.

## Scope and limitations

Read `docs/kaiak.md`, `AGENTS.md`, `docs/CODING-RULES.md`, `docs/TECH-STACK.md`, and the relevant gateway/control-protocol contracts before reviewing the implementation. Prioritized cache-write accounting, exclusion of cache reads from token limits, the log vocabulary, and the OTLP exporter; followed credential and error flows into adjacent packages.

**This directory has no `.git` directory or release history.** `git diff v0.9.0..HEAD` could not run. Findings apply to the supplied source; their introduction relative to v0.9.0 cannot be established. The exporter and its interaction with the existing credential boundary are the main findings in the explicitly identified new work. Provider/control error handling findings concern adjacent code and are not claimed as new regressions.

No production source, existing tests, schemas, contracts, or dependency manifests were changed. Added regression tests deliberately assert safe/correct behavior and **fail on the audited implementation**. They use synthetic credentials and local servers, never a real backend or collector. This is an audit, not a remediation change.

## Confirmed findings

| ID | Severity | Finding |
|---|---|---|
| B1 | high | Config authors can exfiltrate OTLP credentials through a backend's `api_key_env` |
| B2 | high | Export redirects forward configured credentials and log payloads to another destination |
| B3 | medium | Collector error messages can disclose export credentials in stderr |
| B4 | medium | Provider/control errors can disclose remote header contents and credentials in logs |
| B5 | medium | A truncated collector rejection is counted as successful export |
| B6 | medium | A second stop signal during the final log flush is ignored |
| B7 | low | A valid `Retry-After` longer than one day causes an early retry |
| B8 | low | `kaiak.limit.used` changes currency scale between operational and request logs |

### B1 — Config authors can select telemetry credentials as backend keys

**Locations:** `gateway/internal/config/schema.go:48`, `gateway/internal/config/schema.go:53`, `gateway/internal/provider/provider.go:230`, `protocol/schema/config.schema.json:219`; the same restriction is copied into `control/kaiak-control/schema/config.schema.json:219` and used by `control/kaiak-control/src/config/index.ts:25`.

**Failure:** The credential boundary reserves only `KAIAK_*`. The new exporter places gateway-owned secrets in `OTEL_EXPORTER_OTLP_HEADERS` and `OTEL_EXPORTER_OTLP_LOGS_HEADERS`, but either name is accepted as a backend's `api_key_env`. Both validation halves accept the configuration. The provider then reads that environment value and sends it to the backend URL chosen by the config author.

**Scenario:** A person allowed to publish model configuration, but not read the gateway's environment, adds an OpenAI-compatible backend at an endpoint they control and sets `api_key_env` to `OTEL_EXPORTER_OTLP_LOGS_HEADERS`. A models probe alone sends the entire header-list string as a bearer credential. Percent encoding does not protect it: the recipient can decode the embedded collector token. Endpoint variables can also carry credentials and are not reserved. This crosses the same boundary the existing `KAIAK_*` exclusion explicitly protects; it does not require access to an ordinary client's key.

**Proof:** `TestAuditBOTELCredentialsMustNotBeBackendKeys` passes a real document through `config.Check`, then probes the resulting backend. Both header variable names reach the fake backend:

```text
validated config exfiltrated OTEL_EXPORTER_OTLP_LOGS_HEADERS to its chosen backend:
"Bearer authorization=Bearer%20audit-b-export-secret"
```

The Node regression also fails because `validateConfig` accepts the reference.

**Fix sketch:** Reserve gateway-owned OTEL settings at both validation boundaries and in the provider's final credential guard. Reserving the `OTEL_` namespace is simpler than maintaining an incomplete list of potentially secret exporter settings. Update the shared schema, packaged copy, contract, and fixtures together. Keep arbitrary backend key references outside the reserved namespaces.

### B2 — Export redirects forward credentials to another destination

**Locations:** `gateway/internal/otlplog/exporter.go:136`, `gateway/internal/otlplog/exporter.go:340`, `gateway/internal/otlplog/exporter.go:345`.

**Failure:** The exporter's `http.Client` has no `CheckRedirect` policy. Go follows redirects; custom secret headers such as `X-Api-Key` are not covered by its special treatment of `Authorization`. A 307/308 also resends the complete export body. Authorization's built-in protection is insufficient as well: same-host redirects can change port or downgrade HTTPS to HTTP, a risk already documented and prevented in `gateway/internal/control/client.go:62`.

**Scenario:** A collector ingress is misconfigured or has an open redirect. Its 307 points at another service. That service receives the collector API key and the gateway log batch. This requires a redirect from the configured destination, not arbitrary unauthenticated access to the gateway.

**Proof:** `TestAuditBRedirectMustNotForwardCredential` uses two local servers and different host names (`127.0.0.1` and `localhost`). The target receives `X-Api-Key: audit-b-secret` following the first server's 307.

**Fix sketch:** Refuse redirects with `http.ErrUseLastResponse`, as the control-plane client already does, and report a sanitized failure requiring the operator to configure the final endpoint. If redirects are intentionally supported, use an explicit origin/scheme policy that also protects custom credentials and the payload; relying on Go's default sensitive-header list is insufficient.

### B3 — Collector diagnostics can expose export credentials in stderr

**Locations:** `gateway/internal/otlplog/exporter.go:393`, `gateway/internal/otlplog/exporter.go:419`, `gateway/internal/otlplog/exporter.go:483`. Transport errors at `gateway/internal/otlplog/exporter.go:347` are also not made generally safe by removing the outer `url.Error`.

**Failure:** `Status.message` and partial-success `errorMessage` are remote response content copied directly into `exception.message`. Clipping to 256 bytes limits length, not disclosure. The documented promise that export header values never reach logs is therefore false.

**Scenario:** An authentication proxy answers 401 with a JSON diagnostic containing the invalid bearer token. The warning publishes that token to container logs. A collector's partial-success message can do the same. Readers of stderr need not have access to the original environment or the collector itself. The triggering remote service already knows the credential; the vulnerability is expanding its audience and retention into logs.

**Proof:** Both subcases of `TestAuditBCollectorEchoMustNotLeakCredential` fail:

```text
collector answered 401 Unauthorized: invalid credential: Bearer audit-b-secret
collector rejected 1 records: invalid credential: Bearer audit-b-secret
```

**Fix sketch:** Keep status, rejected count, and a local error classification in operational logs. Do not publish arbitrary collector response text by default. If diagnostic text is retained, define and test redaction of configured header values, URL credentials, and encoded forms before clipping; exact-string replacement alone cannot uphold a general prohibition on response content. Apply the same safe-error boundary to transport errors. Preserve the existing stderr-only reporting path so this does not introduce a feedback loop.

### B4 — Provider and control-plane errors also publish remote header contents

**Locations:** `gateway/internal/provider/wire.go:201`, `gateway/internal/provider/wire.go:107`, `gateway/internal/routing/modelcheck.go:108`, `gateway/internal/server/api.go:217`; `gateway/internal/control/transport.go:106`, `gateway/internal/control/transport.go:121`, `gateway/internal/control/client.go:560`.

**Failure:** Provider transport errors are wrapped and logged verbatim. Go's HTTP parser sometimes puts the offending response bytes in the error, including malformed header lines. Separately, the control client's protocol-mismatch error explicitly interpolates the entire remote `Kaiak-Protocol` header value, and its purported error-code parser returns any remote string without validating it as a code. These paths violate the log contract even though ordinary OpenAI error-body messages are otherwise kept out of request logs.

**Scenarios:**

- A broken backend/proxy returns an invalid header line containing a credential or response text. The models probe logs that line as part of its transport error; request-path transport failures have the same raw-error flow. With OTLP enabled these operational/request records also reach the collector.
- A misconfigured control endpoint reflects the bearer credential into `Kaiak-Protocol`. Protocol validation rejects the response, but its diagnostic writes the credential to the log. Any unexpected string in that header is logged, not just a numeric version.

**Proof:** `TestAuditBMalformedBackendResponseMustNotLeakCredential` gets this probe error, which `modelcheck.go:108` logs unchanged:

```text
malformed MIME header: missing colon: "Bearer audit-b-provider-secret"
```

`TestAuditBProtocolMismatchMustNotLogHeaderValue` calls the real `send` and `logFetchFailure` methods and observes:

```text
protocol version mismatch: control plane answered 200 with Kaiak-Protocol
["Bearer audit-b-control-secret"], gateway speaks 4
```

**Fix sketch:** Convert remote transport/protocol failures into safe local diagnostics before passing them to logging. Retain the backend ID, status and safe failure class; omit parser-echoed wire bytes. For a version mismatch report absent/duplicate/invalid or a validated numeric version, not the raw header. Validate error codes against the protocol vocabulary before logging them. A length cap is useful but is not secret redaction.

### B5 — Truncated partial-success replies become successful exports

**Locations:** `gateway/internal/otlplog/exporter.go:359`, `gateway/internal/otlplog/exporter.go:365`, `gateway/internal/otlplog/exporter.go:388`.

**Failure:** `io.ReadAll` errors are ignored. JSON decode errors then mean zero rejected records. Thus HTTP 200 headers alone cause a batch to be counted as exported, even if the response body reporting rejection was truncated. A non-JSON 200 login/proxy page has the same false-success result. The `exported` metric claims collector acceptance, which has not been established.

**Scenario:** A collector rejects the batch and starts sending a partial-success response, but the connection closes before its JSON completes. The exporter records `{Exported:1 Failed:0 Dropped:0}`, emits no failure diagnostic, and discards the batch. Monitoring cannot distinguish this from successful delivery.

**Proof:** `TestAuditBTruncatedPartialSuccessMustNotCountExported` sends a declared 1000-byte response but ends after `{"partialSuccess":{"rejectedLogRecords":"1"`. The test observes the success counts above.

**Fix sketch:** Check response read errors and validate the success response's encoding and shape. Do not turn malformed or incomplete acknowledgements into successful acceptance. Classify an unreadable acknowledgement conservatively and explicitly; avoid blindly retrying a known partial success because accepted records could be duplicated. The project's broad “a 2xx is delivered” contract needs narrowing too: OTLP distinguishes full and partial success using the response message, not status alone. See the [OTLP response contract](https://opentelemetry.io/docs/specs/otlp/#otlphttp-response).

### B6 — Second stop signals are ignored once final log flushing begins

**Locations:** `gateway/cmd/kaiak/main.go:547`, `gateway/cmd/kaiak/main.go:563`, `gateway/cmd/kaiak/main.go:568`, `gateway/cmd/kaiak/main.go:692`.

**Failure:** The second-signal watcher exits when `drained` closes. `hurry` is sampled only once before that close. The deferred final exporter flush runs later, using a background context with its original deadline, and cannot observe another stop signal or cancellation of `run`'s context.

**Scenario:** Requests have drained quickly, but the collector is stalled. The exporter is still flushing within the remaining drain deadline. A second Ctrl-C/SIGTERM should reduce waiting to the documented one-second floor, but no goroutine is consuming that signal. The process can continue waiting for the remaining configured deadline. This is bounded waiting, not an infinite goroutine leak, but contradicts the lifecycle contract.

**Proof:** `TestAuditBSecondSignalDuringLogFlush` waits until the collector receives `kaiak stopped`, then stalls its response and sends the second signal. With a 10-second drain deadline and 20-second batch timeout, the process still has not returned after two seconds. Releasing the fake collector lets it finish.

**Fix sketch:** Keep the existing signal/cancellation watcher alive through the exporter flush and let that flush shorten its deadline when `hurry` fires. Stop and join the watcher after the final flush. Preserve ordering: usage flush and final status first, exporter last; do not add a competing signal consumer.

### B7 — Valid long Retry-After values are treated as absent

**Locations:** `gateway/internal/otlplog/exporter.go:430`, `gateway/internal/otlplog/exporter.go:444`; fallback retry selection at `gateway/internal/otlplog/exporter.go:302`.

**Failure:** A seconds-form value over 86400 is returned as “unreadable.” That selects exponential backoff rather than the existing “wait exceeds remaining timeout, fail the batch” path. Two days is valid HTTP syntax and safely representable as a Go duration. Date-form headers can represent the same delay and do not have this cutoff.

**Scenario:** A collector with a long suspension answers `503 Retry-After: 172800`. The gateway retries after 250–500 ms instead of abandoning this batch under its default 10-second timeout. Many gateways unnecessarily continue hitting a service that explicitly told them to wait.

**Proof:** `TestAuditBLongRetryAfterMustNotRetryEarly` observes two HTTP requests; its injected retry clock records a sub-second backoff instead of no retry. The parser also returns `-1ns` for the valid header.

**Fix sketch:** Accept representable nonnegative seconds. Saturate values that would overflow to a duration beyond the batch deadline, rather than treating them as absent. Existing deadline comparison can then fail the batch immediately. This also follows the [OTLP throttling guidance](https://opentelemetry.io/docs/specs/otlp/#otlphttp-throttling).

### B8 — The same logged money field uses two different scales

**Locations:** `gateway/internal/limits/limits.go:337`, compared with `gateway/internal/server/api.go:293` and the log field contract in `docs/specs/GATEWAY.md:2147`.

**Failure:** The `limit keeps its usage across a model-set change` event logs `bestUsed` directly as `kaiak.limit.used`. Cost counters hold nano-USD. Request refusal logs convert the same field to dollars. The operational event therefore represents $2 as 2,000,000,000 while a request event represents it as 2, defeating the documented single vocabulary and consistent meaning.

**Scenario:** A config reload changes the model set covered by a USD budget. A log query or alert aggregating `kaiak.limit.used` sees a billion-fold jump even though accounting and the budget itself are correct.

**Proof:** `TestAuditBCarriedCostLogUsesDollars` settles $2, changes a monthly limit's model set, and observes `kaiak.limit.used=2e+09` on the carry-over event.

**Fix sketch:** Apply the same measure-aware conversion used by request refusal logs: cost counters to dollars, token/request counters unchanged. Keep the current field name and document/test one unit across both event types.

## Suspicions requiring further evidence

These are **not included in the eight confirmed findings**.

1. **Potential medium availability risk — record count does not bound exporter bytes.** `gateway/internal/otlplog/handler.go:92`, `gateway/internal/otlplog/exporter.go:209`, `gateway/internal/otlplog/exporter.go:283`. The queue is bounded at 10,000 records, but record strings, attribute arrays, and the fully encoded batch have no exporter byte cap. B4 demonstrates a remote-controlled diagnostic path; long parser errors could produce much larger records than the documented approximate 1 KB. The queue backing slice also retains consumed records until the backing allocation is released. No production-sized heap/throughput experiment was performed, so this report does not claim a measured OOM, an unbounded record leak, or an exploitable memory growth rate. **Next check:** feed bounded large malformed upstream responses while stalling export and measure retained heap and request latency. **Possible fix:** bound safe diagnostic strings at their source and, if needed, add a queue/batch byte budget with drop accounting.

2. **Potential low correctness risk — persisted token totals may retain the old cache-read semantics.** `gateway/internal/limits/snapshot.go:17` and `gateway/internal/limits/persist.go:20` both use format 2. They restore already-summed counts, with no ability to subtract old cache reads. If v0.9.0 wrote these same formats while counting cached input, an upgrade could restore that older meaning until the token window rolls over or authoritative totals replace it. The missing release history prevents confirming that prerequisite. **Next check:** compare these two constants and formats at v0.9.0 and replay an actual old snapshot. **Possible fix if confirmed:** invalidate the changed cache format or prescribe the project's explicit manual cleanup; do not add a migration or dual-format reader.

## Requested areas checked without a confirmed accounting defect

- **Cache-write metering:** `meter.go:256` clamps negative totals to zero, cached reads to prompt, then writes to the remaining prompt. Plain input is the remainder, so the three input units partition prompt without subtraction overflow. Reasoning is capped at completion and remains inside output. Embeddings deliberately count prompt only, per contract. Both stream and non-stream paths use this parser; absent/malformed usage follows the existing estimate path.
- **Pricing and clamping:** `accounting.go:214` includes all three input units in tier selection and saturates the sum. Tier comparison remains strictly above the threshold. Missing read/write prices fall back to that tier's plain-input price; an explicit zero remains zero. Cost is calculated before protocol clamping, then units/cost above `2^53-1` are clamped and reported. No double pricing of reasoning was found.
- **Token limits:** `limits.go:560` and `control/kaiak-control/src/usage/aggregate.ts:89` both sum plain input + cache writes + output, excluding cache reads and the already-included reasoning share. Gateway sums saturate; control sums use `bigint`. Reservations still use full estimated input plus output allowance as required, and settlement releases the reservation before applying actual usage. Local/shared generation handling and USD accounting were not changed by selectively removing cache reads.
- **Version boundaries:** current source and schema copies agree on protocol **4**, config **4**, spool **3**, and last-known-good **5**. Usage schemas require the cache-write unit, and both halves' fixture checks passed. Old spool/LKG files are discarded through versioned-state handling, not silently read as current. See the separate suspicion about limits snapshots.
- **Log vocabulary:** searched gateway production log calls and compared literal attribute names with the field tables. No undeclared dotted log field name was found. Duration helpers use seconds, sizes remain bytes, request logs combine the three input units into `gen_ai.usage.input_tokens`, and cache reads/writes remain distinct parts. B8 is a real unit mismatch. B3/B4 show that declared names do not guarantee safe values. Backend/control URLs are intentionally present in some documented operational fields; the OTLP startup field logs only host/port. This review is not a claim that every runtime error string was exhaustively enumerated.
- **OTEL settings:** reviewed enable/disable rules, empty-value handling, LOGS-over-general precedence, endpoint joining, protocol restriction, timeout overflow checks, percent-decoded headers/resources, service-name precedence and reserved resource identity. The documented subset intentionally differs from the full SDK; not implementing gRPC, compression or certificate variables is not reported as a defect. Header parsing rejects control characters without echoing the value. B1/B2/B3 remain the consequential credential gaps.
- **OTLP encoding:** outgoing lower-camel-case fields, string-encoded 64-bit integers/timestamps, numeric severity, typed attributes and special floating-point values match the reviewed [OTLP JSON mapping](https://opentelemetry.io/docs/specs/otlp/#json-protobuf-encoding). No third-party SDK or collector was installed; wire tests use a local receiver, so this is not certification against every collector. Response handling defects are B5/B7.
- **Concurrency and lifetime:** queue insertion performs no collector I/O; the single sender owns retries. Queue/drop counters and shutdown synchronization were covered by the original race suite. Reports are wired to the original stderr logger, avoiding exporter feedback. Cancellation stops in-flight network activity and `Close` joins the sender. No race or accumulating goroutine leak was observed. This does not make stderr nonblocking: the existing synchronous stderr handler still writes on the caller's goroutine. Final exporter flushing occurs after usage/final status and stopped/error logging; B6 is the remaining signal-lifetime defect.

## Verification and reproduction commands

Run from the repository root unless stated otherwise. `GOCACHE` below only puts compiler artifacts in a writable temporary directory. No Go dependency download is needed.

### Existing tests before adding regressions

```sh
cd gateway
GOCACHE=/tmp/kaiak-audit-b-gocache go test -race ./...
```

Passed, including the binary e2e suite. Output:

```text
ok   kaiak/cmd/kaiak                 2.158s
ok   kaiak/e2e                     103.941s
ok   kaiak/internal/accounting     (cached)
ok   kaiak/internal/auth           (cached)
ok   kaiak/internal/clip           (cached)
ok   kaiak/internal/config         (cached)
ok   kaiak/internal/control        13.735s
?    kaiak/internal/fakebackend    [no test files]
?    kaiak/internal/fakebackend/cmd/fakebackend [no test files]
?    kaiak/internal/fakecontrol    [no test files]
ok   kaiak/internal/limits         (cached)
ok   kaiak/internal/logattr        (cached)
ok   kaiak/internal/metrics        (cached)
ok   kaiak/internal/otlplog        1.785s
ok   kaiak/internal/provider       2.540s
ok   kaiak/internal/routing        (cached)
ok   kaiak/internal/schemacheck     (cached)
ok   kaiak/internal/server         12.098s
ok   kaiak/internal/sse            (cached)
ok   kaiak/internal/state          (cached)
```

```sh
cd control
npm ci --offline --ignore-scripts
npm test
```

Dependencies installed from the existing local npm cache. Existing suite output:

```text
tests 574
suites 38
pass 574
fail 0
cancelled 0
skipped 0
todo 0
duration_ms 1953.013
```

Initial sandboxed attempts could not bind loopback listeners; the Go attempt also initially encountered the protected default compiler cache. Tests were rerun with local-listener permission and the temporary cache. These environmental failures are not counted as product findings.

### Added regressions — expected failures until fixed

```sh
cd gateway
GOCACHE=/tmp/kaiak-audit-b-gocache go test -race -count=1 -timeout=30s \
  ./internal/otlplog ./internal/provider ./internal/control ./internal/limits ./cmd/kaiak \
  -run TestAuditB -v
```

All nine top-level Go regressions fail for their intended assertions, with no race detector report:

```text
FAIL TestAuditBRedirectMustNotForwardCredential
FAIL TestAuditBCollectorEchoMustNotLeakCredential
FAIL TestAuditBTruncatedPartialSuccessMustNotCountExported
FAIL TestAuditBLongRetryAfterMustNotRetryEarly
FAIL TestAuditBOTELCredentialsMustNotBeBackendKeys
FAIL TestAuditBMalformedBackendResponseMustNotLeakCredential
FAIL TestAuditBProtocolMismatchMustNotLogHeaderValue
FAIL TestAuditBCarriedCostLogUsesDollars
FAIL TestAuditBSecondSignalDuringLogFlush
```

For one reproduction, replace `-run TestAuditB` with its full test name and use the corresponding package. For the control-side B1 reproduction:

```sh
cd control
node --test kaiak-control/src/config/audit-b.test.ts
```

Observed expected failure: `AssertionError: accepted a backend referencing OTEL_EXPORTER_OTLP_HEADERS`.

The original suites passing does not mean the final tree is green: the added tests intentionally expose defects. No existing failing test was skipped, removed, or weakened.

### Additional cross-half verification

```sh
cd gateway
GOCACHE=/tmp/kaiak-audit-b-gocache go test -race -tags crosshalf ./e2e \
  -run TestAcrossHalves -count=1 -timeout=2m
```

Passed: `ok kaiak/e2e 54.003s`. This exercises the actual sample control plane and two gateways, including the explicit cache-write/cache-read token-total case, budget propagation, lost acknowledgements, outage/restart, and shutdown usage delivery.

## Files added

1. `AUDIT-B.md` — this report.
2. `gateway/internal/otlplog/audit_b_test.go` — B2, B3, B5, B7.
3. `gateway/internal/provider/audit_b_test.go` — B1 and provider-side B4.
4. `gateway/internal/control/audit_b_test.go` — control-side B4.
5. `gateway/internal/limits/audit_b_test.go` — B8.
6. `gateway/cmd/kaiak/audit_b_test.go` — B6.
7. `control/kaiak-control/src/config/audit-b.test.ts` — control-side B1.

The audit-installed `control/node_modules/` was removed after verification; it was absent initially. Reinstall with `npm ci --offline --ignore-scripts` (or `npm ci --ignore-scripts` if the local cache is unavailable) before rerunning Node or cross-half tests. Temporary command transcripts and the Go build cache are outside the project under `/tmp/kaiak-audit-b-*`. No production fix is included.
