# Plan: OTLP log export

## Goal

Send every gateway log line to an OpenTelemetry collector over OTLP/HTTP, beside
the existing output on stderr, for infrastructure that cannot read container output
(requested 2026-10-05). The content is exactly what the gateway logs today: the
per-request line (outcome, refusal detail, attempts, timings, units, cost, key ID —
`GATEWAY.md` → Observability: Logs) and every operational event (config applied,
circuits, drain, control-plane contact).

## Scope

- A gateway-only exporter: an `slog` handler that fans each record out to the
  existing stderr handler and to an OTLP batcher.
- Configured per gateway from the standard `OTEL_*` environment variables, so a
  collector sidecar or agent works with no kaiak-specific setup.
- OTLP/HTTP with JSON encoding, hand-written on the standard library.
- Delivery that never blocks a request: a bounded queue, background batches,
  retries with backoff, drops counted in a metric.
- Spec, DEPLOYMENT, tests (a fake collector), and a live check against a real
  OpenTelemetry Collector.

## Out of scope

- Traces and the `traceparent` test (`docs/BACKLOG.md` → OpenTelemetry export).
- OTLP metrics: `/metrics` stays Prometheus text, which collectors scrape.
- gRPC and protobuf encoding (the user's target accepts OTLP/HTTP, 2026-10-05).
- Configuration from the control plane: endpoint headers often carry credentials,
  and config holds no secrets (settled with the user, 2026-10-05).
- Turning stderr off: it stays always on for now (user, 2026-10-05).
- The sample control plane's logs.

## Decisions

Settled with the user (2026-10-05):

1. **Per gateway, from environment variables** — not from the pushed config.
2. **stderr always on; OTLP on when configured.**
3. **Every log line**, not only request lines.
4. **OTLP/HTTP.**
5. **One log vocabulary, OpenTelemetry's where it fits** (chosen over translating in
   the exporter only, 2026-10-05). The gateway's own log attributes — stderr and OTLP
   alike — take the OpenTelemetry semantic-convention names where the meaning
   matches (HTTP and error: stable; GenAI: still in development, in
   `semantic-conventions-genai`), and `kaiak.*` names for the rest (`kaiak.key_id`,
   `kaiak.limit.*`, `kaiak.attempts`, …). Token counts follow the GenAI meaning:
   `gen_ai.usage.input_tokens` is all input, cache reads and writes are its parts —
   computed, not renamed. One rename for anyone querying today's fields; allowed
   before release. Usage records, the protocol and Prometheus metrics keep their own
   names.

Made while planning (confirm in review):

6. **Standard variable names**, a subset of the OTel SDK spec:
   - `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` (used as is) or `OTEL_EXPORTER_OTLP_ENDPOINT`
     (`/v1/logs` appended). Export is **on when either is set**;
     `OTEL_LOGS_EXPORTER=none` or `OTEL_SDK_DISABLED=true` turn it off (an operator
     injecting an endpoint for other telemetry can opt this gateway out).
   - `OTEL_EXPORTER_OTLP_LOGS_HEADERS` / `OTEL_EXPORTER_OTLP_HEADERS` (`k=v,k=v`,
     URL-encoded values) — never logged.
   - `OTEL_EXPORTER_OTLP_LOGS_TIMEOUT` / `OTEL_EXPORTER_OTLP_TIMEOUT` (ms, default
     10000).
   - `OTEL_EXPORTER_OTLP_LOGS_PROTOCOL` / `OTEL_EXPORTER_OTLP_PROTOCOL`: only
     `http/json` accepted. The spec's default is `http/protobuf`, so an unset protocol
     means `http/json` here and the spec says so; `grpc` or `http/protobuf` set
     explicitly fail the start with a clear message.
   - `OTEL_SERVICE_NAME` (default `kaiak`), `OTEL_RESOURCE_ATTRIBUTES`.
   - A malformed value fails the start, as every `KAIAK_*` variable does.
7. **Resource:** `service.name`, `service.version` (the build version),
   `service.instance.id` (the gateway instance ID, once known — see 9), plus
   `OTEL_RESOURCE_ATTRIBUTES`.
8. **Record mapping:** time → `timeUnixNano`; level → `severityNumber` (DEBUG 5,
   INFO 9, WARN 13, ERROR 17) and `severityText`; message → `body` (string);
   attributes typed (string, int, double, bool), `slog` groups flattened with `.`.
9. **Delivery:**
   - A bounded queue (default 10 000 records), batches of up to 512 records or every
     1 s, one export in flight.
   - Retry on network errors, `429`, `502`, `503`, `504` with exponential backoff
     (honouring `Retry-After`), up to the timeout; other statuses drop the batch.
   - A full queue drops the **newest** records (the queue is the backlog of an
     outage; the oldest go first when it recovers) and counts them.
   - Metrics: `kaiak_log_export_records_total{outcome="exported|dropped|failed"}`.
     Export problems are logged to stderr only, rate-limited, never re-exported (no
     feedback loop).
