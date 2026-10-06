# Audit B — Messages, Responses and Anthropic providers

## Verdict

**Do not ship the new APIs for metered, shared use yet.** There are **9 findings: 3 High, 3 Medium and 3 Low**. Partial Messages streams undercharge generated output, and the Responses hosted-tool boundary has two independent holes. The ordinary completed-response accounting and routing paths are substantially better covered and passed their existing tests.

Reviewed against `AGENTS.md`, `docs/kaiak.md`, `docs/CODING-RULES.md`, the gateway, control-protocol and backend-verify contracts, and the relevant stack rules. This is a review of the supplied copy, not a historical diff. No production source or existing tests were changed. Three new `audit_b_test.go` files retain **11 failing reproduction tests**, all named `TestAuditB…` and confirmed with `go test -race`.

Frequency describes the trigger in legitimate use, not a prediction of exploit frequency: `daily`, `occasional`, `rare`, or `adversarial`. Local tests prove gateway behavior. No paid cloud requests were made; upstream behavior cited below comes from primary API documentation or SDK schemas. Conditional security consequences are identified explicitly.

## High

### B-H1 — Initial Messages usage makes later partial output effectively free [High] [occasional]

- **Where:** `gateway/internal/accounting/messages_usage.go:83`, `:134`; `gateway/internal/accounting/meter.go:191`, especially `:204`.
- **Mechanism:** `message_start.message.usage` makes `reported()` non-nil immediately, commonly with `output_tokens: 1`. Subsequent text/thinking/tool deltas increase the content estimate but do not invalidate that initial output count. If the client disconnects, the backend errors, or draining stops the response before the final `message_delta.usage`, settlement returns the initial count and `estimated: false`. The output reservation is then released against that understated record. Cost, token limits and downstream totals all inherit it.
- **Proof:** `TestAuditBMessagesPartialOutput` supplies input/cache usage, then 4,000 bytes of generated text, and settles partial. Actual result: `tokens_in:100`, `tokens_cached:20`, `tokens_cache_write:10`, **`tokens_out:1`, `estimated:false`, `partial:true`**. The documented byte heuristic would estimate 1,000 output tokens. A client can deliberately cancel before final usage to repeat this, but ordinary cancellation is sufficient.
- **Contract:** this exposes a defect in the agreement as well as the code. The “use the backend report when one arrived” rule is implemented literally, but a report preceding the generated output is not its usage. It conflicts with billing tokens generated before disconnect.
- **Fix sketch:** track which generated content each output usage update covers. Preserve known input/cache counts; estimate output not covered by a subsequent usage report and flag the record estimated. A final usage update covering all output remains authoritative. Define this partial-report rule in the accounting contract; do not discard known cache counts just because output needs estimation.

### B-H2 — The allowed `shell` type includes hosted execution [High] [occasional]

