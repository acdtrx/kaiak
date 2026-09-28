# Step 7 — accounting

**Status:** done (2026-09-24) — phase 3 continues with step 8; suite green, no expected reds.

## Intent

Settle every request into a usage record: units from the backend (or estimated and
flagged), cost from the price table, partial usage on disconnect.

## Files likely touched

- `gateway/internal/accounting/` — usage extraction (non-stream body, final stream
  chunk), estimation, pricing, usage record type, record sink interface

## Decisions made during planning

- Usage units map: `tokens_in`, `tokens_out`, `tokens_cached`, `tokens_reasoning`
  (from the OpenAI usage detail fields when present).
- Estimation when usage is missing: about 4 bytes per token over the prompt body and
  the streamed content; record flagged `estimated`.
- Disconnect: count streamed output so far (estimated unless the backend reported),
  flag `partial`.
- Price in force = latest entry with effective date ≤ request start; unpriced → cost 0.
- The **record sink** is an interface: in file mode it feeds the local limit counters
  (step 8) and metrics (step 9); P2 adds the control-plane sender. Record fields follow
  `CONTROL-PROTOCOL.md` so P2 does not reshape them.

## Acceptance criteria

- Tests: exact usage from stream and non-stream; missing usage → estimated flag;
  disconnect → partial record; price selection across effective dates; unpriced = 0.

## Result

Commands run (2026-09-24):

- `scripts/check-gateway.sh` — gofmt, `go vet`, staticcheck 2026.2.1 clean;
  `go test -race ./...` **pass** (`cmd/kaiak`, `accounting`, `auth`, `config`,
  `provider`, `routing`, `server`, `state`). Also `-count=5` on `accounting` and
  `server` — pass, no flakes.
- `npm test` in `control/` — 87 tests **pass**; `npm run lint` — `tsc` clean,
  `boundaries ok`.
- Manual, built binary against three fake backends from a throwaway main (deleted with
  every scratch file), model prices 2 / 0.5 / 8 USD per million in / cached / out:
  - non-stream chat, backend usage 1200 prompt (1000 cached), 300 completion (120
    reasoning) → `tokens_in 200, tokens_cached 1000, tokens_out 300,
    tokens_reasoning 120, cost_usd 0.0033, estimated false, partial false`;
  - streamed chat, client did not ask for usage → the client saw no usage chunk, the
    log line had the same exact units and cost;
  - backend without usage → `tokens_in 16` (62-byte body), `tokens_out 5` ("Hello
    from the fake"), `cost_usd 0.000072, estimated true`;
  - stream the client dropped after two chunks → `relay_end client_closed,
    tokens_in 9, tokens_out 3, estimated true, partial true`.

Delivered:

- Protocol change, its own commit (user decisions, settled 2026-09-24): price units
  `tokens_in`, `tokens_cached`, `tokens_out` only, disjoint, unpriced `tokens_cached`
  charged at the `tokens_in` price; `defaults` take any non-null JSON value (objects
  included); public model names may not end in `/props` (`public_model_name` in the
  schema; Go walker and ajv both). Fixtures: `full.json` gains a
  `chat_template_kwargs` object default, `price-history.json` loses its
  `tokens_reasoning` price, `defaults-object-value.json` is gone (now valid), new
  invalid `price-reasoning-unit.json` and `model-name-props-suffix.json`;
  `kaiak-control` types (`UsageUnit`, `PriceUnit`, `DefaultValue`).
- `gateway/internal/accounting`:
  - `Meter` (`NewMeter(endpoint, requestBytes)`, `Answered(status, stream)`,
    `Observe(event)`, `Settle(complete) (Units, Flags)`); a streaming member scanner
    for non-stream bodies (keeps top-level `usage` and `choices` without collecting the
    body); `EstimateTokens` (4 bytes a token, rounded up — step 8 uses it for the
    input estimate).
  - `PriceAt`, `Cost` (nano-USD); `UsageRecord` with the protocol's JSON names;
    `Sink`, `Fanout`; `Recorder` (`NewRecorder(instanceID, sink)`,
    `Settle(Request, meter, complete)`).
- `gateway/internal/server`: stage `accounting` between routing and provider (opens
  the meter, registers settlement as a finisher); the relay loop calls `Answered` and
  `Observe` (before the hidden check and before writing); `request.usage` holds the
  settled record for later finishers (step 8's release runs after it) and the log
  line; the log line is now the first finisher registered, so it runs last and
  carries units, `cost_usd`, `estimated`, `partial`. `NewAPI` takes the recorder.
- `cmd/kaiak`: `accounting.NewRecorder(instanceID, accounting.Fanout{})` — the fan-out
  has **no sinks yet**: records are settled and logged, and step 8 (limits) and step
  9 (metrics) register the first sinks; P2 adds the control-plane sender.
- `fakebackend`: `Usage.CachedTokens`, `Usage.ReasoningTokens` (reported in the
  OpenAI detail objects).
- Docs: `CONTROL-PROTOCOL.md` — units and price units, object defaults, `/props`
  rule, usage record fields; `GATEWAY.md` — accounting stage placement, one record
  per routed request, usage mapping, estimation bytes, partial, no-answer and error
  status rules, cost arithmetic, sinks, log fields; `ARCHITECTURE.md` — stage order
  and `accounting` entry.

Decisions beyond the plan:

- **One record per routed request, always.** A request with no backend answer
  (connect failure, first-byte timeout, refused backend credential, client gone
  before the first event) gets a record with zero units, flagged `partial`; a relayed
  backend error status gets zero units, unflagged. Estimating these would bill
  requests that may never have reached a model; keeping the record gives every routed
  request exactly one settlement (step 8 releases its reservation from it). Requests
  refused before routing get none.
- **Estimation bytes**: input = the client's request body as received (the basis
  limits use too); output = decoded UTF-8 bytes of generated content only (chat
  content, refusal, reasoning text counted once when both `reasoning` and
  `reasoning_content` are sent, tool-call names and arguments; completions text).
  Non-stream choices are kept up to 4 MiB; beyond, their raw size counts.
- **Cost as integer nano-dollars** (`cost_nano_usd`), computed once per record in
  float64 and rounded: exact integer sums downstream, ≤ 0.5 n$ rounding per record;
  a test sums a million records exactly. Rejected: float64 USD in the record (sums
  drift); integer pico-dollars (JavaScript numbers lose exactness past ~$9,000).
- **Last usage report wins** in a stream (backends reporting on every chunk send
  running totals); a usage object with neither `prompt_tokens` nor
  `completion_tokens` is no report.
- **The meter reads the client-format response** (`Event.Payload` for stream chunks,
  the relayed bytes for bodies), so a translating provider needs no accounting
  change; the response-model rewrite never touches `usage`.
- **Sinks are called synchronously** on the request goroutine and must not block —
  no goroutine or queue in accounting itself; a sink that does I/O brings its own
  queue (P2's sender).
- **`gateway_time`** is the settlement time (UTC); price selection uses the request's
  start.

Deviations: none from the acceptance criteria.

Open doubts:

- A `504` first-byte timeout on a non-stream request may have been billed by a cloud
  backend (it generated, we stopped waiting); the record counts zero. Observable only
  by comparing with the provider's invoice.
- `cost_nano_usd` fits a JavaScript number exactly only up to ~$9M per value;
  `kaiak-control` aggregates beyond that need `BigInt` (noted in the spec).
