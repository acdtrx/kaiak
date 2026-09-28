# Step 9 — metrics

**Status:** done (2026-09-24) — **phase 3 complete**: full suite green, committed.

## Intent

Prometheus text exposition on the admin port: ops metrics and usage metrics, kept
separate from usage records (principle 7).

## Files likely touched

- `gateway/internal/metrics/` — counters, gauges, histograms, text exposition writer

## Decisions made during planning

- Hand-written exposition: counter, gauge, histogram with fixed buckets; label sets
  validated at registration. No summaries.
- Ops: request latency, time to first token, output tokens/s, errors by class,
  in-flight per backend. (Circuit state and queue depth arrive with P3.)
- Usage: requests, tokens by unit, estimated cost — labels team, workload, user, key
  ID (switchable off in config), model, backend, status.
- Metric and label names follow Prometheus conventions (`kaiak_` prefix, base units,
  `_total` counters); names recorded in `GATEWAY.md`.

## Acceptance criteria

- Tests: exposition output parses as valid text format (golden test); counters move
  with requests; key-ID label disappears when switched off.
- Phase 3 end: full suite green, committed.

## Result

Commands run (2026-09-24):

- `scripts/check-gateway.sh` — gofmt, `go vet`, staticcheck 2026.2.1 clean;
  `go test -race ./...` **pass** (`cmd/kaiak`, `accounting`, `auth`, `config`,
  `limits`, `metrics`, `provider`, `routing`, `server`, `state`). Also `-count=5` on
  `server`, `metrics`, `cmd/kaiak` — pass, no flakes.
- `npm test` in `control/` — 87 tests **pass**; `npm run lint` — `tsc` clean,
  `boundaries ok`.
- Manual, built binary against the fake backend from a throwaway main (deleted with
  every scratch file): workload key with 3 requests/min, model `qwen` priced 10 / 20
  USD per million, model `gone` on a backend with nothing listening. Chat `200`,
  stream `200`, `gone` → `502 upstream_unavailable`, fourth → `429`; API port
  `/metrics` → `404`; admin `/metrics` → `Content-Type: text/plain; version=0.0.4;
  charset=utf-8`, 149 lines, including:

  ```
  kaiak_errors_total{class="rate_limited"} 1
  kaiak_errors_total{class="upstream_unavailable"} 1
  kaiak_request_duration_seconds_count{endpoint="chat_completions",model="qwen",status_class="2xx"} 2
  kaiak_time_to_first_token_seconds_count{model="qwen",backend="fake"} 1
  kaiak_usage_requests_total{team="research",workload="wl",key_id="k-wl",model="gone",backend="down",status="partial"} 1
  kaiak_usage_tokens_total{team="research",workload="wl",key_id="k-wl",model="qwen",backend="fake",status="complete",unit="tokens_out"} 8
  kaiak_usage_cost_usd_total{team="research",workload="wl",key_id="k-wl",model="qwen",backend="fake",status="complete"} 0.0003
  kaiak_backend_in_flight_requests{backend="fake"} 0
  ```

  SIGHUP → `kaiak_config_loads_total{trigger="sighup",result="applied"} 1`.

Delivered:

- `gateway/internal/metrics`: `Registry` (`Counter`, `ScaledCounter` — integer
  sub-units written as count ÷ divisor, `Gauge`, `GaugeFunc` — read at scrape time,
  `Histogram`); names and label sets validated at registration, label-value count at
  use (both panic: programming errors); atomic values per series, series lookup under
  a read lock; `WriteText` / `Handler` (format 0.0.4, sorted families and series,
  escaping, invalid UTF-8 replaced). `Ops` (`NewOps(reg, router, holder)`: request
  duration, first token, decode rate, errors, in-flight gauge, config loads and
  timestamp, build info). `UsageSink` (`NewUsageSink(reg, holder)`).
- `gateway/internal/server`: `/metrics` on the admin handler (`NewAdmin(holder,
  reg)`); ops metrics observed in the first-registered finisher (after settlement,
  before the log line) and for refused paths; `endpointNone` as the zero endpoint;
  error-code → class mapping; `NewAPI` takes `*metrics.Ops`.
- `gateway/internal/accounting`: `Meter.StreamContentSeen()` (first-token moment).
- `cmd/kaiak`: registry, ops and usage sink built and wired; the usage sink is the
  fan-out's first sink; `loadConfig` counts every load (startup and SIGHUP).
- Docs: `GATEWAY.md` Observability — metric table, label rules, error classes,
  cardinality, principle 7, key-ID switch; sinks bullet; `ARCHITECTURE.md` package
  map.

Decisions beyond the plan:

- **Principle 7** — the usage sink is fed the settled record (one settlement, so
  dashboards and billing never count tokens two ways) but keeps its own in-memory
  state; nothing reads metrics back into a record. Recorded in `GATEWAY.md`.
- **Key-ID switch** — read from the live config at each record; off → empty value →
  label absent on new series; old series stay until restart (no counter drops).
- **Empty label values are left out** of the exposition (Prometheus-equivalent),
  so absent owner parts and a switched-off key ID produce no label at all.
- **Only config names as label values** — ops `model` only after the access check,
  `endpoint` only for a matched path; clients cannot mint series.
- **Usage `status`** is `complete` / `partial` from the record: the record carries no
  HTTP status, and adding one would change the protocol for a dashboard label. HTTP
  outcome lives in the ops metrics (`status_class`, error classes).
- **Cost** exposed as `kaiak_usage_cost_usd_total` (float in USD), counted as integer
  nano-dollars so sums stay exact.
- **Time to first token** — streams only (first event with generated content, role-
  only chunks excluded); a non-stream answer arrives whole, so its duration is its
  latency.
- **Output tokens/s** — a per-request histogram of decode speed for streams that ran
  to their end, not a counter-derived rate: aggregate throughput is already
  `rate(kaiak_usage_tokens_total{unit="tokens_out"})`, while per-request decode speed
  is what shows a slow backend.
- **`kaiak_config_loads_total{trigger,result}`** instead of "reloads": startup loads
  count too (a fresh process shows `applied 1`).
- Relayed backend error statuses count as `upstream_error`, including backend `4xx`.
- The limiter's `Usage()` is not exported as metrics — not in the step's list.
