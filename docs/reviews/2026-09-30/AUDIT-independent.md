# kaiak audit — 2026-10-01

Four reproduced findings, all **medium** severity. No high-severity defect was established. The existing test suites passed; the prescribed baseline is **incomplete**, because its pinned Staticcheck dependency was unavailable offline.

## Baseline and isolation

The supplied project was copied from `/tmp/kaiak` to `/tmp/kaiak-audit/work`. All installation, compilation, testing and added reproduction tests occurred in that copy. The original 596 files still match the copy byte-for-byte, excluding new files added only to the copy: [comparison](logs/source-comparison.log), [SHA-256 inventory](logs/source-sha256.json).

Environment: macOS arm64, Go 1.27.1, Node.js 26.9.0, npm 11.19.1. External downloads were disabled with `GOPROXY=off`, `GOSUMDB=off`, `GOTOOLCHAIN=local`, and npm's `--offline`. Network tests used localhost. Test servers were stopped; the final [process check](logs/process-cleanup.log) contains only the checking command itself.

Baseline checks ran before adding the failing audit tests:

| Check | Result | Evidence |
| --- | --- | --- |
| `npm ci --offline --no-audit --no-fund`, in `work/control` | PASS; 69 packages installed from cache | [log](logs/npm-ci.log) |
| `scripts/check-all.sh` | INCOMPLETE; initial attempt hit the sandbox's default Go-cache restriction | [initial log](logs/baseline.log) |
| Same script, with `GOCACHE=/tmp/kaiak-audit/go-cache` | Gateway gofmt and vet passed; stopped at unavailable Staticcheck 2026.2.1 | [log](logs/baseline-local-cache.log) |
| Gateway `go test -race ./...` | PASS, including gateway e2e | [log](logs/gateway-tests-localhost.log) |
| Control `npm test` | PASS: 565 tests, 37 suites, zero failures or skips | [log](logs/control-tests-localhost.log) |
| Control `npm run lint` | PASS: TypeScript and import boundaries | [log](logs/control-lint.log) |
| Live-test kit gofmt and `go vet -tags crosshalf ./...` | PASS | [gofmt](logs/live-gofmt.log), [vet](logs/live-vet.log); empty output means success |
| Live-test kit `go run . -self-test` | PASS: vLLM, llama-server, OpenAI, Azure OpenAI, and two-backend vLLM, all with local fakes | [log](logs/live-self-test-localhost.log) |
| `go test -race -tags crosshalf -run '^TestAcrossHalves$' -count=1 ./e2e` | PASS: actual sample control plane and two gateways | [log](logs/crosshalf.log) |
| `CGO_ENABLED=0 go build -trimpath -o /tmp/kaiak-audit/bin/kaiak ./cmd/kaiak` | PASS: native arm64 executable | [log](logs/build.log) |

The remaining checks were invoked separately after the script stopped. Staticcheck was not run for either Go module; this report does not claim that `check-all.sh` passed. The decisive baseline output was:

```text
==> gofmt (gateway)
==> go vet (gateway)
==> staticcheck 2026.2.1 (gateway)
go: honnef.co/go/tools/cmd/staticcheck@2026.2.1: module lookup disabled by GOPROXY=off
```

Initial sandboxed network tests could not bind localhost (`EPERM`); they were rerun with localhost access and passed. Those environment failures remain in [gateway-tests.log](logs/gateway-tests.log), [control-tests.log](logs/control-tests.log), and [live-self-test.log](logs/live-self-test.log), and are not findings.

## Findings

### M1 — A temporary control-plane 429 permanently discards usage

**Severity: medium. Frequency: occasional**, when a host application throttles the control-plane usage endpoint. The stock sample does not install such a throttle.

**Location:** [gateway/internal/control/usage.go](work/gateway/internal/control/usage.go), line **402**; the real adapter's relevant response mapping is [control/kaiak-control/src/fastify/index.ts](work/control/kaiak-control/src/fastify/index.ts), lines **102–106**.

