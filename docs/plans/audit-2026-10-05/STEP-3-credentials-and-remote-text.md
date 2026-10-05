# Step 3 — credentials and remote text

**Status:** done (2026-10-05)

## Intent

H1 on both halves, and M5: no remote bytes in provider or control-plane log lines.

## Files likely touched

- Gateway: `config/schema.go` (the `api_key_env` guard), `provider/provider.go` (last
  guard), `provider/wire.go`, `routing/modelcheck.go`, `server/api.go` (transport
  errors as local classes), `control/transport.go`, `control/client.go` (version
  mismatch, error codes).
- kaiak-control: `src/config/` validation for `OTEL_`.
- Tests: Codex's `provider/audit_b_test.go`, `control/audit_b_test.go` and
  `audit-b.test.ts` ported.

## Acceptance criteria

- Ported repros fail before, pass after; step 1's `OTEL_` fixtures pass on both halves.
- `scripts/check-all.sh` green; suite recorded.

## Result

**What changed**

- H1, gateway — `config/schema.go`: `reservedEnvPrefixes` (`KAIAK_`, `OTEL_`)
  mirrors the schema's `^(KAIAK|OTEL)_`; `IsReservedEnvName` checks both, and the
  `api_key_env` issue reads `must not start with KAIAK_ or OTEL_: …`. The provider's
  last guard (`provider.go` `credential`) calls the same function, so it refuses
  `OTEL_` too (comment updated). The three `api-key-env-otel-*` fixtures pass.
