# Step 20 — provider: one stream-format type per API; shared module helpers

**Status:** not started

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