- **Where:** `gateway/internal/server/inbound_responses.go:66`, `:75`; `gateway/internal/server/inbound.go:159`.
- **Mechanism:** the allowlist treats `shell` as necessarily client-run and checks only `type`. Both `{"type":"shell","environment":{"type":"container_auto"}}` and `container_reference` pass intact. OpenAI explicitly defines these as hosted shell environments; local execution is a separate environment. `store:false` and standard service tier do not disable hosted tools. See the [official shell guide](https://developers.openai.com/api/docs/guides/tools-shell) and [shell tool schema](https://github.com/openai/openai-python/blob/main/src/openai/types/responses/function_shell_tool_param.py).
- **Proof:** `TestAuditBHostedShellRefused` exercises both environments through the complete gateway handler. Both reach the fake backend and return 200 instead of `400 hosted_tool_unsupported`.
- **Impact:** a regular hosted-shell request can cause provider-side execution and container charges outside the gateway's token-only accounting and budgets. The test proves forwarding, not a live container charge.
- **Contract:** the spec's unconditional classification of `shell` as client-run is wrong; this is not merely a missing implementation check.
- **Fix sketch:** validate the execution environment of shell definitions, allowing only the documented local form and a deliberately established safe default. Reject hosted and unknown environments. Apply this wherever tool definitions can enter; reject repeated owned environment members so checking one occurrence cannot leave another unchecked.

### B-H3 — Responses input items can add hosted tools without passing the allowlist [High] [occasional]

- **Where:** `gateway/internal/server/inbound_responses.go:33`, `:75`.
- **Mechanism:** validation inspects top-level `tools` and `tool_choice`, but treats all `input` as opaque. Responses supports a developer-role `additional_tools` input item whose `tools` are executable definitions. For example, `input:[{"type":"additional_tools","role":"developer","tools":[{"type":"web_search"}]}, …]` reaches the provider. This is a documented way to introduce tools, not an arbitrary nested object. See [OpenAI's tool-loading guidance](https://developers.openai.com/api/docs/guides/prompt-caching) and the [AdditionalTools input schema](https://github.com/openai/openai-python/blob/main/src/openai/types/responses/response_input_item_param.py).
- **Proof:** `TestAuditBAdditionalHostedToolsRefused` confirms that the generating and token-counting handlers both forward that definition and answer 200 from the fake backend. The counting request is not itself billed; the generating request is the dangerous path.
- **Impact:** the blanket hosted-tool refusal is bypassed, permitting unpriced tool activity on supporting deployments. New tools inserted this way also evade the claimed future-proof allowlist.
- **Fix sketch:** inspect documented input items that introduce tool definitions and apply the same validation as top-level tools, including duplicate-member checks. Alternatively refuse those item types until supported. The upstream schema also defines `tool_search_output.tools`; include that surface in the correction. Do not recursively interpret arbitrary function arguments or ordinary prompt text as executable definitions. Expand the spec's owned-field list accordingly.

## Medium

### B-M1 — Stored item references bypass the stateless Responses restriction [Medium] [occasional]

- **Where:** `gateway/internal/server/inbound_responses.go:42`, `:48`.
- **Mechanism:** only `previous_response_id`, `conversation` and `background` are checked. `input:[{"type":"item_reference","id":"msg_saved"}]` passes unchanged; the reference type is optional in the upstream schema, so `input:[{"id":"msg_saved"}]` is another form. These reference backend-held items rather than sending the full conversation. See the [Responses input schema, ItemReference](https://github.com/openai/openai-python/blob/main/src/openai/types/responses/response_input_item_param.py).
- **Proof:** `TestAuditBStoredItemReferenceRefused` confirms both forms reach the backend. This validates the missing boundary check; the fake does not implement stored-item lookup.
- **Impact and prerequisites:** if an item exists and is accessible under the configured provider credential, a request can depend on it despite the stateless contract. Different deployments need not possess that item, input reservation counts the short reference rather than its content, and kaiak has no ownership check separating its client keys from other users of that provider credential. Cross-client access additionally requires a usable item ID; this audit does not claim IDs can be enumerated. `store:false` prevents storing the new response, not reading an existing item.
- **Fix sketch:** reject stored-item reference forms at the input-item boundary on both Responses endpoints, with a field-specific stateful refusal. Preserve complete inline messages/output items that legitimately carry their own IDs. Add the restriction explicitly to the stateless contract.

### B-M2 — Media estimation uses field names instead of the format's actual content paths [Medium] [occasional]

- **Where:** `gateway/internal/accounting/estimate.go:182`, `:202`, `:239`, `:270`.
- **Mechanism, false media:** every Messages member named `source` is interpreted as a media source, including arbitrary `tool_use.input.source`. Every Responses array named `content` is interpreted as content parts, including values inside a function's JSON Schema. An object that merely resembles media replaces a large amount of ordinary prompt text with the fixed 1,000-token allowance. The struct-based discriminator also accepts case variants, unlike the exact-key inbound contract.
- **Mechanism, missed media:** Responses function-call output can be an array of image/file/text parts under **`output`**, not `content`. File-ID images and file-ID/URL documents there receive no media allowance. This shape is in the [official function-call output schema](https://github.com/openai/openai-python/blob/main/src/openai/types/responses/response_function_call_output_item_list_param.py).
- **Proof:** `TestAuditBToolDataIsNotMedia` gets **1,030 instead of 52,536** estimated tokens for ordinary Messages tool input, and **1,037 instead of 52,545** for a Responses tool schema. `TestAuditBResponsesToolOutputMediaEstimate` gets **33–38 total tokens** for a request carrying an image or document, below the required 1,000 for the media alone.
- **Impact:** understated reservations and inaccurate fallback billing when usage is unavailable. Actual complete usage corrects settlement, but cannot repair admission decisions or an unanswered attempt's estimate. The false-media forms are also deliberately constructible; real multimodal tool results trigger the missing-media side without unusual input.
- **Fix sketch:** walk the format's documented content locations, distinguish media blocks by exact discriminator keys, and keep tool argument/schema JSON as text. Include nested content sources and multimodal function outputs at their documented paths. Test both “looks like media but is data” and “real media outside a message content array.”

### B-M3 — A leading SSE comment ends the first-event retry window [Medium] [occasional]

- **Where:** `gateway/internal/provider/wire.go:154`, `:177`, `:597`; `gateway/internal/server/upstream.go:587` (response commitment and relay loop).
- **Mechanism:** `sendWire` reads one SSE **block** and returns it as the first event, even when `HasData` is false. A comment/keepalive therefore stops the first-event timer and commits the response before any API event. An immediately following Messages/Responses error is then treated as a mid-stream failure, so another deployment cannot be tried. The same mistake changes a wait for first data into a stall-timeout wait.
- **Proof:** `TestAuditBCommentBeforeFirstErrorRemainsRetriable` sends `: keepalive`, then the first data event, an Anthropic `overloaded_error`. Actual result: both blocks are relayed and the call ends with `ErrIncomplete`; expected before-first-event behavior is `CodeErrorEvent`, which the attempt loop can retry. The SSE reader explicitly documents that a comment dispatches no event.
- **Fix sketch:** retain first-event semantics until the first data-bearing event, keeping preamble buffering bounded or deliberately discarding only non-event preamble blocks. Do not stop the first-event timer or commit client headers for comments. Keep normal mid-stream comment forwarding and stall accounting unchanged.

## Low

### B-L1 — Duplicate nested policy members leave unchecked alternatives on the wire [Low] [adversarial]

- **Where:** `gateway/internal/server/inbound_responses.go:84–96`; `gateway/internal/provider/anthropic_price.go:64–89`.
- **Mechanism:** `tool_choice` collects repeated member names but rejects only repeated `type`. For `allowed_tools`, repeated `tools` is last-wins for validation while all occurrences are forwarded. The price scanner similarly decodes `cache_control` and `ttl` into last-wins maps. An earlier `ttl:"1h"` followed by `ttl:"5m"`, or an earlier hosted tool list followed by an allowed list, survives unchecked. Repeating an enclosing `content` member can likewise hide an earlier cache policy.
- **Proof:** `TestAuditBDuplicateAllowedToolsRefused` observes both lists, including `web_search`, reaching the backend. `TestAuditBDuplicateCachePolicyRefused` confirms acceptance of repeated `ttl` and repeated `cache_control` with a disallowed earlier value.
- **Limit of the evidence:** this establishes ambiguous validation/forwarding, **not a demonstrated pricing or hosted-tool exploit against a current cloud parser**. A backend using the same last-wins interpretation selects the checked value. Severity is Low for that reason. The existing top-level and tool-type protections correctly recognize this class of parser disagreement but do not cover all newly owned policy paths.
- **Fix sketch:** reject duplicates of every member used to navigate or enforce owned policy, after decoding escaped names. Leave unrelated tool schemas and prompt JSON opaque. Extend the spec's narrow duplicate-member rule with these owned paths.

### B-L2 — Responses stream estimates omit tool names [Low] [occasional]

- **Where:** `gateway/internal/accounting/responses_usage.go:83–97`.
- **Mechanism:** output estimation counts argument/input deltas but ignores `response.output_item.added`, where function/custom tool names arrive. Non-stream accounting counts names; the streaming contract says to count them too. The difference matters when final usage never arrives, including a normal disconnect during a tool call.
- **Proof:** `TestAuditBResponsesStreamToolNameEstimate` sends a 64-byte function name and two argument bytes. Settlement reports **1** estimated output token instead of **17**. This is separate from B-H1: Responses normally has no initial usable usage report, so it takes the estimation path.
- **Fix sketch:** count a tool name once when its output item is introduced, keyed by item identity/index as needed. Avoid recounting arguments repeated in item-done/final-response snapshots. Retain support for overlapping items.

### B-L3 — Unknown stream events have nested payload data rewritten [Low] [rare]

- **Where:** `gateway/internal/provider/wire.go:675–676`; `gateway/internal/provider/stream_end.go:116`, `:150`.
- **Mechanism:** the nested rewrite path is selected once by format and applied to every JSON event. Messages rewrites any top-level `message.model`, not just `message_start.message.model`; Responses rewrites any `response.model`, even outside the documented `response.*` events. An extension event containing such an object has its data changed despite the unknown-event passthrough guarantee.
- **Proof:** `TestAuditBUnknownEventsPreserved` sends `event: vendor.extension` with nested payload values. In both formats, one `"model":"payload-value"` becomes `"model":"pub"`. The event otherwise relays and ends normally.
- **Fix sketch:** select the extra nested rewrite only for events that carry the format's response envelope. Preserve unknown event payloads. Keep the existing top-level response-model handling and byte-preserving edit mechanism.

## What was checked and found sound

- **Completed-response units and prices:** Messages reads input, cache-read and cache-write counts as disjoint values, merging per-field stream updates. Responses subtracts cached/written tokens from inclusive input and clamps inconsistent details. Reasoning remains a subset of output, not an extra charge. Tier selection includes all three input units; missing cache prices fall back to plain input. Gateway token-limit settlement and control aggregation count input + writes + output, excluding cache reads as specified. B-H1 concerns incomplete report coverage, not these completed-response formulas.
- **Pipeline and attempts:** all new body routes use auth, per-key concurrency, bounded body parsing, model access, output parameters, limits and the shared attempt loop. Endpoint filtering preserves only eligible deployments, including queued requests. `endpoint_not_served` makes no backend call or usage record. `upstream_endpoint_missing` is retried across backends and is circuit-neutral. Refused price options are not retried and are neutral. Sent-unanswered retries retain their own estimated/partial records, and one reservation settles against their sum.
- **Token-counting endpoints:** no output-token reservation or usage record, including retried attempts; requests still count against RPM and pass model access and routing. Existing handler and end-to-end tests passed. The ordinary full limits stage still applies, as the spec requires.
- **Direct request policy:** duplicate top-level keys, escaped duplicate names, repeated tool `type`, wrong owned-field types, ordinary hosted-tool definitions/choices, stateful top-level Responses fields, and Messages `container`/`mcp_servers` are refused. `store:false` is forced on generating Responses calls. Unknown ordinary fields survive byte-preserving edits. The holes above are specific additional surfaces, not missing validation everywhere.
- **Provider wiring:** Anthropic and Foundry URL construction, credential header selection, fixed `anthropic-version`, Anthropic-only `standard_only`, standard OpenAI service tier, recognized model/path 404s, counting-endpoint distinctions, bounded models probes and Foundry's documented no-probe behavior match the contracts. Direct price-option checks cover top-level speed/geography and cache TTL at the documented direct block locations; token counting leaves those options untouched. Live cloud 404 signatures were not verified.
- **Secrets and content:** outbound headers are constructed from an allowlist; inbound bearer/x-api-key credentials, cookies, beta/version overrides and vendor pricing headers are not copied. Bearer credentials take precedence over a nonempty x-api-key fallback. Usage records contain identifiers, units and flags, not prompt/response bodies. Transport errors are classified; first-event error details are not relayed; HTTP backend 5xx text is suppressed and error logging is bounded/sanitized. Backend 4xx and mid-stream error-event bodies are intentionally relayed under the spec. No independent provider-secret or prompt-body logging leak was found in the reviewed paths.
- **Ordinary stream behavior:** real captured vLLM/llama-server fixtures cover overlapping blocks/items, Messages `message_stop`, Responses `response.completed`/`response.incomplete`, output-limit endings, nested model rewriting, usage and reasoning. First data error events without a comment preamble retry; mid-stream errors relay and then abort. Missing terminal events produce partial records and circuit failures. B-M3 and B-L3 are exceptions to otherwise passing coverage.
- **Config/protocol removal:** config format 5, protocol 5, closed model schemas, Go/TypeScript models and model props agree on removal of model `defaults`. Group `child_defaults` remains distinct and valid. The control package's schema copy matches `protocol/schema/` byte for byte. Shared fixtures pass on both halves.
- **Backend verification:** the new Anthropic tests pass for paged-list URL, credential/version headers, reported context metadata, missing models and credential refusal. Foundry sends no request and reports the fact in its note. Redirect refusal, body caps, timeout/abort behavior and credential-safe failure messages remain covered. No new control-package defect was confirmed, so no speculative failing Node test was added.
- **Candidate deliberately not counted:** forwarding `cache_control.ttl:"1h"` inside a nested tool-result text block was observed, but a client schema permitting that shape does not establish that a provider accepts and bills a cache breakpoint there. This was not retained as a finding or failing regression test. Paid-cloud acceptance remains untested.

## Verification and retained reproductions

The copy initially lacked Node dependencies. Installed the existing lockfile with `npm ci --ignore-scripts`; no dependency declarations or lockfile changes. Go used a temporary writable build cache. Local test-server access and pinned lint-tool downloads required execution outside the restrictive sandbox; the completed runs below had that access.

| Check | Result |
| --- | --- |
| Existing `go test -race ./...`, before audit tests | PASS, all gateway packages and e2e |
| `cd control && npm test` | PASS: 576 tests, 0 failures, 0 skipped |
| `cd control && npm run lint` | PASS: TypeScript and boundaries |
| `go test -race -tags crosshalf ./e2e -run TestAcrossHalves -count=1` | PASS: `ok kaiak/e2e 48.885s` |
| Final `scripts/check-all.sh` | gofmt, vet and staticcheck PASS; gateway tests FAIL only on the 11 retained audit tests, so the script stops there |
| Live-test kit vet, pinned staticcheck, `go run . -self-test`, run separately | PASS; all seven fake-backed configurations; the kit's existing unsupported-operation skips remain |

Re-run every retained reproduction from `gateway/`:

```sh
GOCACHE=/tmp/kaiak-audit-b-gocache go test -race \
  ./internal/accounting ./internal/server ./internal/provider \
  -run '^TestAuditB' -count=1 -v
```

Confirmed output, abridged only to test names:

```text
--- FAIL: TestAuditBMessagesPartialOutput
--- FAIL: TestAuditBResponsesStreamToolNameEstimate
--- FAIL: TestAuditBToolDataIsNotMedia
--- FAIL: TestAuditBResponsesToolOutputMediaEstimate
FAIL kaiak/internal/accounting
--- FAIL: TestAuditBHostedShellRefused
--- FAIL: TestAuditBAdditionalHostedToolsRefused
--- FAIL: TestAuditBStoredItemReferenceRefused
--- FAIL: TestAuditBDuplicateAllowedToolsRefused
FAIL kaiak/internal/server
--- FAIL: TestAuditBDuplicateCachePolicyRefused
--- FAIL: TestAuditBUnknownEventsPreserved
--- FAIL: TestAuditBCommentBeforeFirstErrorRemainsRetriable
FAIL kaiak/internal/provider
```

The tests live in `gateway/internal/accounting/audit_b_test.go`, `gateway/internal/server/audit_b_test.go` and `gateway/internal/provider/audit_b_test.go`. The final full run reported no race-detector failure and no failure in an existing test. The suite is intentionally red until the findings are addressed; no production fix is included in this audit.