- H1, kaiak-control — no code change (step 1's synced schema already refuses them);
  regression test added.
- M5, provider (`wire.go`): the errors a provider returns carry no backend bytes, so
  every log line that prints them is safe at once.
  - `sendWire`: a failed `client.Do` reaches `fail` as its class
    (`upstream_unavailable: backend local: malformed response`).
  - `fetchModelsList` (the probe): a failed `Do` and a failed read of the models
    list are named by their class (`backend local: malformed response`,
    `backend local: read models list: connection closed`).
  - Body reads (`readEvent`, stream and plain body, the pending error included) go
    through the new `readFailure`: `io.EOF` and the event stream's own errors
    (`sse.ErrTruncated`, `sse.ErrTooLarge`) stay as they are; anything else — the
    connection's — becomes its class. This covers a body broken off after its
    first bytes, including a chunked trailer line Go quotes whole (found while
    writing the test).
  - Doc comments: `Probe`, `fetchModelsList`, the `ErrStalled…` block;
    `routing.ProbeFunc` now states its error carries no backend text, as it is
    logged as is.
- M5, control client:
  - `send` (`transport.go`): a failed `Do` is its class; a `Kaiak-Protocol`
    mismatch reads `control plane answered <status> without Kaiak-Protocol` /
    `with an invalid Kaiak-Protocol` / `with Kaiak-Protocol <n>` (`protocolSeen`:
    one value, an integer written canonically, else invalid), then `gateway speaks 4`.
  - `errorCode`: a code is kept only when it matches `^[a-z]+(-[a-z]+)*$` and is at
    most 64 characters; else `""`, so `statusError` reads `control plane answered
    <status>` alone.
  - Reads named by class: `read config snapshot: …`, `read usage ack: …`
    (`usage.go`), `read config stream: …` (`stream.go`; the event stream's own
    errors kept as they are).
  - `openStream`: `open config stream: the answer is not text/event-stream` — the
    answer's media type is no longer quoted.
- `docs/ARCHITECTURE.md`: `netfail`'s users (`provider`, `control`, `otlplog`).
- No doc quoted an old error text (checked `docs/`, `GUIDE.md`, `README.md`); the
  specs already state the rule (step 1).

**Every log site checked** (operational lines carrying an error or remote value in
`provider`, `routing`, `control`, `server`, `cmd/kaiak`)

- Fixed at the source (the provider's errors): `routing/modelcheck.go:108` `model
  check skipped: the backend did not answer`; `routing/circuit.go:377,380` `probe
  failed` and `circuit opened` (`kaiak.circuit.last_error`); `server/api.go:217`
  `kaiak.upstream.error.message` (Send's and Next's errors); the circuit's failure
  reason from `server/upstream.go` `classifyAttempt` (`kaiak.circuit.last_error` on
  `circuit opened`). No change at those lines.
- Fixed at the source (the control client's errors): `client.go:353` (boot retry),
  `client.go:560` `logFetchFailure`, `client.go:571,573` `config stream failed`,
  `status.go:152,154` `status report not delivered`, `usage.go:423` `usage batch not
  delivered; retrying`, and the exit line `kaiak stopped with an error`
  (`cmd/kaiak/main.go:329`, Boot's error wraps these).
- Already shape-checked, unchanged: `usage.go:415` `usage batch refused…` (its
  `error.type` must also be one of `batchRefusals`); epochs, control-plane IDs and
  batch IDs on `totals…`, `config event…` and the ack mismatch (`hex32` in the
  message schemas); the backend error `code`/`type` (`backendErrorFields`).
- Local only, unchanged: the data-directory lines (`spool*.go`, `lastknowngood.go`,
  `cmd/kaiak` limits snapshot/totals), `config rejected` (codes only), drain and
  listener lines.

**Decisions made during the step**

- **Classified where the error is built, not where it is logged.** The provider and
  the control client own the exchange; making their errors safe covers every line
  that prints them (four packages, more than a dozen lines) instead of guarding each,
  and keeps local texts that are useful (`models list answered 503`, `the answer is
  not a models list`, the timeouts) instead of collapsing them to `connection
  failed`. Plain error strings (`errors.New(netfail.Class(err))`), as step 2's
  exporter does: no caller inspects a transport error's chain (checked:
  `errors.Is`/`As` uses in `server`, `routing`, `control`, `cmd/kaiak` and the
  tests); context ends are checked on the context first everywhere.
- `netfail` needed no new class.
- `protocolSeen` treats `04` and `+4` as invalid (not the number): the gateway
  compares the header literally, so naming it `4` would read as a contradiction.
- A code in the protocol's shape is logged even if the answer is not the control
  plane's (`bearer-…` would be): the contract's accepted residual — a shape, not a
  vocabulary (step 1).
- **Not changed, for review:**
  - Message validation errors (`ValidationError` from `Decode*`: `config event
    ignored: malformed`, `totals event ignored: malformed`, a rejected snapshot or
    ack on the fetch/usage lines) name the issue's JSON Pointer, which holds the
    message's member names, and a syntax error's offending character. Values are
    never quoted (every `schemacheck` message is fixed text). These answers passed
    the `Kaiak-Protocol` check, and the paths are what makes a mismatch between the
    halves debuggable; logging codes only (as `config rejected` does) would be a
    contract change. Left as is; a backlog entry if wanted.
  - `stream event ignored: unknown event` logs the SSE event name at debug —
    never written (nothing below `INFO` is).
  - The API listener's `ErrorLog` (`net/http` server messages) concerns clients, not
    backends or the control plane — outside M5.

**Regression tests — before (the step's base, `35544cb`) → after**

Codex's ports (renamed):

- B1 → `provider` `TestLogExportVariablesAreNeverSentAsACredential`
  (`OTEL_EXPORTER_OTLP_HEADERS`, `…_LOGS_HEADERS`, `…_LOGS_ENDPOINT`; checks both
  `config.Check` and the provider's last guard): `a config naming
  OTEL_EXPORTER_OTLP_HEADERS as a backend's api_key_env was accepted` and `the probe
  sent OTEL_EXPORTER_OTLP_HEADERS to the backend as Authorization "Bearer
  authorization=Bearer%20export-secret"` (each of the three) → pass.
- B4 provider → `TestBackendBytesNeverReachAProviderError` (probe, send, and a body
  broken by a chunked trailer line): `backend local: Get "…/v1/models": net/http:
  HTTP/1.x transport connection broken: malformed MIME header: missing colon:
  "Bearer provider-secret"`, the same for the `Post`, and `malformed MIME header:
  missing colon: "Bearer provider-secret"` from the body's read → pass (`backend
  local: malformed response`, `upstream_unavailable: backend local: malformed
  response`, `malformed response`).
- B4 control → `control` `TestProtocolMismatchLogsNoHeaderValue` (the token echoed,
  absent, `3`, two values, `04`): `…control plane answered 200 with Kaiak-Protocol
  [\"Bearer cp-secret-token\"], gateway speaks 4` (and `[]`, `["3"]`, `["4" "4"]`,
  `["04"]`) → pass.
- `audit-b.test.ts` → kaiak-control `config.test.ts` "a backend's api_key_env cannot
  name a gateway OTEL_ variable" (the three names): passes on this branch (step 1's
  schema); shown failing with the pre-step-1 schema copy (`2ab42a4`) restored for the
  run — `accepted a backend referencing OTEL_EXPORTER_OTLP_HEADERS`, with the three
  fixtures failing `the document is rejected` — then the copy put back.

Added for the rest of M5:

- `TestErrorCodesAreLoggedOnlyInTheirShape`: `control plane answered 401 Bearer
  cp-secret-token`, `… 401 no--code`, `… 401 Unauthorized`, a 65-character code →
  pass (`control plane answered 401`); `unauthorized`,
  `protocol-version-mismatch` and a 64-character code still logged.
- `TestTransportFailuresAreLoggedAsTheirClass`: `fetch config snapshot: Get "…":
  net/http: HTTP/1.x transport connection broken: malformed MIME header: missing
  colon: "Bearer cp-secret-token"` → pass (`fetch config snapshot: malformed
  response`).

No existing test changed: none asserted remote text in a log
(`TestProtocolMismatchIsLoggedAsAnErrorAndRetried` checks `protocol version
mismatch` and `gateway speaks 4`, both kept).

**Suite** — Go test cache cleared; `scripts/check-all.sh`: **all checks passed**
(exit 0). gofmt, vet, staticcheck clean; `go test -race` every package ok (`e2e`
107 s); live-test kit gofmt, vet, staticcheck, self-test pass; `control` `npm test`
578 / 578 pass, `npm run lint` `tsc` clean and `boundaries ok`; cross-half e2e ok
(54 s). Step 1's expected reds (the `OTEL_` fixtures in `internal/config`) cleared.
