# Pre-release review — 2026-10-05 (main at `699637f`)

Scope: what changed since `v0.9.0` — the cache-write unit (`docs/plans/cache-write/`),
cache reads leaving token limits (`1454a17`), the OpenTelemetry log vocabulary and the
OTLP log exporter (`docs/plans/otlp-logs/`). Four read-only reviewers on a detached
worktree at `699637f`, one per area:

- **[W]** the cache-write unit;
- **[L]** token limits;
- **[V]** the log vocabulary;
- **[X]** the OTLP exporter.

An independent review of a plain copy (Codex, no git history) joins as **[B]**; its
report is `AUDIT-independent.md`. Its nine reproduction tests were re-run by the main
session against the copy: all fail on `699637f` as claimed.

Every finding is tagged by frequency under legitimate use: `daily` / `occasional` /
`rare` / `adversarial`.

## Verdict

**The accounting is right.** Metering, clamping, pricing, tier choice, the token-limit
sum on both halves, the version bumps and old-format handling agree with the specs and
each other ([W], [L], [B]). The log vocabulary matches the spec's tables exactly — 127
keys, both ways, types included ([V], mechanical scan) — and the standard names carry
their standard meaning. The request path never waits on the collector ([X], [B]).

**The exporter's edges are not.** It follows redirects (credentials and logs to another
host, and silent loss behind a login redirect), echoes collector text into stderr, and
counts unreadable answers as delivered. And one existing behaviour became a daily
problem with the token-limit change: `Retry-After` on token refusals is far too long.

## Findings

### High

- **H1 — OTLP credentials can be selected as a backend key** [B] `adversarial`
  - `config/schema.go:48,53`, `provider/provider.go:230`, both config schemas, kaiak-control
    `config/index.ts:25`. The `api_key_env` guard reserves `KAIAK_*` only; a config
    author can name `OTEL_EXPORTER_OTLP_HEADERS` (or `…_LOGS_HEADERS`) and the gateway
    sends the collector's credentials as a bearer token to a backend URL of the
    author's choosing. A models probe is enough. Confirmed (`TestAuditBOTELCredentialsMustNotBeBackendKeys`,
    node `audit-b.test.ts`).
  - Fix: reserve the `OTEL_` prefix beside `KAIAK_` in both schemas, both validators
    and the provider's last guard; fixtures; spec (`api_key_env`, settled 2026-09-25,
    N-S2).

### Medium

- **M1 — The exporter follows redirects** [X][B][V] `rare` (security) / `occasional`
  (loss)
  - `otlplog/exporter.go:136`: no `CheckRedirect`, unlike the provider and control
    clients. A 307/308 resends the batch and every custom credential header
    (`api-key`, `x-honeycomb-team`, `DD-API-KEY`) to another host; a same-host
    https→http redirect sends `Authorization` in clear. A 301/302/303 to a login page
    answering 200 counts the whole batch as exported while nothing arrives (an auth
    proxy in front of the collector). Confirmed by three repros.
  - Fix: `http.ErrUseLastResponse`; a 3xx fails the batch, unretried, reported.
- **M2 — Collector response text reaches stderr** [B] `occasional`
  - `exporter.go:393,419,483`: `Status.message` and partial-success `errorMessage`
    are copied into `exception.message`. An auth proxy echoing the bearer token puts
    it in the logs. Confirmed.
  - Fix: report status, rejected count and a local class; never the remote text.
- **M3 — Unreadable 2xx answers count as delivered** [B][X] `occasional`
  - `exporter.go:359,365,388`: read errors ignored, a malformed body read as "rejected
    nothing". A truncated partial-success answer, or a non-JSON 200, becomes
    `exported`. Confirmed.
  - Fix: an empty body is success; a read error or a body that is not an
    `ExportLogsServiceResponse` is `failed` (not retried — some records may have been
    accepted). Spec's "a 2xx is delivered" narrowed.
- **M4 — Token refusals give a far too long `Retry-After`** [L] `daily` (agent
  workloads near a minute limit)
  - `limits/window.go:317-349` (`waitFor`, `resetIn`): an in-flight reservation is
    treated as staying until its slot expires (≈60 s) or the hour ends. With cache
    reads out of the count, requests now settle at a few percent of their reservation
    and free the room in seconds. Test: refused with `Retry-After 59s`, admitted 3 s
    later; on an hour limit "Retry after 55m0s", admitted 5 s later. The OpenAI SDK
    honours ≤ 60 s, so agents sleep a minute for nothing.
  - Fix: track in-flight reservations for minute windows too; when the window would
    fit without them, answer a short retry (1–5 s) instead of the slot expiry; same
    for `x-ratelimit-reset-tokens`. Spec line under Refusal.
