# Step 6 — the capture stage

**Status:** done

## Intent

Add capture to the request pipeline: decide whether a request is captured, read the
session header, hold a copy of the request and of the bytes the client receives
under the capture budget, and once the request is over hand it to a background
builder, which builds the span — every attribute but the content — after the
response has ended and hands it to the exporter. Steps 7–8 fill in the content.

## Files likely touched

- `gateway/internal/capture/` (new):
  - the budget (`KAIAK_CAPTURE_MEMORY_BYTES`: take, give, drop when it cannot);
  - a capture's state for one request: the request copy, the response buffer, the
    session ID, whether it was dropped and why;
  - the background builder (amended 2026-10-13, OVERVIEW decision 24): from the
    copies — still held in the budget — and a summary the finisher hands over (IDs,
    key, groups, model, endpoint, stream, attempts, backend, status, error type,
    usage, cost, timings, the session) to an `otlptrace.Span`, encoded,
    size-checked against the exporter's `MaxSpanBytes()` (a method: the envelope
    carries the operator's resource), its budget charge swapped from the copies to
    the encoded span, enqueued with a `release` that gives the budget back —
    `release` can run inside `Enqueue` (a span queued once the exporter has shut
    down), so the budget's lock is never held across `Enqueue`; its drops are
    counted; at exit its backlog is built before the trace export's final flush;
  - one content hook per API, empty here (steps 7–8).
