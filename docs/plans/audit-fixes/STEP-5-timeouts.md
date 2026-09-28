# Step 5 — timeouts

**Status:** done (2026-09-25) — `e4c5fad`, `db29a5a`, `b8412b0`. Phase 2 continues
with step 6.

## Items

- **D2 (H1, H7)** backend config: `first_event_timeout_ms` (streams, default 60000),
  `response_timeout_ms` (non-stream, default 1800000; not retried, not a circuit
  failure), `stall_timeout_ms` (between events, default 120000; circuit failure);
  replaces `first_byte_timeout_ms` (no compatibility — feature-building mode). Both
  halves + fixtures + spec; fix `examples/config.json` and the live kit's config.
- **H2** `IdleTimeout` 120 s (API + admin); body-read deadline 60 s covering disposal of
  unread bodies (the [B] unauthenticated probe); per-write deadline 60 s = client gone.
- **H11** stream establishment bounded (connect + response headers) before the idle
  timer.
- **M13** terminal state tracked (finish reason / `[DONE]`); EOF before it = partial +
  upstream failure, no retry after bytes reached the client.

## Acceptance criteria

- Tests per item incl. the [B] socket probe; P3 reliability e2e green.

## Result

Each regression test was run with its fix switched off and failed (or blocked until
killed), then passed with the fix.

- **H11** (`e4c5fad`): `followStream`
  bounds `openStream` (connect + headers) by `IdleTimeout` (45 s) with its own
  cancellation cause, then the idle timer takes over; the default control-plane HTTP
  client has `ResponseHeaderTimeout` 30 s (snapshot/status/usage keep their own
  bounds). Tests: `TestStreamOpenIsBounded` ([B]'s header-stall probe: blocked past
  the 10 s test limit with the bound off), `TestControlPlaneAnswersHaveAHeaderTimeout`.
- **H2** (`db29a5a`):
  `server.ClientTimeouts{Idle, BodyRead, Write}` passed to `Listen` (both listeners),
  env `KAIAK_IDLE_TIMEOUT_MS` / `KAIAK_BODY_READ_TIMEOUT_MS` /
  `KAIAK_WRITE_TIMEOUT_MS` (defaults 120000 / 60000 / 60000; 0 refused). A wrapper
  sets the read deadline for requests with a body (cleared by the inbound stage once
  read), renews the write deadline before every write/flush, clears a stale write
  deadline at each request's start, and sets `Connection: close` on an answer given
  before the body was read. Findings while verifying on a real socket (Go 1.27):
  net/http discards an unread body (≤ 256 KiB) in `chunkWriter.writeHeader`, i.e.
  before the first byte of the answer leaves — the read deadline does apply to it,
  but then the answer's write deadline, set when the handler wrote, has run out by
  the time the discard gives up, so the client got nothing; closing the connection
  skips the discard (the 401 leaves at once, the close follows within the body-read
  bound). net/http also clears the read deadline itself when its background read
  starts at the body's end; a read deadline set on a body-less request would end
  that already-running background read and cancel the request — hence body requests
  only. Tests (real listener): `TestUnauthenticatedStalledBodyIsBounded` ([B]'s
  probe), `TestSlowBodyIsBounded`, `TestIdleConnectionIsClosed`,
  `TestClientThatStopsReadingIsCutOff` (all four hung past their limits with the
  wrapper and idle timeout off), `TestStreamOutlivesTheBodyReadDeadline` (guard),
  `TestClientTimeoutSettings`.
