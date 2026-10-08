# Step 20 — provider: one stream-format type per API; shared module helpers

**Status:** done (2026-10-08)

## Intent

A client API format's stream rules live in one type in one file, the backend modules
share the steps that are not their choice, and `wire.go` is split by job. Today a
format's rules are spread over `stream_end.go`, `wire.go` (a type assertion picks the
error-event fallback) and `body.go` (a second JSON decode per OpenAI usage chunk), and
all seven modules repeat the body-edit step and pass `stripUsage` back into the core.

## Findings

- provider F1: `streamEnd` becomes a per-format `streamFormat` (one per `Format`) that
  owns `observe` → `{errorEvent, usageOnly}` (the OpenAI one reads `usage` in the decode
  it already does), `complete`, `nestedModel` and `relayedError(payload)` (its own
  message paths and fallback); `relayedErrorEvent` keeps only the SSE framing;
  `isUsageOnlyChunk` goes. The format's request edits (`store: false`, `include_usage`)
  sit beside it so `passthroughBody` holds no format branch.
- provider F2 (decision 1 — helpers the modules call, not one shared struct):
  `wireCall` takes the edits; `sendWire` runs `passthroughBody` and `editError` and
  keeps `stripUsage` internal; modules that refuse first (`refusePriceOptions`) still do
  so before `sendWire`; `bearer(credential)` and `apiKey(credential)` header helpers,
  each module's `header()` choosing which. Each module keeps its struct, `Send`, `url`,
  `header` and probe.
- provider F5: `wire.go` splits into `send.go`, `response.go`, `notfound.go`,
  `probe.go`; `passthroughBody` / `standardServiceTier` move to `body.go`; family rules
  in `anthropic_rules.go` / `openai_rules.go`; `deploymentFailure(resp, req, call)`
  gives `sendWire` one release path.
- provider F8: `sse.Reader.Next` returns a transport error already in the gateway's own
  words (`netfail.Class`); `provider`'s and `control/stream.go`'s switches go.
- server hint: `providerName` (the GenAI provider name per backend type) becomes a field
  of provider's `backendKind`, so `server` names no backend type.

## Files likely touched

- `gateway/internal/provider/*.go` (every module, `wire.go` → new files, `body.go`,
  `stream_end.go`), `gateway/internal/sse/`, `gateway/internal/control/stream.go`,
  `gateway/internal/server/api.go` (or `requestlog.go`).

## Decisions made during planning

- The split (F5) lands last, as a pure move in its own commit.
- `docs/specs/GATEWAY.md` → Providers is unchanged: the modules still read as full
  providers and the core still names no type.

## Removal checklist (clean at phase end)

- `git grep -nE 'isUsageOnlyChunk|stripUsage|\(\*responsesStreamEnd\)' gateway/internal/provider` → none outside `sendWire`'s internals.
- `git ls-files gateway/internal/provider/wire.go` → none (or a small file whose job its
  name says).

## Acceptance criteria

- At most one JSON decode per stream event in provider (state it in the Result).
- Provider tests (`messages_test.go`, `responses_test.go`, `error_event_test.go`, the
  step-4 backend-type fixture) pass unchanged.
- `scripts/check-gateway.sh` green, or reds named with the step that clears them.

## Result

**What changed**

- **F1 — `stream_end.go` → `stream_format.go`.** `streamEnd` is now `streamFormat`, one
  type per `Format` (`openAIStream`, `messagesStream`, `responsesStream`, built by
  `newStreamFormat`). It has `requestEdits(req) (edits, stripUsage)`, `observe(payload)
  observation{errorEvent, usageOnly}`, `complete`, `nestedModel` and
  `relayedError(payload)`.
  - The OpenAI type reads `choices` and `usage` in one `json.Decoder` pass, with exact
    keys. `isUsageOnlyChunk` is gone.
  - Each type's `relayedError` calls `withGatewayMessage(payload, plain)` with its own
    plain error event (Messages `api_error`, Responses `server_error`; for OpenAI, which
    has no error event, an OpenAI error). The type assertion on `*responsesStreamEnd`
    is gone.
  - `relayedErrorEvent` keeps only the SSE framing and the model rewrite.
  - `passthroughBody` has no format branch: it appends
    `newStreamFormat(req.Endpoint.Format()).requestEdits(req)` (`store: false` on
    Responses, `stream_options.include_usage` on an OpenAI stream). The edit order is
    unchanged.
