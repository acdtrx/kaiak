# Step 1 — the contract

**Status:** not started

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
- **Every rule is dated** — decisions 1–11 `(settled 2026-10-12, with the user)`,
  12–30 `(settled 2026-10-13, with the user)`.
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
