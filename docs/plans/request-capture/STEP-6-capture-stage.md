# Step 6 — the capture stage

**Status:** not started

## Intent

Add capture to the request pipeline: decide whether a request is captured, read the
session header, hold a copy of the request and of the bytes the client receives
under the capture budget, and at the request's end build the span — every attribute
but the content — and hand it to the exporter. Steps 7–8 fill in the content.

## Files likely touched

- `gateway/internal/capture/` (new):
  - the budget (`KAIAK_CAPTURE_MEMORY_BYTES`: take, give, drop when it cannot);
  - a capture's state for one request: the request copy, the response buffer, the
    session ID, whether it was dropped and why;
  - the span builder: from a summary the server hands over (IDs, key, groups, model,
    endpoint, stream, attempts, backend, status, error type, usage, cost, timings,
    the session) to an `otlptrace.Span`, encoded, size-checked against
    `otlptrace.MaxSpanBytes`, enqueued with a `release` that gives the budget back;
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
- The finisher (runs after settlement, before the request line): builds the summary,
  calls the capture's span builder, records the outcome for the line.
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
- **The finisher runs after the client has its last byte;** parsing and encoding there
  never delays the response (decision 24).
- **No content yet:** the span carries every non-content attribute; the content
  attributes come with steps 7–8.

## Acceptance criteria

- Component tests as above, green.
- A captured request and a non-captured one send byte-identical requests to the
  backend (a test compares them).
- `scripts/check-gateway.sh` green.
