# Step 4 — operator visibility (D6)

**Status:** done (2026-09-25) — D6 and the stop signal during the boot wait. **Phase 2
complete** (steps 3–4): `GOFLAGS=-count=1 scripts/check-all.sh` green.

- Limit refusals (`rate_limit_exceeded`, `budget_exceeded`, `budget_unavailable`) log
  `limit_scope`, `limit_id`, `limit_type`, `limit` (share enforced), `limit_configured`,
  `used`; every line logs the owner (`team`/`workload` or `user`).
- Relayed backend errors (status ≥ 400) log `error_code` (the class, e.g.
  `upstream_client_error`, `upstream_rate_limited`) and `upstream_error_code/type`.
- `kaiak_time_to_first_token_seconds{model,backend}` measured from the answering
  attempt's send; log `ttft_ms` and `stream`.
- Attempt metrics (outcome, duration) and retry counts published when each attempt ends
  / each retry is sent, not at request end ([B]'s probe as the test; matches the spec).

## Result

- **Limit refusals** (`rate_limit_exceeded`, `budget_exceeded`, `budget_unavailable`)
  log `limit_scope`, `limit_id` (owner ID, `global` for a global limit), `limit_type`,
  `limit` (enforced — the share), `limit_configured`, `used` (absent for
  `budget_unavailable`) and `requested` for token limits. `limits.Rejection` gained `ID`;
  an unavailable refusal now carries `Limit`/`Max` too. New counter
  `kaiak_limit_rejections_total{scope_kind,type}` (16 series, at 0 from startup).
- **Owner on every line** once authenticated: `team` + `workload`, or `user`; `stream`
  once the model passed its access check (body endpoints).
- **Relayed backend errors**: a relayed `4xx` logs `error_code` = its class
  (`upstream_client_error`, `upstream_rate_limited`) and `upstream_error_code/type` from
  its first 4 KiB (the existing identifier-only `backendErrorFields`). A `5xx` keeps
  `error_code=upstream_error` (the gateway's own answer code, also its class).
- **TTFT** measured from the answering attempt's send and published as the first content
  event arrives; `ttft_ms` on the line (same measure). Request duration still from arrival.
- **Attempt metrics at release**: `releaseAttempt` publishes outcome + duration (same
  moment as the circuit report, guarded by `released` → exactly once); retries counted
  when the retry is sent (after its slot is acquired). `kaiak_request_attempts` stays at
  request end.
- **Stop signal during boot** (`cmd/kaiak`): `stopOnSignal` wraps the boot context; a
  SIGTERM/SIGINT during snapshot retries or the first-totals wait returns from `run`
  with nil, logging `kaiak stopped reason="signal terminated during boot"`, no listener.
- **Docs**: `GATEWAY.md` — backend error bodies (4xx fields), metric table (TTFT, limit
  rejections, retries), upstream attempts timing, series at 0, cardinality, log fields,
  Lifecycle (stop during boot). All dated 2026-09-25.

Failing-first tests (each red before its change, output kept in the session):

- `server/TestLimitRefusalsLogTheLimit` — before: no `limit_*`, owner or `stream` fields,
  no `kaiak_limit_rejections_total`.
- `server/TestBudgetUnavailableLogsTheLimit` — before: no limit fields.
- `server/TestRelayedBackendErrorsLogTheirClass` — before: no `error_code`,
  `upstream_error_code/type` on a relayed 400/429.
- `server/TestTimeToFirstTokenIsTheAnsweringAttempts` — before: no `ttft_ms`/`stream`;
  the metric counted the first attempt's 300 ms timeout.
- `server/TestAttemptMetricsPublishedAsAttemptsEnd` (the independent reviewer's probe,
  both "completes" and "client leaves") — before: the failed first attempt, the retry and
  the TTFT were invisible while the retry streamed (2 s timeout).
- `cmd/kaiak/TestStopSignalDuringTheBootWaitExits` (control plane down; first totals
  withheld; 60 s boot wait) — before: `run` still booting 5 s after SIGTERM.

Suite: `GOFLAGS=-count=1 scripts/check-all.sh` → `all checks passed` (gateway race tests
incl. e2e, staticcheck, live kit, control 472 tests + lint, cross-half e2e).

## Decisions made during the step

- **`budget_unavailable` is not counted in `kaiak_limit_rejections_total`**: no caller's
  limit was hit — it is platform-side and already has `kaiak_errors_total{class}`. Its log
  line still names the limit it could not check.
- **USD limits log dollars** (`limit`, `limit_configured`, `used` as floats), like
  `cost_usd`; counts stay in the limit's unit.
- **`requested` added for token refusals** (not in the brief): it is what tells "request
  too large for the limit" (`requested > limit_configured`) from "window full".
- **`stream` logged only once the model passed its access check** on body endpoints (the
  inbound fields are parsed by then); earlier refusals omit it.
- **TTFT published at first content**, not at request end (the reviewer's fix sketch);
  the output rate stays at request end (it needs settled tokens).
- **Three control-mode `cmd/kaiak` tests changed how they stop**: they pre-loaded SIGTERM
  "taken once run serves"; a signal before the listeners bind now ends the boot, so they
  send it once the API listener is bound (`stopWhenServing`). Their assertions are
  unchanged.

## Decisions for the user to confirm

1. `budget_unavailable` excluded from `kaiak_limit_rejections_total` (counted only in
   `kaiak_errors_total{class="budget_unavailable"}`).
2. USD limit fields on the log line in dollars; the extra `requested` field for token
   refusals.
3. A relayed `4xx`'s `error_code` is its metric class (`upstream_client_error`,
   `upstream_rate_limited`), while a `5xx` keeps the gateway's `upstream_error`.
4. The stop signal during boot exits 0 with no status/flush to the control plane (nothing
   served, no usage).
5. `docs/DEPLOYMENT.md` starter alerts do not yet use `kaiak_limit_rejections_total`
   (D9 / step 5 owns the alerts page).
