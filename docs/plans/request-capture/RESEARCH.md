# Request capture — research

Facts read from source on 2026-10-12/13, for planning. Permalink bases:

- **GENAI** = `https://github.com/open-telemetry/semantic-conventions-genai/blob/5293effb97f18ddb0602da884dadee6119d67679`
  (`main`, 2026-10-12)
- **LF** = `https://github.com/langfuse/langfuse/blob/7ece17c17e798af6a79d8af036b38ce64a76e32b`
  (`main`, matching v4.58.0, 2026-10-13)
- **OTLP** = opentelemetry-proto v1.11.1, `docs/specification.md`

## OpenTelemetry GenAI conventions

- **Where they live:** the GenAI conventions left the core repository on 2026-05-05
  (PR #3696) for `open-telemetry/semantic-conventions-genai`, which has **no release or
  tag yet** (changelog fragments only, "Schema URL: TODO"). The last release carrying
  them is core semconv v1.41.1 (2026-05-11). Everything GenAI is `Development`. The
  gateway's log and metric vocabulary already follows that repository's `main` at a
  named commit (`docs/specs/GATEWAY.md` → Logs: `06ec68e`, `4f85037`).
- **Unreleased changes on `main` that matter here:** the inference span is named
  `gen_ai.client.inference` (`docs/gen-ai/client-inference.md`, #521);
  `gen_ai.usage.cache_creation.input_tokens` → `gen_ai.usage.cache_write.input_tokens`
  (#440 — the gateway's logs already use `cache_write`); `gen_ai.request.top_k` is an
  int (#217); `gen_ai.system_instructions` parts are text only (#257);
  `gen_ai.output.messages[].finish_reason` is deprecated for
  `gen_ai.response.finish_reasons` (#363).
- **The inference span** (GENAI `docs/gen-ai/client-inference.md#L42-L433`):
  - name `{gen_ai.operation.name} {gen_ai.request.model}`; kind `CLIENT`;
  - required: `gen_ai.operation.name`, `gen_ai.provider.name` (well-known values
    include `openai`, `anthropic`, `azure.ai.openai`; "SHOULD be set based on the
    instrumentation's best knowledge and may differ from the actual upstream
    provider");
  - conditionally required: `error.type`; `gen_ai.conversation.id` "when readily
    available" — "a new UUID, a trace identifier, or a hash of request content SHOULD
    NOT be used as a fallback"; `gen_ai.request.model`; `gen_ai.request.stream` (if
    and only if streaming); `gen_ai.request.choice.count` (if not 1);
    `gen_ai.request.seed`; `gen_ai.request.top_k`; `gen_ai.output.type`;
    `server.port`;
  - recommended: `gen_ai.request.max_tokens`, `temperature`, `top_p`,
    `frequency_penalty`, `presence_penalty`, `stop_sequences`; `gen_ai.response.id`,
    `gen_ai.response.model`, `gen_ai.response.finish_reasons`,
    `gen_ai.response.time_to_first_chunk`; `gen_ai.usage.input_tokens` (all input,
    cache reads and writes included), `output_tokens`, `cache_read.input_tokens`,
    `cache_write.input_tokens`, `reasoning.output_tokens`; `server.address`;
  - opt-in: `gen_ai.input.messages`, `gen_ai.output.messages`,
    `gen_ai.system_instructions`, `gen_ai.tool.definitions`.
- **Content** (GENAI `docs/gen-ai/gen-ai-spans.md#L905-L1009`, `model/gen-ai/*.json`):
  - input messages "MUST be provided in the order they were sent to the model"; each
    output message is one generation (choice); on error, output only "when the
    operation produced output, including partial streamed output";
  - on spans the content "MAY be recorded as a JSON string if structured format is
    not supported and SHOULD be recorded in structured form otherwise";
  - **system instructions:** `gen_ai.system_instructions` "SHOULD be used when the
    corresponding provider or API allows to provide system instructions or messages
    separately from the chat history. Instructions that are part of the chat history
    SHOULD be recorded in `gen_ai.input.messages`". The example puts Responses'
    `instructions` in `gen_ai.system_instructions` and a `system` item of `input` in
    the input messages;
  - **message:** `{role, parts, name?}` (role `system|user|assistant|tool`, any
    string allowed); output messages add `finish_reason` (deprecated on `main`);
  - **parts** (`type`, then fields; † required): `text` (content†); `tool_call` (id,
    name†, arguments: any); `tool_call_response` (id, response†: any);
    `server_tool_call`, `server_tool_call_response`; `blob` (modality†, mime_type,
    content† — base64 in JSON); `file` (modality†, mime_type, file_id†); `uri`
    (modality†, mime_type, uri†); `reasoning` (content†); a **generic part**: any
    `type`, additional properties allowed. Modality: `image|video|audio|document`;
  - **tool definitions:** `{type: "function", name†, description, parameters}` or a
    generic `{type†, name†}`;
  - **size and privacy:** content is "considered sensitive and often large";
    instrumentations "SHOULD NOT capture them by default, but SHOULD provide an option
    for users to opt in"; recording on spans suits "situations where telemetry volume
    is manageable"; storing content externally and referencing it is recommended in
    production, but **no reference format exists yet** ("TODO"); truncation that keeps
    the JSON structure MAY be offered; the streaming-chunks section is "TODO".
- **The details event** `gen_ai.client.inference.operation.details` is a log event
  carrying the same content attributes, structured. Langfuse has no logs endpoint.

## Langfuse (LF)

- **Ingestion:** `POST /api/public/otel/v1/traces`, Basic auth (`public:secret`),
  `application/json` or protobuf, gzip optional
  (`web/src/server/otel/processOtelIngestion.ts#L56-L105`). Body limit
  `LANGFUSE_OTEL_INGESTION_MAX_BODY_BYTES`, default 512 MiB (413 above), a warning
  above 16 MiB. Recommended header `x-langfuse-ingestion-version: 4`.
- **Reads the standard content attributes**
  (`packages/shared/src/server/otel/OtelIngestionProcessor.ts#L2405-L2433`):
  `gen_ai.input.messages` → input, `gen_ai.output.messages` → output,
  `gen_ai.system_instructions` prepended as a system message (unless one exists);
  `langfuse.observation.input|output` win when present; a span event
  `gen_ai.client.inference.operation.details` is checked first (`#L2083-L2105`).
- **Content must be a JSON string:** `convertValueToPlainJavascript` (`#L1572`)
  handles `stringValue`, `intValue`, `doubleValue`, `boolValue`, `arrayValue`; a
  `kvlistValue` falls through to `JSON.stringify` of the OTLP wrapper — structured
  messages arrive garbled.
- **Tools:** `gen_ai.tool.definitions` as a JSON string or array
  (`packages/shared/src/server/ingestion/extractToolsBackend.ts#L545-L550`).
- **Session and user** (`#L2645-L2683`): session ← `langfuse.session.id`, `session.id`,
  `gen_ai.conversation.id`; user ← `langfuse.user.id`, `user.id`.
- **Model** (`#L2853`): `gen_ai.response.model`, then `gen_ai.request.model`. **Model
  parameters:** every `gen_ai.request.*` but the model.
- **Usage** (`#L3147-L3290`): input and output tokens, `cache_read.input_tokens`,
  `cache_write.input_tokens` (or `cache_creation`), `reasoning.output_tokens`; cache
  tokens subtracted from input — the convention's inclusive rule. Any other
  `gen_ai.usage.*` key becomes an extra usage key and is summed into the total
  (`worker/src/services/IngestionService/index.ts#L1603-L1607`).
- **Generation:** `gen_ai.operation.name` `chat` (and others) → GENERATION
  (`ObservationTypeMapper.ts#L249-L266`). Span status code 2 → level ERROR.
- **Not mapped:** `gen_ai.response.time_to_first_chunk` (completion start time comes
  from `langfuse.*` and `ai.*` only).
- **IDs:** a string trace or span ID is used as is (`#L3826`) — hex, as OTLP JSON
  requires.
- **Media:** inline media of 1 KiB or more is moved to Langfuse's media store — `data:`
  URIs and semconv `blob` parts are recognised
  (`packages/native/src/otel_media/provider_shapes.rs`).

## OTLP/HTTP JSON, traces (OTLP)

- `{"resourceSpans":[{"resource":{"attributes":[…]},"scopeSpans":[{"scope":{"name":…},"spans":[…]}]}]}`;
  a span: `traceId`, `spanId` (hex strings, not base64), `name`, `kind` (integer:
  3 = CLIENT), `startTimeUnixNano`, `endTimeUnixNano` (decimal strings), `attributes`,
  `status: {code, message}` (0 unset, 1 ok, 2 error). A root span has no
  `parentSpanId`.
- Path `/v1/traces`; the response is `ExportTraceServiceResponse`, partial success
  `partialSuccess.rejectedSpans`. Retry only 429, 502, 503, 504.
- Body limits: servers MUST limit the body (recommended 64 MiB) and answer 413. The
  OpenTelemetry Collector's OTLP/HTTP receiver defaults to **20 MiB**
  (`config/confighttp/server.go#L37`, v0.162.0), checked again after decompression.

## The OpenTelemetry Go trace SDK (2026-10-12)

- v1.47.0: `otel`, `otel/sdk/trace` and the propagator add about **1.85 MB** to a
  stripped static binary (5.50 → 7.35 MB on a minimal `net/http` program) and link 11
  modules: OpenTelemetry's `otel`, `trace`, `sdk`, `metric`, `log`, `auto/sdk`, and
  `xxhash`, `logr`, `stdr`, `uuid`, `golang.org/x/sys`. No gRPC or protobuf.
- Its batch and simple span processors report export errors through `otel.Handle`
  (the global logger: Go `log` lines on stderr) and drops only through experimental
  metrics (`OTEL_GO_X_OBSERVABILITY`) on the global meter; `NewTracerProvider` reads
  `OTEL_TRACES_SAMPLER` and the span limits from the environment, `WithResource`
  merges `OTEL_RESOURCE_ATTRIBUTES`; bad values reach `otel.Handle`, never fail.

## The gateway today (read 2026-10-12)

- **Stages:** `server/pipeline.go:220-287` — `stage{name, run, scope}`, the list in
  `newPipeline` ("A new stage is a new entry here"); `API.serve` (`api.go:108-133`)
  runs them; finishers (`rq.finishers`) run in reverse registration order from the
  deferred `rq.finish()`, panics included. Order: admission, auth, key concurrency,
  inbound, model access, endpoint list, upload measured, model params, limits,
  attempts (routing, meter, provider, relay). Finishers: settlement, limiter settle,
  `releaseBody`, key exit, then the metrics and the request line (registered first,
  run last).
- **Body:** `rq.body` holds the client's bytes as received (`pipeline.go:155-160`); edits
  are spliced into a new slice by the provider (`provider/body.go:83-174`), never
  into `rq.body`. **The body is released at the relay's start** (`relay.go:29` →
  `bodies.go:154-160`) — holding it for a stream's life was removed by the
  2026-10-05 audit (`docs/plans/audit-fixes/STEP-7-memory-and-state.md:40-43`).
- **No translation:** every request passes through in its own format to a type that
  serves it natively; what the client sent is `rq.body`, what it received is the
  relayed bytes.
- **Relay:** `relayResponse` (`relay.go:28-120`); `provider.Event{Data, Payload,
  Hidden}` (`provider.go:195-207`) — `Data` is what the client gets (model renamed to
  the public name, error messages replaced), `Hidden` the usage chunk the client did
  not ask for. All client bytes, gateway error bodies included, pass `statusWriter`
  (`api.go:189-208`). Non-stream bodies stream through in 32 KiB pieces; SSE blocks are
  capped at 16 MiB. Only the final attempt's bytes reach the client.
- **At the end:** `rq.id` (from a valid `X-Request-Id`, else generated), `rq.keyID`,
  `rq.identity.Group` (`ID`, `PathIDs`), `rq.model`, `rq.endpoint`, `rq.inbound.Stream`,
  `rq.attempts` / `rq.answeringAttempt()`, `rq.ttft`, `rq.relayEnd`, `errorCode(rq)`,
  `rq.usage` (`accounting.UsageRecord`); the request line (`requestlog.go:20-117`)
  is the model for reading them.
- **Headers:** no client header is forwarded to a backend; outbound headers are built
  from scratch (`send.go:285-316`). No `Kaiak-*` client header exists yet.
- **Budgets:** body `KAIAK_BODY_MEMORY_BYTES` (512 MiB), uploads
  `KAIAK_UPLOAD_MEMORY_BYTES` (256 MiB), usage `KAIAK_USAGE_MEMORY_BYTES` (64 MiB); the
  JSON body cap `global.max_request_body_bytes` defaults to 4 MiB. Settings parse in
  `cmd/kaiak/settings.go`.
- **Groups:** `config.Group{ID, PathIDs, AllowedModels, Limits}`
  (`config/snapshot.go:467-480`), resolved in `resolveGroup` (`:636-666`); a key's
  group at `:584`. The schema's `$defs.group` (`protocol/schema/config.schema.json:436-467`)
  has no `required`.
- **Telemetry:** `otlp.Signal` (`signal.go:7-70`: logs, metrics); `ReadSettings`
  (`settings.go:90-157`) — off with `OTEL_SDK_DISABLED`, `OTEL_<SIG>_EXPORTER=none`,
  or no endpoint and no `otlp` exporter; only `http/json`; malformed values fail the
  start. The client (`client.go`) never follows redirects, retries 429/502/503/504,
  counts a 2xx as delivered only when its body is empty or a valid export response.
  `otlplog` queues up to 10 000 records (drop newest), batches 512; its counts are the
  `otel.sdk.processor.log.processed` and `otel.sdk.exporter.log.exported` metrics
  (`internal/metrics/logexport.go`). At exit, `finishExport` bounds each exporter by
  the drain deadline (`cmd/kaiak/main.go:330-371`). `fakeotlp` decodes logs and
  metrics only, and keeps no path. The telemetry tree imports only the standard
  library and `netfail` (`scripts/check-gateway.sh:39-56`).
- **Protocol 8 churn:** about 215 fixture files carry a config (two with 7 142 groups:
  `config/valid/counters-at-bound.json`, `config/invalid/counters-exceeded.json`);
  `examples/config.json`, `examples/local-config.json`, `scripts/live/config.go` and
  some 30 Go and 4 TypeScript test files build configs with groups. A schema-kind
  invalid fixture missing the new member would still pass for the wrong reason
  (`fixturetest.go:123-158` checks only `[schema]`).