The sender decides that a batch is permanently invalid from its error code alone. It does not require HTTP 400 or 413. A host Fastify hook returning 429 is converted by the supplied adapter into `429 {"error":"request-invalid",...}`. The gateway treats that answer as a permanent batch refusal, removes the batch from its default in-memory queue, and proceeds to the next sequence. With a data directory, it quarantines the batch instead of retrying it automatically.

**Impact:** temporary throttling loses accounting records and understates authoritative usage/cost totals. The whole rejected batch is affected. Default stateless operation has no retained copy to replay. This contradicts [CONTROL-PROTOCOL.md](work/docs/specs/CONTROL-PROTOCOL.md), lines **668–683**, which restrict permanent batch rejection to specified 400/413 answers and retry other refusals.

**Proof run:** the first reproduction installs a host hook and exercises the real Fastify adapter. The second feeds that exact status/code to the gateway's existing fake-control harness, queues two records, and asserts that the first batch is retried.

```text
status=429; Kaiak-Protocol=3; body={"error":"request-invalid","detail":"Too many requests"}
first sequence=1 HTTP=429 code=request-invalid sender=rejected
next sequence=2 sender=acked counted_records=1
temporary 429 dropped batch 1 instead of retrying the same ID
--- FAIL: TestAudit429MustRetryUsage
```

Sources: [rate-limit-control.mjs](repros/rate-limit-control.mjs), [audit_usage_test.go](repros/audit_usage_test.go). Output: [adapter](logs/rate-limit-control.log), [gateway](logs/usage-429.log).

**Fix direction:** require both the documented HTTP status and permanent-refusal code before setting a batch aside. Add coverage for 429 with a recognized error code; retain the batch ID and retry.

### M2 — A failed budget carry-over commits the config and permanently loses its spend

**Severity: medium. Frequency: rare**, requiring a store write failure during a limit's model-set edit. The reproduction injects a transient failure through the supported store interface; it does not claim that the sample's ordinary in-memory store fails spontaneously.

**Location:** [control/kaiak-control/src/usage/index.ts](work/control/kaiak-control/src/usage/index.ts), lines **327–333** and **242**; config persistence and notification happen in [config-versions/index.ts](work/control/kaiak-control/src/config-versions/index.ts), lines **73–75**.

Publication saves and announces the new config before writing carried window totals. If `addWindowTotals` fails, publication throws, but the new version remains current. Retrying the same config succeeds without repairing the carry: it compares against the already-committed new model set, so the old budget identity is no longer a predecessor.

**Impact:** a spent budget disappears from authoritative totals. Fresh gateways can receive the edited config without its already-spent allowance and permit more spending. Existing gateways may temporarily retain locally carried counters; that does not restore the control plane's record. [CONTROL-PROTOCOL.md](work/docs/specs/CONTROL-PROTOCOL.md), lines **795–808**, requires model-set edits to preserve predecessor spend.

**Proof run:** count $100 against a $100 monthly limit scoped to `demo`; change that limit to all models; fail the carry write once; retry the publish successfully. The $100 never reappears in the active window:

```text
before edit: [{"type":"usd_per_month","models":["demo"],"window_start":"2026-10-01T00:00:00Z","used":"100000000000"}]
publish threw: injected transient store write failure
committed config version after failure: 2
after failed publish: []
after successful retry: []
AssertionError [ERR_ASSERTION]: spent $100 must survive the model-set edit and retry
```

Source: [carry-failure.mjs](repros/carry-failure.mjs). Full output: [carry-failure.log](logs/carry-failure.log).

**Fix direction:** make config publication and carry-over atomic, or durably recoverable and idempotent before exposing the new version. Merely asking the caller to retry is insufficient.

### M3 — An accepted deployment inventory produces undeliverable status and excessive limit shares

**Severity: medium. Frequency: daily**, continuously for a deployment whose status exceeds 64 KiB. Small configurations do not trigger it.

**Location:** [control/kaiak-control/src/fastify/index.ts](work/control/kaiak-control/src/fastify/index.ts), lines **40** and **152**. The gateway includes every configured deployment in [gateway/cmd/kaiak/main.go](work/gateway/cmd/kaiak/main.go), lines **694–712**. Zero live gateways becomes a divisor of one in [gateway/internal/limits/shared.go](work/gateway/internal/limits/shared.go), line **69**.

