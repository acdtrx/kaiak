# Step 2 — log vocabulary

**Status:** done (2026-10-05)

## Intent

Every gateway log line follows the field table step 1 settled — on stderr today,
and on OTLP once steps 3–4 land. One vocabulary for both outputs.

## Files likely touched

- `gateway/internal/server/api.go` (the request line) and every other `slog` call in
  `gateway/` whose attributes the table renames (operational events: config, circuits,
  drain, control-plane contact, limits).
- Token counts: the request line computes `gen_ai.usage.input_tokens` (plain + read +
  written) and its cache parts from the record's units, rather than renaming.
- Tests that read log fields (`server`, `e2e`, `cmd/kaiak`, `limits`, `control`, …).
- `scripts/live/checks.go`: the live-test kit parses the request line.
- `docs/DEPLOYMENT.md`, `docs/testing/LIVE-BACKENDS.md`, the architecture pages, the
  GUIDE — wherever a log field is named.

## Decisions made during planning

- Attribute keys stay flat dotted strings in `slog` (`"http.response.status_code"`),
  not `slog` groups: the JSON on stderr then shows the same keys OTLP will carry.
- No dual output of old and new names (no backwards compatibility).

## Acceptance criteria

- Every attribute the gateway logs is in the table, and the table's names are what
  the code writes (a test over the request line's keys for success, limit refusal,
  `401` and upstream error).
- Repo-wide grep, outside `docs/plans` and `docs/reviews`: no old field name left
  where a log field is meant.
- `scripts/check-all.sh` green (live-test kit self-test included). Suite recorded.

## Result

**What changed**

- `gateway/internal/server/api.go` — the request line follows the field table:
  every name moved; `http.request.method` keeps the convention's ten methods and
  writes `_OTHER` plus `http.request.method_original` (clipped) for any other
  (`methodAttrs`); `gen_ai.usage.input_tokens` is computed as `tokens_in +
  tokens_cached + tokens_cache_write` from the summed record units, its two cache
  parts beside it; durations (`kaiak.request.duration`,
  `kaiak.queue.wait_duration`, `kaiak.time_to_first_token`) in seconds to the
  microsecond; added `gen_ai.operation.name` (`endpoint.operationName` in
  `pipeline.go`: `chat`, `text_completion`, `embeddings`, none for the model
  endpoints), `kaiak.backend.type` once routed and `gen_ai.provider.name` only for
  `openai` (`openai`) and `azure-openai` (`azure.ai.openai`) (`providerName`);
  `limitAttrs` writes `kaiak.limit.*` (`limit` → `kaiak.limit.enforced`).
