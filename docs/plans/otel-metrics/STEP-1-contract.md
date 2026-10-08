# Step 1 — contract

**Status:** not started

## Intent

The specs say what the plan builds before any code moves: the dependency ruling, the
target metric list, OTLP metric export, its settings, and the telemetry tree's
boundary. Docs only.

## Files likely touched

- `docs/TECH-STACK.md` — the dependency rule restated (decision 11), dated
  2026-10-08, with the SDK's exporters as rejected and why (decision 1, the spike's
  findings) and the trace SDK core named as the traces plan's to take; the "OTLP log
  export: hand-written" ruling extended to metrics; the telemetry tree (decision 13).
- `AGENTS.md` — Project Facts: the hard constraint "zero third-party Go dependencies"
  and "standard library only" replaced by the new rule, pointing to `TECH-STACK.md`.
- `docs/specs/GATEWAY.md` → Observability:
  - the Metric list rewritten to the target table (OVERVIEW → The metric list):
    OpenTelemetry name, kind, unit, attributes, Prometheus name, and a "Was" column
    as the log tables have;
  - the naming rule: one definition, the Prometheus translation
    (`UnderscoreEscapingWithSuffixes`), labels translated the same way; decisions 2,
    4, 6, 14–19 with their reasons;
  - Series at 0, Cardinality (the usage recount with `gen_ai.operation.name` and
    `gen_ai.provider.name`; the ops recount with status codes and `error.type`),
    Errors by key, the key-ID and group switches, Upstream attempts — every bullet
    that names a metric or label, in the new names;
  - **OTLP metric export**, beside OTLP log export: content (every family, same
    names), resource (shared with logs), the data model mapping (counter → monotonic
    sum, up-down counter → non-monotonic sum, gauge, explicit histogram; start time
    = the process start, or the series' creation), temporality (decisions 7, 20),
    the interval and timeout, delivery (the shared rules — reference OTLP log export
    rather than restate), what counts as delivered (`rejectedDataPoints`), at exit
    (decision 21), the self-observability counters (decision 9);
  - Conventions followed: the GenAI pin moved to `4f85037` (decision 10).
- `docs/specs/GATEWAY.md` → Configuration sources: the metrics variables (decision
  8) beside the log export ones; one endpoint variable enables both signals,
  `OTEL_LOGS_EXPORTER`/`OTEL_METRICS_EXPORTER=none` opt one out.
- `docs/specs/GATEWAY.md` → Lifecycle → Draining: the final metric export in the
  exit order.
- `docs/ARCHITECTURE.md` — the telemetry tree in the gateway's module list.

## Decisions made during planning

- The table in `GATEWAY.md` is the contract; the OVERVIEW's table is the draft. Where
  writing it settles a detail the draft leaves open (the exporters' `error.type`
  values, up-down counter vs gauge for each family read at collect), the step
  records it in its Result.
- Units of read-at-collect families: a count of things in progress (active
  requests, queue size) is an up-down counter, as the conventions model
  `http.server.active_requests`; a state or a timestamp is a gauge.

## Acceptance criteria

- Every metric family and label the gateway will write after step 7 is in the table;
  every bullet of Observability names metrics and labels in the new form.
- `AGENTS.md` and `TECH-STACK.md` state the new rule, dated, with no remaining
  "zero third-party" or "standard library only" claim for the gateway
  (`git grep -n -i "zero third-party\|standard library only"` outside plans and
  reviews).
- `scripts/check-all.sh` green (docs only; run to record). Suite recorded.

## Result