- **F2.** `wireCall` takes `edits []memberEdit` in place of `body` and `stripUsage`.
  - `sendWire` runs `passthroughBody` and `editError` itself; `stripUsage` stays inside
    the core (`passthroughBody` → `sendWire` → `upstreamResponse`).
  - New core helpers `bearer(credential)` and `apiKey(credential)`. `openai`, `vllm`,
    `llama-server` and `openai-compatible` use `bearer`; `azure-openai` uses `apiKey`;
    `azure-anthropic` uses `apiKey` plus its version header; `anthropic` keeps its own
    `X-Api-Key` header.
  - `anthropic` and `azure-anthropic` still refuse price options before `sendWire`.
    Every module keeps its struct, `Send`, `url`, `header` and probe.
- **Server hint.** `backendKind.providerName` plus `provider.ProviderName(t)`;
  `server/requestlog.go` calls it, and its own `providerName` is gone. The values are
  the same, so log output is identical. New `TestProviderNames` pins all seven types.
- **F8.**
  - `sse.Reader.Next` returns a failure of the underlying stream as
    `errors.New(netfail.Class(err))`. `ErrTooLarge`, `ErrTruncated` and `io.EOF` stay
    as they are.
  - In provider, the stream path returns the reader's error directly. `readFailure`
    stays for the non-stream body path only, without its `sse` cases.
  - `control/stream.go` drops its truncated/too-large/netfail switch; one
    `read config stream: %w` gives the same text. `netfail` is no longer imported
    there.
  - New `TestReaderWordsAConnectionFailureByItsClass` (sse).
  - `docs/ARCHITECTURE.md`: `sse` now uses `netfail`.
- **F5 — `deploymentFailure`, then the move.**
  - The code change: `deploymentFailure(resp, req, call) *Error` covers 401/403 →
    `upstream_auth_failed` and the 404/405 classification, with the same texts. A
    `release` closure (stop the timer, cancel) is the one release path, used by the
    build error, `deploymentFailure` and `fail`. `fail` releases first and then reads
    the cause; a timer's cause set before the release stays, so the outcomes are the
    same.
  - Then a pure move: `wire.go` is deleted (unstaged `rm`, as is the `git mv` of
    `stream_end.go`; the commit picks both up). A line-multiset check of the old
    `wire.go`, `body.go` and `anthropic.go` against the new files found no difference
    beyond one orientation comment at the top of each new file.

  File map:
  - `send.go`: the wire-core comment, `wireCall`, `sendWire`, `deploymentFailure`,
    `maxPreambleBlocks`, `readFirst`, `editError`, `bearer`, `apiKey`, `wireHeaders`.
  - `response.go`: `clientHeaders`, `upstreamResponse`, `errResponseClosed`,
    `maxSSEBlockBytes`, `pieceSize`, `newUpstreamResponse`, `Status`, `Header`,
    `Stream`, `Next`, `read`, `end`, `readEvent`, `readFailure`, `succeeded`,
    `complete`, `rewriteChunkModel`, `relayedErrorEvent`, `Close`.
  - `notfound.go`: `maxNotFoundBody`, `readNotFound`, `notFoundFields`,
    `readNotFoundFields`, `missingModelNamedOrCoded`, `missingModelCoded`,
    `errorAnswer`, `readErrorAnswer`, `namesWord`.
  - `probe.go`: `probeReadTimeout`, `maxProbeBody`, `fetchModelsList`,
    `PathMissingError` (+ `Error`, `BaseURLHint`), `versionPathHint`, `listedModels`.
  - `body.go` (added): `passthroughBody`, `standardServiceTier`.
  - `openai_rules.go`: `openAICore`.
  - `anthropic_rules.go`: `anthropicModelMissing`, and `anthropicVersion`, moved from
    `anthropic.go` because both Anthropic types use it.
  - The `provider.go` comment now points at `send.go`.
- **Tests.**
  - `TestIsUsageOnlyChunk` moved from `body_test.go` to the new `stream_format_test.go`
    as `TestOpenAIStreamReadsTheUsageOnlyChunk`. The case table is unchanged; it now
    calls `newStreamFormat(FormatOpenAI).observe(p).usageOnly`.
  - `error_event_test.go` and `model_test.go` use the renamed `newStreamFormat`,
    `.errorEvent`, `format:` and `openAIStream`. No assertion changed.

