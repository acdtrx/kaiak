# Step 1 — the contract

**Status:** done — awaiting the main session's review

## Intent

Write the whole feature's contract into the specs, and move the shared schemas and
fixtures to protocol 8 — every group states `capture`. The specs describe the
finished feature; the code follows in steps 2–10.

## Files likely touched

- `docs/specs/CONTROL-PROTOCOL.md`:
  - **One version:** the current version becomes 8; the bump rule already names "a
    field made required".
  - **The group tree → Shape:** `capture` (boolean, required) — whether the gateway
    captures the content of requests made with keys naming this group
    (`GATEWAY.md` → Request capture); each group states its own: not inherited, not in
    `child_defaults`, ancestors' values never consulted (OVERVIEW decisions 6–7,
    settled 2026-10-13 with the user, the reason it is required: a privacy choice the
    app makes for every group). The Resolution fixtures paragraph names the new
    `capture` member of `expected`.
  - Everywhere the version 7 is named (the request-check table, the config's top-level
    shape).
- `docs/specs/GATEWAY.md`:
  - **Request pipeline:** the capture stage, after the body is parsed and before the
    model-access check (decision 17), with its finisher; the stage list and the
    "seam" sentence (prompt logging is now a stage).
  - **A new `## Request capture` section** (after Accounting): purpose; who is
    captured (decisions 6, 7, 17); what (decision 16); the span (decisions 13–15,
    20) with an attribute table in the request line's style; the message mapping per
    API (decision 19) with one worked example per API; streams; the session header
    (decisions 9, 21 — ignored when malformed, `kaiak.session_id.ignored`); never
    getting in the way of serving (decision 18); memory and the export limit
    (decisions 22–24); the deviations named — content as JSON strings (14), trace
    export's explicit switch (25); the provider name's custom values (20), quoting
    the pinned text that allows them.
  - **Client API:** `Kaiak-Session-Id` beside Request IDs — optional, never refused
    (decision 21).
  - **Configuration sources → OTLP export:** the traces signal and its variables
    (decision 25); `KAIAK_CAPTURE_MEMORY_BYTES` with the other budgets; the
    config-applied warning (decision 26).
  - **Observability:**
    - the Logs rule on sensitive content: content leaves only as captures (decision
      11); the session ID never logged;
    - the request line's `kaiak.capture` and `kaiak.session_id.ignored` (decision
      27);
    - `gen_ai.provider.name` on the request line and in the usage metrics: every type
      has a value now (decision 20), the 2026-10-06 rule amended and dated;
    - the exporters' counts: the span processor and exporter (decision 28);
    - a **Trace export** subsection beside log and metric export: the resource and
      scope (as logs), the span mapping, delivery (as logs, but bounded by the
      budget), at exit (the order with the other exporters).
  - **Lifecycle:** the drain's exporter order includes traces.
- `AGENTS.md` → Project-Specific Rules: "Nothing sensitive in logs, metrics or usage
  records" — add that content leaves the gateway only as request captures, for groups
  whose `capture` is on, to the trace destination (decision 11).
- `docs/kaiak.md` principle 9 ("Prompts and responses are not logged"): the same
  amendment, one sentence.
- `docs/TECH-STACK.md`:
  - OTLP export: traces, hand-written, settled with its date;
  - the trace SDK not taken (decision 12), with the reasoning, in the
    dependency paragraph that leaves it open today;
  - the telemetry tree gains `otlptrace`.
- `docs/ARCHITECTURE.md`: the capture stage and the trace export in the data flow.
- `docs/BACKLOG.md`:
  - remove "Prompt/response logging to an external system" (Pipeline stages) — this
    plan builds it;
  - update "OpenTelemetry traces": a span exporter exists now; distributed tracing
    stays deferred with its trigger;
  - add: capture on the other endpoints (trigger: a user asks to see their
    embeddings, rerank, image or audio requests); an end-user ID for captures
    (`user.id`; trigger: someone wants per-user views in the tool); truncating large
    captures (trigger: `too_large` drops show up in the counts); gzip on OTLP export
    (trigger: export bandwidth matters).
- `protocol/schema/config.schema.json`: `protocol_version` const 8; `$defs.group`
  gains `capture` (`boolean`) in `properties` and a `required: ["capture"]`.
  `child_defaults` unchanged. `status.schema.json`: const 8.
