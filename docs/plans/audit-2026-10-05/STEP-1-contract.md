# Step 1 — contract

**Status:** done (2026-10-05)

## Intent

Write the fixes' rules into the contracts and docs first: what the gateway reserves,
what a log line may never carry, how the exporter treats redirects, unreadable answers
and `Retry-After`, the refusal's retry rule, the L5/L6 vocabulary changes, and the
doc items D1–D5.

## Files likely touched

- `docs/specs/CONTROL-PROTOCOL.md` and `GATEWAY.md`: `api_key_env` reserves `OTEL_`
  (H1, extending the 2026-09-25 N-S2 decision); the remote-text rule for log lines
  (M2/M5); exporter Delivery — redirects refused (M1), what counts as delivered (M3),
  the retry wait (L1), a second signal during the final flush and the unthrottled exit
  report (L2), panicking values (L3); Limits → Refusal — the short `Retry-After` when
  only in-flight reservations block (M4); the field tables — `kaiak.limit.group`
  (L5), `kaiak.limit.used` in dollars on every line (L4), no status code for a
  response never sent (L6).
- `protocol/schema/config.schema.json` (+ `npm run sync-schemas`), fixtures and
  `cases.json` for `OTEL_` references.
- Docs: D1 (`control/kaiak-control/GUIDE.md` LiteLLM rule), D3 and D5
  (`docs/DEPLOYMENT.md`), D4 leftovers (`DEPLOYMENT.md:750`, `BACKLOG.md` OpenTelemetry
  entry, `docs/testing/LIVE-BACKENDS.md:370-372`, the Units bullet, a superseded note
  on `docs/plans/cache-write/OVERVIEW.md` decision 8). D2 is a release note — record it
  in this step's Result for the release.

## Acceptance criteria

- Each rule stated in its owning section, dated 2026-10-05, with the reason.
- Schema copies in sync; the new fixtures fail on both halves (expected reds, cleared
  by step 3); everything else green.

## Result

**What changed**