- **D2 / H1 / H7 / M13** (`b8412b0`):
  - config: schema, Go walker, snapshot defaults, kaiak-control types, fixtures
    (`full.json`, `at-bounds.json`, snapshot `full.json`, `timeout-string.json`),
    `examples/config.json` (GPU backends `first_event_timeout_ms` 120000; the
    thinking model's 16 384-token ceiling now falls under the 30 min response
    default), the live kit (all three = `-request-timeout`);
  - provider: the first-event timer (streams) or response timer (non-stream) from
    send; for streams a stall timer that runs only while a read waits; new
    `CodeResponseTimeout` (client answer still `504 upstream_timeout`); `Next`
    returns `ErrStalled`, `ErrResponseTimeout`, `ErrIncomplete` (wrapped); a
    successful stream is complete on `[DONE]` or every seen choice index having a
    non-null `finish_reason`; a successful JSON body when its top-level value closed
    (tracked by the model rewriter already scanning it);
  - server: `retryReason` never retries `CodeResponseTimeout`; `circuitOutcome`:
    response timeout neutral (before or after first bytes), `upstream_stalled` /
    `upstream_incomplete` failures; relay_end values `upstream_stalled`,
    `upstream_incomplete`, `upstream_timeout`; error metric classes follow;
  - fakebackend: `EndAfter` (clean end mid-stream), `Body` alone answers 200.
  - Tests: `TestResponseTimeoutIsNotRetried` (failed: 3 attempts, 3 records),
    `TestStalledStreamEndsAsUpstreamFailure` (blocked until killed),
    `TestSteadyStreamOutlivesTheStallTimeout` (guard),
    `TestStreamEndingBeforeItsTerminalChunkIsIncomplete` ([B]'s probe: ended
    cleanly), `TestJSONBodyEndingEarlyIsAnUpstreamFailure` (unclosed JSON failed;
    the short-of-Content-Length case passed before the fix — the "already an error"
    verification), `TestStreamCompleteness` and
    `TestSuccessfulAnswerEndingBeforeTheFirstEventIsUnavailable` (provider),
    new `TestOutcomeClassification` rows (response timeout neutral, stalled,
    incomplete stream, incomplete JSON), e2e `TestFirstEventTimeoutRetried` (was the
    first-byte test, now streamed) and `TestResponseTimeoutNotRetried` (failed with
    3 attempts). Changed as D2 requires: the retry and timed-out-attempt tests
    stream (the first-event timeout is a stream's), the test config's `slow` backend
    has all three timeouts at 150 ms.

Suite: `scripts/check-all.sh` → `all checks passed` (gateway race tests incl. e2e,
live-kit self-test, control 436/436, lint, cross-half e2e). No expected reds.

## Decisions for the user to confirm

1. **relay_end names**: `upstream_stalled`, `upstream_incomplete`, and
   `upstream_timeout` for a non-stream body still arriving at its response timeout
   (neutral for the circuit, like the timeout before the first bytes).
2. **Completion rule**: `[DONE]`, or every choice index seen has a non-null
   `finish_reason`; checked on `2xx` answers only. A `2xx` stream or JSON body that
   ends before its first event (empty) is `502 upstream_unavailable` and retried.
   Any finish reason (incl. `content_filter`) counts as an end.
3. **Stall progress**: any SSE block, keep-alive comments included, resets the stall
   timer (a proxy injecting keep-alives in front of a hung engine would defeat it).
4. **Client write-deadline expiry** keeps the existing classification of a client
   that left: `client_closed`, circuit success for a `2xx` under way (the brief said
   "neutral"; the spec's existing rule is success).
5. **Client timeouts are env-configurable** (trivial with the existing helper); 0 is
   refused rather than meaning "off".
6. **Early refusals close the connection** when the body was not read (cost: a
   keep-alive client loses its connection on a 401/503-before-body; gain: no wait on
   unsent bodies at all).
7. **A body-read timeout answers `400 invalid_body`** (existing code) rather than a
   new code or `408`.
8. **Stream-open bound = the stream idle timeout (45 s)**; control-plane
   response-header timeout 30 s.

## Deviations

- M13's terminal tracking lives in the provider (its `Response.Next` returns
  `ErrIncomplete`), not the relay: completeness depends on the wire format, which
  only the provider knows (a translating provider will check its own terminal
  event); the relay maps the error to `relay_end`.
- The per-write deadline is applied by the listener's writer wrapper (every write
  and flush on both listeners), not inside the relay only.