10. **Instance ID:** known at start in every mode (`KAIAK_INSTANCE_ID`, default the
   hostname — `cmd/kaiak`), so it is in the resource from the first record. The
   logger is built before the settings are read today; the wiring step reorders that
   so a start failure still reaches stderr.
11. **Drain:** the exporter flushes within the drain's flush reserve
    (`KAIAK_DRAIN_FLUSH_RESERVE_MS`), after usage — usage is the record, logs are
    not. What does not fit is dropped and counted.
12. **No new dependency:** OTLP/HTTP JSON is a documented encoding; the OTel Go SDK
    would be the gateway's first third-party module. Recorded in `TECH-STACK.md`.

## Constraints

- Zero third-party Go dependencies.
- Never block the request path: the handler only enqueues.
- Nothing sensitive: the exported content is the stderr content; headers from the
  environment are never logged or echoed.

## Risks

- **A slow or down collector:** bounded by the queue, the drop counter and one
  export in flight; tested with a collector that stalls and one that refuses.
- **Convention drift:** the GenAI conventions are still in development and have
  renamed fields before. Mitigation: the spec names the conventions version the
  vocabulary follows; a later rename is one change to the field table and its
  tests.
- **Breadth of the rename:** every test, live-kit check and doc that reads a log
  field. Mitigation: one step owns it (step 2), with a repo-wide grep for the old
  names.
- **JSON encoding mistakes** (OTLP JSON uses lowerCamelCase field names, string
  64-bit integers, hex IDs): checked against a real collector in the live check, not
  only the fake.

## Tag

No anchor tag needed: gateway-only, additive, off unless configured. It goes into the
next release with the cache-write unit and the token-limit change.

## Phases and steps

- **Phase 1 — OTLP log export** (steps 1–4). Green at the end.
  1. `STEP-1-contract.md` — `GATEWAY.md` (the log field table; Configuration
     sources; Observability → OTLP log export; the metric), `DEPLOYMENT.md`,
     `TECH-STACK.md`.
  2. `STEP-2-log-vocabulary.md` — every gateway log line moves to the field table:
     the request line, operational events, tests, the live-test kit, docs.
  3. `STEP-3-exporter.md` — the exporter package: env parsing, record encoding,
     queue, batches, retries, drops, flush; unit tests against a fake collector.
  4. `STEP-4-wiring-and-e2e.md` — wired into `cmd/kaiak` (logger, drain, metrics);
     e2e with a fake collector; live check against an OpenTelemetry Collector.

Expected reds inside the phase: none planned — step 1 is docs, and step 2 moves
code and tests together.

## Verification

- Unit: each variable and its fallback; malformed values; protocol refusal; the
  mapping of every attribute kind and group; batching by size and time; retry on
  retryable statuses and `Retry-After`; drop on a full queue; no blocking with a
  stalled collector; flush on shutdown.
- e2e: a gateway with a fake collector exports its request lines and boot events,
  the same attributes as stderr; with no endpoint, nothing is attempted.
- Live: an `otel/opentelemetry-collector` container (OTLP/HTTP receiver, `debug`
  exporter) shows the records with their attributes and resource.
- `scripts/check-all.sh` green.

**Verification status:** done (2026-10-05); phase 1 green.

- [x] Unit: every variable and its fallback, malformed values, the protocol refusal;
  every attribute kind and group; batching by size and time; retries, `Retry-After`;
  drop-newest; a stalled collector never blocks; flush and close
  (`STEP-3-exporter.md`).
- [x] Wiring: settings read with the others (a malformed one fails the start, on
  stderr); a start that fails once the exporter runs still exports its lines, the
  cause included; the error line written once (`STEP-4-wiring-and-e2e.md`).
- [x] Metric: `kaiak_log_export_records_total{outcome}` at 0 from startup when on,
  absent when off, following the exporter's counts (`STEP-4-wiring-and-e2e.md`).
- [x] e2e: every stderr line of a gateway's life — boot, a success, a `401`, a limit
  refusal, the drain, `kaiak stopped` — reaches a fake collector with the same
  message, level, time and attributes, under the resource; headers sent and never
  logged; no export with no endpoint or with `OTEL_LOGS_EXPORTER=none`; a refused
  batch counted failed and reported on stderr only; a stalled collector bounds the
  exit (`STEP-4-wiring-and-e2e.md`).
- [x] Live: an `otel/opentelemetry-collector:0.162.0` (OTLP/HTTP receiver, `debug`
  exporter) shows the records with their typed attributes and resource
  (`STEP-4-wiring-and-e2e.md`).
- [x] `scripts/check-all.sh` green 3× in a row (`STEP-4-wiring-and-e2e.md`).
