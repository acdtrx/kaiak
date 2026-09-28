# Step 8 — metrics and logs

**Status:** done (2026-09-25) — commits `d9e3770` (L3), `2a353dd` (H9), `cb18bd2`
(M4), `4eb42dd` (L2), `b69c230` (L5), `6c4abc2` (L17), `d8c163c` (live kit)

## Items

- **H9** drop `backend` from usage metrics; `user` label switchable like `key_id`;
  series estimate in the spec.
- **M4** `kaiak_upstream_attempts_total{backend,deployment_model,outcome}`,
  `kaiak_upstream_attempt_duration_seconds{backend}` (all attempts, stream and not),
  `backend` on retries.
- **L3** truncate client strings in logs and errors (256 bytes); **L2** optional bearer
  token for `/metrics`; **L5** document relayed backend error bodies; **L17** fix the
  TECH-STACK spool text.

## Acceptance criteria

- Tests; metrics table updated; live kit metrics check updated.

## Result

Each new test was run and seen failing before its fix: a behavioral failure, or a
build failure where the test needs the new API (`NewAdmin`'s token,
`settings.metricsToken`, `Snapshot.UserLabel`, `Ops.ObserveUpstreamAttempt`).

**L3 — client strings clipped** (`internal/clip` — new package; `server/api.go`,
`server/errors.go`, `auth/auth.go`)

- `clip.String`: at most 256 bytes, cut back to a UTF-8 character start, then `…`.
  Used for the log line's `method`, `path`, `model`, and the error messages that echo
  them (`unknown_url`, `method_not_allowed`, `model_not_found`). Authorization still
  compares the full model name.
- Tests: `server.TestClientStringsAreClippedInLogsAndErrors` (1 MiB path, 4 KiB
  method, 1 MiB model in the path, 900-byte model in the body; bounded body and log
  line, the marker present, the key in neither) — before the fix: 1 MiB answers and
  2 MiB log lines. `clip.TestString`.

**H9 — usage-metric cardinality** (`metrics/usage.go`; `config/snapshot.go`,
`document.go`, `schema.go`; `protocol/schema/config.schema.json`; kit
`config/types.ts`; fixtures `config/valid/full.json`,
`messages/config-snapshot/valid/full.json`, invalid `cases.json` reason)

- Usage labels: `team, workload, user, key_id, model, status` (no `backend`).
- `global.metrics.user_label` (boolean, default true), same semantics as
  `key_id_label`, read per record.
- GATEWAY.md: series estimate `label sets × models × statuses × 6` with a worked
  example (20 hosts, 500 keys: 18,000 → 1,836 series with both switches off).
- Tests: `metrics.TestUsageSinkCountsRecords` (no backend, user switch),
  `server.TestUserLabelSwitchedOff`, `config.TestDefaultsAppliedForOmittedFields` /
  `TestExplicitValuesOverrideDefaults` (default true; full fixture sets false).
  Existing expectations lost their `backend=` label; `e2e.TestFirstEventTimeoutRetried`
  now tells the two attempts' records apart by `status` and checks each attempt on
  its backend through `kaiak_upstream_attempts_total` (it was red after the H9
  commit and fixed in the M4 commit — the H9 commit ran `./internal/...` only).

**M4 — per-backend ops metrics** (`metrics/ops.go`; `server/upstream.go`,
`server/metrics.go`)

- `kaiak_upstream_attempts_total{backend,deployment_model,outcome}` — outcomes, a
  fixed set of 12: `success`; failures `unavailable`, `timeout`, `auth_failed`,
  `model_missing`, `server_error`, `broke_off`; neutral `response_timeout`,
  `rate_limited`, `client_error`, `canceled`, `internal`. An unknown outcome panics
  (programming error), like error classes.
- `kaiak_upstream_attempt_duration_seconds{backend}` (the request-duration buckets):
  from the attempt's send to its release — the end of the relay for the answering
  attempt (stream or not), the failure/first event for a retried one.
- `kaiak_retries_total{model,backend,reason}`: the backend of the attempt retried.
- `circuitOutcome` became `classifyAttempt`, returning the named outcome, the circuit
  class and the reason: one classification feeds both, at the attempt's release.
- Tests: `server.TestUpstreamAttemptMetrics` (paced stream timed past 100 ms,
  500→retry→success, relayed 400, 429 on both pair deployments, connect refused ×3,
  stream cut after its first event), `metrics.TestOpsMetrics`; retry tests and the
  admin family list updated.

**L2 — `/metrics` token** (`server/admin.go`; `cmd/kaiak/main.go`)