- Operational events, every `slog` call in `gateway/` (non-test): `cmd/kaiak/main.go`,
  `internal/server/{drain,listener}.go`, `internal/config/loader.go`,
  `internal/control/{client,lastknowngood,spool,spooldisk,status,stream,usage}.go`,
  `internal/limits/{limits,shared}.go`, `internal/routing/{circuit,modelcheck}.go`,
  `internal/accounting/accounting.go`, `internal/state/state.go` — keys renamed per
  the operational-events table; `listening` splits the bound address into
  `server.address` (host, from `net.SplitHostPort`) and `server.port` (an int); the
  duration strings and `_ms` integers became seconds as doubles (to the
  millisecond; a config load's `kaiak.duration` to the microsecond). The control
  client's `With("control_url")` is `kaiak.control.url`; the attributes callers
  hand the apply path are `file.path` and `kaiak.config.version`. All keys are flat
  dotted strings, no `slog` groups.
- New package `gateway/internal/logattr` (with a unit test): `Seconds` (to the
  millisecond) and `SecondsMicro` (to the microsecond) — the one seconds conversion
  the six logging packages share (CODING-RULES §2). `docs/ARCHITECTURE.md` and
  `docs/architecture/gateway.html` list it beside `clip`.
- Comments naming log fields: `internal/server/metrics.go` (`errorCode` is the
  line's `error.type`), `internal/server/upstream.go` (`kaiak.relay_end`),
  `internal/metrics/ops.go` (`CountRequestError`).
- New test `internal/server/api_test.go`, `TestRequestLineKeysFollowTheFieldTable`:
  table-driven over the exact set of keys on the request line for a success, a
  success on an `azure-openai` backend (`gen_ai.provider.name`), a limit refusal, a
  `401`, a backend error status, an unreachable backend and an unknown method
  (`_OTHER`), with the values that identify each case.
- Tests reading log fields, moved to the new names and units, none weakened:
  `server` (accounting, backend_errors, circuit, cooldown, drain, keylimit, limits,
  listener, pathmissing, queue, retry, retrybudget, server, timeouts, upstream,
  visibility; negative checks such as "no `queue_wait_ms`" now name the new key),
  `accounting`, `config/loader`, `control` (client, memory, seed, usage),
  `limits` (limits, shared — the outage line now asserts 60.001 s since contact,
  60 s grace, 30.001 s lasted), `routing/circuit`, `state`, `cmd/kaiak` (main,
  stateless), and the e2e (backendtypes, control, e2e, grouptree, media,
  reliability, sample, shared, startup, stateless, timeouts, zeroseries). Input
  token expectations are the whole prompt (`gen_ai.usage.input_tokens` 100 for 60
  plain + 40 read; 2036 for 3 + 2033 written; the e2e tier and cache-write cases
  `c.prompt`). The e2e harness reads listener URLs through a `listenerURL` helper
  joining `server.address` and `server.port`.
- Live-test kit: `scripts/live/process.go` (the same `listenerURL`),
  `checks.go` (the usage-log check reads `gen_ai.usage.input_tokens` for "no input
  tokens"; the failure hint prints `error.type` and
  `kaiak.upstream.error.message`), `twobackends.go`.
- Docs: `docs/DEPLOYMENT.md` (the `draining` line's times in seconds, the limit
  refusal's fields, Config load cost's `kaiak.duration`/`kaiak.config.size`, the
  Logs bullet rewritten around the new names); `docs/testing/LIVE-BACKENDS.md`
  (`kaiak.usage.estimated`, `kaiak.backend.id`, `error.type`,
  `kaiak.upstream.error.message`, the cache-token fields).

**Decisions made during the step**

- `kaiak.upstream.error.message` is written for upstream failures (connection,
  timeout, broken relay), not for a backend error status: the code only ever set it
  for those, and the table's "an upstream failure" reads the same way. The new
  test pins it: a backend `500` with an error body carries
  `kaiak.upstream.error.code`/`.type` and no message.
- The `401` line carries `gen_ai.operation.name` (the path matched a body endpoint)
  and no model (authentication runs before the body is read), as the table's
  presence rules give it.
- A method-not-allowed or unknown-path refusal carries no `gen_ai.operation.name`:
  it is refused before an endpoint is matched.
- `server.address` comes from `net.SplitHostPort` on the listener's address, so an
  IPv6 zone survives; `server.port` is a number.
- The outage line's durations lose their old rounding to whole seconds (the spec
  fixes the millisecond for operational events).
- The live kit's pass message prints `in … (cache read …, cache write …), out …
  (reasoning …)`, matching the new meaning of input.
- Not a log field, left as is: `docs/BACKLOG.md:318` (`status` and `error_code` on
  usage records, in a shelved plan's history), the units `tokens_*` (usage records,
  config prices, metrics), metric labels (`backend`, `deployment_model`, `key_id`),
  the warning's message text naming the config field `max_request_body_bytes`.

**Grep** — `git grep -nw` for every old name of the two tables' Was columns,
outside `docs/plans` and `docs/reviews`:

- Distinctive names (`latency_ms`, `ttft_ms`, `queue_wait_ms`, `error_code`,
  `relay_end`, `retry_refused`, `upstream_error*`, `auth_failure`, `limit_*`,
  `duration_ms`, `lasted_ms`, `open_ms`, `wait_ms`, `waited_ms`, `boot_wait_ms`,
  `delay_ms`, `cut_off`, `in_flight`, `kept_bytes`, `bound_bytes`,
  `found_version`, `want_version`, `circuits_half_open`, `instance_id`,
  `running_config`, `flush_reserve`, `cut_after`, `since_contact`,
  `usage_waiting`, `kept_records`, `next_sequence`, `batch_instance`,
  `applied_sequence`, `previous_epoch`, `from_models`, `output_default`,
  `last_error`, `config_file`, `control_url`, `data_dir`, `body_memory_bytes`,
  `request_id`, `key_id`, `deployment_model`, `tokens_*`, `cost_usd`): every hit
  left is a usage-record or protocol field (`request_id`, `key_id`,
  `next_sequence`, `live_gateways`, `window_start`, `gateway_time`, the
  `tokens_*` units), a config field or price unit, a metric name or label, the
  spec's own Was columns, or `GATEWAY.md`'s non-log mentions (error codes such as
  `upstream_error`); none names a log field.
- Generic names (`status`, `backend`, `model`, `error`, `file`, `trigger`,
  `reason`, `group`, …): too common to grep usefully, so the code side was checked
  instead — a scan of every string literal inside the gateway's log calls and
  attribute lists finds only messages, values and dotted keys, and the set of keys
  equals the two tables' attributes, less the three step 3–4 add
  (`kaiak.log_export.failed|dropped|endpoint`). Tests, the e2e harness, the live
  kit and docs were read for the generic names at every log-reading site
  (`logLine`, `logFields`, `settled`, `msg(…)`, `line[…]`, text-handler
  substrings).

**Suite** — `scripts/check-all.sh`, green:

```
==> gofmt / go vet / staticcheck 2026.2.1 (gateway)
==> go test -race (gateway): ok cmd/kaiak, e2e, accounting, auth, clip, config,
    control, limits, logattr, metrics, provider, routing, schemacheck, server, sse,
    state
==> gofmt / go vet / staticcheck (live-test kit); self-test passed for vllm,
    llama-server, openai, azure-openai, vllm with two backends
==> npm test (control): tests 574, pass 574, fail 0
==> npm run lint (control): boundaries ok
==> cross-half e2e: ok kaiak/e2e 53.966s
all checks passed
```
