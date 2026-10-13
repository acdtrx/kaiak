# Step 10 — the live-test kit and the docs

**Status:** not started

## Intent

Let the live-test kit check captures against real servers, and finish the
operator- and app-facing docs.

## Files likely touched

- `scripts/live/`:
  - a small OTLP/HTTP receiver of its own (the kit is a separate module and cannot
    import the gateway's `fakeotlp`), started with the gateway, which gets
    `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` pointing at it;
  - one test group with `capture: true`;
  - checks per chat endpoint the kind serves, streamed and not: a span arrives; its
    input holds the prompt sent; its output equals the answer the kit received
    (text, tool calls, reasoning when the model gives it); the usage matches the
    usage record;
  - its own self-test (`check-gateway.sh` runs it) extended.
- `docs/DEPLOYMENT.md`:
  - **Request capture:** what it is, who decides (the app, per group), the trace
    variables and the explicit switch, `KAIAK_CAPTURE_MEMORY_BYTES` and `GOMEMLIMIT`,
    what is dropped and how to see it (`kaiak.capture`, the counts);
  - **Langfuse:** the endpoint, the Basic auth header (percent-encoded in
    `OTEL_EXPORTER_OTLP_TRACES_HEADERS`), `x-langfuse-ingestion-version: 4`; what it
    shows (session from `Kaiak-Session-Id`, metadata from the `kaiak.*` attributes;
    time to first token not shown);
  - **sending the session ID:** a header per request; the pi extension example
    (`before_provider_headers`); SDKs' per-request headers;
  - **upgrade notes, the next release** (protocol 8): every group needs `capture`;
    gateways and control plane upgrade together; apps decide each group's value;
    `gen_ai.provider.name` now on every type — the self-hosted types' usage metrics
    gain a `gen_ai_provider_name` label (`vllm`, `llama-server`, …) and the request
    line the attribute, so dashboards and alerts that matched its absence change.
- `docs/testing/LIVE-BACKENDS.md` — the capture checks.
- `README.md` — the protocol version and a line on capture if the README lists
  features.
- `docs/architecture/*.html` — the version, and the capture stage where the pipeline
  is drawn.

## Acceptance criteria

- The kit's self-test passes; its capture checks pass against the fake backend.
- The docs name every variable, the explicit switch, the drops and the header.
- `scripts/check-all.sh` green.