Both halves accept inventories whose mandatory status exceeds the adapter's 64 KiB body limit. A valid example with 30 backends, 20 models deployed on each backend, and ordinary-length model paths produces about 88 KiB. Every report is rejected before status intake. Gateways never join the live set, or expire from it, even while their config and usage connections work.

**Impact:** operators lose gateway status, and gateways receive an understated live count. With every report rejected, each gateway enforces the entire configured per-minute limit instead of its fleet share. Backend concurrency shares use the same incorrect count. This is a persistent healthy-control-plane failure, distinct from the backlog's outage/restart and draining-count limitations. The relevant contracts are [CONTROL-PROTOCOL.md](work/docs/specs/CONTROL-PROTOCOL.md), lines **254–257** and **838–855**.

**Proof run:** Node validates and publishes the config, validates its status, then submits reports for two instances through the real Fastify adapter. A separate Go test parses the same config and runs the actual `servingStatus` builder and limiter. It verifies the generated status decodes, then fails because its encoded size exceeds the receiver cap.

```text
valid config: 30 backends, 20 models, 600 deployments; status bytes=88020; cap=65536
audit-1 status=413 body={"error":"request-invalid","detail":"Request body is too large"}
audit-2 status=413 body={"error":"request-invalid","detail":"Request body is too large"}
live_gateways after both reports: 0

real servingStatus: 30 backends, 20 models, JSON bytes=88023 (receiver cap=65536)
after totals live_gateways=0: gateway requests_per_minute share=60; two gateways would enforce total=120 instead of 60
valid applied config creates undeliverable status
--- FAIL: TestAuditRealStatusSize
```

The 120-RPM fleet result is the sum of the two identical limiter shares, not a claim that a two-process 120-request load test was run.

Sources: [status-size.mjs](repros/status-size.mjs), [audit_status_test.go](repros/audit_status_test.go). Inputs/output: [config](repros/status-config.json), [Node status](repros/status.json), [actual Go status](repros/status-go.json). Logs: [adapter](logs/status-size.log), [Go generator and limiter](logs/status-go.log).

**Fix direction:** align accepted config size/cardinality with the worst-case encoded status, including circuit metadata, or change the status transport to support those inventories. Raising a fixed cap alone leaves the same mismatch at a larger inventory.

### M4 — Editing a returned config changes the active store even when publication rejects it

**Severity: medium. Frequency: occasional**, in a library host that uses the supplied memory store and edits a retrieved config before publishing it. This is an in-process API problem, not an unauthenticated network mutation endpoint.

**Location:** [control/kaiak-control/src/storage/memory.ts](work/control/kaiak-control/src/storage/memory.ts), lines **48–49**; history reads at **58–59** expose the same references.

`saveConfig` copies incoming documents, but `latestConfig` returns the stored object itself. The public `currentConfig()` exposes that object. A normal read-edit-publish flow therefore changes the stored config before validation. Rejecting publication does not undo the edit; there is neither a new version nor a publication event.

**Impact:** the active config can become invalid while retaining its old version. Fresh snapshot readers see the mutated document, while already-running gateways can keep their previous config. This breaks the guarantee that a rejected publish leaves the current config unchanged in [CONTROL-PROTOCOL.md](work/docs/specs/CONTROL-PROTOCOL.md), lines **92–94**.

**Proof run:** publish the example config, retrieve it with `currentConfig()`, change its RPM limit from 600 to -1, then try to publish. Validation correctly rejects the negative limit, but the stored active value is already -1:

```text
publish rejected: [{"code":"schema","message":"/global/limits/0/value must be >= 0","path":"/global/limits/0/value"}]
active version=1; active requests_per_minute=-1
AssertionError [ERR_ASSERTION]: rejected edit must not alter the stored active config
-1 !== 600
```

Source: [mutable-config.mjs](repros/mutable-config.mjs). Full output: [mutable-config.log](logs/mutable-config.log).