- `protocol/fixtures/`:
  - **every config document** (`config/valid`, `config/invalid`, `config/resolved`,
    `messages/config-event`, `messages/status`, `duplicate-members`): protocol 8,
    and every group given `capture` — a mix of `true` and `false` across the valid
    ones, so neither half can pass by assuming one value. Converted in one pass by a
    throwaway script, not kept; the two 7 142-group fixtures included;
  - **the resolved fixtures** gain `capture` in each `expected` group, and
    `group-tree.json` has a parent with `true` and a child with `false` and the
    reverse — the flag is the group's own;
  - **new invalid fixtures** with `cases.json` entries: a group without `capture`; a
    `capture` that is not a boolean (`"true"`, `1`); `capture` inside
    `child_defaults`; `protocol_version` 7 (rename the existing `-6`/`-7` fixtures as
    the last bump did);
  - **every schema-kind invalid fixture checked** to fail for its own reason, not for
    a missing `capture` (`RESEARCH.md`: the runner checks only `[schema]`).
- `examples/config.json`, `examples/local-config.json`: protocol 8, `capture` on every
  group — the examples show both values and a comment-free way to read them (the
  examples have no comments: the DEPLOYMENT notes explain).

## Decisions made during planning

- **The specs describe the finished feature now;** the code follows.
- **Every rule is dated** `(settled 2026-10-13)` — decisions 1–30 alike, all settled
  with the user by 2026-10-13.
- **The section is `## Request capture`;** the span is described as what it is, a
  capture exported as a trace, so a reader looking for "traces" finds it under
  Observability → Trace export, which points to it.

## Acceptance criteria

- The specs read consistently; a grep checklist recorded in the result: no passage
  naming protocol 7 as current; no rule left saying prompts are never sent anywhere;
  every new variable, error code, attribute and metric in its table.
- The schema validates every valid fixture and refuses every invalid one as its case
  says; each schema-kind invalid fixture fails for its own reason (checked by
  removing the cause and seeing it validate).
- `examples/*.json` validate against the schema.
- Expected reds recorded: both halves fail the moved fixtures and version checks
  until steps 2 and 3.

## Result

### What changed