- `gateway/internal/server/pipeline.go` — the stage `capture`, after `inbound` and
  before `model_access` (decision 17), scoped to the three endpoints:
  - when `rq.identity.Group.Capture` is true and traces are on: reads
    `Kaiak-Session-Id` — a malformed value is dropped with its reason for
    `kaiak.session_id.ignored` (decision 21), never refused; takes the body's length
    from the budget and copies `rq.body` (the body is released when the relay starts
    — the copy is the capture's, charged to its budget); registers a finisher;
  - when the group captures but traces are off: marks `export_off` for the request
    line, nothing else.
- `gateway/internal/server/api.go` — `statusWriter` tees what it writes into the
  capture's response buffer when the request has one, each append taken from the
  budget; a failed take drops the capture (`memory`) and frees what it held. Only
  bytes the client received pass here, gateway error bodies included.
- The finisher (runs after settlement, before the request line): gathers the summary
  — cheap reads of request state — and hands it with the copies to the builder,
  records the outcome for the line (`queued`, or `memory` for a capture dropped
  while the request ran). It builds nothing: finishers run inside the handler,
  before net/http ends the response (amended 2026-10-13).
- `gateway/internal/server/requestlog.go` — `kaiak.capture` and
  `kaiak.session_id.ignored` (decision 27).
- **The provider name** (decision 20): the one function that names it gives every
  type a value; the request line, the usage metrics and the span all read it. The
  tests that pinned its absence on self-hosted types change with it.
- The metrics inputs from step 5: drops by reason.
- Tests: the stage's decisions, the header's rule (valid, too long, bad characters,
  empty — each ignored with its reason, the request served), the budget (a copy that does not
  fit; a response that outgrows it mid-stream; everything given back on every path,
  panics included), the line's attribute, the span's attributes for a success, a
  refusal after the stage, an upstream error, a cut stream.

## Decisions made during planning

- **The tee sits on the client writer,** not beside the meter: it sees exactly what
  the client received — the public model name, replaced error messages, no hidden
  usage chunk — with no change to the relay.
- **The span is built by a background builder, after the client's response has
  ended** (decision 24, amended 2026-10-13 at this step: finishers run inside the
  handler, before net/http ends the response — the last chunk, HTTP/2's
  `END_STREAM` — so a build in the finisher would delay every captured response).
  The finisher only hands off; the request line says `queued`, and the build's own
  drops are counted only (decision 27, amended).
- **No content yet:** the span carries every non-content attribute; the content
  attributes come with steps 7–8.

## Acceptance criteria

- Component tests as above, green.
- A captured request and a non-captured one send byte-identical requests to the
  backend (a test compares them).
- `scripts/check-gateway.sh` green.

## Result

### What changed

- `gateway/internal/capture/` (new):
  - `capture.go` — `Captures` (`New(memory, exporter, logger)`, `Start`, `Finish`, `Drops`,
    `InUse`), `Capture` (`Append`, `Hand`, `SessionIgnored`), the request line's
    outcomes (`OutcomeQueued`, `OutcomeMemory`, `OutcomeExportOff`), `DefaultMemory`
    (moved from `cmd/kaiak`), and `Exporter` — the two methods of
    `otlptrace.Exporter` the builder uses;
  - `budget.go` — take, give, and `swap` (one charge for another, in one step);
  - `session.go` — `SessionHeader` and the rule: the first value, 1–256 bytes of
    `!`–`~`, else `empty`, `too_long`, `invalid_characters` in that order;
  - `span.go` — `Summary` and `Usage` (what the finisher hands over) and the span:
    every non-content attribute of the spec's table, name `chat {model}`, kind
    `CLIENT`, status error with `error.type` (the line's, else `kaiak.relay_end`);
    `server.address`/`server.port` from the backend's `base_url`;
  - `content.go` — `exchange` (the request body, the client's bytes, `stream`,
    `status`) and one empty hook per API, called from the span.
- `gateway/internal/server/`:
  - `capture.go` (new) — the stage `startCapture` and `captureSummary`;
  - `pipeline.go` — the stage `capture` after `inbound`, scope `chatRequests` (a body
    endpoint whose operation is `chat`); `rq.capture`, `rq.captureOutcome`;
  - `api.go` — `NewAPI` takes the captures (nil with trace export off);
    `statusWriter` tees `p[:n]` of every write into the request's capture;
  - `requestlog.go` — `kaiak.capture`, `kaiak.session_id.ignored`;
    `gen_ai.provider.name` on every routed line; `tokensOf`/`costUSD` shared by the
    line and the summary.
- `gateway/internal/provider/provider.go` — `ProviderName`: the well-known value,
  else the type's own name. `internal/metrics/usage.go` reads it unchanged (only a
  backend the config no longer has gives none).
- `gateway/internal/metrics/traceexport.go` — `CaptureDrops.Shutdown` and `.Failed`;
  the processor's `shutdown` series is the builder's and the exporter's summed, and
  `_OTHER` counts the builds that failed.
- `gateway/cmd/kaiak/` — `exports.captures`, made with the trace export in
  `startExports`; `newGraph` registers the capture's drops; `finishTraces`: the
  builder's `Finish`, then the trace flush, within one `exitBound`.
- Docs: `docs/specs/GATEWAY.md` (Request pipeline stage 2; Request capture — Capture
  never gets in the way of serving, The span, Memory, The export limit, On the
  request line and in the counts; the processor's metric row; Exporters' own counts;
  the request line's `kaiak.capture` row; OTLP trace export — Delivery, Counted, At
  exit; Lifecycle → Draining step 5 and the Kubernetes note; a build's fault and
  its `_OTHER` count wherever the drop reasons are listed — the metric row, Naming's
  deviations, series at 0, Exporters' own counts, Counted — and `capture build
  failed` in the operational field table), `docs/ARCHITECTURE.md`
  (request captures, `server`'s finishers, `capture`, `cmd/kaiak`'s exit), this
  file's Files and Decisions. OVERVIEW decisions 24 and 27 were amended by the main
  session before the step.
- No dependency added.

### The builder's design

- **One goroutine**, started by `New`, stopped by `Finish`. Its backlog is a slice
  under a mutex with a wake channel; no count bound — each job holds its copies'
  bytes in the budget, which bounds it.
- **The finisher only hands off**: `Hand` moves the copies (their bytes still
  charged) and the summary into a job and returns `queued`, or `memory` for a
  capture dropped while the request ran. The summary is copies of request state
  plus the token sums; nothing else runs on the request's path but `bytes.Clone` of
  the body and the tee's appends.
- **Build**: the span (summary + content hook) → `otlptrace.Encode` → over
  `MaxSpanBytes()` dropped `too_large`; else `budget.swap(copies, encoded)` — a
  swap that does not fit gives the copies back and drops `memory` — then `Enqueue`
  with a release that gives the encoded size back. No lock is held across `Enqueue`.
- **A fault in a build never reaches serving** (asked by the main session at
  review, 2026-10-13): each build runs under a `recover`. A panic anywhere in it —
  the content hooks (which parse client JSON from step 7 on), the span, the
  exporter's `MaxSpanBytes` — or an encoding error drops that capture alone: the
  bytes it holds at that moment go back to the budget (a local `charged` tracks
  them: the copies, then the encoded span after the swap, then nothing from
  `Enqueue` on — the exporter's release owns them, so nothing is given twice), it is
  counted (`Drops.Failed`, `error.type` `_OTHER`), and one error line `capture build
  failed` carries `kaiak.request.id` and `exception.stacktrace`
  (`runtime/debug.Stack`) — never the panic's value or an error's text, which a
  parser can fill with client content. The builder goes on with the next job.
  `Captures.readContent` (the content hooks, `content` by default) is the tests'
  seam.
- **The budget's flow**: `Start` takes the body's length (fails → `memory`, nothing
  copied); `Append` grows the answer's buffer by doubling from 4 KiB, each growth
  taken first (fails → `memory`, all given back, later appends ignored); the builder
  swaps the charge to the encoded span; the exporter's release gives it back.
- **At exit**: `Finish(ctx)` marks finishing and waits for the builder to empty the
  backlog; when `ctx` ends first it drops the jobs not reached (counted `shutdown`,
  bytes given back) and waits for the build in progress, which completes. A job
  handed after the builder stopped is dropped the same way (no request can be in
  flight then: the drain waits for every handler). `finishTraces` runs `Finish` and
  the trace `ForceFlush` within one `exitBound`, so the shutdown sizing (1 s floor per
  export step) is unchanged.

### Decisions made while implementing

- **`gen_ai.request.stream` comes from the summary** (`rq.inbound.Stream`, the
  client's `stream: true` as the inbound stage parsed it), not from the content
  hooks: steps 7–8 must not write it again.
- **The builder's drain and the trace flush share one bound**, rather than a bound
  each: the documented shutdown time stays grace + timeout + about 10 s.
- **The builder's shutdown drops are counted, not reported** on the final `trace
  export failing` line, whose `kaiak.trace_export.dropped` stays the exporter's.
- **`Captures.InUse()` is exported**: the server's tests check that every path gives
  everything back; it is also what a capture-memory gauge would read.
- **The stage's scope is the endpoints whose operation is `chat`** — exactly chat
  completions, Messages and Responses.
- **The response buffer doubles** (as the body budget's does), so the budget holds
  the buffer's capacity: an answer can be dropped when it nears half the budget left.
- **The session ID's length is in bytes**: a valid value's characters are bytes.

### Tests

- `internal/capture` (10 tests): built and queued, the charge moved to the encoded
  span and given back on export; a copy that does not fit; an answer that outgrows
  the budget mid-way (dropped once, everything given back); `too_large`; an encoded
  span over the budget (`memory` at build); release within `Enqueue`; `Finish` builds
  the backlog in order, and at its bound with the builder held still drops the rest
  (`shutdown`, bytes back) while the build in progress completes, and drops a late
  hand; the span's attributes and status for a routed request with a session, a
  refusal with an ignored session, a cut stream on Anthropic (exact key sets);
  `server.port` defaults; the session rule (13 cases); a build fault — a panicking
  content hook, a panic in `MaxSpanBytes` before the swap, a panic in `Enqueue` after
  it released the span — drops that capture alone (counted failed, `InUse` 0, never
  given twice), the next capture is queued, and the one error line names the request
  with the builder's stack and not the panic's marker.
- `internal/server/capture_test.go` (8 tests, through `fakeotlp`): the stage's
  decisions (chat completions, Messages, Responses and a refusal after the stage
  captured; another group, completions, embeddings, count tokens and a body refused
  by the inbound stage not; `export_off` with trace export off, the session header
  not read); the session header (none, valid, 256, 257, a space, non-ASCII, empty —
  the request served, the reason on the line and the span, the value never on the
  line, never sent to the backend); the span's exact attribute sets, name, kind, IDs,
  times and status for a success, a stream, a refusal, an upstream error, a cut
  stream; the budget (a copy over it; an answer outgrowing it mid-stream while the
  client receives every chunk); everything given back on every path (success,
  refusal, upstream error, client gone, a cut stream — the handler's abort panic —
  and the drain's cut); **the response's end not delayed** (stream and not: with the
  builder held at the exporter, the client reads to EOF, then the build runs);
  **byte-identical backend requests** with capture on and off (four requests on the
  three APIs: bodies, headers, paths); **a build fault never reaches serving**
  (an exporter that panics in the first build: both requests answered `200` and
  `queued`, the second's span exported, one failed build counted, `InUse` 0, one
  error line with the request ID and a stack, no marker in the log).
- `internal/server/api_test.go`: the field table gains four cases (`queued`, an
  ignored session, a refusal after the stage, `export_off`) and the provider name on
  every routed line.
- `cmd/kaiak` (2 tests): captures handed before exit are built and exported before
  the final metric export, which counts them; a builder held still at the bound (the
  1 s floor) drops the capture it did not reach (`shutdown`), gives every byte back,
  and exit completes once the build in progress ends.
- The provider name: `TestProviderNames`, the usage metric tests in
  `internal/metrics`, and the label sets pinned in `internal/server` (`metrics`,
  `retry`, `rerank`, `speech`, `speech_batch`, `transcription`, `images`,
  `image_edits`) and `e2e` (`e2e`, `grouptree`, `images`, `image_edits`, `rerank`,
  `speech`, `transcription`) now carry `gen_ai_provider_name` for self-hosted types.
- Mutation checks (reverted): building in `Hand` fails the delay test for both
  modes ("the response's end waited for the capture's build"); removing the tee
  fails the budget test; flushing traces before the builder's `Finish` fails the
  exit test (no spans exported); keeping the span's charge across `Enqueue` fails the
  fault test (−1497 bytes held: given twice).
- `go test -race -count=3 ./internal/capture`: green.

### Suite

- `scripts/check-gateway.sh`: green — gofmt, vet, staticcheck, the telemetry
  boundary clean; `go test -race -count=1 ./...` passes every package (951 tests,
  examples and fuzz targets listed); the live-test kit's lint and self-test pass.
- `scripts/check-all.sh`: green. Control `npm test`: 679 tests, 678 pass, 0 fail, 1
  skipped (as before); `npm run lint` clean. Final lines:

  ```
  ==> npm run lint (control)
  > tsc -p . && node scripts/check-boundaries.ts
  boundaries ok
  ==> cross-half e2e (sample control plane + two gateways; two cores over one store)
  ok  	kaiak/e2e	80.681s
  all checks passed
  ```

### For steps 7–10

- Steps 7–8 fill `chatCompletionsContent`, `messagesContent`, `responsesContent`
  (`internal/capture/content.go`; each may move to its API's file) from `exchange`;
  `status >= 400` gives no output messages. The exact key sets pinned by
  `TestCaptureSpanAttributes` (server) and `TestSpanAttributes` (capture) grow with
  the content and parameter attributes.
- Step 10: the upgrade notes should say self-hosted usage series and request lines
  now carry `gen_ai.provider.name` (`docs/DEPLOYMENT.md`'s older note says it is
  absent for them).
