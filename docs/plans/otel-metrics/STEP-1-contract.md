# Step 1 — contract

**Status:** done (2026-10-08)

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

### What changed

- `docs/specs/GATEWAY.md` → Observability: the Metric list rewritten as one table
  (OpenTelemetry name | Kind | Unit | Attributes | Prometheus | Meaning | Was), with
  the attribute renames common to every family stated once above it; new bullets
  **Naming** (one definition, the `UnderscoreEscapingWithSuffixes` translation, no
  unit `1`, no `target_info` / `otel_scope_*`, the conventions' names, the attribute
  vocabulary, the named deviations), **The request duration's attributes** (route
  list, `_OTHER`, no status for a client that left), **Circuit state, one family**,
  **Exporters' own counts**; every other bullet that named a metric or label in the
  new names (alerts in PromQL keep Prometheus names); Usage labels with operation,
  provider and modality; Series at 0, Cardinality (usage and ops recounted), Errors
  by key, Key-ID and group switches, Upstream attempts, Config load cost. **OTLP
  metric export** added after OTLP log export (content, resource, data model,
  temporality, interval and timeout, size, delivery, counts, problems, at exit).
  Logs: "One vocabulary" now includes metrics; Conventions followed covers metrics,
  GenAI pin `4f85037`; Units rule covers metrics; request-line rows for the status,
  `kaiak.key.group`, `kaiak.deployment.model`, `kaiak.endpoint`; new operational
  fields `kaiak.metric_export.endpoint` and `kaiak.metric_export.failed`; OTLP log
  export counts in the `otel.sdk.*` names.
- `GATEWAY.md` → Configuration sources: "OTLP log export" became "OTLP export",
  per signal, with the metric variables; Providers' `OTEL_` reservation names the
  metrics headers; Lifecycle → Draining: the final metric export in the exit order,
  the second signal and the termination-grace note; metric names in Routing,
  Limits, Accounting, Control-plane mode and the connection cap.
- `docs/TECH-STACK.md`: the dependency rule (settled 2026-10-08) replacing zero
  third-party dependencies, trace SDK core named as allowed; "OTLP export:
  hand-written" for both signals with the SDK exporters' rejection and the spike's
  findings; the telemetry tree; Metrics bullet; build-info name; staticcheck wording.
- `AGENTS.md` Project Facts: gateway line and hard constraint restated.
- `docs/ARCHITECTURE.md`: gateway line, a metrics data-flow bullet, `metrics`
  entry, `telemetry/` tree replacing `otlplog`, `netfail` users, endpoint row.
- Outside the brief, found by the acceptance grep or stating the old rule:
  `README.md`, `docs/kaiak.md` (principle 3, and 7 names OTLP),
  `docs/architecture/gateway.html` (the "at a glance" line only).

### Details settled here

- **Up-down counter vs gauge**: an up-down counter when the value still means
  something summed over its series (across attributes or gateways):
  `kaiak.backend.active_requests`, `…_limit`, `kaiak.queue.size`,
  `kaiak.usage.queue.{batches,records,size}`, and — departing from this file's
  planning note "a state is a gauge" — `kaiak.circuit.state` and
  `kaiak.deployment.cooling_down` (unit `{deployment}`): the conventions' state
  pattern decision 16 cites (`hw.status`, `k8s.node.condition.status`) is an
  up-down counter, and summed by state it counts deployments. Gauges: timestamps,
  `kaiak.config.size`, `kaiak.build.info`, the `kaiak.control.*` flags. Same
  Prometheus output either way; up-down counters are cumulative under every
  temporality preference.
- **Read at collect**: the gauges and up-down counters of routing and the control
  connection, and the three `otel.sdk.*` counters (read from the exporters' own
  counters — so `lowmemory` keeps them cumulative). The usage queue up-down counters
  are not marked read at collect.
- **Name**: `kaiak.backend.active_requests_limit`, not `….active_requests.limit` —
  no metric name is also a namespace; same Prometheus name as the plan's table.
- **Exporter `error.type`**: the collector's status as a string (the status that
  ended the export), `rejected` (partial success), `malformed_response`, `timeout`,
  and `netfail`'s connection classes as identifiers (`connection_refused`,
  `name_not_resolved`, `host_unreachable`, `connection_closed`, `tls_failure`,
  `connection_failed`). Log queue: `queue_full`, `shutdown`. Component types
  `batching_log_processor`, `otlp_http_json_log_exporter`,
  `otlp_http_json_metric_exporter`, names `<type>/0`. Failure series are created on
  first use (a Series-at-0 exception: the status values come from the collector).
- **Timeouts**: one metric export, retries included, is bounded by the shorter of
  `OTEL_METRIC_EXPORT_TIMEOUT` and the OTLP timeout (10 s with the defaults).
- **Request duration `error.type`** also takes `kaiak.relay_end` for a response
  broken off after it started — the value `kaiak.request.errors` carries.
- **Size**: an export over 4 MiB of JSON is split into requests of at most 4 MiB
  (whole metrics where they fit, a large metric's points split) — the usage
  families reach tens of MB at the target scale with the key-ID label on. Step 9
  implements; revisit the bound there if it proves wrong.
- **At exit**: final metric export after the admin listener stops and before
  `kaiak stopped`, so its failure line reaches the log flush; bounded as the log
  flush. `metric export failing` is an ordinary line (also exported as a log).
- The spec said "all five triggers" / "5 triggers" for config loads; the code has
  four (`startup`, `sighup`, `control`, `seed`) — corrected to four.
- Left for later steps: `TECH-STACK.md`'s `-ldflags -X …internal/metrics.version`
  path (step 3); the `otlplog/settings.go` comment pointing at "Configuration
  sources: OTLP log export", now "OTLP export" (step 2 moves the file);
  `DEPLOYMENT.md`, `BACKLOG.md`, `gateway.html` metric names (steps 6, 11, 12).

### Suite

`scripts/check-all.sh` (docs only) — green, exit 0: gofmt, vet, staticcheck,
race tests, control `npm test` (629 tests, 628 pass, 1 skipped, 0 fail), lint
(`boundaries ok`), cross-half e2e `ok kaiak/e2e 69.353s`, "all checks passed".
Acceptance grep (`zero third-party\|standard library only` outside plans and
reviews): remaining hits are the live-test kit (`ARCHITECTURE.md`,
`scripts/live/main.go` — a separate module, still standard library only),
`netfail` (a fact about that package) and `TECH-STACK.md` naming the old rule as
rejected.