- `docs/specs/CONTROL-PROTOCOL.md`: the current version `8` (dated, its reason the
  required `capture`); the bump rule names "a group's `capture`"; the request-check
  table and the repeated-header example at `8`; the config's `protocol_version` the
  integer `8`; the group tree's Shape gains `capture`, and a new **Capture** bullet
  (required boolean, the group's own — not inherited, not in `child_defaults`,
  ancestors never read; why required; consent and access in the tool are the
  app's); labels reach no capture; the Resolution fixtures paragraph names
  `expected`'s `capture` and the group-tree fixture's both directions.
- `docs/specs/GATEWAY.md`:
  - **Client API**: `Kaiak-Session-Id` beside Request IDs (optional, never refused,
    never forwarded, echoed or logged; the rule in Request capture → Sessions).
  - **Request pipeline**: the seam sentence (request capture is one stage); in stage
    2, capture after the inbound parse and before the model-access check, with its
    finisher (stage numbers kept, so no reference moves).
  - **Request capture** (new, after Accounting): purpose and one destination;
    content leaves only as a capture; never in the way of serving; which requests
    (the key's own group, the three endpoints, every outcome from the stage on, what
    is not captured); what is captured; the span; the attribute table (type,
    presence, meaning); the parameters per API; the content format and the mapping
    per API; streams; a worked example per API; sessions; memory; the export limit;
    the request line and the counts; groups capture with trace export off; the
    conventions followed (`5293eff`) and the named deviations (content as JSON
    strings — quoting the convention's text; the explicit switch; provider custom
    values and absence before routing; `gen_ai.response.model` the public name; what
    is not written).
  - **Configuration sources**: `KAIAK_CAPTURE_MEMORY_BYTES`; OTLP export gains the
    traces signal's variables, `OTEL_TRACES_EXPORTER`, **Trace export needs an
    explicit switch** (a named deviation), and the not-read list (`OTEL_BSP_*`,
    span limits, the sampler, propagators).
  - **Observability**: the Logs rule (content only in captures; the session header's
    value never logged); the conventions-followed bullet names `5293eff` for
    captures and the provider rule; the request line's `gen_ai.provider.name` row
    amended (custom values for `vllm`, `vllm-omni`, `llama-server`,
    `openai-compatible`, the pinned quote, `kaiak` rejected) and rows
    `kaiak.capture`, `kaiak.session_id.ignored`; operational rows
    `kaiak.trace_export.endpoint`, `kaiak.config.capture_groups`,
    `kaiak.trace_export.failed`/`dropped`; the usage labels' provider, the naming
    deviations (provider custom values, the span processor's component type and
    `error.type` values), the exporters' own counts (component types, the span
    processor's `error.type`), series at 0, cardinality (one provider per model);
    **OTLP trace export** (new: content, resource and scope, span mapping, delivery
    bounded by the capture budget, counts, problems, at exit); the log and metric
    exports' exit order with the trace flush.
  - **Lifecycle**: the drain's step 5, the second signal and the Kubernetes sizing
    name the trace flush (before the metrics and the logs).
- `AGENTS.md`: the content rule amended (content leaves only as a request capture);
  the One-pipeline rule's example says request capture, not prompt logging.
- `docs/kaiak.md`: principle 9 amended in one sentence; v1 scope gains request
  capture; "prompt logging" leaves the Out-of-v1 list (its backlog entry is gone).
- `docs/TECH-STACK.md`: OTLP export with traces; **the trace SDK not taken**
  (dated, with the measurement and the reasons) in place of "not decided here"; the
  telemetry tree gains `otlptrace`.
- `docs/ARCHITECTURE.md`: the data flow's trace destination and a request-capture
  bullet; the `server` stage list with capture and the client writer; new
  `capture` and `telemetry/otlptrace` entries; `fakeotlp` takes traces; `cmd/kaiak`'s
  exporter wiring and exit order.
- `docs/BACKLOG.md`: "Prompt/response logging to an external system" removed;
  "OpenTelemetry traces" rewritten (span export exists; distributed tracing deferred,
  same trigger); added: capture on the other endpoints, an end-user ID for captures,
  truncating large captures, gzip on OTLP export.
- `protocol/schema/config.schema.json`: `protocol_version` const 8; `$defs.group`
  gains `capture` (boolean) and `required: ["capture"]`; `child_defaults` unchanged.
  `status.schema.json`: const 8.
- `examples/config.json`, `examples/local-config.json`: protocol 8; every group
  states `capture`, chosen to read on their own: the dev workload, the eval project
  and its batch capture, production and people do not (`search-rag` off above
  `search-rag-dev` on); locally `demo-app` captures.
- `STEP-5-trace-wiring.md`: the Metric list's two span rows, moved there (below).

### Fixtures

- **Moved to protocol 8** by one throwaway script that inserts text (each file's
  formatting kept: a new line after `parent` or the opening brace in multi-line
  groups, a member in single-line ones): 217 fixture documents — `config/valid` 16,
  `config/invalid` 155, `config/resolved` 5, `messages/config-event` 7,
  `duplicate-members` 5 (repeats intact), `messages/status` 29 (version only) — the
  two 7 142-group fixtures included; 215 version bumps (`protocol-version-missing.json`
  has none, and the old version fixture is renamed below). 14 935 groups given `capture`, alternating in
  document order from a parity that flips per file (deterministic; the fixtures now hold
  7 494 `true` and 7 493 `false` members, `expected` groups included); `groups-as-array.json`'s array element too.
  Each resolved fixture's `expected` group carries its config group's value, after
  `path`; `group-tree.json` has `research` (false) above `rag` (true) above
  `rag-prod` (false). `config_hash` recomputed for both valid config events (the
  SHA-256 of the config's compact JSON, which held before).
- **Renamed** (as the last bump did): `config/invalid/protocol-version-6.json` and
  `messages/status/invalid/protocol-version-6.json` → `protocol-version-7.json`,
  value 7, cases "must be 8".
- **Added** (`config/invalid`, schema kind, with `cases.json` entries):
  `group-capture-missing.json`, `group-capture-string.json` (`"true"`),
  `group-capture-number.json` (`1`), `child-defaults-capture.json`.
- **Checked** with kaiak-control's ajv (`Ajv2020`, its options) over
  `protocol/schema/`, from a throwaway script (no dependency added):
  - 51 documents that must validate do (valid configs and messages, resolved
    configs, both examples, the duplicate-member files as `JSON.parse` reads them);
    212 schema-kind invalid fixtures are refused; 42 semantic-kind ones pass the
    schema;
  - the 208 schema-kind fixtures that existed at protocol 7 fail with exactly the
    errors they had then (instance path, keyword, schema path; the renamed version
    fixtures against their old selves), none naming `capture`; the 4 new ones fail
    with the capture error alone (`required`, `type` ×2, `additionalProperties`);
  - **cause removed**: each of the 126 config-kind schema fixtures (`config/invalid`
    122, config-event 4) was repaired from its own errors — the offending member
    deleted, a const or enum set, a missing member filled with a valid default, never
    a `capture` — and then validated: 126 of 126;
  - every group of every config document states a boolean `capture` (14 967 seen),
    but the three new fixtures built to break it.

### Grep checklist

- Protocol 7 as current — `docs/specs/`, `protocol/schema/`, `examples/`,
  `AGENTS.md`, `docs/kaiak.md`, `TECH-STACK.md`, `ARCHITECTURE.md`, `BACKLOG.md`: 0.
  Fixtures at 7: only the two `protocol-version-7.json`. Still at 7, for their steps:
  `control/kaiak-control/GUIDE.md` (step 2), `scripts/live/config.go` (step 3),
  `README.md`, `docs/architecture/*.html` (step 10); `docs/DEPLOYMENT.md`'s 0.15.x
  upgrade notes are history.
- A rule saying prompts never leave the gateway: none — principle 9, `AGENTS.md`,
  GATEWAY.md's Logs rule and Request capture all say content leaves only as a
  capture; "logs never hold prompt or response content" (Providers) stays true;
  principle 5's "prompt logging" is the seams list, left as written.
- In their tables: every variable (`KAIAK_CAPTURE_MEMORY_BYTES`, the four
  `OTEL_EXPORTER_OTLP_TRACES_*`, `OTEL_TRACES_EXPORTER`) in Configuration sources;
  `kaiak.capture` (`queued`, `memory`, `too_large`, `export_off`) and
  `kaiak.session_id.ignored` (`empty`, `too_long`, `invalid_characters`) on the
  request line; every span attribute in Request capture's table; the operational
  attributes in theirs. No error code is new: capture refuses nothing. The two span
  metrics are named in Exporters' own counts, the deviations and series at 0; their
  Metric list rows land with step 5 (below).

### Suite

`scripts/check-all.sh` stops at the gateway's `go test`, as expected. Red packages
and their clearing step:
- `cmd/kaiak` (15 tests), `internal/config` (25), `internal/control` (28): every
  failure is the gateway refusing the moved fixtures, examples and status —
  `/protocol_version: must be 7`, `/groups/<id>/capture: unknown field` — or what
  follows from it (a config never applied, a boot that never waits for totals, a
  helper goroutine left by an earlier failed test in `TestRunExitsWithNoConfigAtBoot`
  and `TestStopSignalDuringTheBootWaitExits/control_plane_down`), and
  `TestProtocolVersionIsTheSchemasConst` (const 8, want 7) — step 3.
- `internal/metrics` `TestFullScrapeIsTheSpecsMetricList` was red on the first run:
  it reads the Metric list, which named the two span families before any code
  registers them. As media phase B did for its tables, the rows moved to step 5,
  which registers the families (`STEP-5-trace-wiring.md` holds them); re-run after
  the move, `internal/metrics`, `internal/server` and `internal/provider` are green.

Every other gateway package green, `e2e` included (its configs are inline, at 7 until
step 3). Run apart: the live-test kit's lint and self-test green; `npm run lint`
green; the cross-half e2e green (neither half's code moved — red from step 2 until
step 3, as planned). `npm test`: 195 failed, 8 cancelled, 464 passed — every failure
is kaiak-control refusing the protocol-8 fixtures (`must be equal to constant`,
`must NOT have additional properties: capture`, "publish succeeded" false), the
schema copy's drift check (`npm run sync-schemas`), and the sample's tests that wait
for a published `minimal.json` and time out — step 2. Under `npm test`,
`control-plane.test.ts` hangs (no test timeout): two tests wait for a status
(`ready.json`, now at 8) that the protocol-7 checker refuses before it reaches the
store; it was stopped after 10 minutes, and run alone with `--test-timeout` it
reports 24 failures and those 2 timeouts, all from the move — step 2.

### For the main session

- **Phase 1 ends green only with the span rows out of the Metric list** — done here,
  as the last bump did; step 5 lands them.
- **Decided while writing, worth a look:** the session header is read from its first
  value, as `x-request-id` is, and its reasons judged in the order `empty`,
  `too_long`, `invalid_characters`; `otel.sdk.processor.span.processed` counts
  `shutdown` too (the log queue's twin), and the exit order puts traces before metrics
  (STEP-5's order); a `trace export failing` line with
  `kaiak.trace_export.failed`/`dropped` and `kaiak.trace_export.endpoint` on `kaiak
  starting`; the export-off warning is its own line (`kaiak.config.capture_groups`),
  as the cap-over-budget warnings are; `server.address`/`server.port` from the
  backend's `base_url`; Responses reasoning items map their `summary` and their
  `content`'s `reasoning_text` (vLLM's); unknown Responses input items become a
  generic part in an `assistant` message, or a `tool` one for the client-run tools'
  `…_output` items; `gen_ai.response.model` is the public name the client received
  (a named deviation); `gen_ai.request.reasoning.level` and `gen_ai.output.type` are
  not written.
- **Requests the inbound stage refuses after parsing** (hosted tools, stateful
  fields, stored objects, repeated members) are not captured: the stage runs after
  inbound, as decision 17 and step 6 place it.
- **No startup warning for a body cap above the capture budget** (STEP-5 left it
  open): such a capture is dropped `memory`, which the line and the counts show.
- ARCHITECTURE.md describes the `capture` and `otlptrace` packages now, as the specs
  describe the finished feature — no later step lists ARCHITECTURE.md.
