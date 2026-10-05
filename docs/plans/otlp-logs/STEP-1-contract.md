# Step 1 — contract

**Status:** not started

## Intent

Write down the gateway's log vocabulary and the OTLP log export as contract: the
field table every log line follows, the variables, the record mapping, delivery and
its limits, the metric, and the no-dependency choice. Steps 2–4 implement what this
step writes.

## Files likely touched

- `docs/specs/GATEWAY.md`:
  - Observability → Logs: **the field table** — every attribute the gateway logs
    today (the request line's and the operational events'), its new name, and its
    meaning where it differs from today's (token counts: `gen_ai.usage.input_tokens`
    is all input; cache read and creation are its parts). Standard names from the
    OpenTelemetry conventions where the meaning matches — HTTP (`http.request.method`,
    `url.path`, `http.response.status_code`, `server.address`), error (`error.type`),
    GenAI (`gen_ai.request.model`, `gen_ai.response.model`, `gen_ai.request.stream`,
    `gen_ai.operation.name`, `gen_ai.provider.name` if a well-known value fits each
    backend type, `gen_ai.usage.*`) — and `kaiak.*` for the rest. The conventions
    version followed is named (HTTP from `semantic-conventions`, GenAI from
    `semantic-conventions-genai`, checked on the day). The request line's message,
    and whether the OTLP record carries an `eventName`, are settled here;
  - Configuration sources: the `OTEL_*` variables accepted, their fallbacks and
    defaults, `http/json` as the only protocol, start failures;
  - Observability: a new *OTLP log export* bullet beside *Logs* — every line, the
    same content as stderr, the resource, the mapping, delivery (queue, batches,
    retries, drop-newest, one export in flight), drain behaviour after usage, export
    problems on stderr only;
  - the metrics table: `kaiak_log_export_records_total{outcome}`.
- `docs/DEPLOYMENT.md`: how to point a gateway at a collector (sidecar or node agent),
  the variables, a minimal Collector config with the `otlp` receiver on HTTP, the
  drop metric to alert on.
- `docs/TECH-STACK.md`: the OTLP/HTTP JSON exporter is hand-written; the OTel Go SDK
  rejected as the gateway's first third-party module (dated).
- `docs/BACKLOG.md`: the *OpenTelemetry export* entry's *Logs* layer now names this
  export as built (the entry stays for traces and the `traceparent` test).

## Decisions made during planning

- All decisions in OVERVIEW 5–12 are recorded in the spec, dated 2026-10-05.
- Before naming a field, read its definition in the conventions: a standard name is
  used only where the meaning matches; otherwise `kaiak.*`.
- Inventory the current fields from the code (every `slog` call in `gateway/`), not
  only the spec: the table must cover every attribute a log line can carry.

## Acceptance criteria

- The field table covers every attribute the gateway logs (checked against the
  code), each standard name checked against its convention's definition.
- The spec states every variable, default and failure; the mapping; delivery and drop
  rules; the metric; drain order — each dated.
- No code change. Suite run and recorded (green: docs only).

## Result

_Not started._