**Decisions made during the step**

- **The error-event message paths stay the same for every format.** Each format owns
  only its plain fallback event. `withGatewayMessage` replaces `message`,
  `error.message` and `response.error.message` in every relayed error event, as
  before. Narrowing each format to its own paths would stop replacing a message that a
  non-standard server puts elsewhere. That loosens the spec's "every message it
  carries becomes the gateway's" (Providers: error events), and it is not one of
  decision 10's visible changes.
- **`requestEdits` is a method of `streamFormat`**, so there is one switch on `Format`
  (`newStreamFormat`). `passthroughBody` builds a format value to ask for the edits, and
  `newUpstreamResponse` builds a fresh one for a stream's events. That is one small
  allocation per request.
- **The OpenAI chunk is read with exact keys.** This keeps the usage-only test's
  `"Choices"` case false. Two edges differ from before, on input no known server sends;
  no test asserts either:
  - Completeness also reads `choices` by its exact key. Before, a struct decode matched
    `"Choices"` case-insensitively.
  - A chunk that repeats `choices` with a mistyped earlier copy now says nothing.
    Before, the usage-only reading took the last copy.

**Report vs code**

- Line numbers came from `034329e`. The code had moved, but the shapes were as
  reported.
- F2 says six modules share the two credential headers. Five call a helper (four
  `bearer`, `azure-openai` `apiKey`), and `azure-anthropic` calls `apiKey` plus its
  version header.
- F5 places the 401/403 check outside `deploymentFailure` (`sendWire:128-149`). It is
  inside here, so every early return shares the one release.
- F3 was not done (rejected).

**Test counts** (top-level tests from `go test -list`; in brackets, passes including
subtests)

- `internal/provider`: 54 (201) → 55 (202). Added `TestProviderNames`;
  `TestIsUsageOnlyChunk` was renamed and moved.
- `internal/sse`: 5 (15) → 6 (16). Added `TestReaderWordsAConnectionFailureByItsClass`.
- `internal/control`: 79 (241) → 79 (241).
- `internal/server`: 188 (457) → 188 (457).

**JSON decodes per stream event in provider**

- **Before:**
  - OpenAI chunk: 1 (`observe`, struct). When the usage chunk was hidden (client did
    not ask for usage), 3: plus `isUsageOnlyChunk`'s map decode of the payload and a
    decode of its `choices`.
  - Messages and Responses: 1.
- **After:** 1 for every format. The OpenAI chunk is one `json.Decoder` pass.
- An error event, the stream's last, still adds the edit that replaces its message, as
  before.

**Removal checklist**

- `git grep --untracked -nE 'isUsageOnlyChunk|stripUsage|\(\*responsesStreamEnd\)' gateway/internal/provider`:
  - No `isUsageOnlyChunk` and no `(*responsesStreamEnd)`.
  - `stripUsage` appears only in the core: `passthroughBody` (`body.go`), `sendWire`
    (`send.go`), `upstreamResponse` (`response.go`), `streamFormat.requestEdits`, and
    the `passthroughBody` tests (`body_test.go`, `endpoints_test.go`).
  - No module and no `wireCall` field mentions it.
- `wire.go` is gone from the working tree. `git ls-files` still lists it until the
  deletion is staged.
- Also clean: `streamEnd|newStreamEnd|\.ending\b` (the only hits are control's
  `logStreamEnd` and server test names), `providerName(` in `server`, and `netfail` in
  `control/stream.go`.

**Suite**: `scripts/check-gateway.sh` passed (exit 0) on the final code.

```
==> gofmt / go vet / staticcheck 2026.2.1 (gateway)
==> go test -race -count=1 (gateway): ok cmd/kaiak, e2e (105s), accounting, auth, clip,
    config, control, limits, logattr, metrics, netfail, otlplog, provider, routing,
    schemacheck, server, sse
==> live-test kit: gofmt / vet / staticcheck; self-test passed for vllm, llama-server,
    openai, azure-openai, anthropic, azure-anthropic, vllm with two backends
gateway checks passed
```