- **M5 — Remote bytes in provider and control errors reach the logs** [B]
  `occasional` (a broken backend or proxy) — predates this release, but OTLP now
  ships these lines off the host
  - Go's HTTP parser puts offending header bytes in its error (`malformed MIME
    header: missing colon: "Bearer …"`), logged by the models probe and request path
    (`provider/wire.go:107,201`, `routing/modelcheck.go:108`, `server/api.go:217`);
    the control client logs the raw `Kaiak-Protocol` value and unvalidated error codes
    (`control/transport.go:106,121`, `client.go:560`). Confirmed.
  - Fix: classify transport and protocol failures into local messages before logging;
    a version mismatch reports absent / invalid / the parsed number; error codes
    validated against the protocol's vocabulary.

### Low

- **L1 — `Retry-After` edge values** [X][B] `occasional`
  - `exporter.go:302-313,430-444`: `Retry-After: 0` (or a past date) retries back to
    back for the batch timeout (7 418 POSTs in 1 s); a valid value above one day is
    treated as absent and retried after ~0.4 s.
  - Fix: wait `max(Retry-After, backoff)`; saturate large values so the deadline check
    fails the batch.
- **L2 — A second stop signal is ignored during the final log flush** [X][B]
  `occasional` (rollout during a collector outage), `daily` for a developer with
  `OTEL_LOGS_EXPORTER=otlp` and no local collector
  - `cmd/kaiak/main.go:541-568,692`: the signal watcher ends with the drain; the flush
    runs to the drain deadline. With a stalled collector a pod lingers ~60 s after
    `kaiak stopped`. Confirmed.
  - Fix: keep the watcher through the flush; a signal cuts it to the 1 s floor. Also
    close the gap noted at step 4: `Close`'s final report bypasses the once-a-minute
    limit, so drops at exit are never silent.
- **L3 — The exporting handler panics where slog's handlers recover** [X] `rare`
  - `otlplog/handler.go:152-157`: a panicking `Error()`/`MarshalJSON` (a typed-nil
    error) crashes a background goroutine with export on; stderr alone prints
    `<nil>`. Fix: recover in `convert` as slog does.
- **L4 — `kaiak.limit.used` in nano-USD on one line, dollars on another** [B][V]
  `occasional` (reloads changing a USD limit's model set)
  - `limits/limits.go:334-337`. Fix: convert by measure, as `limitAttrs` does.
- **L5 — `kaiak.limit.id` vs `kaiak.limit.group`** [V] — one concept, two keys and two
  spellings of "global" (request line `id` = `"global"`; operational lines `group` =
  empty). The last free chance to unify before release.
- **L6 — `http.response.status_code` 499 for a response never sent** [V] `daily`
  (clients that leave first) — the HTTP convention sets it only when a response was
  sent. Decide: omit it (`error.type=client_closed` says what happened), or keep 499
  as a recorded departure.
- **L7 — A wrongly typed usage detail voids the whole report** [W] `rare`
  - `accounting/meter.go:238,258`: `cache_write_tokens` as `2033.0`, a string or an
    object makes the record estimated. Same for the other detail fields. Fix: decode
    details leniently (a bad detail counts 0).
- **L8 — Inconsistent cache counts are clamped silently** [W] `rare` (a backend whose
  `prompt_tokens` excludes cache tokens) — `meter.go:267-270`. Fix: log once per
  deployment and/or count, like `kaiak_usage_clamped_records_total`.
- **L9 — Exporter memory is bounded by records, not bytes** [B] suspicion — mostly
  closed by M5 (the long remote strings); otherwise records are clipped. Record only.

### Docs

- **D1** GUIDE's LiteLLM rule copies the tier-0 `tokens_cache_write` into upper tiers,
  pricing writes below plain input there [W]. Copy only `tokens_in`/`tokens_out`;
  leave cache units out (fallback: the tier's `tokens_in`).
- **D2** Release note: stored hour totals (`limits.json`, `totals.json`, the control
  plane's current hour) still include cache reads until the hour rolls over [W][L][B].
- **D3** `DEPLOYMENT.md`: leaving cache reads out does not lower the sizing floor —
  admission still needs the full input estimate [L].
- **D4** Leftovers [V]: `DEPLOYMENT.md:750` (`limit_id`); `BACKLOG.md` OpenTelemetry
  entry still says logs are "planned"; `LIVE-BACKENDS.md:370-372` (`tokens_reasoning`
  on the log line?); the Units bullet omits config loads' microseconds; the cache-write
  plan's decision 8 predates `1454a17` (note it as superseded).
- **D5** `DEPLOYMENT.md` notes [X][V]: one export in flight caps throughput at ~512
  records per round trip — use a nearby collector or sidecar; when stderr is ingested
  with dynamic mapping, map duration and cost fields as `double` (slog writes `0`,
  `30`).

## Recommended before the release

H1, M1–M5, L1–L6, D1–D5: everything that is daily, occasional or a credential path,
plus the two vocabulary choices (L5, L6) that cost a rename after release. L7–L9 stay
recorded here.