**Fix direction:** return detached config snapshots from current/history reads, or establish and enforce immutable snapshots at the public API boundary. Preserve the stored document through a rejected publish.

## Re-running the evidence

The two Go reproduction files are already installed in the corresponding packages of the audit copy. They intentionally fail on the supplied code; a full suite run in this copy now includes these added failures. Production source files were not edited.

```sh
export GOCACHE=/tmp/kaiak-audit/go-cache
export GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local

node /tmp/kaiak-audit/repros/rate-limit-control.mjs
cd /tmp/kaiak-audit/work/gateway
go test -race -count=1 -run '^TestAudit429' -v ./internal/control

node /tmp/kaiak-audit/repros/carry-failure.mjs
node /tmp/kaiak-audit/repros/status-size.mjs
go test -count=1 -run '^TestAuditRealStatusSize$' -v ./cmd/kaiak
node /tmp/kaiak-audit/repros/mutable-config.mjs
```

Run commands individually, or allow their expected nonzero exits. The two Node adapter probes succeed after demonstrating the bad responses; the Go tests and the two assertion-based Node reproductions fail intentionally. The gateway usage reproduction needs localhost binding. None needs an external service.

## What was checked and found sound

These conclusions are bounded by source review and the passing baseline tests; they are not assertions that every possible input is safe.

- **Request pipeline and authentication:** shared key fixtures, group resolution, expiry boundary, model authorization, and refusal paths passed. Provider body rewriting preserves unknown fields and rejects duplicate owned fields in the covered cases.
- **Limits, routing and failure handling:** the existing limiter/routing suites and e2e exercised reservations, output ceilings, queue limits and cancellation, retry/failover rules, backend 429 cooldown, and circuit open/half-open/close behavior. No additional reproducible defect was established in these paths.
- **Usage and pricing:** tests for token mapping, cached and reasoning usage, service tiers, last streaming usage, partial usage, unanswered requests and fallback estimates passed. The local live-kit checks verified usage logs and metrics against known fake-backend outputs.
- **Provider modules:** URL/credential/probe and service-tier tests passed. Local fake-backend self-tests covered vLLM, llama-server, OpenAI and Azure OpenAI, including streaming, embeddings, output ceilings and two-backend failover.
- **Config and shared contracts:** Go and Node validation suites passed, including shared fixtures; the control package's schema copy matched `protocol/schema` byte-for-byte. TypeScript and dependency-boundary lint passed. The reported config defects concern state publication and transport bounds, rather than an observed schema-copy drift.
- **Gateway/control protocol:** existing tests passed for lost-ack resend without double-counting, bounded rejected-batch retention, spool restart, epoch/config resync, rejected config, boot from last-known-good, retry/backoff, timeout handling and refusing redirects that could expose the control token. Cross-half e2e passed with the real sample and two gateway processes.
- **Security and operations:** reviewed request/header ownership, credential handling, logging/accounting boundaries and resource limits alongside the existing tests. Reviewed README and deployment instructions; the native binary built and the local operational/e2e checks passed. No additional reproduced secret disclosure or client-to-backend credential injection was established.

`AGENTS.md`, `docs/kaiak.md`, the subsystem specs, architecture, coding/stack guidance, README, deployment documentation and backlog informed the review. Known backlog limitations were excluded, including llama-server aliases/multi-sequence accounting, encoded usage/totals size bounds, old-epoch spool ordering, slow readers, and live-count behavior during outages/draining. M3 concerns a different message's size limit and demonstrates persistent undercount with a reachable control plane.

## Limits of this audit

- Staticcheck could not be completed without an external download. Baseline Go output includes legitimate cached package results; the new reproductions use `-count=1`.
- No external model server or cloud API was contacted. Provider coverage here is tests and local fakes, not certification against current vendor deployments.
- Container image builds, registry operations, cluster deployment and sustained production-scale load were not exercised. The binary build targeted the local OS/architecture.
- M2 uses an explicitly injected store failure; M1 uses an explicitly installed host throttling hook. Their prerequisites are part of the findings, not behavior attributed to an unmodified running sample.
- All four findings are reproduced. No unreproduced suspicion is presented as a defect. No fixes were applied.