- H1 — `api_key_env` reserves `OTEL_` beside `KAIAK_`, enforced the way `KAIAK_` is:
  a schema pattern (`not: ^KAIAK_` → `not: ^(KAIAK|OTEL)_` in
  `protocol/schema/config.schema.json`, description updated; kaiak-control's copy
  synced with `npm run sync-schemas`). The gateway mirrors the schema by hand
  (`config/schema.go` `reservedEnvPrefix`, `IsReservedEnvName`, also the provider's
  last guard) — step 3.
  - New invalid fixtures (`kind: schema`, each with its `cases.json` entry):
    `config/invalid/api-key-env-otel-headers.json`
    (`OTEL_EXPORTER_OTLP_HEADERS`), `api-key-env-otel-logs-headers.json`
    (`OTEL_EXPORTER_OTLP_LOGS_HEADERS`), `api-key-env-otel-logs-endpoint.json`
    (`OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` — pins the whole prefix, not the header
    names). Copies of `api-key-env-kaiak-prefix.json` with the name changed.
  - Spec: `CONTROL-PROTOCOL.md` Config → `api_key_env` (beside N-S2), `GATEWAY.md`
    Providers → credentials (schema both halves + the provider's guard) and
    Configuration sources → the OTLP headers bullet; `GUIDE.md` rule 4;
    `DEPLOYMENT.md` Secrets.
- `GATEWAY.md`, each dated 2026-10-05 with its reason:
  - Observability → Logs: **No remote text** (M2/M5) — the rule, its three
    cases (transport/protocol failures as local classes; `Kaiak-Protocol` mismatch
    as absent / invalid / the parsed number; control-plane error codes only in the
    protocol's code shape, backend `code`/`type` only in their existing identifier
    shape), the collector's `Status.message` / `errorMessage` never logged; rejected:
    redaction, clipping. The Logs bullet's "never" list, the `exception.message`,
    `kaiak.upstream.error.message`, `usage batch refused…` and `log export failing`
    rows point to it.
  - OTLP log export → Delivery: **What counts as delivered** (M3), **No redirects**
    (M1), **Retries** rewritten — `max(Retry-After, backoff)`, saturating large
    values (L1); the `failed` outcome lists a redirect and an unreadable answer;
    **the report at exit is never held back** (L2).
  - OTLP log export → Record mapping: panicking values rendered as `slog` renders
    them — `<nil>` for a nil pointer, else `!PANIC: <value>` (L3; checked against
    `log/slog/handler.go`).
  - OTLP log export → At exit, and Lifecycle → Draining: a second signal during the
    final flush cuts it to its 1 s (L2).
  - Limits → Refusal (M4): a token limit blocked only by in-flight reservations
    counts a fixed 2 s toward `Retry-After`, and `x-ratelimit-reset-tokens` on that
    refusal is `2s`, minute and hour windows alike; minute windows keep in-flight
    reservations apart from settled usage. The Rate-limit headers bullet refers to
    it. Refusal's log-field reference renamed to `kaiak.limit.group`.
  - Field tables: `kaiak.limit.group` replaces `kaiak.limit.id` on the request
    line, absent for a global limit (L5); the operational identity row says
    "absent", not "empty"; `kaiak.limit.used` in dollars for USD limits on both
    rows (L4); `http.response.status_code` present only when a response was sent
    (L6).
  - L6's other readers: the request metric keeps its rule — a client that left
    stays in `status_class="4xx"` (recorded with 499), so `_count` stays the client
    request count; `kaiak_errors_total{class="client_closed"}` and
    `kaiak_request_errors_total{code="client_closed"}` unchanged (stated under the
    metric list's `status_class`). The Client API table row keeps 499 as the
    metric's status and says the log line carries none. `kaiak.tried` uses the
    gateway's error code for the gateway's own outcomes, not 499 — unaffected.
  - Units bullet: config loads' `kaiak.duration` is to the microsecond too (D4;
    `config/loader.go` uses `SecondsMicro`).
- `CONTROL-PROTOCOL.md` Shape → Errors: the code shape `^[a-z]+(-[a-z]+)*$`, at
  most 64 characters; the gateway logs a code only in that shape and never logs
  `detail`. Every code kaiak-control and the sample emit today fits it.
- Docs:
  - D1 `control/kaiak-control/GUIDE.md`: copy only `tokens_in`/`tokens_out` from
    the tier at 0; cache units without a long-context rate stay out and fall back
    to the tier's `tokens_in`.
  - D3 `DEPLOYMENT.md`: the sizing floor includes cache reads — admission reserves
    the full input estimate.
  - D5 `DEPLOYMENT.md`: one export in flight, ≤ 512 records a round trip — keep the
    collector near (with a worked figure); no redirects; map durations, cost and
    USD limit values as `double` under dynamic mapping.
  - D4: `DEPLOYMENT.md` `kaiak.limit.id` / `limit_id` → `kaiak.limit.group` (lines
    494, 649, 750) and the status code's absence for `client_closed`; `BACKLOG.md`
    OpenTelemetry entry: logs "built", not "planned"; `LIVE-BACKENDS.md` reasoning
    check names the log field (`gen_ai.usage.reasoning.output_tokens`) beside the
    record's unit; the Units bullet (above); `docs/plans/cache-write/OVERVIEW.md`
    decision 8 gains a dated "superseded by `1454a17`" note, the decision itself
    untouched.

**Decisions made during the step**

- `OTEL_` is case-sensitive, matching `KAIAK_` (the schema pattern is
  case-sensitive and accepts `kaiak_…` today): environment names are case-sensitive
  and the gateway reads only the upper-case variables, so `otel_…` is another
  variable. No lowercase fixture — none exists for `kaiak_` either.
- A third fixture (`OTEL_EXPORTER_OTLP_LOGS_ENDPOINT`) beside the two header names,
  so the fixtures pin the prefix rather than a list.
- Control-plane error codes: a **shape** in the protocol (not a vocabulary list),
  per decision 6 — the review's "validated against the vocabulary" would need a
  list kept in step on both halves; the shape refuses free text equally.
- M4's `x-ratelimit-reset-tokens` override applies on the refusal only; admitted
  responses keep "until the window holds nothing".
- L5/L6 reasons written as "Rejected: …" rather than narrating the old lines.
- No `DEPLOYMENT.md` text for M4 (not a doc item); the spec states it.

**For the release notes** (D2)

> Token limits no longer count input read from the cache. Hour totals stored before
> the upgrade still include it until their hour ends: the file-mode
> `limits.json`, the control-plane totals cache `totals.json`, and the control
> plane's current hour. An hourly token limit may refuse a little early during the
> first hour after the upgrade; nothing to do — it corrects itself when the hour
> rolls over. (Deleting `limits.json` is not a fix: it also holds the month's USD
> spend.) Per-minute windows are not stored and start correct.

**Suite (expected reds)** — `scripts/check-all.sh` stopped at `go test -race`;
the remaining stages were run directly.

- Gateway: gofmt, vet, staticcheck pass; `go test -race ./...` FAIL in
  `internal/config` only — `TestInvalidFixtures/api-key-env-otel-headers.json`,
  `…/api-key-env-otel-logs-endpoint.json`, `…/api-key-env-otel-logs-headers.json`
  (`want a *ValidationError …, got <nil>`): the hand-written schema check still
  reserves only `KAIAK_`. Cleared by step 3. Every other package and `e2e` pass.
  Live-test kit: gofmt, vet, staticcheck, self-test pass.
- `control`: `npm test` 577 / 577 pass — the new fixtures included, since
  kaiak-control validates with the synced JSON Schema itself; schema-copy check
  passes. `npm run lint`: `tsc` clean, `boundaries ok`.
- Cross-half e2e (`TestAcrossHalves`): pass.
- **Differs from the plan**: OVERVIEW expected kaiak-control to fail the new
  fixtures too; it cannot — its validation is the schema. Step 3's kaiak-control
  part needs no validation change (the ported `audit-b.test.ts` should already
  pass; it still belongs in the suite as a regression test).