- `KAIAK_METRICS_TOKEN` (non-empty = on): `/metrics` needs `Authorization: Bearer
  <token>`, SHA-256 digests compared with `subtle.ConstantTimeCompare`; else `401`
  with `WWW-Authenticate: Bearer`, plain text. `/healthz`, `/readyz` stay open.
  GATEWAY.md: env var, and a NetworkPolicy recommendation (admin port to the
  scraper only).
- Tests: `server.TestMetricsTokenGuardsMetricsOnly` (missing, wrong, longer,
  `Basic`, bare token → 401; right → 200; probes open; no token → open),
  `cmd/kaiak.TestMetricsTokenSetting`.

**L5 — backend error bodies** (`server/upstream.go`, `errors.go`, `metrics.go`,
`pipeline.go`, `api.go`)

- Backend `4xx`: relayed untouched (documented: may name backend internals).
- Backend `5xx`: answered with the gateway's `upstream_error` ("The model backend
  failed to process the request.") under the backend's status, keeping
  `Retry-After`/`Retry-After-Ms`; the first ≤ 256 bytes of the backend's text go to
  the log line as `upstream_body` (clipped; the rest is not read); `error_code`
  `upstream_error`; class `upstream_error` as before; circuit failure as before.
  New Client API table row.
- Tests: `server.TestBackendErrorBodies` (503 naming a host and device, 100 KB body:
  replaced, Retry-After kept, log clipped; a 400 relayed byte for byte) — failed
  before; `TestAllAttemptsFailingAnswerTheLastError` now expects the gateway's body
  with the third attempt's status and Retry-After, and the third's text in the log.

**L17** — TECH-STACK persistence text lists the data directory's files (versioned
JSON per sealed batch plus the index, `limits.json`, `totals.json`,
`last-known-good.json`, `kaiak.lock`).

**Live kit** (`scripts/live/checks.go`) — the metrics check also wants ≥ 1
successful upstream attempt, attempt durations ≥ successes, and no `backend=` on any
usage series. No existing check used the removed label. Self-test: all four kinds
passed (`metrics … 6 successful upstream attempts`; two backends: 21).

Docs: GATEWAY.md (metrics table rows, usage labels, cardinality, key-ID and user
switches, upstream attempts, admin port and token, env var, log fields `upstream_body`
and clipping, Client API row, upstream failures / outcome table / retries /
accounting wording for 5xx); ARCHITECTURE.md (`clip` package; attempt classification
feeds metrics); TECH-STACK.md (L17). CONTROL-PROTOCOL.md does not describe
`global.metrics`, so no edit there.

Suite: `GOFLAGS=-count=1 scripts/check-all.sh` → `all checks passed` (gofmt, vet,
staticcheck, gateway race tests incl. e2e and the new `clip` package, live kit
self-test 13/13 ×3 + 16/16, control 436/436, lint, cross-half e2e). No expected
reds. Between commits: the H9 commit alone left `e2e.TestFirstEventTimeoutRetried`
red (it read usage records by `backend`); the M4 commit cleared it. No flaky
failure seen in this step's runs.

## Decisions for the user to confirm

1. **L5 — 5xx bodies replaced** (the brief's recommendation, implemented): status
   kept (not mapped to 502), `Retry-After` kept, code `upstream_error`. The backend's
   text is logged as `upstream_body` (≤ 256 bytes). It is an error message, not
   generated content, but a backend could echo input in it; the alternative is to
   log nothing of it.
2. **M4 outcomes**: 12 named outcomes mirroring the circuit classes (failures split
   by cause, neutral split by cause). Series are created on first use, **not
   pre-initialized** per deployment (fewer idle series; `rate()` on a never-seen
   outcome is absent rather than 0). Label values are config names and the fixed
   outcomes only.
3. **M4 duration end point**: the attempt's slot release — for the answering
   attempt right after its relay ended (a few finishers later at most), for a
   retried attempt when the retry was decided (its first event/status).
4. **L2**: an empty `KAIAK_METRICS_TOKEN` means "no token" (open), not an error.
   401 body is plain text `unauthorized`.
5. **L3**: clipped strings end with `…` (3 bytes), so a clipped value is up to 259
   bytes; the method is clipped too (Go accepts any token as a method).
6. **H9**: `user_label` default true (as `key_id_label`); switching `key_id_label`
   off does not also drop `user`.

## Deviations

- New package `internal/clip` (one helper shared by `server` and `auth`, which
  cannot import each other that way round).
- The live kit's attempt check is `≥ 1` success rather than "≥ chat records": a
  chat record can come from an attempt that did not succeed (a relayed 4xx, an
  unanswered attempt), so an exact relation would make real-backend runs flaky.
